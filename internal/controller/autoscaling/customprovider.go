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
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// decisionsAPIVersion is the only decision-server response schema this
	// client understands. The server declares its schema in the payload's
	// apiVersion, so that field — not the shape of what follows — is what says
	// how to read the body. An unrecognised value is refused rather than parsed
	// hopefully: a future schema is free to reuse "replicas" for something that
	// is not an absolute count, and quietly scaling a fleet off a
	// misinterpreted number is worse than not scaling at all.
	decisionsAPIVersion = "llmscaling.inference.x-k8s.io/v1alpha1"

	// defaultDecisionsPath is the request path used when
	// customProvider.path is empty.
	defaultDecisionsPath = "/decisions"
)

// decisionsResponse is the part of the decision-server payload we read. Fields
// outside replicas.active are carried only to identify which decision is ours.
type decisionsResponse struct {
	APIVersion string     `json:"apiVersion"`
	Decisions  []decision `json:"decisions"`
}

// decision is one service's scaling decision.
type decision struct {
	Namespace string `json:"namespace"`
	ServiceID string `json:"serviceId"`
	Replicas  struct {
		// Active is a pointer so an omitted count stays distinguishable from a
		// deliberate 0. The difference matters: 0 is a real recommendation that
		// clamps the fleet to minReplicas, while a missing field means the
		// server had nothing to say and the sync must be skipped.
		//
		// int64 rather than int32 so an out-of-range count reaches the range
		// check below with its own error, instead of failing the whole decode
		// with an overflow message that names no service.
		Active *int64 `json:"active"`
	} `json:"replicas"`
}

// queryDecisionReplicas asks the custom metric provider what replica count it
// recommends for serviceID, and returns decisions[].replicas.active from the
// matching decision.
//
// Unlike a Prometheus sample this is the recommendation itself, not an input to
// the HPA ratio — it is an absolute count of replicas, so the caller must not
// scale it by readyReplicas.
//
// namespace, when non-empty, further narrows the match. Matching is applied to
// the reply even though serviceId was already sent as a query parameter: the
// server decides what to answer with, and a decision for a service we did not
// ask about must not be able to drive this scaler. Anything other than exactly
// one match is an error — the same rule the Prometheus path applies to a
// multi-series result, and for the same reason, since picking one of several
// decisions would silently drive the fleet off whichever the server happened to
// list first.
func queryDecisionReplicas(serverAddress, path, serviceID, namespace string, headers map[string]string) (int32, error) {
	if serverAddress == "" {
		return 0, fmt.Errorf("serverAddress is empty")
	}
	if serviceID == "" {
		return 0, fmt.Errorf("customProvider.serviceId is empty")
	}
	if path == "" {
		path = defaultDecisionsPath
	}

	query := url.Values{"serviceId": []string{serviceID}}
	requestURL := fmt.Sprintf("%s/%s?%s",
		strings.TrimRight(serverAddress, "/"), strings.TrimLeft(path, "/"), query.Encode())

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	applyHeaders(req, headers)

	httpClient := http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to query decision server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("decision server returned status %d: %s", resp.StatusCode, string(body))
	}

	var decisions decisionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&decisions); err != nil {
		return 0, fmt.Errorf("failed to decode decision response: %w", err)
	}
	if decisions.APIVersion != decisionsAPIVersion {
		return 0, fmt.Errorf("unsupported decision schema %q, this operator only understands %q",
			decisions.APIVersion, decisionsAPIVersion)
	}

	match, err := selectDecision(decisions.Decisions, serviceID, namespace)
	if err != nil {
		return 0, err
	}

	active := match.Replicas.Active
	if active == nil {
		return 0, fmt.Errorf("decision for serviceId %q carries no replicas.active", serviceID)
	}
	if *active < 0 || *active > math.MaxInt32 {
		return 0, fmt.Errorf("decision for serviceId %q recommends %d replicas, which is out of range", serviceID, *active)
	}
	return int32(*active), nil
}

// selectDecision picks the one decision addressed to serviceID (and namespace,
// when set) out of the reply, erring on none and on more than one.
func selectDecision(decisions []decision, serviceID, namespace string) (decision, error) {
	matches := make([]decision, 0, 1)
	for _, d := range decisions {
		if d.ServiceID != serviceID {
			continue
		}
		if namespace != "" && d.Namespace != namespace {
			continue
		}
		matches = append(matches, d)
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return decision{}, fmt.Errorf("no decision for serviceId %q%s among the %d returned",
			serviceID, forNamespace(namespace), len(decisions))
	default:
		return decision{}, fmt.Errorf("%d decisions match serviceId %q%s, expected 1 — set customProvider.namespace to disambiguate; the first two are for namespaces %q and %q",
			len(matches), serviceID, forNamespace(namespace), matches[0].Namespace, matches[1].Namespace)
	}
}

// forNamespace renders the namespace filter for an error message, or "" when no
// filter is set.
func forNamespace(namespace string) string {
	if namespace == "" {
		return ""
	}
	return fmt.Sprintf(" in namespace %q", namespace)
}
