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
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	autoscalingv1alpha1 "github.com/modelsphere/llm-operator/api/autoscaling/v1alpha1"
)

const (
	// podDeletionCostAnnotation biases ReplicaSet scale-down: pods with a lower
	// cost are removed first. Best-effort, not guaranteed. See
	// https://kubernetes.io/docs/concepts/workloads/controllers/replicaset/#pod-deletion-cost
	podDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"

	// coldPodDeletionCost is applied to pods chosen for removal so the ReplicaSet
	// prefers them over the pods we keep (which stay at the default cost of 0).
	coldPodDeletionCost = -100

	// deploymentKind is the target kind that supports the rollout guard and
	// pod-deletion-cost-based scale-down.
	deploymentKind = "Deployment"

	// eventReasonScalingDeferred is the Normal event emitted when a scale action
	// starts being held back, or the guard reason differs from the previous
	// AbleToScale reason. A previous False, Synced, ScaleDownStabilized, or
	// DeferralTimedOut counts as changed. It is not repeated on every sync,
	// and ScaleDownStabilized does not emit it.
	eventReasonScalingDeferred = "ScalingDeferred"
	// eventReasonDeferralTimedOut is the Warning event emitted when the deferral
	// timeout forces the write the guard was holding back.
	eventReasonDeferralTimedOut = "DeferralTimedOut"

	// scaleDownStabilizedMessage is HPA's AbleToScale text when the stabilization
	// window keeps a higher recent recommendation. It is the message for
	// ReasonScaleDownStabilized on every such sync, write or no-op.
	scaleDownStabilizedMessage = "recent recommendations were higher than current one, applying the highest recent recommendation"
)

// scaleRecommendation is a timestamped desired-replica recommendation, retained
// per scaler to implement the scale-down stabilization window.
type scaleRecommendation struct {
	time    time.Time
	desired int32
}

// LLMScalerReconciler reconciles a LLMScaler object
type LLMScalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clock is the time source for deferral deadlines and scale-down
	// stabilization. Nil uses the wall clock.
	Clock clock.PassiveClock
	// Recorder emits scaling events. Nil skips them.
	Recorder events.EventRecorder

	// recMu guards the per-scaler in-memory state: recommendations and
	// lastForcedWrite.
	recMu           sync.Mutex
	recommendations map[types.NamespacedName][]scaleRecommendation
	// lastForcedWrite is when this process last forced a deferred scale write.
	// The status write that records the step can fail after the target write
	// succeeded; a retry would otherwise see the deadline still passed and
	// force another step immediately. Pacing to one forced write per
	// syncPeriod is per process, which is fine because a restart already
	// costs at least one sync. Cleared when the deferral ends and when the
	// scaler is deleted.
	lastForcedWrite map[types.NamespacedName]time.Time
}

// now returns the reconciler's clock, or the wall clock when none was injected.
func (r *LLMScalerReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock.Now()
}

// +kubebuilder:rbac:groups=autoscaling.modelsphere.dev,resources=llmscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.modelsphere.dev,resources=llmscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.modelsphere.dev,resources=llmscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *LLMScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var scaler autoscalingv1alpha1.LLMScaler
	if err := r.Get(ctx, req.NamespacedName, &scaler); err != nil {
		if apierrors.IsNotFound(err) {
			r.forgetRecommendations(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling LLMScaler", "name", scaler.Name)

	targetRef := scaler.Spec.TargetRef
	gv, err := schema.ParseGroupVersion(targetRef.APIVersion)
	if err != nil {
		logger.Error(err, "invalid apiVersion in targetRef")
		// Stop before a recommendation. ScalingActive, DecisionAvailable, and
		// ScalingLimited are left as they are.
		r.recordScaleFailure(ctx, &scaler, metav1.ConditionFalse, autoscalingv1alpha1.ReasonInvalidSpec,
			fmt.Sprintf("targetRef.apiVersion %q does not parse: %s", targetRef.APIVersion, err.Error()))
		return ctrl.Result{}, nil
	}

	// 1. Use unstructured to dynamically support Deployment, StatefulSet, or LeaderWorkerSet
	targetObj := &unstructured.Unstructured{}
	targetObj.SetGroupVersionKind(gv.WithKind(targetRef.Kind))

	err = r.Get(ctx, types.NamespacedName{Name: targetRef.Name, Namespace: scaler.Namespace}, targetObj)

	// Determine the sync and retry intervals
	syncPeriod := time.Duration(scaler.Spec.SyncPeriodSeconds) * time.Second
	if syncPeriod == 0 {
		syncPeriod = 15 * time.Second
	}
	retryPeriod := time.Duration(scaler.Spec.RetryPeriodSeconds) * time.Second
	if retryPeriod == 0 {
		retryPeriod = 10 * time.Second
	}

	if err != nil {
		logger.Error(err, "unable to fetch target resource", "kind", targetRef.Kind, "name", targetRef.Name)
		// Requeue since the target might be created later. ScalingActive,
		// DecisionAvailable, ScalingLimited, deferredSince, and the replica
		// counts are left as they are, so the deadline carries over once the
		// target is readable again. AbleToScale is False with the read failure.
		reason, msg := targetReadFailure(err, targetRef.Kind, targetRef.Name)
		r.recordScaleFailure(ctx, &scaler, metav1.ConditionFalse, reason, msg)
		return ctrl.Result{RequeueAfter: retryPeriod}, nil
	}

	// 2. Read the target once. The scale-action decision compares against
	// spec.replicas to avoid redundant writes. The settle guard and
	// status.currentReplicas use the raw ready count (a missing field reads as
	// 0), so a fully-unready fleet is not mistaken for a settled one.
	state := readWorkloadState(targetObj, targetRef.Kind)
	// Widen back to the int64 the logs and the metric path already use.
	// In-range replica counts are unchanged; see nestedReplica.
	specReplicas := int64(state.Spec)
	readyReplicas := int64(state.Ready)
	// The HPA-style ratio is scaled off *ready* replicas (actual serving
	// capacity) so it doesn't compound on pods that were requested but haven't
	// started. With none ready yet (e.g. initial rollout) there is no
	// per-replica baseline, so the ratio alone falls back to spec.
	metricReady := readyReplicas
	if metricReady == 0 {
		metricReady = specReplicas
	}

	logger.Info("Fetched target resource", "kind", targetRef.Kind, "specReplicas", specReplicas, "readyReplicas", readyReplicas)

	// 3. Compute the recommendation: metrics, clamped to min/max, damped on the
	// way down by the stabilization window. This is what we want, independent of
	// how fast we are allowed to get there. Both providers set ScalingActive
	// here, before clamping; DecisionAvailable mirrors it. A missing signal is
	// held at spec.replicas, apart from the min/max bounds, and ScalingLimited
	// is left as it is.
	rec := r.recommendReplicas(ctx, &scaler, req.NamespacedName, specReplicas, metricReady)
	recommendedReplicas := rec.replicas

	// 4. Execute Scale Action, rate-limited by maxStepReplicas. The cap belongs
	// here rather than in the recommendation: it governs how far a single write
	// may travel toward desiredReplicas, not what the right replica count is. Each
	// sync moves one step closer, so the fleet converges on the recommendation
	// over several periods instead of in one burst. Scale-up is never capped,
	// so there is no ScaleUpLimit on ScalingLimited.
	targetReplicas := scaleGuard(int32(specReplicas), recommendedReplicas, scaler.Spec.ScaleDown.MaxStepReplicas)
	if targetReplicas != recommendedReplicas {
		logger.Info("Scale-down step capped", "recommended", recommendedReplicas, "thisStep", targetReplicas, "maxStepReplicas", scaler.Spec.ScaleDown.MaxStepReplicas)
	}
	// ScalingLimited is fixed once the recommendation and the step cap are both
	// known, and before the write, and only when ScalingActive is True. A later
	// write failure persists it, the same way it persists ScalingActive. When
	// ScalingActive is False, leave it alone, including when it has never been set.
	if rec.signal {
		limitedStatus, limitedReason, limitedMsg := scalingLimitedCondition(
			rec.raw, scaler.Spec.MinReplicas, scaler.Spec.MaxReplicas,
			recommendedReplicas, targetReplicas, scaler.Spec.ScaleDown.MaxStepReplicas)
		setScalingLimitedCondition(&scaler, limitedStatus, limitedReason, limitedMsg)
	}

	// One deferral timeout covers every guard. deferredSince is set on the first
	// sync that holds a write and kept across guard changes, no-ops while a
	// guard still matches, and forced writes. It is cleared only when no guard
	// matches. It lives in status, so the deadline survives an operator restart.
	// applied is the target's spec.replicas after this sync. A failed target
	// update records AbleToScale and returns before replica counts are written.
	now := r.now()
	timeout := deferTimeout(&scaler)
	deferral := evaluateGuards(state, ScaleIntent{From: state.Spec, To: targetReplicas})
	applied := int32(specReplicas)
	noop := targetReplicas == int32(specReplicas)
	guarded := deferral.reason != ""
	// How long a forced write is still blocked by the per-process pace. Zero
	// means one is allowed (no prior write, or a sync period has elapsed).
	paceRemaining := r.forcedWriteRemaining(req.NamespacedName, syncPeriod, now)
	requeueAfter := syncPeriod

	// AbleToScale=True precedence: a guard hold (the three cases below) and
	// DeferralTimedOut (inside writeScaleTarget) describe the write and win;
	// otherwise ScaleDownStabilized when the window raised the recommendation;
	// otherwise Synced. ScaleDownStabilized is not a guard, so it does not
	// change which branch runs or whether deferredSince moves.
	switch {
	case noop && (!guarded || scaler.Status.DeferredSince == nil):
		// Nothing is being held back. A guard that matches on a no-op does not
		// start a deferral, and a no-op with no guard ends one. A stabilized
		// no-op takes this same path: it ends a deferral and does not emit
		// ScalingDeferred.
		r.endDeferral(req.NamespacedName, &scaler)
		setSyncedOrStabilized(&scaler, rec.stabilized, "replica count is already at the step target")
	case noop:
		// A guard still matches and the clock is already running. There is
		// nothing to write, so deferredSince stays and this sync is not holding
		// a write: the next sync stays on syncPeriod.
		logger.Info("No scale write this sync; deferral clock still running", "reason", deferral.message, "deferredSince", scaler.Status.DeferredSince.Time)
		setAbleToScaleCondition(&scaler, metav1.ConditionTrue, deferral.reason,
			holdNoopMessage(deferral.message, scaler.Status.DeferredSince.Time, timeout))
	case guarded && !pastDeferTimeout(scaler.Status.DeferredSince, timeout, now):
		r.holdScaleDeferred(&scaler, deferral, int32(specReplicas), targetReplicas, timeout, now)
		logger.Info("Deferring scaling", "reason", deferral.message, "current", specReplicas, "ready", readyReplicas, "target", targetReplicas)
		until := scaler.Status.DeferredSince.Time.Add(timeout).Sub(now)
		requeueAfter = deferralRequeueAfter(syncPeriod, true, timeout, until, 0)
	case guarded && paceRemaining > 0:
		// Deadline passed, but this process already forced a write within the
		// last sync period. See lastForcedWrite.
		msg := holdPacedMessage(int32(specReplicas), targetReplicas, deferral.message, scaler.Status.DeferredSince.Time)
		logger.Info("Pacing forced scale", "reason", deferral.message, "current", specReplicas, "target", targetReplicas, "retryAfter", paceRemaining)
		if holdReasonChanged(&scaler, deferral.reason) {
			r.event(&scaler, corev1.EventTypeNormal, eventReasonScalingDeferred, msg)
		}
		setAbleToScaleCondition(&scaler, metav1.ConditionTrue, deferral.reason, msg)
		requeueAfter = deferralRequeueAfter(syncPeriod, true, timeout, 0, paceRemaining)
	default:
		// A normal write (no guard) or a forced write the pace allows. A failed
		// target update returns before any deferral state is recorded.
		wrote, err := r.writeScaleTarget(ctx, &scaler, targetObj, req.NamespacedName, int32(specReplicas), targetReplicas, guarded, rec.stabilized, deferral, timeout, now)
		if err != nil {
			r.recordTargetWriteFailure(ctx, &scaler, err)
			return ctrl.Result{}, err
		}
		applied = wrote
	}

	// 4. Update Status. currentReplicas is the raw ready count, not the ratio
	// baseline: a fully-unready fleet reports 0.
	scaler.Status.CurrentReplicas = int32(readyReplicas)
	scaler.Status.DesiredReplicas = recommendedReplicas
	scaler.Status.AppliedReplicas = applied
	scaler.Status.ObservedGeneration = scaler.Generation
	if err := r.Status().Update(ctx, &scaler); err != nil {
		logger.Error(err, "failed to update scaler status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// writeScaleTarget writes targetReplicas onto the workload. forced is set when
// a guard matched and the deadline has passed: the deferral clock is kept and
// the write is paced from now. Otherwise no guard matched, and the deferral
// ends. stabilized selects ScaleDownStabilized over Synced on that unforced
// write; DeferralTimedOut still wins when forced is set. The pace is recorded
// only after the target update succeeds, so a failed write can be retried
// immediately; the status update that follows may still fail, which is what
// the pace is there to absorb. ScalingLimited was already set by the caller
// when this sync had a signal, and a failed write leaves it in place.
func (r *LLMScalerReconciler) writeScaleTarget(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler, targetObj *unstructured.Unstructured, key types.NamespacedName, specReplicas, targetReplicas int32, forced, stabilized bool, deferral scaleDeferral, timeout time.Duration, now time.Time) (int32, error) {
	logger := logf.FromContext(ctx)
	targetRef := scaler.Spec.TargetRef

	var timedOutEvent string
	if forced {
		elapsed := now.Sub(scaler.Status.DeferredSince.Time).Truncate(time.Second)
		timedOutEvent = fmt.Sprintf("%s for %s (deferTimeoutSeconds %d); scaling %d -> %d anyway",
			deferral.message, elapsed, int64(timeout/time.Second), specReplicas, targetReplicas)
		logger.Info("Scaling target after deferral timeout", "reason", deferral.reason, "from", specReplicas, "to", targetReplicas, "deferredFor", elapsed)
	} else {
		logger.Info("Scaling target", "from", specReplicas, "to", targetReplicas)
	}

	if targetReplicas < specReplicas {
		// Cache-Aware Scale-Down: bias the ReplicaSet toward removing the
		// coldest-cache pods by setting a low pod-deletion-cost on exactly
		// the pods being sacrificed. This runs ONLY here (at shrink time),
		// never per-metric, to respect the pod-deletion-cost guidance that
		// frequent metric-driven updates overload the apiserver. It is
		// best-effort; the preStop drain keeps scale-down safe regardless of
		// which pod is ultimately removed. Only applies to Deployment/
		// ReplicaSet targets — StatefulSet/LWS scale-down is ordinal-based
		// and ignores pod-deletion-cost.
		behavior := scaler.Spec.ScaleDown.Behavior
		if (behavior == "" || behavior == "CacheAware") && targetRef.Kind == deploymentKind {
			numToRemove := int(specReplicas) - int(targetReplicas)
			if err := r.markColdestPodsForDeletion(ctx, scaler, targetObj, numToRemove); err != nil {
				// Non-fatal: proceed with the scale-down even if hinting failed.
				logger.Error(err, "Could not set pod-deletion-cost hints; scaling down without them")
			}
		} else {
			logger.Info("Skipping targeted deletion", "behavior", behavior, "kind", targetRef.Kind)
		}
	}

	if err := unstructured.SetNestedField(targetObj.Object, int64(targetReplicas), "spec", "replicas"); err != nil {
		logger.Error(err, "failed to set replicas field")
		return 0, err
	}
	if err := r.Update(ctx, targetObj); err != nil {
		logger.Error(err, "failed to update target resource")
		return 0, err
	}
	// lastScaleTime moves only when the target accepted the new replica count.
	stamped := metav1.NewTime(now)
	scaler.Status.LastScaleTime = &stamped

	// Pace the descent: pin the fleet at what we just wrote for one
	// stabilization window, and drop the recommendations that were measured
	// against the larger fleet. Only after the write succeeds — a failed
	// update leaves the target where it was, so there is nothing to hold at.
	if targetReplicas < specReplicas {
		r.holdAfterScaleDown(key, targetReplicas,
			time.Duration(scaler.Spec.ScaleDown.StabilizationWindowSeconds)*time.Second, now)
	}

	if forced {
		// Record the pace before the caller writes status. The guard still
		// matches, so deferredSince stays and the next stuck step does not
		// wait out the timeout again.
		r.recordForcedWrite(key, now)
		r.event(scaler, corev1.EventTypeWarning, eventReasonDeferralTimedOut, timedOutEvent)
		setAbleToScaleCondition(scaler, metav1.ConditionTrue, autoscalingv1alpha1.ReasonDeferralTimedOut, timedOutEvent)
	} else {
		r.endDeferral(key, scaler)
		setSyncedOrStabilized(scaler, stabilized, scaledMessage(specReplicas, targetReplicas))
	}
	return targetReplicas, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LLMScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Ignore status-only updates so the controller's own status writes don't
		// re-trigger reconciliation instantly. Periodic evaluation is driven by
		// RequeueAfter: syncPeriod on a normal sync, or sooner while a deferral
		// is waiting on its deadline or the forced-write pace.
		For(&autoscalingv1alpha1.LLMScaler{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("llmscaler").
		Complete(r)
}

// deferTimeout is how long a deferral may last before the held write is forced.
// Nil spec.deferTimeoutSeconds is the CRD default: objects created before the
// field existed, and objects built in memory, do not carry it.
func deferTimeout(scaler *autoscalingv1alpha1.LLMScaler) time.Duration {
	seconds := autoscalingv1alpha1.DefaultDeferTimeoutSeconds
	if scaler.Spec.DeferTimeoutSeconds != nil {
		seconds = *scaler.Spec.DeferTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// pastDeferTimeout reports whether an in-progress deferral has reached its
// deadline. A timeout of 0 never expires, so the guards may defer
// indefinitely. A nil deferredSince has not started, so it is not past
// the deadline — the caller stamps it and waits.
func pastDeferTimeout(since *metav1.Time, timeout time.Duration, now time.Time) bool {
	if timeout <= 0 || since == nil {
		return false
	}
	return now.Sub(since.Time) >= timeout
}

// minDeferralRequeue is the shortest RequeueAfter a deferred sync returns.
// Anything shorter would tight-loop the reconciler.
const minDeferralRequeue = time.Second

// deferralRequeueAfter is how long to wait before the next sync.
//
// holding is true when a guard matched and this sync did not write. timeout
// is the deferral timeout (0 never forces). untilDeadline is
// deferredSince+timeout-now, negative when the deadline has already passed.
// pacingRemaining is how long the forced-write pace still blocks a write; 0
// means pacing is not what is holding the sync.
//
// A held write with a non-zero timeout waits until the earlier of syncPeriod
// and the deadline. A write held only by the forced-write pace waits until
// the earlier of syncPeriod and the remaining pace. Anything else, including
// a timeout of 0, waits syncPeriod. A computed wait under a second is raised
// to one second.
func deferralRequeueAfter(syncPeriod time.Duration, holding bool, timeout, untilDeadline, pacingRemaining time.Duration) time.Duration {
	wait := syncPeriod
	switch {
	case pacingRemaining > 0:
		wait = min(syncPeriod, pacingRemaining)
	case holding && timeout > 0:
		wait = min(syncPeriod, untilDeadline)
	}
	if wait < minDeferralRequeue {
		return minDeferralRequeue
	}
	return wait
}

// forcedWritePace reports whether a forced write may proceed and, when it may
// not, how much of the sync period is left since last. A scaler with no
// recorded write is allowed. syncPeriod <= 0 is not paced.
func forcedWritePace(last time.Time, recorded bool, syncPeriod time.Duration, now time.Time) (allowed bool, remaining time.Duration) {
	if !recorded || syncPeriod <= 0 {
		return true, 0
	}
	if elapsed := now.Sub(last); elapsed < syncPeriod {
		return false, syncPeriod - elapsed
	}
	return true, 0
}

// forcedWriteRemaining is how long this process must wait before forcing
// another write for key. Zero means a forced write is allowed.
func (r *LLMScalerReconciler) forcedWriteRemaining(key types.NamespacedName, syncPeriod time.Duration, now time.Time) time.Duration {
	last, recorded := r.lastForcedWriteAt(key)
	allowed, remaining := forcedWritePace(last, recorded, syncPeriod, now)
	if !allowed {
		return remaining
	}
	return 0
}

func (r *LLMScalerReconciler) lastForcedWriteAt(key types.NamespacedName) (time.Time, bool) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	t, ok := r.lastForcedWrite[key]
	return t, ok
}

func (r *LLMScalerReconciler) recordForcedWrite(key types.NamespacedName, now time.Time) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	if r.lastForcedWrite == nil {
		r.lastForcedWrite = make(map[types.NamespacedName]time.Time)
	}
	r.lastForcedWrite[key] = now
}

func (r *LLMScalerReconciler) clearForcedWrite(key types.NamespacedName) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	delete(r.lastForcedWrite, key)
}

// endDeferral clears the deferral. It runs only when this sync found no guard
// matching: a normal write, or a no-op with nothing to hold. The pace record
// goes with it so the next deferral starts clean.
func (r *LLMScalerReconciler) endDeferral(key types.NamespacedName, scaler *autoscalingv1alpha1.LLMScaler) {
	scaler.Status.DeferredSince = nil
	r.clearForcedWrite(key)
}

// holdScaleDeferred records that this sync is not writing targetReplicas.
// deferredSince is stamped on entry and then left alone, so a guard reason that
// changes from rollout to settling is still one deferral and the deadline does
// not move. The Normal event fires on entry and when that reason changes, not
// on every sync.
func (r *LLMScalerReconciler) holdScaleDeferred(scaler *autoscalingv1alpha1.LLMScaler, deferral scaleDeferral, specReplicas, targetReplicas int32, timeout time.Duration, now time.Time) {
	entering := scaler.Status.DeferredSince == nil
	if entering {
		stamped := metav1.NewTime(now)
		scaler.Status.DeferredSince = &stamped
	}
	// Read the previous reason before overwriting the condition.
	reasonChanged := holdReasonChanged(scaler, deferral.reason)
	msg := holdMessage(specReplicas, targetReplicas, deferral.message, scaler.Status.DeferredSince.Time, timeout)
	setAbleToScaleCondition(scaler, metav1.ConditionTrue, deferral.reason, msg)
	if entering || reasonChanged {
		r.event(scaler, corev1.EventTypeNormal, eventReasonScalingDeferred, msg)
	}
}

// holdReasonChanged reports whether the Normal deferral event should fire for
// this guard reason. It is true unless AbleToScale is already True with that
// reason. A previous False, Synced, ScaleDownStabilized, or DeferralTimedOut
// counts as changed. ScaleDownStabilized is not a guard reason, so a guard
// that starts after it emits once, the same way it would after Synced.
// A TargetNotFound sync sets AbleToScale False, so a returning guard emits once.
func holdReasonChanged(scaler *autoscalingv1alpha1.LLMScaler, reason string) bool {
	prev := meta.FindStatusCondition(scaler.Status.Conditions, autoscalingv1alpha1.ConditionAbleToScale)
	if prev == nil || prev.Status != metav1.ConditionTrue {
		return true
	}
	switch prev.Reason {
	case autoscalingv1alpha1.ReasonSynced, autoscalingv1alpha1.ReasonScaleDownStabilized, autoscalingv1alpha1.ReasonDeferralTimedOut:
		return true
	}
	return prev.Reason != reason
}

// holdMessage describes the scale action being held back. "wanted"
// is the step-capped write, which is what the guard is holding, not the uncapped
// recommendation (that stays on status.desiredReplicas).
func holdMessage(specReplicas, targetReplicas int32, guardMessage string, since time.Time, timeout time.Duration) string {
	limit := "never forced (deferTimeoutSeconds is 0)"
	if timeout > 0 {
		limit = fmt.Sprintf("will apply after %ds", int64(timeout/time.Second))
	}
	return fmt.Sprintf("holding %d replicas (wanted %d): %s; deferred since %s, %s",
		specReplicas, targetReplicas, guardMessage, since.UTC().Format(time.RFC3339), limit)
}

// holdNoopMessage is the condition message when a guard still
// matches but the step target is already spec.replicas. The clock keeps
// running; the sync does not start or clear deferredSince.
func holdNoopMessage(guardMessage string, since time.Time, timeout time.Duration) string {
	limit := "never forced (deferTimeoutSeconds is 0)"
	if timeout > 0 {
		limit = fmt.Sprintf("will apply after %ds", int64(timeout/time.Second))
	}
	return fmt.Sprintf("nothing to write this sync, but the guard is still active (%s) and the deferral clock keeps running; deferred since %s, %s",
		guardMessage, since.UTC().Format(time.RFC3339), limit)
}

// holdPacedMessage is the condition message when the deadline has
// passed but another forced write must wait out the sync period.
func holdPacedMessage(specReplicas, targetReplicas int32, guardMessage string, since time.Time) string {
	return fmt.Sprintf("holding %d replicas (wanted %d): %s; deferred since %s, forced write paced to one per sync period",
		specReplicas, targetReplicas, guardMessage, since.UTC().Format(time.RFC3339))
}

// setSyncedOrStabilized sets AbleToScale when no guard hold and no
// DeferralTimedOut describe this sync.
//
// Precedence among AbleToScale=True reasons, per sync:
//  1. A guard hold (RolloutInProgress or ScaleDownSettling), in every hold
//     case: deferring a write, a no-op while a deferral is already running,
//     and pacing a forced write. DeferralTimedOut, when the timeout forces
//     the write, is in this tier too. Those callers set the condition
//     themselves and do not call this.
//  2. ScaleDownStabilized, when the stabilization window returned a value
//     higher than the clamped recommendation. It is not a guard. The caller
//     has already applied the same deferral handling it would for Synced.
//     This does not start, keep, or clear deferredSince, and it does not
//     emit ScalingDeferred.
//  3. Synced.
func setSyncedOrStabilized(scaler *autoscalingv1alpha1.LLMScaler, stabilized bool, syncedMsg string) {
	if stabilized {
		setAbleToScaleCondition(scaler, metav1.ConditionTrue, autoscalingv1alpha1.ReasonScaleDownStabilized, scaleDownStabilizedMessage)
		return
	}
	setAbleToScaleCondition(scaler, metav1.ConditionTrue, autoscalingv1alpha1.ReasonSynced, syncedMsg)
}

func setAbleToScaleCondition(scaler *autoscalingv1alpha1.LLMScaler, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&scaler.Status.Conditions, metav1.Condition{
		Type:               autoscalingv1alpha1.ConditionAbleToScale,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: scaler.Generation,
	})
}

func setScalingLimitedCondition(scaler *autoscalingv1alpha1.LLMScaler, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&scaler.Status.Conditions, metav1.Condition{
		Type:               autoscalingv1alpha1.ConditionScalingLimited,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: scaler.Generation,
	})
}

// scalingLimitedCondition is the HPA ScalingLimited condition for one sync
// whose ScalingActive is True. The caller invokes it only then, after the step
// cap and before the target write.
//
// Precedence, the constraint that bound the final write:
//  1. ScaleDownLimit when scaleGuard changed the write (target != recommended).
//     There is no ScaleUpLimit: scale-up is never capped.
//  2. TooFewReplicas when raw, the provider count before clamping and before
//     stabilization, is below minReplicas.
//  3. TooManyReplicas when raw is above maxReplicas.
//  4. DesiredWithinRange otherwise.
//
// A raw count that is both below min and above max (minReplicas > maxReplicas)
// reports TooFewReplicas. Stabilization lifting the count afterwards does not
// clear a clamp reason; only the step cap outranks it.
func scalingLimitedCondition(raw, minReplicas, maxReplicas, recommended, target, maxStep int32) (metav1.ConditionStatus, string, string) {
	if target != recommended {
		return metav1.ConditionTrue, autoscalingv1alpha1.ReasonScaleDownLimit,
			fmt.Sprintf("the desired replica count is decreasing faster than the maximum scale rate (maxStepReplicas %d)", maxStep)
	}
	if raw < minReplicas {
		return metav1.ConditionTrue, autoscalingv1alpha1.ReasonTooFewReplicas,
			"the desired replica count is less than the minimum replica count"
	}
	if raw > maxReplicas {
		return metav1.ConditionTrue, autoscalingv1alpha1.ReasonTooManyReplicas,
			"the desired replica count is more than the maximum replica count"
	}
	return metav1.ConditionFalse, autoscalingv1alpha1.ReasonDesiredWithinRange,
		"the desired count is within the acceptable range"
}

// scaledMessage is the AbleToScale text for a normal write.
func scaledMessage(from, to int32) string {
	return fmt.Sprintf("scaled %d -> %d", from, to)
}

// targetReadFailure is the AbleToScale reason for a target Get that failed.
func targetReadFailure(err error, kind, name string) (string, string) {
	if apierrors.IsNotFound(err) {
		return autoscalingv1alpha1.ReasonTargetNotFound, fmt.Sprintf("target %s %q not found", kind, name)
	}
	return autoscalingv1alpha1.ReasonTargetUnreadable, fmt.Sprintf("could not get target %s %q: %s", kind, name, err.Error())
}

// recordScaleFailure sets AbleToScale and writes status.observedGeneration.
// Callers use it when the sync stops before a recommendation (an unparseable
// apiVersion, or a target Get that failed) and when the target write failed.
// ScalingActive, DecisionAvailable, ScalingLimited, the replica counts,
// deferredSince, and lastScaleTime are left as they stand. A target write
// failure therefore keeps the ScalingActive and ScalingLimited computed
// earlier in that sync. A status-write error is logged; the caller keeps its
// original result.
func (r *LLMScalerReconciler) recordScaleFailure(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler, status metav1.ConditionStatus, reason, msg string) {
	logger := logf.FromContext(ctx)
	setAbleToScaleCondition(scaler, status, reason, msg)
	scaler.Status.ObservedGeneration = scaler.Generation
	if err := r.Status().Update(ctx, scaler); err != nil {
		logger.Error(err, "failed to update scaler status")
	}
}

// recordTargetWriteFailure records AbleToScale False with TargetWriteFailed
// after spec.replicas could not be written. The ScalingActive computed earlier
// in this sync is kept, and so is the ScalingLimited computed with the
// recommendation before the write. DecisionAvailable mirrors that ScalingActive.
// deferredSince, the replica counts, and lastScaleTime are left as they stand.
// The status write is best-effort.
func (r *LLMScalerReconciler) recordTargetWriteFailure(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler, writeErr error) {
	r.recordScaleFailure(ctx, scaler, metav1.ConditionFalse, autoscalingv1alpha1.ReasonTargetWriteFailed,
		fmt.Sprintf("could not write spec.replicas: %s", writeErr.Error()))
}

func (r *LLMScalerReconciler) event(obj client.Object, eventType, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	// The note is a %s argument so a message containing % is not treated as a format.
	r.Recorder.Eventf(obj, nil, eventType, reason, reason, "%s", msg)
}

// stabilizeDesired records the latest desired-replica recommendation for a
// scaler and returns the maximum recommendation within the stabilization window.
// This is HPA-style scale-down stabilization: replicas are held at the recent
// peak until the metric has stayed low for the whole window, while scale-up is
// unaffected (the current value is then the window's max).
func (r *LLMScalerReconciler) stabilizeDesired(key types.NamespacedName, desired int32, window time.Duration, now time.Time) int32 {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	if r.recommendations == nil {
		r.recommendations = make(map[types.NamespacedName][]scaleRecommendation)
	}

	cutoff := now.Add(-window)
	kept := make([]scaleRecommendation, 0, len(r.recommendations[key])+1)
	for _, rec := range r.recommendations[key] {
		if rec.time.After(cutoff) {
			kept = append(kept, rec)
		}
	}
	kept = append(kept, scaleRecommendation{time: now, desired: desired})
	r.recommendations[key] = kept

	stabilized := desired
	for _, rec := range kept {
		stabilized = max(stabilized, rec.desired)
	}
	return stabilized
}

// recommendReplicas turns the metric source into the replica count this scaler
// wants, clamped to [minReplicas, maxReplicas] and damped on the way down by the
// stabilization window. This is the target, not the next write — the caller
// rate-limits how fast to approach it. Stabilization can never lower a
// scale-up, since it only ever returns the window maximum, but it is still
// entered on one: it has to record every recommendation, because the peak it
// later holds at is itself a scale-up. stabilized reports whether that window
// returned a value higher than the clamped recommendation. The replica count
// itself is unchanged by surfacing the flag.
//
// Only the first step differs between providers: Prometheus derives the count
// from per-replica measurements, Custom is handed the count outright. Both feed
// the same clamping and damping below, so a scaler behaves identically once the
// number exists. Both set ScalingActive before that clamping; DecisionAvailable
// mirrors it. The count in a Prometheus MetricsAvailable message is the raw
// maximum, not the clamped or stabilized value returned here.
//
// When the provider has no usable signal, the count is held at spec.replicas,
// apart from the min/max bounds (the clamp below still runs, and so does
// stabilization). signal is false, ScalingActive is False, and the caller
// leaves ScalingLimited alone. raw is the provider count before that clamp
// and is meaningful only when signal is true.
func (r *LLMScalerReconciler) recommendReplicas(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler, key types.NamespacedName, specReplicas, readyReplicas int64) replicaRecommendation {
	logger := logf.FromContext(ctx)

	// Use the metric recommendation when at least one metric was evaluated — it
	// may legitimately be 0 for an idle target (clamped up to minReplicas below).
	// Only when every metric failed do we hold at spec.replicas, apart from
	// the min/max bounds.
	var (
		maxDesiredReplicas int32
		haveMetric         bool
	)
	if scaler.Spec.MetricProvider == autoscalingv1alpha1.MetricProviderCustom {
		maxDesiredReplicas, haveMetric = r.desiredFromCustomProvider(ctx, scaler)
	} else {
		maxDesiredReplicas, haveMetric = r.computeDesiredFromMetrics(ctx, scaler, readyReplicas)
	}

	desiredReplicas := int32(specReplicas)
	var raw int32
	if haveMetric {
		raw = maxDesiredReplicas
		desiredReplicas = maxDesiredReplicas
	}

	// Ensure desired is within min/max boundaries. Clamp detection for
	// ScalingLimited compares raw, the provider count before this clamp and
	// before stabilization, not the value after it.
	if desiredReplicas < scaler.Spec.MinReplicas {
		desiredReplicas = scaler.Spec.MinReplicas
	}
	if desiredReplicas > scaler.Spec.MaxReplicas {
		desiredReplicas = scaler.Spec.MaxReplicas
	}

	// Scale-down stabilization: hold at the highest recommendation seen within
	// the window so a brief metric dip doesn't shrink the fleet. Scale-up stays
	// immediate, since the current recommendation is then the window's max.
	// raised is the ScaleDownStabilized signal: the window returned a value
	// higher than the clamped recommendation. Replica selection is unchanged.
	raised := false
	if window := time.Duration(scaler.Spec.ScaleDown.StabilizationWindowSeconds) * time.Second; window > 0 {
		stabilized := r.stabilizeDesired(key, desiredReplicas, window, r.now())
		if stabilized > desiredReplicas {
			raised = true
			logger.Info("Scale-down stabilized", "raw", desiredReplicas, "stabilized", stabilized, "windowSeconds", scaler.Spec.ScaleDown.StabilizationWindowSeconds)
		}
		desiredReplicas = stabilized
	}

	return replicaRecommendation{
		replicas:   desiredReplicas,
		raw:        raw,
		signal:     haveMetric,
		stabilized: raised,
	}
}

// replicaRecommendation is one sync's replica target.
// replicas is after the min/max clamp and scale-down stabilization. That is
// status.desiredReplicas, still before the maxStepReplicas cap.
// raw is the provider count before either of those. It is meaningful only
// when signal is true, and ScalingLimited compares it with min/max.
// signal is false when this sync has no usable scaling signal. The caller
// then leaves ScalingLimited as it is.
// stabilized is true when the stabilization window returned a value higher
// than the clamped recommendation.
type replicaRecommendation struct {
	replicas   int32
	raw        int32
	signal     bool
	stabilized bool
}

// scaleGuard limits a single scale-down write to maxStep replicas below
// current, so a collapsed metric shrinks the fleet gradually (maxStep per sync
// period) instead of jumping from N to minReplicas in one write. It is a rate
// limit on the write, not on the recommendation: desired stays the target and
// later syncs keep stepping toward it. desired is returned unchanged when it is
// not a scale-down or is already within the step, so scale-up is never affected.
// maxStep <= 0 means unlimited.
func scaleGuard(current, desired, maxStep int32) int32 {
	if maxStep <= 0 || desired >= current {
		return desired
	}
	if floor := current - maxStep; floor > desired {
		return floor
	}
	return desired
}

// holdAfterScaleDown replaces a scaler's recommendation history with the replica
// count just written, so the fleet is pinned there for one stabilization window
// before it may shrink again.
//
// Two things happen at once, and both are the point:
//
//   - Discarding the history. Every retained recommendation was computed against
//     a fleet size that no longer exists — the metric is per-replica, so removing
//     pods is expected to raise it on the ones that remain. Deciding the next step
//     from samples taken before this one landed is what lets a whole descent
//     resolve in seconds off a single collapsed reading.
//   - Seeding the new count. Clearing alone would have the opposite effect: an
//     empty history makes stabilizeDesired return the current (low) recommendation
//     immediately. The seeded entry is what holds the floor, since stabilization
//     returns the window maximum, and it ages out exactly one window later.
//
// Scale-up is unaffected — a higher recommendation still wins the max. Seeding the
// count we wrote (not the one we scaled down from) matters: seeding the old, larger
// count would read as a scale-up recommendation and undo the step.
func (r *LLMScalerReconciler) holdAfterScaleDown(key types.NamespacedName, replicas int32, window time.Duration, now time.Time) {
	if window <= 0 {
		return // stabilization is off; the operator opted out of pacing too
	}
	r.recMu.Lock()
	defer r.recMu.Unlock()
	if r.recommendations == nil {
		r.recommendations = make(map[types.NamespacedName][]scaleRecommendation)
	}
	r.recommendations[key] = []scaleRecommendation{{time: now, desired: replicas}}
}

// forgetRecommendations drops the in-memory state for a deleted scaler: its
// stabilization history and its forced-write pace.
func (r *LLMScalerReconciler) forgetRecommendations(key types.NamespacedName) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	delete(r.recommendations, key)
	delete(r.lastForcedWrite, key)
}

// computeDesiredFromMetrics evaluates each metric's PromQL query and returns the
// maximum desired replica count (HPA-style: ceil(readyReplicas * value/target))
// and whether any metric was successfully evaluated.
//
// It always sets ScalingActive, and DecisionAvailable mirrors it. True /
// MetricsAvailable names how many metrics evaluated and recommends the raw
// maximum: the value returned here, before the caller clamps it or runs
// scale-down stabilization. False / DecisionUnavailable names prometheus and
// the last error.
func (r *LLMScalerReconciler) computeDesiredFromMetrics(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler, readyReplicas int64) (int32, bool) {
	logger := logf.FromContext(ctx)
	var maxDesired int32
	evaluated := 0
	var lastErr error

	for _, metric := range scaler.Spec.Metrics {
		currentValue, err := queryPrometheusScalar(scaler.Spec.ServerAddress, metric.Query, scaler.Spec.ServerHeaders)
		if err != nil {
			lastErr = err
			logger.Error(err, "failed to fetch metric", "name", metric.Name, "query", metric.Query)
			continue
		}

		// A non-finite sample is a missing measurement, not a zero. histogram_quantile
		// over a histogram with no observations in the window returns NaN — routine
		// for an idle latency guardrail — and a division by a zero-valued series
		// returns +Inf. Skip it here, before evaluated is incremented: only finite
		// samples count. A non-finite result is not counted.
		if math.IsNaN(currentValue) || math.IsInf(currentValue, 0) {
			lastErr = fmt.Errorf("query returned %v", currentValue)
			logger.Error(lastErr, "ignoring non-finite metric value",
				"name", metric.Name, "query", metric.Query)
			continue
		}

		targetValue, err := parseTargetValue(metric.Target)
		if err != nil {
			lastErr = err
			logger.Error(err, "invalid target value", "target", metric.Target)
			continue
		}

		ratio := currentValue / targetValue
		// The HPA-style ratio needs a per-replica baseline, but a fleet scaled
		// to zero has no ready replicas to scale off — readyReplicas * ratio is
		// then 0 no matter what the metric says, and the fleet could never
		// wake up. Evaluate against a single-replica baseline instead, so a
		// positive signal (queue depth, pending requests) recommends ceil of
		// the ratio and brings the fleet back. This only changes the at-zero
		// case. When the fleet is non-zero but none of it is ready, the caller
		// passes specReplicas in for this ratio only; the settle guard and
		// status.currentReplicas keep the raw ready count.
		metricDesired := int32(math.Ceil(float64(max(readyReplicas, 1)) * ratio))
		logger.Info("Metric calculation", "name", metric.Name, "query", metric.Query, "current", currentValue, "target", targetValue, "calculatedDesired", metricDesired)

		evaluated++
		if metricDesired > maxDesired {
			maxDesired = metricDesired
		}
	}
	if evaluated == 0 {
		setDecisionCondition(scaler, metav1.ConditionFalse, autoscalingv1alpha1.ReasonDecisionUnavailable,
			noPrometheusMetricMessage(lastErr))
		return maxDesired, false
	}
	// N is maxDesired, the raw maximum before clamping and stabilization.
	setDecisionCondition(scaler, metav1.ConditionTrue, autoscalingv1alpha1.ReasonMetricsAvailable,
		fmt.Sprintf("evaluated %d of %d metrics; recommends %d replicas", evaluated, len(scaler.Spec.Metrics), maxDesired))
	return maxDesired, true
}

// noPrometheusMetricMessage is the ScalingActive text when the Prometheus
// provider evaluated nothing. The prefix names the source. lastErr is the last
// query failure, non-finite sample, or target-parse error, when there was one.
func noPrometheusMetricMessage(lastErr error) string {
	if lastErr == nil {
		return "no prometheus metric could be evaluated"
	}
	return fmt.Sprintf("no prometheus metric could be evaluated: %s", lastErr.Error())
}

// desiredFromCustomProvider fetches the decision server's replica
// recommendation for this scaler, and reports whether it could be read.
//
// The count is returned as-is. It is an absolute replica count rather than a
// per-replica measurement, so the readyReplicas ratio that the Prometheus path
// applies has no meaning here — multiplying by it would compound the server's
// own decision on every sync. Clamping to min/max and scale-down damping still
// apply upstream: the CRD bounds are this operator's rail, not the server's.
//
// A failure returns false rather than 0, so a decision server that is down or
// answering in a schema we don't know is not read as a recommendation of zero.
// The count is held at spec.replicas, apart from the min/max bounds — the same
// rule the Prometheus path applies to a query that cannot be evaluated.
// ScalingActive says why, on every return. DecisionAvailable mirrors it.
// ScalingLimited is left as it is.
func (r *LLMScalerReconciler) desiredFromCustomProvider(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler) (int32, bool) {
	logger := logf.FromContext(ctx)

	provider := scaler.Spec.CustomProvider
	if provider == nil {
		// Admission rejects this pairing, so reaching it means the CRD schema in
		// the cluster predates the validation rule.
		logger.Error(fmt.Errorf("spec.customProvider is not set"),
			"Could not query the custom metric provider", "metricProvider", scaler.Spec.MetricProvider)
		setDecisionCondition(scaler, metav1.ConditionFalse, autoscalingv1alpha1.ReasonInvalidSpec,
			"spec.customProvider is not set")
		return 0, false
	}

	replicas, err := queryDecisionReplicas(scaler.Spec.ServerAddress, provider.Path,
		provider.ServiceID, provider.Namespace, scaler.Spec.ServerHeaders)
	if err != nil {
		// Expected while the decision server has not caught up with a new
		// service, so no error-level stack trace; the condition carries it.
		logger.Info("No decision from the custom metric provider; holding replicas",
			"serverAddress", scaler.Spec.ServerAddress, "serviceId", provider.ServiceID, "error", err.Error())
		setDecisionCondition(scaler, metav1.ConditionFalse, autoscalingv1alpha1.ReasonDecisionUnavailable,
			fmt.Sprintf("custom provider returned no decision: %s", err.Error()))
		return 0, false
	}

	logger.Info("Custom provider decision", "serviceId", provider.ServiceID, "calculatedDesired", replicas)
	setDecisionCondition(scaler, metav1.ConditionTrue, autoscalingv1alpha1.ReasonDecisionReceived,
		fmt.Sprintf("decision server recommends %d replicas", replicas))
	return replicas, true
}

// setDecisionCondition writes ScalingActive and the deprecated
// DecisionAvailable together, with the same status, reason, message, and
// observedGeneration.
func setDecisionCondition(scaler *autoscalingv1alpha1.LLMScaler, status metav1.ConditionStatus, reason, msg string) {
	for _, condType := range []string{
		autoscalingv1alpha1.ConditionScalingActive,
		autoscalingv1alpha1.ConditionDecisionAvailable, //nolint:staticcheck // deprecated mirror of ScalingActive, still written
	} {
		meta.SetStatusCondition(&scaler.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: scaler.Generation,
		})
	}
}

// markColdestPodsForDeletion biases the ReplicaSet toward removing the
// coldest / least-valuable pods on scale-down. If scaleDown.deletionCostQuery is
// set (and the source is Prometheus), each pod's pod-deletion-cost is computed
// from that PromQL expression; otherwise a newest-pod-first heuristic marks just
// the numToRemove sacrificed pods. Runs only at scale-down time (not per-metric)
// to respect the apiserver-load guidance for this annotation. Patching a live
// Pod's annotation is an in-place metadata write and does not restart it.
func (r *LLMScalerReconciler) markColdestPodsForDeletion(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler, targetObj *unstructured.Unstructured, numToRemove int) error {
	if numToRemove <= 0 {
		return nil
	}
	logger := logf.FromContext(ctx)

	// Locate the target's pods via its label selector.
	sel, found, err := unstructured.NestedStringMap(targetObj.Object, "spec", "selector", "matchLabels")
	if err != nil || !found || len(sel) == 0 {
		return fmt.Errorf("target has no spec.selector.matchLabels to locate pods")
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(scaler.Namespace), client.MatchingLabels(sel)); err != nil {
		return fmt.Errorf("failed to list target pods: %w", err)
	}

	// Query-based costs take precedence when configured against a Prometheus
	// source; each matched pod's cost is set from the expression's per-pod value.
	// The query is PromQL, so under the Custom metric provider there is nothing
	// to run it against — serverAddress is a decision server that answers with
	// replica counts, not a query API — and the heuristic below takes over.
	if q := scaler.Spec.ScaleDown.DeletionCostQuery; q != "" {
		if scaler.Spec.MetricProvider == autoscalingv1alpha1.MetricProviderCustom {
			logger.Info("Ignoring scaleDown.deletionCostQuery: it is PromQL and metricProvider is Custom, so using the newest-pod-first heuristic instead")
		} else {
			costs, err := queryPodDeletionCosts(scaler.Spec.ServerAddress, q, scaler.Spec.ServerHeaders)
			if err != nil {
				return fmt.Errorf("deletion-cost query failed: %w", err)
			}
			return r.applyPodDeletionCosts(ctx, pods.Items, costs)
		}
	}

	// Heuristic fallback: newer pods have colder KV caches, so order by
	// creationTimestamp descending and mark just the sacrificed pods.
	slices.SortFunc(pods.Items, func(a, b corev1.Pod) int {
		return b.CreationTimestamp.Compare(a.CreationTimestamp.Time)
	})

	numToRemove = min(numToRemove, len(pods.Items))
	for i := range numToRemove {
		pod := &pods.Items[i]
		if err := r.patchPodDeletionCost(ctx, pod, coldPodDeletionCost); err != nil {
			return err
		}
		logger.Info("Marked pod for cache-aware scale-down", "pod", pod.Name, "podDeletionCost", coldPodDeletionCost)
	}
	return nil
}

// applyPodDeletionCosts writes the per-pod costs (keyed by pod name) onto the
// matching pods. Pods with no entry in the map are left untouched.
func (r *LLMScalerReconciler) applyPodDeletionCosts(ctx context.Context, pods []corev1.Pod, costs map[string]float64) error {
	logger := logf.FromContext(ctx)
	for i := range pods {
		pod := &pods[i]
		v, ok := costs[pod.Name]
		if !ok {
			continue // no metric for this pod; leave its cost unchanged
		}
		cost := clampToInt32(math.Round(v))
		if err := r.patchPodDeletionCost(ctx, pod, cost); err != nil {
			return err
		}
		logger.Info("Set pod-deletion-cost from query", "pod", pod.Name, "podDeletionCost", cost)
	}
	return nil
}

// patchPodDeletionCost sets the pod-deletion-cost annotation on a single pod via
// an in-place merge patch (does not restart the pod).
func (r *LLMScalerReconciler) patchPodDeletionCost(ctx context.Context, pod *corev1.Pod, cost int32) error {
	base := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[podDeletionCostAnnotation] = strconv.Itoa(int(cost))
	if err := r.Patch(ctx, pod, base); err != nil {
		return fmt.Errorf("failed to set pod-deletion-cost on pod %s: %w", pod.Name, err)
	}
	return nil
}

// clampToInt32 clamps a float to the int32 range accepted by the annotation.
func clampToInt32(f float64) int32 {
	switch {
	case f > math.MaxInt32:
		return math.MaxInt32
	case f < math.MinInt32:
		return math.MinInt32
	default:
		return int32(f)
	}
}

// promSample is one element of a PromQL instant-query result vector.
type promSample struct {
	labels map[string]string
	value  float64
}

// promInstantQuery runs a PromQL instant query against Prometheus and returns
// the result vector as (labels, value) samples.
func promInstantQuery(serverAddress, promQL string, headers map[string]string) ([]promSample, error) {
	if serverAddress == "" {
		return nil, fmt.Errorf("serverAddress is empty")
	}

	queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", strings.TrimRight(serverAddress, "/"), url.QueryEscape(promQL))
	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	applyHeaders(req, headers)

	httpClient := http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query prometheus: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Cap the body the way the decision client does. The error is quoted
		// on ScalingActive, and an error page can be large.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("prometheus returned status %d: %s", resp.StatusCode, string(body))
	}

	var promResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return nil, fmt.Errorf("failed to decode prometheus response: %w", err)
	}
	if promResp.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed with status: %s", promResp.Status)
	}

	samples := make([]promSample, 0, len(promResp.Data.Result))
	for _, res := range promResp.Data.Result {
		if len(res.Value) != 2 {
			continue
		}
		valStr, ok := res.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		samples = append(samples, promSample{labels: res.Metric, value: v})
	}
	return samples, nil
}

// queryPrometheusScalar runs an instant query expected to return a single
// (aggregated) value.
//
// More than one series is an error rather than "take the first". The metrics
// worth scaling on are all multi-dimensional at the source — per backend, model,
// route, peer or pod — so an under-aggregated query returns one series per pod
// and resolving it silently would drive the whole fleet off whichever one
// Prometheus happened to list first. Failing instead surfaces the missing
// avg()/sum(), and a metric that fails to evaluate is skipped, not treated as
// zero (see computeDesiredFromMetrics).
func queryPrometheusScalar(serverAddress, promQL string, headers map[string]string) (float64, error) {
	samples, err := promInstantQuery(serverAddress, promQL, headers)
	if err != nil {
		return 0, err
	}
	switch len(samples) {
	case 1:
		return samples[0].value, nil
	case 0:
		return 0, fmt.Errorf("query returned no data: %s", promQL)
	default:
		return 0, fmt.Errorf("query returned %d series, expected 1 — aggregate it (e.g. wrap in avg(...) or sum(...)); first two are %s and %s: %s",
			len(samples), formatLabels(samples[0].labels), formatLabels(samples[1].labels), promQL)
	}
}

// formatLabels renders a sample's label set as {k="v",...}, sorted by name so
// the two series quoted in the multi-series error above line up and the
// dimension that needs aggregating away is the one that visibly differs.
func formatLabels(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// queryPodDeletionCosts runs a PromQL instant query that returns per-pod values
// and maps them by the "pod" label, for deriving pod-deletion-cost from a
// user-defined expression at scale-down time.
func queryPodDeletionCosts(serverAddress, promQL string, headers map[string]string) (map[string]float64, error) {
	samples, err := promInstantQuery(serverAddress, promQL, headers)
	if err != nil {
		return nil, err
	}

	costs := make(map[string]float64, len(samples))
	for _, s := range samples {
		if pod := s.labels["pod"]; pod != "" {
			costs[pod] = s.value
		}
	}
	if len(costs) == 0 {
		return nil, fmt.Errorf("deletion-cost query returned no per-pod samples (missing 'pod' label?)")
	}
	return costs, nil
}

// applyHeaders sets the given headers on the request, overriding any existing values.
func applyHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}

// parseTargetValue parses a metric target into the finite, positive float64 the
// scaling ratio divides by.
//
// A "%" suffix is rejected rather than interpreted. It used to be stripped and
// the remainder parsed, so "80%" compared as 80 — against a 0–1 ratio such as
// vllm:kv_cache_usage_perc that pins the ratio near zero and holds the fleet at
// minReplicas forever, with nothing in the log to say why. The notation cannot
// be resolved without knowing the scale of the query it is compared to (0–1
// fraction or 0–100 percent-valued series), which is why it asks for the
// absolute value instead of guessing at a factor of 100.
//
// Zero, negative and NaN targets are rejected for the same reason the metric
// value is checked for finiteness: they make the ratio non-finite, which is then
// discarded silently and reads as a recommendation of zero replicas.
func parseTargetValue(val string) (float64, error) {
	val = strings.TrimSpace(val)
	if strings.HasSuffix(val, "%") {
		return 0, fmt.Errorf("target %q: the %% suffix is not supported, use the absolute value the query returns (e.g. \"0.8\" against a 0-1 ratio, \"80\" against a 0-100 series)", val)
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, fmt.Errorf("target %q must be a finite positive number", val)
	}
	return f, nil
}
