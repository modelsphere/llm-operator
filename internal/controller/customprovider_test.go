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

	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "github.com/modelsphere/llm-operator/api/v1alpha1"
)

const (
	// sampleServiceID is the serviceId from the decision server's own example.
	sampleServiceID = "fallback-modelforge-01"

	// otherNamespace stands in for a second namespace reporting the same
	// serviceId, which is the case customProvider.namespace exists to resolve.
	otherNamespace = "staging"
)

// decisionBody wraps decisions in the envelope the server sends, so each test
// only has to spell out the part it is about. Format verbs in decisions are
// filled from args.
func decisionBody(decisions string, args ...any) string {
	return fmt.Sprintf(`{"apiVersion":%q,"decisions":[%s]}`,
		decisionsAPIVersion, fmt.Sprintf(decisions, args...))
}

// TestQueryDecisionReplicas covers how a decision payload is resolved to a
// replica count: the schema must be one we know, exactly one decision must
// address us, and the count must be present and sane. Everything else is an
// error the caller skips the sync on rather than a zero.
func TestQueryDecisionReplicas(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		body      string
		want      int32
		wantErr   string
	}{
		{
			name: "a matching decision resolves to its active count",
			body: decisionBody(`{"namespace":%q,"serviceId":"svc","replicas":{"active":15}}`, decisionNamespace),
			want: 15,
		},
		{
			name: "zero is a decision, not a missing one",
			body: decisionBody(`{"namespace":%q,"serviceId":"svc","replicas":{"active":0}}`, decisionNamespace),
			want: 0,
		},
		{
			name:    "a decision with no active count is not a zero",
			body:    decisionBody(`{"namespace":%q,"serviceId":"svc","replicas":{}}`, decisionNamespace),
			wantErr: "carries no replicas.active",
		},
		{
			name:    "a negative count is refused",
			body:    decisionBody(`{"serviceId":"svc","replicas":{"active":-1}}`),
			wantErr: "out of range",
		},
		{
			name:    "a count past int32 is refused rather than wrapped",
			body:    decisionBody(`{"serviceId":"svc","replicas":{"active":4294967296}}`),
			wantErr: "out of range",
		},
		{
			name:    "an unknown schema is refused, not parsed hopefully",
			body:    `{"apiVersion":"llmscaling.inference.x-k8s.io/v2","decisions":[{"serviceId":"svc","replicas":{"active":3}}]}`,
			wantErr: "unsupported decision schema",
		},
		{
			name:    "a payload with no apiVersion is refused",
			body:    `{"decisions":[{"serviceId":"svc","replicas":{"active":3}}]}`,
			wantErr: "unsupported decision schema",
		},
		{
			name:    "an empty decision list is not a zero",
			body:    decisionBody(``),
			wantErr: `no decision for serviceId "svc"`,
		},
		{
			// The server answers with what it likes; a decision for someone else
			// must not drive this scaler just because it came back.
			name:    "a decision for another service does not match",
			body:    decisionBody(`{"namespace":%q,"serviceId":"other","replicas":{"active":9}}`, decisionNamespace),
			wantErr: `no decision for serviceId "svc"`,
		},
		{
			name: "the namespace filter picks between same-named services",
			body: decisionBody(`{"namespace":%q,"serviceId":"svc","replicas":{"active":2}},
			                    {"namespace":%q,"serviceId":"svc","replicas":{"active":7}}`,
				otherNamespace, decisionNamespace),
			namespace: decisionNamespace,
			want:      7,
		},
		{
			name: "duplicates are an error, not a pick-first",
			body: decisionBody(`{"namespace":%q,"serviceId":"svc","replicas":{"active":2}},
			                    {"namespace":%q,"serviceId":"svc","replicas":{"active":7}}`,
				otherNamespace, decisionNamespace),
			wantErr: "2 decisions match",
		},
		{
			name:      "the namespace filter is reported when nothing matches",
			body:      decisionBody(`{"namespace":%q,"serviceId":"svc","replicas":{"active":2}}`, otherNamespace),
			namespace: decisionNamespace,
			wantErr:   fmt.Sprintf("in namespace %q", decisionNamespace),
		},
		{
			name:    "a body that is not JSON is an error",
			body:    `<html>502 Bad Gateway</html>`,
			wantErr: "failed to decode",
		},
	}

	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(tt.body))
		}))

		got, err := queryDecisionReplicas(srv.URL, "", "svc", tt.namespace, nil)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", tt.name, err)
		case tt.wantErr == "" && got != tt.want:
			t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
		case tt.wantErr != "" && err == nil:
			t.Errorf("%s: expected an error containing %q, got value %d", tt.name, tt.wantErr, got)
		case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
			t.Errorf("%s: error = %q, want it to contain %q", tt.name, err, tt.wantErr)
		}
		srv.Close()
	}
}

// TestQueryDecisionReplicasSamplePayload runs a real reply from the decision
// server through the client verbatim, so the parser is pinned against the wire
// format and not only against the fixtures the table above builds.
func TestQueryDecisionReplicasSamplePayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
  "apiVersion": "llmscaling.inference.x-k8s.io/v1alpha1",
  "decisions": [
    {
      "namespace": "modelforge",
      "serviceId": "fallback-modelforge-01",
      "replicas": {
        "active": 15
      }
    }
  ]
}`))
	}))
	defer srv.Close()

	got, err := queryDecisionReplicas(srv.URL, "", sampleServiceID, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 15 {
		t.Errorf("got %d replicas, want 15", got)
	}
}

// TestQueryDecisionReplicasRequest pins the request the server actually sees:
// the documented path and query parameter, plus any configured headers.
func TestQueryDecisionReplicasRequest(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		trailingSlash bool
		wantPath      string
	}{
		{name: "an empty path falls back to the default", wantPath: defaultDecisionsPath},
		{name: "a configured path is used", path: "/v2/decisions", wantPath: "/v2/decisions"},
		{name: "a path without a leading slash still forms one", path: "decisions", wantPath: defaultDecisionsPath},
		{name: "a trailing slash on serverAddress is not doubled", path: defaultDecisionsPath, trailingSlash: true, wantPath: defaultDecisionsPath},
	}

	for _, tt := range tests {
		var gotPath, gotServiceID, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			gotPath = req.URL.Path
			gotServiceID = req.URL.Query().Get("serviceId")
			gotAuth = req.Header.Get("Authorization")
			_, _ = w.Write([]byte(decisionBody(`{"serviceId":%q,"replicas":{"active":4}}`, sampleServiceID)))
		}))

		address := srv.URL
		if tt.trailingSlash {
			address += "/"
		}
		got, err := queryDecisionReplicas(address, tt.path, sampleServiceID, "",
			map[string]string{"Authorization": "Bearer t"})
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tt.name, err)
		}
		if got != 4 {
			t.Errorf("%s: got %d replicas, want 4", tt.name, got)
		}
		if gotPath != tt.wantPath {
			t.Errorf("%s: path = %q, want %q", tt.name, gotPath, tt.wantPath)
		}
		if gotServiceID != sampleServiceID {
			t.Errorf("%s: serviceId = %q, want %q", tt.name, gotServiceID, sampleServiceID)
		}
		if gotAuth != "Bearer t" {
			t.Errorf("%s: Authorization = %q, want %q", tt.name, gotAuth, "Bearer t")
		}
		srv.Close()
	}
}

// TestQueryDecisionReplicasNon200 checks that a non-OK reply is an error even
// when its body happens to be a well-formed decision payload.
func TestQueryDecisionReplicasNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(decisionBody(`{"serviceId":"svc","replicas":{"active":3}}`)))
	}))
	defer srv.Close()

	_, err := queryDecisionReplicas(srv.URL, "", "svc", "", nil)
	if err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Errorf("error = %v, want it to mention status 503", err)
	}
}

// TestQueryDecisionReplicasMisconfigured covers the two settings that make a
// request impossible to build. They are caught before the round trip so the
// error names the field rather than the URL.
func TestQueryDecisionReplicasMisconfigured(t *testing.T) {
	if _, err := queryDecisionReplicas("", "", "svc", "", nil); err == nil ||
		!strings.Contains(err.Error(), "serverAddress") {
		t.Errorf("empty serverAddress: error = %v, want it to name serverAddress", err)
	}
	if _, err := queryDecisionReplicas("http://example.invalid", "", "", "", nil); err == nil ||
		!strings.Contains(err.Error(), "serviceId") {
		t.Errorf("empty serviceId: error = %v, want it to name serviceId", err)
	}
}

// TestRecommendReplicasCustomProvider pins the semantic that separates the two
// providers: the decision server's count is an absolute replica count, so it is
// used as-is rather than multiplied by readyReplicas the way a per-replica
// Prometheus sample is. The CRD bounds still apply on top of it — the server
// recommends, this operator keeps the rail.
func TestRecommendReplicasCustomProvider(t *testing.T) {
	tests := []struct {
		name               string
		active             string
		min, max           int32
		readyReplicas      int64
		want               int32
		unknownAPIVersion  bool
		absentCustomConfig bool
	}{
		{
			name: "the decision is used as-is, not scaled by readyReplicas",
			// 8 ready replicas would turn a per-replica metric into a much larger
			// number; a decision of 15 is already the answer.
			active: "15", min: 1, max: 20, readyReplicas: 8, want: 15,
		},
		{
			name:   "a decision above maxReplicas is clamped",
			active: "15", min: 1, max: 5, readyReplicas: 8, want: 5,
		},
		{
			name:   "a decision of zero clamps up to minReplicas",
			active: "0", min: 2, max: 20, readyReplicas: 8, want: 2,
		},
		{
			name:   "an unreadable decision holds the fleet where it is",
			active: "15", min: 1, max: 20, readyReplicas: 8, want: 8,
			unknownAPIVersion: true,
		},
		{
			name:   "a missing customProvider holds the fleet where it is",
			active: "15", min: 1, max: 20, readyReplicas: 8, want: 8,
			absentCustomConfig: true,
		},
	}

	for _, tt := range tests {
		apiVersion := decisionsAPIVersion
		if tt.unknownAPIVersion {
			apiVersion = "some.other.api/v1"
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(fmt.Appendf(nil,
				`{"apiVersion":%q,"decisions":[{"namespace":%q,"serviceId":"svc","replicas":{"active":%s}}]}`,
				apiVersion, decisionNamespace, tt.active))
		}))

		scaler := &autoscalingv1alpha1.LLMScaler{
			Spec: autoscalingv1alpha1.LLMScalerSpec{
				MetricProvider: autoscalingv1alpha1.MetricProviderCustom,
				ServerAddress:  srv.URL,
				CustomProvider: &autoscalingv1alpha1.CustomProviderSpec{ServiceID: "svc"},
				MinReplicas:    tt.min,
				MaxReplicas:    tt.max,
			},
		}
		if tt.absentCustomConfig {
			scaler.Spec.CustomProvider = nil
		}

		r := &LLMScalerReconciler{}
		key := types.NamespacedName{Namespace: "ns", Name: "s"}
		// specReplicas is what "hold where it is" means, so it is set to the same
		// figure as readyReplicas to keep the failure cases unambiguous.
		got := r.recommendReplicas(context.Background(), scaler, key, tt.readyReplicas, tt.readyReplicas)
		if got != tt.want {
			t.Errorf("%s: recommendReplicas = %d, want %d", tt.name, got, tt.want)
		}
		srv.Close()
	}
}

// TestRecommendReplicasDefaultProviderIsPrometheus checks that an LLMScaler with
// no metricProvider set still goes down the PromQL path — the field defaults to
// Prometheus at admission, but objects reaching the controller from a test or an
// older CRD may carry it empty.
func TestRecommendReplicasDefaultProviderIsPrometheus(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[0,"1.6"]}]}}`))
	}))
	defer srv.Close()

	scaler := &autoscalingv1alpha1.LLMScaler{
		Spec: autoscalingv1alpha1.LLMScalerSpec{
			ServerAddress: srv.URL,
			MinReplicas:   1,
			MaxReplicas:   20,
			Metrics: []autoscalingv1alpha1.MetricSpec{
				{Name: "kv-cache", Query: "avg(kv)", Target: "0.8"},
			},
		},
	}

	r := &LLMScalerReconciler{}
	key := types.NamespacedName{Namespace: "ns", Name: "s"}
	// ceil(4 * 1.6/0.8) = 8, i.e. the per-replica ratio the Custom path skips.
	if got := r.recommendReplicas(context.Background(), scaler, key, 4, 4); got != 8 {
		t.Errorf("recommendReplicas = %d, want 8", got)
	}
	if gotPath != "/api/v1/query" {
		t.Errorf("path = %q, want the Prometheus query API", gotPath)
	}
}
