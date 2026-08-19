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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	autoscalingv1alpha1 "gitlab.4pd.io/inference-production-stack/llm-operator/api/v1alpha1"
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

	// recMu guards recommendations, the per-scaler history of desired-replica
	// recommendations used for scale-down stabilization.
	recMu           sync.Mutex
	recommendations map[types.NamespacedName][]scaleRecommendation
}

// +kubebuilder:rbac:groups=autoscaling.4pd.io,resources=llmscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.4pd.io,resources=llmscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.4pd.io,resources=llmscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch

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
		return ctrl.Result{}, nil // Stop reconciling if spec is invalid
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
		// Requeue since the target might be created later
		return ctrl.Result{RequeueAfter: retryPeriod}, nil
	}

	// 2. Extract replica counts. The HPA-style calculation is scaled off the
	// number of *ready* replicas (actual serving capacity) so we don't compound
	// on pods that were just requested but haven't started yet. The scale-action
	// decision compares against spec.replicas to avoid redundant writes.
	specReplicas, found, err := unstructured.NestedInt64(targetObj.Object, "spec", "replicas")
	if err != nil || !found {
		// Default to 1 if not explicitly set (or handle error)
		specReplicas = 1
	}
	readyReplicas, found, err := unstructured.NestedInt64(targetObj.Object, "status", "readyReplicas")
	if err != nil || !found || readyReplicas == 0 {
		// No ready pods reported yet (e.g. initial rollout); fall back to spec.
		readyReplicas = specReplicas
	}

	logger.Info("Fetched target resource", "kind", targetRef.Kind, "specReplicas", specReplicas, "readyReplicas", readyReplicas)

	// 3. Compute the recommendation: metrics, clamped to min/max, damped on the
	// way down by the stabilization window. This is what we want, independent of
	// how fast we are allowed to get there.
	recommendedReplicas := r.recommendReplicas(ctx, &scaler, req.NamespacedName, specReplicas, readyReplicas)

	// 4. Execute Scale Action, rate-limited by maxStepReplicas. The cap belongs
	// here rather than in the recommendation: it governs how far a single write
	// may travel toward desiredReplicas, not what the right replica count is. Each
	// sync moves one step closer, so the fleet converges on the recommendation
	// over several periods instead of in one burst. Scale-up is never capped.
	targetReplicas := scaleGuard(int32(specReplicas), recommendedReplicas, scaler.Spec.ScaleDown.MaxStepReplicas)
	if targetReplicas != recommendedReplicas {
		logger.Info("Scale-down step capped", "recommended", recommendedReplicas, "thisStep", targetReplicas, "maxStepReplicas", scaler.Spec.ScaleDown.MaxStepReplicas)
	}

	switch reason := scaleDeferReason(targetObj, targetRef.Kind, specReplicas, readyReplicas, targetReplicas); {
	case targetReplicas == int32(specReplicas):
		// Already at the target; nothing to write.
	case reason != "":
		logger.Info("Deferring scaling", "reason", reason, "current", specReplicas, "ready", readyReplicas, "target", targetReplicas)
	default:
		logger.Info("Scaling target", "from", specReplicas, "to", targetReplicas)

		if targetReplicas < int32(specReplicas) {
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
				if err := r.markColdestPodsForDeletion(ctx, &scaler, targetObj, numToRemove); err != nil {
					// Non-fatal: proceed with the scale-down even if hinting failed.
					logger.Error(err, "Could not set pod-deletion-cost hints; scaling down without them")
				}
			} else {
				logger.Info("Skipping targeted deletion", "behavior", behavior, "kind", targetRef.Kind)
			}
		}

		err = unstructured.SetNestedField(targetObj.Object, int64(targetReplicas), "spec", "replicas")
		if err != nil {
			logger.Error(err, "failed to set replicas field")
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, targetObj); err != nil {
			logger.Error(err, "failed to update target resource")
			return ctrl.Result{}, err
		}

		// Pace the descent: pin the fleet at what we just wrote for one
		// stabilization window, and drop the recommendations that were measured
		// against the larger fleet. Only after the write succeeds — a failed
		// update leaves the target where it was, so there is nothing to hold at.
		if targetReplicas < int32(specReplicas) {
			r.holdAfterScaleDown(req.NamespacedName, targetReplicas,
				time.Duration(scaler.Spec.ScaleDown.StabilizationWindowSeconds)*time.Second, time.Now())
		}
	}

	// 4. Update Status
	scaler.Status.CurrentReplicas = int32(readyReplicas)
	scaler.Status.DesiredReplicas = recommendedReplicas
	if err := r.Status().Update(ctx, &scaler); err != nil {
		logger.Error(err, "failed to update scaler status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: syncPeriod}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LLMScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Ignore status-only updates so the controller's own status writes don't
		// re-trigger reconciliation instantly. Periodic evaluation is driven by
		// RequeueAfter(syncPeriod), which paces each scale step.
		For(&autoscalingv1alpha1.LLMScaler{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("llmscaler").
		Complete(r)
}

// scaleDeferReason reports why this sync must not write a new replica count, or
// "" when the scale action may proceed. Both guards exist for the same reason: a
// target that is still absorbing the last change gives unreliable readings, so
// acting on them compounds the error.
func scaleDeferReason(targetObj *unstructured.Unstructured, kind string, specReplicas, readyReplicas int64, targetReplicas int32) string {
	if kind == deploymentKind && rolloutInProgress(targetObj, specReplicas) {
		return "Deployment rollout in progress"
	}
	if targetReplicas < int32(specReplicas) && !scaleDownSettled(targetObj, specReplicas, readyReplicas) {
		return "previous scale-down has not settled"
	}
	return ""
}

// checkTerminatingReplicas gates the drain half of the scale-down settle guard
// (see scaleDownSettled).
var checkTerminatingReplicas = false

// scaleDownSettled reports whether the target has finished absorbing the previous
// scale-down, so the next step may start. Scale-up is deliberately not gated on
// this — capacity is added as soon as the metric warrants it.
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
func scaleDownSettled(targetObj *unstructured.Unstructured, specReplicas, readyReplicas int64) bool {
	if readyReplicas != specReplicas {
		return false
	}
	if !checkTerminatingReplicas {
		return true
	}
	terminating, found, err := unstructured.NestedInt64(targetObj.Object, "status", "terminatingReplicas")
	if err != nil || !found {
		return true
	}
	return terminating == 0
}

// rolloutInProgress reports whether a Deployment target is mid-rollout: the
// controller hasn't observed the latest spec, not every replica is on the new
// pod template, or old-template pods still exist. During a rollout the metric
// average and cache-warmth ranking are unreliable, so scaling is deferred.
// A pure replica change (scaling) is not a template rollout, so this stays false
// for it once the new pods are created.
func rolloutInProgress(targetObj *unstructured.Unstructured, specReplicas int64) bool {
	gen, _, _ := unstructured.NestedInt64(targetObj.Object, "metadata", "generation")
	observedGen, _, _ := unstructured.NestedInt64(targetObj.Object, "status", "observedGeneration")
	if observedGen < gen {
		return true // latest spec not yet acted on
	}

	updated, _, _ := unstructured.NestedInt64(targetObj.Object, "status", "updatedReplicas")
	total, _, _ := unstructured.NestedInt64(targetObj.Object, "status", "replicas")
	// In progress until every replica is on the new template and no older
	// template pods remain.
	return updated < specReplicas || total > updated
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
// rate-limits how fast to approach it . Stabilization can
// never lower a scale-up, since it only ever returns the window maximum, but it
// is still entered on one: it has to record every recommendation, because the
// peak it later holds at is itself a scale-up.
//
// Only the first step differs between providers: Prometheus derives the count
// from per-replica measurements, Custom is handed the count outright. Both feed
// the same clamping and damping below, so a scaler behaves identically once the
// number exists.
func (r *LLMScalerReconciler) recommendReplicas(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler, key types.NamespacedName, specReplicas, readyReplicas int64) int32 {
	logger := logf.FromContext(ctx)

	// Use the metric recommendation when at least one metric was evaluated — it
	// may legitimately be 0 for an idle target (clamped up to minReplicas below).
	// Only when every metric failed to fetch do we hold current replicas.
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
	if haveMetric {
		desiredReplicas = maxDesiredReplicas
	}

	// Ensure desired is within min/max boundaries
	if desiredReplicas < scaler.Spec.MinReplicas {
		desiredReplicas = scaler.Spec.MinReplicas
	}
	if desiredReplicas > scaler.Spec.MaxReplicas {
		desiredReplicas = scaler.Spec.MaxReplicas
	}

	// Scale-down stabilization: hold at the highest recommendation seen within
	// the window so a brief metric dip doesn't shrink the fleet. Scale-up stays
	// immediate, since the current recommendation is then the window's max.
	if window := time.Duration(scaler.Spec.ScaleDown.StabilizationWindowSeconds) * time.Second; window > 0 {
		stabilized := r.stabilizeDesired(key, desiredReplicas, window, time.Now())
		if stabilized != desiredReplicas {
			logger.Info("Scale-down stabilized", "raw", desiredReplicas, "stabilized", stabilized, "windowSeconds", scaler.Spec.ScaleDown.StabilizationWindowSeconds)
		}
		desiredReplicas = stabilized
	}

	return desiredReplicas
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

// forgetRecommendations drops the stabilization history for a deleted scaler.
func (r *LLMScalerReconciler) forgetRecommendations(key types.NamespacedName) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	delete(r.recommendations, key)
}

// computeDesiredFromMetrics evaluates each metric's PromQL query and returns the
// maximum desired replica count (HPA-style: ceil(readyReplicas * value/target))
// and whether any metric was successfully evaluated.
func (r *LLMScalerReconciler) computeDesiredFromMetrics(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler, readyReplicas int64) (int32, bool) {
	logger := logf.FromContext(ctx)
	var maxDesired int32
	haveMetric := false

	for _, metric := range scaler.Spec.Metrics {
		currentValue, err := queryPrometheusScalar(scaler.Spec.ServerAddress, metric.Query, scaler.Spec.ServerHeaders)
		if err != nil {
			logger.Error(err, "failed to fetch metric", "name", metric.Name, "query", metric.Query)
			continue
		}

		// A non-finite sample is a missing measurement, not a zero, and must not
		// count as a successful evaluation. histogram_quantile over a histogram
		// with no observations in the window returns NaN — routine for a latency
		// guardrail whenever that traffic class is idle — and a division by a
		// zero-valued series returns +Inf. Both convert to a large negative
		// int32, so they lose the max() below silently while still setting
		// haveMetric: a NaN guardrail alongside a failing primary metric leaves
		// maxDesired at 0, which clamps the whole fleet to minReplicas.
		if math.IsNaN(currentValue) || math.IsInf(currentValue, 0) {
			logger.Error(fmt.Errorf("query returned %v", currentValue), "ignoring non-finite metric value",
				"name", metric.Name, "query", metric.Query)
			continue
		}

		targetValue, err := parseTargetValue(metric.Target)
		if err != nil {
			logger.Error(err, "invalid target value", "target", metric.Target)
			continue
		}

		ratio := currentValue / targetValue
		metricDesired := int32(math.Ceil(float64(readyReplicas) * ratio))
		logger.Info("Metric calculation", "name", metric.Name, "query", metric.Query, "current", currentValue, "target", targetValue, "calculatedDesired", metricDesired)

		haveMetric = true
		if metricDesired > maxDesired {
			maxDesired = metricDesired
		}
	}
	return maxDesired, haveMetric
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
// answering in a schema we don't know holds the fleet where it is instead of
// collapsing it to minReplicas — the same rule the Prometheus path applies to a
// query that cannot be evaluated.
func (r *LLMScalerReconciler) desiredFromCustomProvider(ctx context.Context, scaler *autoscalingv1alpha1.LLMScaler) (int32, bool) {
	logger := logf.FromContext(ctx)

	provider := scaler.Spec.CustomProvider
	if provider == nil {
		// Admission rejects this pairing, so reaching it means the CRD schema in
		// the cluster predates the validation rule.
		logger.Error(fmt.Errorf("spec.customProvider is not set"),
			"Could not query the custom metric provider", "metricProvider", scaler.Spec.MetricProvider)
		return 0, false
	}

	replicas, err := queryDecisionReplicas(scaler.Spec.ServerAddress, provider.Path,
		provider.ServiceID, provider.Namespace, scaler.Spec.ServerHeaders)
	if err != nil {
		logger.Error(err, "Failed to fetch a decision from the custom metric provider",
			"serverAddress", scaler.Spec.ServerAddress, "serviceId", provider.ServiceID)
		return 0, false
	}

	logger.Info("Custom provider decision", "serviceId", provider.ServiceID, "calculatedDesired", replicas)
	return replicas, true
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
		body, _ := io.ReadAll(resp.Body)
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
