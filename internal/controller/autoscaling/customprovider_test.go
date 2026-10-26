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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/modelsphere/llm-operator/api/autoscaling/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/testutil/cases"
)

const (
	// sampleServiceID is the serviceId from the decision server's own example.
	sampleServiceID = "fallback-modelforge-01"
)

type decisionFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		ServiceID     string `json:"serviceId"`
		Namespace     string `json:"namespace"`
		Body          string `json:"body"`
		Status        int    `json:"status"`
		PinAPIVersion bool   `json:"pinAPIVersion"`
	} `json:"inputs"`
	Expected struct {
		Replicas      int32  `json:"replicas"`
		ErrorContains string `json:"errorContains"`
	} `json:"expected"`
}

func TestQueryDecisionReplicas(t *testing.T) {
	cases.Run(t, "testdata/helpers/decision-replicas", func(t *testing.T, tc decisionFile) {
		assertDecision(t, tc)
	})
}

// TestQueryDecisionReplicasSamplePayload runs a real reply from the decision
// server through the client verbatim, so the parser is pinned against the wire
// format and not only against the fixtures the table above builds.
func TestQueryDecisionReplicasSamplePayload(t *testing.T) {
	cases.Run(t, "testdata/helpers/decision-sample", func(t *testing.T, tc decisionFile) {
		if tc.Inputs.PinAPIVersion && !strings.Contains(tc.Inputs.Body, decisionsAPIVersion) {
			t.Fatalf("sample body must carry apiVersion %s", decisionsAPIVersion)
		}
		assertDecision(t, tc)
	})
}

func assertDecision(t *testing.T, tc decisionFile) {
	t.Helper()
	// Zero is an omitted status, not HTTP 200. WriteHeader panics on it.
	if tc.Inputs.Status < 100 || tc.Inputs.Status > 999 {
		t.Fatalf("status is required, got %d", tc.Inputs.Status)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(tc.Inputs.Status)
		_, _ = w.Write([]byte(tc.Inputs.Body))
	}))
	defer srv.Close()

	got, err := queryDecisionReplicas(srv.URL, "", tc.Inputs.ServiceID, tc.Inputs.Namespace, nil)
	// An empty errorContains means the call succeeds. A case that expects a
	// failure names the substring; an empty string would match every error.
	if tc.Expected.ErrorContains == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != tc.Expected.Replicas {
			t.Fatalf("got %d, want %d", got, tc.Expected.Replicas)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), tc.Expected.ErrorContains) {
		t.Fatalf("error = %v, want it to contain %q", err, tc.Expected.ErrorContains)
	}
}

type decisionRequestFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		ServiceID     string            `json:"serviceId"`
		Path          string            `json:"path"`
		TrailingSlash bool              `json:"trailingSlash"`
		Headers       map[string]string `json:"headers"`
	} `json:"inputs"`
	Expected struct {
		Replicas       int32  `json:"replicas"`
		Path           string `json:"path"`
		ServiceID      string `json:"serviceId"`
		Authorization  string `json:"authorization"`
		PinDefaultPath bool   `json:"pinDefaultPath"`
	} `json:"expected"`
}

// TestQueryDecisionReplicasRequest pins the request the server actually sees:
// the documented path and query parameter, plus any configured headers.
func TestQueryDecisionReplicasRequest(t *testing.T) {
	cases.Run(t, "testdata/helpers/decision-request", func(t *testing.T, tc decisionRequestFile) {
		if tc.Expected.PinDefaultPath && tc.Expected.Path != defaultDecisionsPath {
			t.Fatalf("path %q, want default %q", tc.Expected.Path, defaultDecisionsPath)
		}
		var gotPath, gotServiceID, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			gotPath = req.URL.Path
			gotServiceID = req.URL.Query().Get("serviceId")
			gotAuth = req.Header.Get("Authorization")
			_, _ = fmt.Fprintf(w, `{"apiVersion":%q,"decisions":[{"serviceId":%q,"replicas":{"active":%d}}]}`,
				decisionsAPIVersion, tc.Inputs.ServiceID, tc.Expected.Replicas)
		}))
		defer srv.Close()

		address := srv.URL
		if tc.Inputs.TrailingSlash {
			address += "/"
		}
		got, err := queryDecisionReplicas(address, tc.Inputs.Path, tc.Inputs.ServiceID, "", tc.Inputs.Headers)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != tc.Expected.Replicas {
			t.Fatalf("got %d replicas, want %d", got, tc.Expected.Replicas)
		}
		if gotPath != tc.Expected.Path {
			t.Fatalf("path = %q, want %q", gotPath, tc.Expected.Path)
		}
		if gotServiceID != tc.Expected.ServiceID {
			t.Fatalf("serviceId = %q, want %q", gotServiceID, tc.Expected.ServiceID)
		}
		if gotAuth != tc.Expected.Authorization {
			t.Fatalf("Authorization = %q, want %q", gotAuth, tc.Expected.Authorization)
		}
	})
}

type decisionNon200File struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		ServiceID string `json:"serviceId"`
		Status    int    `json:"status"`
		Body      string `json:"body"`
	} `json:"inputs"`
	Expected struct {
		ErrorContains string `json:"errorContains"`
	} `json:"expected"`
}

// TestQueryDecisionReplicasNon200 checks that a non-OK reply is an error even
// when its body happens to be a well-formed decision payload.
func TestQueryDecisionReplicasNon200(t *testing.T) {
	cases.Run(t, "testdata/helpers/decision-non-200", func(t *testing.T, tc decisionNon200File) {
		// Zero is an omitted status, and WriteHeader panics on it.
		if tc.Inputs.Status < 100 || tc.Inputs.Status > 999 {
			t.Fatalf("status is required, got %d", tc.Inputs.Status)
		}
		// An empty substring matches every error.
		if tc.Expected.ErrorContains == "" {
			t.Fatal("errorContains is required")
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.Inputs.Status)
			_, _ = w.Write([]byte(tc.Inputs.Body))
		}))
		defer srv.Close()

		_, err := queryDecisionReplicas(srv.URL, "", tc.Inputs.ServiceID, "", nil)
		if err == nil || !strings.Contains(err.Error(), tc.Expected.ErrorContains) {
			t.Errorf("error = %v, want it to contain %q", err, tc.Expected.ErrorContains)
		}
	})
}

type decisionMisconfigFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		ServerAddress string `json:"serverAddress"`
		ServiceID     string `json:"serviceId"`
	} `json:"inputs"`
	Expected struct {
		ErrorContains string `json:"errorContains"`
	} `json:"expected"`
}

// TestQueryDecisionReplicasMisconfigured covers the two settings that make a
// request impossible to build. They are caught before the round trip so the
// error names the field rather than the URL.
func TestQueryDecisionReplicasMisconfigured(t *testing.T) {
	cases.Run(t, "testdata/helpers/decision-misconfigured", func(t *testing.T, tc decisionMisconfigFile) {
		// An empty substring matches every error.
		if tc.Expected.ErrorContains == "" {
			t.Fatal("errorContains is required")
		}
		_, err := queryDecisionReplicas(tc.Inputs.ServerAddress, "", tc.Inputs.ServiceID, "", nil)
		if err == nil || !strings.Contains(err.Error(), tc.Expected.ErrorContains) {
			t.Errorf("error = %v, want it to contain %q", err, tc.Expected.ErrorContains)
		}
	})
}

type recommendCustomFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Active               string `json:"active"`
		MinReplicas          int32  `json:"minReplicas"`
		MaxReplicas          int32  `json:"maxReplicas"`
		ReadyReplicas        int64  `json:"readyReplicas"`
		SpecReplicas         int64  `json:"specReplicas"`
		ServiceID            string `json:"serviceId"`
		Namespace            string `json:"namespace"`
		APIVersion           string `json:"apiVersion"`
		PinAPIVersion        bool   `json:"pinAPIVersion"`
		AbsentCustomProvider bool   `json:"absentCustomProvider"`
	} `json:"inputs"`
	Expected struct {
		Replicas            int32  `json:"replicas"`
		HoldReason          string `json:"holdReason"`
		HoldMessageContains string `json:"holdMessageContains"`
	} `json:"expected"`
}

// TestRecommendReplicasCustomProvider pins the semantic that separates the two
// providers: the decision server's count is an absolute replica count, so it is
// used as-is rather than multiplied by readyReplicas the way a per-replica
// Prometheus sample is. The CRD bounds still apply on top of it — the server
// recommends, this operator keeps the rail.
func TestRecommendReplicasCustomProvider(t *testing.T) {
	cases.Run(t, "testdata/helpers/recommend-custom", func(t *testing.T, tc recommendCustomFile) {
		in := tc.Inputs
		if in.PinAPIVersion && in.APIVersion != decisionsAPIVersion {
			t.Fatalf("apiVersion %q, want %q", in.APIVersion, decisionsAPIVersion)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w,
				`{"apiVersion":%q,"decisions":[{"namespace":%q,"serviceId":%q,"replicas":{"active":%s}}]}`,
				in.APIVersion, in.Namespace, in.ServiceID, in.Active)
		}))
		defer srv.Close()

		scaler := &autoscalingv1alpha1.LLMScaler{
			Spec: autoscalingv1alpha1.LLMScalerSpec{
				MetricProvider: autoscalingv1alpha1.MetricProviderCustom,
				ServerAddress:  srv.URL,
				CustomProvider: &autoscalingv1alpha1.CustomProviderSpec{ServiceID: in.ServiceID},
				MinReplicas:    in.MinReplicas,
				MaxReplicas:    in.MaxReplicas,
			},
		}
		if in.AbsentCustomProvider {
			scaler.Spec.CustomProvider = nil
		}

		r := &LLMScalerReconciler{}
		key := types.NamespacedName{Namespace: "ns", Name: "s"}
		// specReplicas is what "hold where it is" means. The files set it to the
		// same figure as readyReplicas so a hold is not confused with a clamp.
		got := r.recommendReplicas(context.Background(), scaler, key, in.ReadyReplicas, in.SpecReplicas).replicas
		if got != tc.Expected.Replicas {
			t.Fatalf("recommendReplicas = %d, want %d", got, tc.Expected.Replicas)
		}
		decision := meta.FindStatusCondition(scaler.Status.Conditions, autoscalingv1alpha1.ConditionDecisionAvailable)
		if decision == nil {
			t.Fatal("DecisionAvailable condition missing")
		}
		if tc.Expected.HoldReason != "" {
			if decision.Status != metav1.ConditionFalse || decision.Reason != tc.Expected.HoldReason {
				t.Errorf("DecisionAvailable = %s/%s, want False/%s", decision.Status, decision.Reason, tc.Expected.HoldReason)
			}
		} else if decision.Status != metav1.ConditionTrue || decision.Reason != autoscalingv1alpha1.ReasonDecisionReceived {
			t.Errorf("DecisionAvailable = %s/%s, want True/%s", decision.Status, decision.Reason, autoscalingv1alpha1.ReasonDecisionReceived)
		} else if decision.Message != fmt.Sprintf("decision server recommends %s replicas", in.Active) {
			// The message names the server's count, before clamping.
			t.Errorf("DecisionAvailable message = %q, want the raw decision", decision.Message)
		}
		if sub := tc.Expected.HoldMessageContains; sub != "" && !strings.Contains(decision.Message, sub) {
			t.Errorf("DecisionAvailable message = %q, want it to contain %q", decision.Message, sub)
		}
	})
}

type metricInput struct {
	Name   string `json:"name"`
	Query  string `json:"query"`
	Target string `json:"target"`
}

type recommendPromFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Sample        string        `json:"sample"`
		ReadyReplicas int64         `json:"readyReplicas"`
		SpecReplicas  int64         `json:"specReplicas"`
		MinReplicas   int32         `json:"minReplicas"`
		MaxReplicas   int32         `json:"maxReplicas"`
		Metrics       []metricInput `json:"metrics"`
	} `json:"inputs"`
	Expected struct {
		Replicas int32  `json:"replicas"`
		Path     string `json:"path"`
	} `json:"expected"`
}

// TestRecommendReplicasDefaultProviderIsPrometheus checks that an LLMScaler with
// no metricProvider set still goes down the PromQL path — the field defaults to
// Prometheus at admission, but objects reaching the controller from a test or an
// older CRD may carry it empty.
func TestRecommendReplicasDefaultProviderIsPrometheus(t *testing.T) {
	cases.Run(t, "testdata/helpers/recommend-default-prometheus", func(t *testing.T, tc recommendPromFile) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			gotPath = req.URL.Path
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"result":[{"metric":{},"value":[0,"%s"]}]}}`, tc.Inputs.Sample)
		}))
		defer srv.Close()

		metrics := make([]autoscalingv1alpha1.MetricSpec, len(tc.Inputs.Metrics))
		for i, m := range tc.Inputs.Metrics {
			metrics[i] = autoscalingv1alpha1.MetricSpec{Name: m.Name, Query: m.Query, Target: m.Target}
		}
		scaler := &autoscalingv1alpha1.LLMScaler{
			Spec: autoscalingv1alpha1.LLMScalerSpec{
				ServerAddress: srv.URL,
				MinReplicas:   tc.Inputs.MinReplicas,
				MaxReplicas:   tc.Inputs.MaxReplicas,
				Metrics:       metrics,
			},
		}

		r := &LLMScalerReconciler{}
		key := types.NamespacedName{Namespace: "ns", Name: "s"}
		got := r.recommendReplicas(context.Background(), scaler, key, tc.Inputs.ReadyReplicas, tc.Inputs.SpecReplicas).replicas
		if got != tc.Expected.Replicas {
			t.Errorf("recommendReplicas = %d, want %d", got, tc.Expected.Replicas)
		}
		decision := meta.FindStatusCondition(scaler.Status.Conditions, autoscalingv1alpha1.ConditionDecisionAvailable)
		if decision == nil || decision.Status != metav1.ConditionTrue || decision.Reason != autoscalingv1alpha1.ReasonMetricsAvailable {
			t.Errorf("DecisionAvailable = %+v, want True/%s", decision, autoscalingv1alpha1.ReasonMetricsAvailable)
		} else if !strings.Contains(decision.Message, fmt.Sprintf("recommends %d replicas", tc.Expected.Replicas)) {
			t.Errorf("DecisionAvailable message = %q, want the raw recommendation", decision.Message)
		}
		if gotPath != tc.Expected.Path {
			t.Errorf("path = %q, want %q", gotPath, tc.Expected.Path)
		}
	})
}
