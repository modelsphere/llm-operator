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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
)

const (
	deletionPoll           = 5 * time.Second
	defaultDeletionMaxWait = time.Hour
	defaultRouteSettle     = 60 * time.Second
	stuckPastGrace         = 5 * time.Minute
	defaultPodGraceSeconds = int64(30)
	deletionListCap        = 20
)

const (
	msgNoRoute      = "no ModelRoute in the helm manifest"
	msgRouteStuck   = "ModelRoute was still present at the deadline"
	msgUninstalled  = "Uninstalled helm release"
	msgSettle       = "waiting for the ModelRoute to settle"
	msgTimedOut     = "deletion budget elapsed with pods still present"
	msgPodsGone     = "release pods are gone"
	msgForceGone    = "force-deleted release pods"
	msgNegativeWait = "duration is negative"
)

// deletionKnobs is the effective teardown budget for one reconcile.
// forceAfter nil means force-delete is off.
type deletionKnobs struct {
	maxWait     time.Duration
	routeSettle time.Duration
	forceAfter  *time.Duration
	invalid     []invalidAnnotation
}

type invalidAnnotation struct {
	key   string
	value string
	why   string
}

// deletionKnobsFrom resolves annotation > spec > built-in default.
// An explicit 0s, in the spec or an annotation, is a real zero and skips
// that wait. An unparseable or negative
// annotation is reported and ignored.
func deletionKnobsFrom(obj *servingv1alpha1.LLMService) deletionKnobs {
	var spec servingv1alpha1.DeletionSpec
	if obj.Spec.Deletion != nil {
		spec = *obj.Spec.Deletion
	}
	var out deletionKnobs
	out.maxWait = resolveDuration(obj, servingv1alpha1.AnnotationDeletionMaxWait, spec.MaxWait, defaultDeletionMaxWait, &out)
	out.routeSettle = resolveDuration(obj, servingv1alpha1.AnnotationRouteSettle, spec.RouteSettle, defaultRouteSettle, &out)
	out.forceAfter = resolveForceAfter(obj, spec.ForceDeleteAfter, &out)
	return out
}

func resolveDuration(obj *servingv1alpha1.LLMService, key string, spec *metav1.Duration, fallback time.Duration, out *deletionKnobs) time.Duration {
	if raw, ok := obj.Annotations[key]; ok {
		d, bad := parseNonNegative(raw)
		if bad == "" {
			return d
		}
		out.invalid = append(out.invalid, invalidAnnotation{key: key, value: raw, why: bad})
	}
	if spec != nil {
		return spec.Duration
	}
	return fallback
}

func resolveForceAfter(obj *servingv1alpha1.LLMService, spec *metav1.Duration, out *deletionKnobs) *time.Duration {
	if raw, ok := obj.Annotations[servingv1alpha1.AnnotationForceDeleteAfter]; ok {
		d, bad := parseNonNegative(raw)
		if bad == "" {
			return &d
		}
		out.invalid = append(out.invalid, invalidAnnotation{key: servingv1alpha1.AnnotationForceDeleteAfter, value: raw, why: bad})
	}
	if spec == nil {
		return nil
	}
	d := spec.Duration
	return &d
}

func parseNonNegative(raw string) (time.Duration, string) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err.Error()
	}
	if d < 0 {
		return 0, msgNegativeWait
	}
	return d, ""
}

func (r *LLMServiceReconciler) warnInvalidKnobs(obj *servingv1alpha1.LLMService, knobs deletionKnobs) {
	for _, inv := range knobs.invalid {
		r.event(obj, corev1.EventTypeWarning, servingv1alpha1.ReasonInvalidAnnotation,
			fmt.Sprintf("Ignored annotation %s=%q: %s", inv.key, inv.value, inv.why))
	}
}
