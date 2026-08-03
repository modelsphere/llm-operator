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
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
)

// LLMScalerReconciler reconciles a LLMScaler object
type LLMScalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	// 3. Fetch metrics and calculate desiredReplicas
	// We'll calculate the desired replicas for each metric and take the maximum (standard HPA behavior)
	var maxDesiredReplicas int32 = 0

	for _, metric := range scaler.Spec.Metrics {
		currentValue, err := fetchMetricFromUpstream(scaler.Spec.ServerAddress, metric.Type, scaler.Spec.Selector, scaler.Spec.ServerHeaders)
		if err != nil {
			logger.Error(err, "failed to fetch metric", "metric", metric.Type)
			continue
		}

		// Parse target value (e.g., "80%" or "5")
		targetValue, err := parseTargetValue(metric.TargetAverageValue)
		if err != nil {
			logger.Error(err, "invalid target value", "target", metric.TargetAverageValue)
			continue
		}

		// Algorithm: desiredReplicas = ceil[readyReplicas * (currentMetricValue / desiredMetricValue)]
		ratio := currentValue / targetValue
		metricDesired := int32(math.Ceil(float64(readyReplicas) * ratio))

		logger.Info("Metric calculation", "metric", metric.Type, "current", currentValue, "target", targetValue, "calculatedDesired", metricDesired)

		if metricDesired > maxDesiredReplicas {
			maxDesiredReplicas = metricDesired
		}
	}

	// Fallback to current replicas if no metrics were successfully calculated
	desiredReplicas := int32(specReplicas)
	if maxDesiredReplicas > 0 {
		desiredReplicas = maxDesiredReplicas
	}

	// Ensure desired is within min/max boundaries
	if desiredReplicas < scaler.Spec.MinReplicas {
		desiredReplicas = scaler.Spec.MinReplicas
	}
	if desiredReplicas > scaler.Spec.MaxReplicas {
		desiredReplicas = scaler.Spec.MaxReplicas
	}

	// 4. Execute Scale Action
	if desiredReplicas != int32(specReplicas) {
		logger.Info("Scaling target", "from", specReplicas, "to", desiredReplicas)

		if desiredReplicas < int32(specReplicas) {
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
			if (behavior == "" || behavior == "CacheAware") && targetRef.Kind == "Deployment" {
				numToRemove := int(specReplicas) - int(desiredReplicas)
				if err := r.markColdestPodsForDeletion(ctx, &scaler, targetObj, numToRemove); err != nil {
					// Non-fatal: proceed with the scale-down even if hinting failed.
					logger.Error(err, "Could not set pod-deletion-cost hints; scaling down without them")
				}
			} else {
				logger.Info("Skipping targeted deletion", "behavior", behavior, "kind", targetRef.Kind)
			}
		}

		err = unstructured.SetNestedField(targetObj.Object, int64(desiredReplicas), "spec", "replicas")
		if err != nil {
			logger.Error(err, "failed to set replicas field")
			return ctrl.Result{}, err
		}

		if err := r.Update(ctx, targetObj); err != nil {
			logger.Error(err, "failed to update target resource")
			return ctrl.Result{}, err
		}
	}

	// 4. Update Status
	scaler.Status.CurrentReplicas = int32(readyReplicas)
	scaler.Status.DesiredReplicas = desiredReplicas
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
	if q := scaler.Spec.ScaleDown.DeletionCostQuery; q != "" {
		costs, err := queryPodDeletionCosts(scaler.Spec.ServerAddress, q, scaler.Spec.ServerHeaders)
		if err != nil {
			return fmt.Errorf("deletion-cost query failed: %w", err)
		}
		return r.applyPodDeletionCosts(ctx, pods.Items, costs)
	}

	// Heuristic fallback: newer pods have colder KV caches, so order by
	// creationTimestamp descending and mark just the sacrificed pods.
	slices.SortFunc(pods.Items, func(a, b corev1.Pod) int {
		return b.CreationTimestamp.Compare(a.CreationTimestamp.Time)
	})

	logger := logf.FromContext(ctx)
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

// queryPodDeletionCosts runs a PromQL instant query that returns per-pod values
// and maps them by the "pod" label, for deriving pod-deletion-cost from a
// user-defined expression at scale-down time.
func queryPodDeletionCosts(serverAddress, promQL string, headers map[string]string) (map[string]float64, error) {
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

	costs := make(map[string]float64, len(promResp.Data.Result))
	for _, res := range promResp.Data.Result {
		pod := res.Metric["pod"]
		if pod == "" || len(res.Value) != 2 {
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
		costs[pod] = v
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

// parseTargetValue handles parsing values like "80%" or "5" into a float64
func parseTargetValue(val string) (float64, error) {
	val = strings.TrimSpace(val)
	if before, ok := strings.CutSuffix(val, "%"); ok {
		numStr := before
		return strconv.ParseFloat(numStr, 64)
	}
	return strconv.ParseFloat(val, 64)
}

// fetchMetricFromUpstream queries the metrics server (e.g., Prometheus)
func fetchMetricFromUpstream(serverAddress string, metricType autoscalingv1alpha1.MetricType, selector, headers map[string]string) (float64, error) {
	if serverAddress == "" {
		return 0, fmt.Errorf("serverAddress is empty")
	}

	// 1. Determine the metric name(s). Multiple candidates are combined with
	// PromQL `or` to stay compatible across vLLM engine versions: the V0 name
	// vllm:gpu_cache_usage_perc was renamed to vllm:kv_cache_usage_perc in the
	// V1 engine (default since mid-2025). Both are 0-1 gauges.
	var metricNames []string
	switch metricType {
	case autoscalingv1alpha1.MetricTypeKVCacheUtilization:
		metricNames = []string{"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"}
	case autoscalingv1alpha1.MetricTypeQueueDepth:
		metricNames = []string{"vllm:num_requests_waiting"}
	default:
		return 0, fmt.Errorf("unknown metric type: %s", metricType)
	}

	// 2. Build the PromQL selector string
	var labelSelectors []string
	for k, v := range selector {
		labelSelectors = append(labelSelectors, fmt.Sprintf(`%s="%s"`, k, v))
	}
	selectorStr := ""
	if len(labelSelectors) > 0 {
		selectorStr = "{" + strings.Join(labelSelectors, ",") + "}"
	}

	// 3. Construct the full PromQL query, combining metric-name candidates with `or`
	terms := make([]string, len(metricNames))
	for i, name := range metricNames {
		terms[i] = name + selectorStr
	}
	promQL := fmt.Sprintf("avg(%s)", strings.Join(terms, " or "))

	// 4. Make the HTTP request
	queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", strings.TrimRight(serverAddress, "/"), url.QueryEscape(promQL))

	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	applyHeaders(req, headers)

	cl := http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to query prometheus: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("prometheus returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// 5. Parse the JSON response
	var promResp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		return 0, fmt.Errorf("failed to decode prometheus response: %w", err)
	}

	if promResp.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed with status: %s", promResp.Status)
	}

	if len(promResp.Data.Result) == 0 {
		return 0, fmt.Errorf("no metric data found for query: %s", promQL)
	}

	valArray := promResp.Data.Result[0].Value
	if len(valArray) != 2 {
		return 0, fmt.Errorf("unexpected value format from prometheus")
	}

	valStr, ok := valArray[1].(string)
	if !ok {
		return 0, fmt.Errorf("prometheus metric value is not a string")
	}

	metricValue, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse metric value as float: %w", err)
	}

	if metricType == autoscalingv1alpha1.MetricTypeKVCacheUtilization && metricValue <= 1.0 {
		metricValue = metricValue * 100
	}

	return metricValue, nil
}
