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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	autoscalingv1alpha1 "github.com/modelsphere/llm-operator/api/autoscaling/v1alpha1"
)

const (
	// msgRolloutInProgress is why the rollout guard holds a scale action.
	msgRolloutInProgress = "Deployment rollout in progress"
	// msgScaleDownNotSettled is why the settle guard holds a scale-down.
	msgScaleDownNotSettled = "previous scale-down has not settled"

	// missingSpecReplicas is the workload default. An unset spec.replicas
	// means one replica, so a missing or invalid field reads as 1.
	missingSpecReplicas int32 = 1
)

// WorkloadState is the slice of a scale target the guards read, taken once
// per sync. Replica counts are int32, the width apps/v1 and LeaderWorkerSet
// store; see nestedReplica for why the int64 from the unstructured object is
// narrowed.
type WorkloadState struct {
	// Spec is spec.replicas. Deployment, StatefulSet, and LeaderWorkerSet all
	// use this field. A LeaderWorkerSet replica is one leader-worker group,
	// not one pod. A missing or invalid value reads as 1. Zero is kept: a
	// fleet scaled to zero reports 0.
	Spec int32

	// Total is status.replicas. On a Deployment it counts non-terminated pods
	// and can exceed Updated while old-template pods remain (maxSurge). On a
	// StatefulSet it is the number of pods the controller has created. On a
	// LeaderWorkerSet it is the number of groups. A missing or invalid value
	// reads as 0. Only the rollout guard consults it, and only for a
	// Deployment (TracksRollout).
	Total int32

	// Updated is status.updatedReplicas: replicas, or groups for a
	// LeaderWorkerSet, on the current template. A missing or invalid value
	// reads as 0. Only the rollout guard consults it, and only for a
	// Deployment (TracksRollout).
	Updated int32

	// Ready is status.readyReplicas, raw. A missing or invalid value reads as
	// 0. The ready==0 fallback to spec is applied by the caller for the metric
	// ratio only; status.currentReplicas stays this raw count. On a Deployment
	// the apps/v1 API counts non-terminating pods with a Ready condition, so a
	// pod drops out of Ready at deletionTimestamp, before it exits. A
	// StatefulSet and a LeaderWorkerSet publish the same field name for their
	// own ready count.
	Ready int32

	// Terminating is status.terminatingReplicas when that field was present
	// and an int64. It is a beta Deployment field, on by default, counting
	// pods with a deletionTimestamp. A StatefulSet and a LeaderWorkerSet have
	// no such field. A missing or invalid value leaves Terminating 0 and
	// TerminatingReported false, which the settle guard treats as settled.
	Terminating int32

	// TerminatingReported is false when status.terminatingReplicas is missing
	// or not an int64. The settle guard then ignores Terminating, including
	// when the drain check is gated on.
	TerminatingReported bool

	// Generation is metadata.generation. A missing or invalid value reads as
	// 0. Only the rollout guard consults it, and only for a Deployment
	// (TracksRollout). Kept as int64: ObjectMeta.generation is an int64, not
	// a replica count.
	Generation int64

	// ObservedGeneration is status.observedGeneration. A missing or invalid
	// value reads as 0. Only the rollout guard consults it, and only for a
	// Deployment (TracksRollout).
	ObservedGeneration int64

	// TracksRollout is true only for a Deployment. StatefulSet and
	// LeaderWorkerSet publish updatedReplicas and observedGeneration under
	// the same names, but the rollout guard does not apply to them, nor to
	// any other kind. The fields are still parsed; this flag is what keeps
	// the guard off.
	TracksRollout bool
}

// ScaleIntent is the replica write this sync would make.
// From is the target's spec.replicas (WorkloadState.Spec) and To is the
// step-capped target.
type ScaleIntent struct {
	// From is the current spec.replicas the write would start from.
	From int32
	// To is the replica count the write would set.
	To int32
}

// Guard decides whether a scale write must wait.
// The first entry in guards whose Hold reports held supplies the
// AbleToScale reason and message.
type Guard interface {
	// Reason is the AbleToScale condition reason used when Hold reports
	// the guard is active.
	Reason() string
	// Hold reports whether this guard is holding the write back.
	// why is the condition message when held is true, and empty otherwise.
	Hold(s WorkloadState, in ScaleIntent) (held bool, why string)
}

// guards is the order a sync consults. The first guard that holds wins.
var guards = []Guard{rolloutGuard{}, settleGuard{}}

// rolloutGuard holds while a Deployment is mid-rollout: the controller hasn't
// observed the latest spec, not every replica is on the new pod template, or
// old-template pods still exist. During a rollout the metric average and
// cache-warmth ranking are unreliable, so scaling is deferred.
// A pure replica change (scaling) is not a template rollout, so this stays
// false for it once the new pods are created. It holds in both directions,
// and on a no-op. Other kinds never hold.
type rolloutGuard struct{}

// Reason is the AbleToScale condition reason for an in-progress rollout.
func (rolloutGuard) Reason() string { return autoscalingv1alpha1.ReasonRolloutInProgress }

// Hold reports whether a Deployment rollout is in progress.
// Direction is ignored: scale-up, scale-down, and a no-op are all held.
func (rolloutGuard) Hold(s WorkloadState, _ ScaleIntent) (bool, string) {
	if !s.TracksRollout {
		return false, ""
	}
	if s.ObservedGeneration < s.Generation {
		return true, msgRolloutInProgress // latest spec not yet acted on
	}
	// In progress until every replica is on the new template and no older
	// template pods remain.
	if s.Updated < s.Spec || s.Total > s.Updated {
		return true, msgRolloutInProgress
	}
	return false, ""
}

// checkTerminatingReplicas gates the drain half of the settle guard.
// It is read when the guard runs, not when the workload is parsed, so a
// caller can change it between syncs.
var checkTerminatingReplicas = false

// settleGuard holds a scale-down until the target has finished absorbing the
// previous one. Scale-up is deliberately not gated on this — capacity is
// added as soon as the metric warrants it. A no-op has nothing to write.
//
// Two conditions, both read off the target's own status:
//
//   - readyReplicas == specReplicas — the fleet has converged on the last write.
//     Also catches the window just after a write, where readyReplicas is still
//     the old, higher count.
//   - terminatingReplicas == 0 — no pod is still draining. Only applied when
//     checkTerminatingReplicas is on.
//
// The second is not redundant. Per the apps/v1 API, a Deployment's
// status.readyReplicas counts "non-terminating pods ... with a Ready Condition",
// so a pod leaves that count the moment it gets a deletionTimestamp — long before
// it exits. Under a long preStop drain the readyReplicas check alone reports
// "settled" while the sacrificed pods are still serving traffic, still scraped
// into the metric average, and still holding their GPUs. That is the current
// behavior with the gate off: the guard then only bridges the brief window
// between our write and the workload controller reflecting it in status.
//
// terminatingReplicas is a beta field (enabled by default); on a cluster that does
// not report it — or on a StatefulSet/LWS target, which has no such field — the
// check degrades to the readyReplicas condition alone even when gated on.
type settleGuard struct{}

// Reason is the AbleToScale condition reason for a scale-down that has
// not settled.
func (settleGuard) Reason() string { return autoscalingv1alpha1.ReasonScaleDownSettling }

// Hold reports whether a scale-down must wait for the previous write to settle.
func (settleGuard) Hold(s WorkloadState, in ScaleIntent) (bool, string) {
	if in.To >= in.From {
		return false, ""
	}
	if s.Ready != s.Spec {
		return true, msgScaleDownNotSettled
	}
	// readyReplicas counts only non-terminating pods, so a draining pod has
	// already left that count. The terminating check is what still sees it,
	// and only when the gate is on and the target reported the field.
	if !checkTerminatingReplicas || !s.TerminatingReported || s.Terminating == 0 {
		return false, ""
	}
	return true, msgScaleDownNotSettled
}

// scaleDeferral is why this sync must not write a new replica count.
// An empty reason means the scale action may proceed. A rollout holds both
// directions, and the settle guard holds scale-down until the raw ready count
// is back at spec.
type scaleDeferral struct {
	// reason is the AbleToScale condition reason (RolloutInProgress or
	// ScaleDownSettling), or "" when nothing defers.
	reason string
	// message is the human description of the guard, for the log and the condition.
	message string
}

// evaluateGuards reports why this sync must not write a new replica count, or
// an empty reason when the scale action may proceed. The first guard that
// holds wins. Both guards exist for the same reason: a target that is still
// absorbing the last change gives unreliable readings, so acting on them
// compounds the error.
func evaluateGuards(s WorkloadState, in ScaleIntent) scaleDeferral {
	for _, g := range guards {
		held, why := g.Hold(s, in)
		if held {
			return scaleDeferral{reason: g.Reason(), message: why}
		}
	}
	return scaleDeferral{}
}

// readWorkloadState reads the replica and rollout fields the guards consult.
// Deployment, StatefulSet, and LeaderWorkerSet publish these under the same
// names, and so does any other kind. The only per-kind choice is
// TracksRollout, set for a Deployment alone.
func readWorkloadState(obj *unstructured.Unstructured, kind string) WorkloadState {
	m := obj.Object
	spec, _ := nestedReplica(m, missingSpecReplicas, "spec", "replicas")
	total, _ := nestedReplica(m, 0, "status", "replicas")
	updated, _ := nestedReplica(m, 0, "status", "updatedReplicas")
	ready, _ := nestedReplica(m, 0, "status", "readyReplicas")
	terminating, terminatingReported := nestedReplica(m, 0, "status", "terminatingReplicas")
	generation, _ := nestedInt64(m, "metadata", "generation")
	observed, _ := nestedInt64(m, "status", "observedGeneration")
	return WorkloadState{
		Spec:                spec,
		Total:               total,
		Updated:             updated,
		Ready:               ready,
		Terminating:         terminating,
		TerminatingReported: terminatingReported,
		Generation:          generation,
		ObservedGeneration:  observed,
		TracksRollout:       kind == deploymentKind,
	}
}

// nestedReplica reads an int64 field and narrows it to int32.
// missing is returned, with ok false, when the field is absent or not an
// int64 — the way NestedInt64 reports a wrong JSON type, so an invalid value
// is treated as missing.
//
// Replica counts are int32 in apps/v1 and the LeaderWorkerSet schema, so
// every value the apiserver can store is inside the int32 range: int32(v)
// equals v, and comparing the narrowed counts gives the same result as
// comparing the int64 values. The scale decision and status.currentReplicas
// are int32 anyway.
func nestedReplica(obj map[string]any, missing int32, fields ...string) (int32, bool) {
	v, ok := nestedInt64(obj, fields...)
	if !ok {
		return missing, false
	}
	return int32(v), true
}

// nestedInt64 reads an int64 field.
// ok is false when it is missing or not an int64, and the value is then 0.
// Generation stays at this width: ObjectMeta.generation is an int64.
func nestedInt64(obj map[string]any, fields ...string) (int64, bool) {
	v, found, err := unstructured.NestedInt64(obj, fields...)
	if err != nil || !found {
		return 0, false
	}
	return v, true
}
