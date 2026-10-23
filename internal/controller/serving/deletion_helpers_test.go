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

package serving

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/testutil/cases"
)

type manifestCase struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Namespace string `json:"namespace"`
		Manifest  string `json:"manifest"`
	} `json:"inputs"`
	Expected struct {
		Error  bool `json:"error"`
		Routes []struct {
			APIVersion string `json:"apiVersion"`
			Namespace  string `json:"namespace"`
			Name       string `json:"name"`
		} `json:"routes"`
	} `json:"expected"`
}

func TestModelRoutesFromManifest(t *testing.T) {
	cases.Run(t, "testdata/helpers/modelRoutesFromManifest", func(t *testing.T, tc manifestCase) {
		got, err := modelRoutesFromManifest(tc.Inputs.Manifest, tc.Inputs.Namespace)
		if tc.Expected.Error {
			if err == nil {
				t.Fatal("expected an error")
			}
			return
		}
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got) != len(tc.Expected.Routes) {
			t.Fatalf("routes = %#v", got)
		}
		for i, want := range tc.Expected.Routes {
			if got[i].APIVersion != want.APIVersion || got[i].Namespace != want.Namespace || got[i].Name != want.Name {
				t.Fatalf("route %d = %#v, want %#v", i, got[i], want)
			}
		}
	})
}

type podCountCase struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Now     string `json:"now"`
		NodesOK bool   `json:"nodesOK"`
		Nodes   []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		} `json:"nodes"`
		Pods []struct {
			Name              string `json:"name"`
			Phase             string `json:"phase"`
			Node              string `json:"node"`
			Grace             *int64 `json:"grace"`
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"pods"`
	} `json:"inputs"`
	Expected struct {
		Remaining int    `json:"remaining"`
		Message   string `json:"message"`
		Pods      []struct {
			Name  string `json:"name"`
			Stuck bool   `json:"stuck"`
			Phase string `json:"phase"`
			Node  string `json:"node"`
			Grace int64  `json:"grace"`
		} `json:"pods"`
	} `json:"expected"`
}

func TestPodCounts(t *testing.T) {
	cases.Run(t, "testdata/helpers/podCounts", func(t *testing.T, tc podCountCase) {
		now, err := time.Parse(time.RFC3339, tc.Inputs.Now)
		if err != nil {
			t.Fatalf("now: %v", err)
		}
		nodes := map[string]bool{}
		for _, n := range tc.Inputs.Nodes {
			nodes[n.Name] = n.Ready
		}
		pods := make([]corev1.Pod, len(tc.Inputs.Pods))
		for i, in := range tc.Inputs.Pods {
			pod := corev1.Pod{}
			pod.Name = in.Name
			pod.Status.Phase = corev1.PodPhase(in.Phase)
			pod.Spec.NodeName = in.Node
			pod.Spec.TerminationGracePeriodSeconds = in.Grace
			if in.DeletionTimestamp != "" {
				ts, err := time.Parse(time.RFC3339, in.DeletionTimestamp)
				if err != nil {
					t.Fatalf("deletionTimestamp: %v", err)
				}
				pod.DeletionTimestamp = &metav1.Time{Time: ts}
			}
			pods[i] = pod
		}
		got := accountPods(pods, nodes, tc.Inputs.NodesOK, now)
		if got.remaining != tc.Expected.Remaining {
			t.Fatalf("remaining = %d, want %d", got.remaining, tc.Expected.Remaining)
		}
		if len(got.pods) != len(tc.Expected.Pods) {
			t.Fatalf("pods = %#v", got.pods)
		}
		for i, want := range tc.Expected.Pods {
			row := got.pods[i]
			if row.Name != want.Name || row.Stuck != want.Stuck || row.Phase != want.Phase || row.Node != want.Node || row.Grace != want.Grace {
				t.Fatalf("pod %d = %#v, want %#v", i, row, want)
			}
		}
		if tc.Expected.Message != "" && podWaitMessage(got.remaining, got.pods) != tc.Expected.Message {
			t.Fatalf("message = %q, want %q", podWaitMessage(got.remaining, got.pods), tc.Expected.Message)
		}
	})
}

type knobsCase struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Annotations      map[string]string `json:"annotations"`
		MaxWait          *cases.Duration   `json:"maxWait"`
		RouteSettle      *cases.Duration   `json:"routeSettle"`
		ForceDeleteAfter *cases.Duration   `json:"forceDeleteAfter"`
		ForceSet         bool              `json:"forceSet"`
	} `json:"inputs"`
	Expected struct {
		MaxWait          cases.Duration  `json:"maxWait"`
		RouteSettle      cases.Duration  `json:"routeSettle"`
		ForceDeleteAfter *cases.Duration `json:"forceDeleteAfter"`
		ForceOff         bool            `json:"forceOff"`
		Invalid          []struct {
			Key      string `json:"key"`
			Contains string `json:"contains"`
		} `json:"invalid"`
	} `json:"expected"`
}

func TestDeletionKnobs(t *testing.T) {
	cases.Run(t, "testdata/helpers/deletionKnobs", func(t *testing.T, tc knobsCase) {
		obj := &servingv1alpha1.LLMService{}
		obj.Annotations = tc.Inputs.Annotations
		if tc.Inputs.MaxWait != nil || tc.Inputs.RouteSettle != nil || tc.Inputs.ForceSet {
			obj.Spec.Deletion = &servingv1alpha1.DeletionSpec{}
			if tc.Inputs.MaxWait != nil {
				obj.Spec.Deletion.MaxWait = &metav1.Duration{Duration: tc.Inputs.MaxWait.Std()}
			}
			if tc.Inputs.RouteSettle != nil {
				obj.Spec.Deletion.RouteSettle = &metav1.Duration{Duration: tc.Inputs.RouteSettle.Std()}
			}
			if tc.Inputs.ForceSet {
				d := metav1.Duration{}
				if tc.Inputs.ForceDeleteAfter != nil {
					d.Duration = tc.Inputs.ForceDeleteAfter.Std()
				}
				obj.Spec.Deletion.ForceDeleteAfter = &d
			}
		}
		got := deletionKnobsFrom(obj)
		if got.maxWait != tc.Expected.MaxWait.Std() || got.routeSettle != tc.Expected.RouteSettle.Std() {
			t.Fatalf("maxWait %s routeSettle %s", got.maxWait, got.routeSettle)
		}
		if tc.Expected.ForceOff {
			if got.forceAfter != nil {
				t.Fatalf("forceAfter = %s, want off", *got.forceAfter)
			}
		} else if tc.Expected.ForceDeleteAfter != nil {
			if got.forceAfter == nil || *got.forceAfter != tc.Expected.ForceDeleteAfter.Std() {
				t.Fatalf("forceAfter = %v", got.forceAfter)
			}
		}
		if len(got.invalid) != len(tc.Expected.Invalid) {
			t.Fatalf("invalid = %#v", got.invalid)
		}
		for i, want := range tc.Expected.Invalid {
			if got.invalid[i].key != want.Key || !strings.Contains(got.invalid[i].why, want.Contains) {
				t.Fatalf("invalid %d = %#v", i, got.invalid[i])
			}
		}
	})
}

type stageCase struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		Stage         string `json:"stage"`
		HasRoute      bool   `json:"hasRoute"`
		RouteGone     bool   `json:"routeGone"`
		SettleDone    bool   `json:"settleDone"`
		Uninstalled   bool   `json:"uninstalled"`
		PodsRemaining int    `json:"podsRemaining"`
		PastDeadline  bool   `json:"pastDeadline"`
		ForceDue      bool   `json:"forceDue"`
	} `json:"inputs"`
	Expected struct {
		Stage string `json:"stage"`
	} `json:"expected"`
}

func TestNextStage(t *testing.T) {
	cases.Run(t, "testdata/helpers/nextStage", func(t *testing.T, tc stageCase) {
		got := nextStage(stageInput{
			Stage:         servingv1alpha1.DeletionStage(tc.Inputs.Stage),
			HasRoute:      tc.Inputs.HasRoute,
			RouteGone:     tc.Inputs.RouteGone,
			SettleDone:    tc.Inputs.SettleDone,
			Uninstalled:   tc.Inputs.Uninstalled,
			PodsRemaining: tc.Inputs.PodsRemaining,
			PastDeadline:  tc.Inputs.PastDeadline,
			ForceDue:      tc.Inputs.ForceDue,
		})
		if string(got) != tc.Expected.Stage {
			t.Fatalf("stage = %s, want %s", got, tc.Expected.Stage)
		}
	})
}
