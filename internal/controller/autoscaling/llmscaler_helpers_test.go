/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/modelsphere/llm-operator/api/autoscaling/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/testutil/cases"
)

// pastDeferBase is the deferredSince instant for TestPastDeferTimeout.
// Whole seconds, so nothing about the deadline depends on the clock reading.
var pastDeferBase = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// forcedWriteBase is the "last forced write" instant for TestForcedWritePace.
var forcedWriteBase = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// stabilizationBase is the origin for TestStabilizeDesired. A fixed base, not
// time.Now(), so a slow run cannot slide a window boundary.
var stabilizationBase = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

var stabilizationKey = types.NamespacedName{Namespace: "ns", Name: "s"}

// deferralRequeueSync is the sync period passed to every deferralRequeueAfter row.
const deferralRequeueSync = 15 * time.Second

type deferTimeoutFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		DeferTimeoutSeconds *int32 `json:"deferTimeoutSeconds"`
	} `json:"inputs"`
	Expected struct {
		Duration      cases.Duration `json:"duration"`
		EqualsDefault bool           `json:"equalsDefault"`
	} `json:"expected"`
}

func TestDeferTimeout(t *testing.T) {
	cases.Run(t, "testdata/helpers/defer-timeout", func(t *testing.T, tc deferTimeoutFile) {
		scaler := &autoscalingv1alpha1.LLMScaler{}
		scaler.Spec.DeferTimeoutSeconds = tc.Inputs.DeferTimeoutSeconds
		got := deferTimeout(scaler)
		if got != tc.Expected.Duration.Std() {
			t.Fatalf("got %s, want %s", got, tc.Expected.Duration.Std())
		}
		if tc.Expected.EqualsDefault {
			want := time.Duration(autoscalingv1alpha1.DefaultDeferTimeoutSeconds) * time.Second
			if got != want {
				t.Fatalf("got %s, want default %s", got, want)
			}
		}
	})
}

type pastDeferFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		NilSince bool           `json:"nilSince"`
		Timeout  cases.Duration `json:"timeout"`
		Elapsed  cases.Duration `json:"elapsed"`
	} `json:"inputs"`
	Expected struct {
		Past bool `json:"past"`
	} `json:"expected"`
}

func TestPastDeferTimeout(t *testing.T) {
	cases.Run(t, "testdata/helpers/past-defer-timeout", func(t *testing.T, tc pastDeferFile) {
		var since *metav1.Time
		if !tc.Inputs.NilSince {
			stamped := metav1.NewTime(pastDeferBase)
			since = &stamped
		}
		now := pastDeferBase.Add(tc.Inputs.Elapsed.Std())
		got := pastDeferTimeout(since, tc.Inputs.Timeout.Std(), now)
		if got != tc.Expected.Past {
			t.Fatalf("got %v, want %v", got, tc.Expected.Past)
		}
	})
}

type requeueFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Holding bool           `json:"holding"`
		Timeout cases.Duration `json:"timeout"`
		Until   cases.Duration `json:"until"`
		Pacing  cases.Duration `json:"pacing"`
	} `json:"inputs"`
	Expected struct {
		After cases.Duration `json:"after"`
	} `json:"expected"`
}

func TestDeferralRequeueAfter(t *testing.T) {
	cases.Run(t, "testdata/helpers/deferral-requeue-after", func(t *testing.T, tc requeueFile) {
		in := tc.Inputs
		got := deferralRequeueAfter(deferralRequeueSync, in.Holding, in.Timeout.Std(), in.Until.Std(), in.Pacing.Std())
		if got != tc.Expected.After.Std() {
			t.Fatalf("got %s, want %s", got, tc.Expected.After.Std())
		}
	})
}

type forcedWriteFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Recorded bool           `json:"recorded"`
		Sync     cases.Duration `json:"sync"`
		At       cases.Duration `json:"at"`
	} `json:"inputs"`
	Expected struct {
		Allowed   bool            `json:"allowed"`
		Remaining *cases.Duration `json:"remaining"`
	} `json:"expected"`
}

func TestForcedWritePace(t *testing.T) {
	cases.Run(t, "testdata/helpers/forced-write-pace", func(t *testing.T, tc forcedWriteFile) {
		at := forcedWriteBase.Add(tc.Inputs.At.Std())
		allowed, remaining := forcedWritePace(forcedWriteBase, tc.Inputs.Recorded, tc.Inputs.Sync.Std(), at)
		if allowed != tc.Expected.Allowed {
			t.Fatalf("allowed=%v, want %v", allowed, tc.Expected.Allowed)
		}
		if tc.Expected.Remaining != nil && remaining != tc.Expected.Remaining.Std() {
			t.Fatalf("remaining=%s, want %s", remaining, tc.Expected.Remaining.Std())
		}
	})
}

type memoryStep struct {
	Op string `json:"op"`
}

type memoryFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Namespace string       `json:"namespace"`
		Scaler    string       `json:"scaler"`
		At        string       `json:"at"`
		Steps     []memoryStep `json:"steps"`
	} `json:"inputs"`
	Expected struct{} `json:"expected"`
}

// TestDeferralMemoryClearedOnEndAndDelete checks the two ways the in-memory
// pace record goes away: the deferral ending, and the scaler being deleted.
func TestDeferralMemoryClearedOnEndAndDelete(t *testing.T) {
	cases.Run(t, "testdata/helpers/deferral-memory", func(t *testing.T, tc memoryFile) {
		r := &LLMScalerReconciler{}
		key := types.NamespacedName{Namespace: tc.Inputs.Namespace, Name: tc.Inputs.Scaler}
		now, err := time.Parse(time.RFC3339, tc.Inputs.At)
		if err != nil {
			t.Fatalf("at: %v", err)
		}
		scaler := &autoscalingv1alpha1.LLMScaler{}
		for _, step := range tc.Inputs.Steps {
			switch step.Op {
			case "record":
				r.recordForcedWrite(key, now)
			case "setSince":
				stamped := metav1.NewTime(now)
				scaler.Status.DeferredSince = &stamped
			case "end":
				r.endDeferral(key, scaler)
			case "forget":
				r.forgetRecommendations(key)
			case "expectSinceCleared":
				if scaler.Status.DeferredSince != nil {
					t.Fatal("endDeferral left deferredSince set")
				}
			case "expectRecordCleared":
				if _, ok := r.lastForcedWriteAt(key); ok {
					t.Fatal("forced-write record still set")
				}
			default:
				t.Fatalf("unknown step %q", step.Op)
			}
		}
	})
}

type reasonFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
	} `json:"inputs"`
	Expected struct {
		Reason string `json:"reason"`
	} `json:"expected"`
}

// TestReasonDeferralTimedOut pins the published condition reason, which users
// match on in alerts and kubectl output.
func TestReasonDeferralTimedOut(t *testing.T) {
	cases.Run(t, "testdata/helpers/reason-deferral-timed-out", func(t *testing.T, tc reasonFile) {
		if autoscalingv1alpha1.ReasonDeferralTimedOut != tc.Expected.Reason {
			t.Fatalf("ReasonDeferralTimedOut = %q, want %q", autoscalingv1alpha1.ReasonDeferralTimedOut, tc.Expected.Reason)
		}
	})
}

type stabilizationOpFile struct {
	At        cases.Duration `json:"at"`
	Recommend *int32         `json:"recommend"`
	HoldAt    *int32         `json:"holdAt"`
	Want      *int32         `json:"want"`
}

type stabilizationFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Window cases.Duration        `json:"window"`
		Ops    []stabilizationOpFile `json:"ops"`
	} `json:"inputs"`
	Expected struct{} `json:"expected"`
}

// TestStabilizeDesired runs each stabilization sequence on its own reconciler.
// The window-0 pin is a row of the same table.
func TestStabilizeDesired(t *testing.T) {
	cases.Run(t, "testdata/helpers/stabilization", func(t *testing.T, tc stabilizationFile) {
		r := &LLMScalerReconciler{}
		window := tc.Inputs.Window.Std()
		for _, op := range tc.Inputs.Ops {
			now := stabilizationBase.Add(op.At.Std())
			switch {
			case op.HoldAt != nil && op.Recommend != nil:
				t.Fatalf("op at %s sets both holdAt and recommend", op.At.Std())
			case op.HoldAt != nil:
				r.holdAfterScaleDown(stabilizationKey, *op.HoldAt, window, now)
			case op.Recommend != nil:
				if op.Want == nil {
					t.Fatalf("recommend at %s has no want", op.At.Std())
				}
				got := r.stabilizeDesired(stabilizationKey, *op.Recommend, window, now)
				if got != *op.Want {
					t.Fatalf("at %s: got %d, want %d", op.At.Std(), got, *op.Want)
				}
			default:
				t.Fatalf("op at %s has neither holdAt nor recommend", op.At.Std())
			}
		}
	})
}

type parseTargetFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Value string `json:"value"`
	} `json:"inputs"`
	Expected struct {
		Value float64 `json:"value"`
		Error bool    `json:"error"`
	} `json:"expected"`
}

func TestParseTargetValue(t *testing.T) {
	cases.Run(t, "testdata/helpers/parse-target-value", func(t *testing.T, tc parseTargetFile) {
		got, err := parseTargetValue(tc.Inputs.Value)
		if (err != nil) != tc.Expected.Error {
			t.Fatalf("parseTargetValue(%q) error = %v, wantErr %v", tc.Inputs.Value, err, tc.Expected.Error)
		}
		if err == nil && got != tc.Expected.Value {
			t.Fatalf("parseTargetValue(%q) = %v, want %v", tc.Inputs.Value, got, tc.Expected.Value)
		}
	})
}

type promScalarFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Result string `json:"result"`
		// Status, when set, is a non-200 reply. ErrorBodyBytes is the body
		// length; a marker is written at offset 1024 so a 1 KiB cap drops it.
		Status         int `json:"status"`
		ErrorBodyBytes int `json:"errorBodyBytes"`
	} `json:"inputs"`
	Expected struct {
		Value         float64  `json:"value"`
		ErrorContains []string `json:"errorContains"`
		// TruncatedBodyBytes is the exact length of the body quoted in the
		// error. The quoted bytes are 'A', and the marker past that offset
		// must be absent. That error is what DecisionAvailable and Ready quote.
		TruncatedBodyBytes int `json:"truncatedBodyBytes"`
	} `json:"expected"`
}

func TestQueryPrometheusScalar(t *testing.T) {
	cases.Run(t, "testdata/helpers/query-prometheus-scalar", func(t *testing.T, tc promScalarFile) {
		const pastLimit = "PAST-THE-LIMIT"
		var errorBody []byte
		if tc.Inputs.Status != 0 {
			if tc.Inputs.Status < 100 || tc.Inputs.Status > 999 {
				t.Fatalf("status %d", tc.Inputs.Status)
			}
			if tc.Inputs.ErrorBodyBytes <= len(pastLimit)+1024 {
				t.Fatalf("errorBodyBytes %d must extend past 1 KiB", tc.Inputs.ErrorBodyBytes)
			}
			errorBody = bytes.Repeat([]byte{'A'}, tc.Inputs.ErrorBodyBytes)
			copy(errorBody[1024:], pastLimit)
		} else if tc.Inputs.ErrorBodyBytes != 0 {
			t.Fatal("errorBodyBytes requires status")
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.Inputs.Status != 0 {
				w.WriteHeader(tc.Inputs.Status)
				_, _ = w.Write(errorBody)
				return
			}
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"result":%s}}`, tc.Inputs.Result)
		}))
		defer srv.Close()

		got, err := queryPrometheusScalar(srv.URL, "some_query", nil)
		if tc.Expected.TruncatedBodyBytes > 0 {
			if err == nil {
				t.Fatal("expected a truncated non-200 error")
			}
			prefix := fmt.Sprintf("prometheus returned status %d: ", tc.Inputs.Status)
			rest, ok := strings.CutPrefix(err.Error(), prefix)
			if !ok {
				t.Fatalf("error = %q, want prefix %q", err, prefix)
			}
			want := strings.Repeat("A", tc.Expected.TruncatedBodyBytes)
			if rest != want {
				t.Fatalf("quoted body length %d, want %d of 'A' and not %s", len(rest), tc.Expected.TruncatedBodyBytes, pastLimit)
			}
			if strings.Contains(err.Error(), pastLimit) {
				t.Fatalf("error quotes the body past 1 KiB: %q", err)
			}
			if len(tc.Expected.ErrorContains) == 0 {
				return
			}
		}
		if len(tc.Expected.ErrorContains) == 0 {
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.Expected.Value {
				t.Fatalf("got %v, want %v", got, tc.Expected.Value)
			}
			return
		}
		if err == nil {
			t.Fatalf("expected error containing %q, got value %v", tc.Expected.ErrorContains, got)
		}
		for _, want := range tc.Expected.ErrorContains {
			// An empty substring matches every error.
			if want == "" {
				t.Fatal("errorContains entry is empty")
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
	})
}

type computeDesiredFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Sample      string `json:"sample"`
		Ready       int64  `json:"ready"`
		MinReplicas int32  `json:"minReplicas"`
		MaxReplicas int32  `json:"maxReplicas"`
		MetricName  string `json:"metricName"`
		Query       string `json:"query"`
		Target      string `json:"target"`
	} `json:"inputs"`
	Expected struct {
		Have    bool  `json:"have"`
		Desired int32 `json:"desired"`
	} `json:"expected"`
}

// TestComputeDesiredFromMetrics covers non-finite samples and scale-from-zero.
func TestComputeDesiredFromMetrics(t *testing.T) {
	cases.Run(t, "testdata/helpers/compute-desired", func(t *testing.T, tc computeDesiredFile) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w,
				`{"status":"success","data":{"result":[{"metric":{},"value":[0,"%s"]}]}}`, tc.Inputs.Sample)
		}))
		defer srv.Close()

		scaler := &autoscalingv1alpha1.LLMScaler{
			Spec: autoscalingv1alpha1.LLMScalerSpec{
				ServerAddress: srv.URL,
				MinReplicas:   tc.Inputs.MinReplicas,
				MaxReplicas:   tc.Inputs.MaxReplicas,
				Metrics: []autoscalingv1alpha1.MetricSpec{{
					Name:   tc.Inputs.MetricName,
					Query:  tc.Inputs.Query,
					Target: tc.Inputs.Target,
				}},
			},
		}
		desired, haveMetric := (&LLMScalerReconciler{}).computeDesiredFromMetrics(
			context.Background(), scaler, tc.Inputs.Ready)
		if haveMetric != tc.Expected.Have {
			t.Errorf("haveMetric = %v, want %v", haveMetric, tc.Expected.Have)
		}
		if desired != tc.Expected.Desired {
			t.Errorf("desired = %d, want %d", desired, tc.Expected.Desired)
		}
		decision := meta.FindStatusCondition(scaler.Status.Conditions, autoscalingv1alpha1.ConditionDecisionAvailable)
		if decision == nil {
			t.Fatal("DecisionAvailable condition missing")
		}
		if !tc.Expected.Have {
			if decision.Status != metav1.ConditionFalse || decision.Reason != autoscalingv1alpha1.ReasonDecisionUnavailable {
				t.Errorf("DecisionAvailable = %s/%s, want False/%s", decision.Status, decision.Reason, autoscalingv1alpha1.ReasonDecisionUnavailable)
			}
			if !strings.Contains(decision.Message, "no prometheus metric could be evaluated") {
				t.Errorf("DecisionAvailable message = %q, want it to name prometheus", decision.Message)
			}
		} else {
			if decision.Status != metav1.ConditionTrue || decision.Reason != autoscalingv1alpha1.ReasonMetricsAvailable {
				t.Errorf("DecisionAvailable = %s/%s, want True/%s", decision.Status, decision.Reason, autoscalingv1alpha1.ReasonMetricsAvailable)
			}
			// The message names the raw maximum, which this helper returns before clamping.
			want := fmt.Sprintf("recommends %d replicas", desired)
			if !strings.Contains(decision.Message, want) || !strings.Contains(decision.Message, "evaluated 1 of 1 metrics") {
				t.Errorf("DecisionAvailable message = %q, want it to contain %q and the evaluation count", decision.Message, want)
			}
		}
	})
}

type scaleGuardFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Current int32 `json:"current"`
		Desired int32 `json:"desired"`
		MaxStep int32 `json:"maxStep"`
	} `json:"inputs"`
	Expected struct {
		Want int32 `json:"want"`
	} `json:"expected"`
}

func TestScaleGuard(t *testing.T) {
	cases.Run(t, "testdata/helpers/scale-guard", func(t *testing.T, tc scaleGuardFile) {
		in := tc.Inputs
		got := scaleGuard(in.Current, in.Desired, in.MaxStep)
		if got != tc.Expected.Want {
			t.Fatalf("scaleGuard(%d, %d, %d) = %d, want %d", in.Current, in.Desired, in.MaxStep, got, tc.Expected.Want)
		}
	})
}
