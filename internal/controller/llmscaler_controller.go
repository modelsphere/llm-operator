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
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "gitlab.4pd.io/inference-production-stack/llm-operator/api/v1alpha1"
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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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

	// 2. Extract current replicas
	currentReplicas, found, err := unstructured.NestedInt64(targetObj.Object, "spec", "replicas")
	if err != nil || !found {
		// Default to 1 if not explicitly set (or handle error)
		currentReplicas = 1
	}

	logger.Info("Fetched target resource", "kind", targetRef.Kind, "currentReplicas", currentReplicas)

	// 3. Fetch metrics and calculate desiredReplicas
	// We'll calculate the desired replicas for each metric and take the maximum (standard HPA behavior)
	var maxDesiredReplicas int32 = 0

	for _, metric := range scaler.Spec.Metrics {
		var currentValue float64
		var err error

		if scaler.Spec.ServerType == autoscalingv1alpha1.ServerTypeCustom {
			// Fetch current metric value using the custom llm-monitor /api/capacity_load API
			currentValue, err = fetchMetricFromCustom(scaler.Spec.ServerAddress, metric.Type, scaler.Spec.Selector)
		} else {
			// Default to Prometheus
			currentValue, err = fetchMetricFromUpstream(scaler.Spec.ServerAddress, metric.Type, scaler.Spec.Selector)
		}

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

		// Algorithm: desiredReplicas = ceil[currentReplicas * (currentMetricValue / desiredMetricValue)]
		ratio := currentValue / targetValue
		metricDesired := int32(math.Ceil(float64(currentReplicas) * ratio))

		logger.Info("Metric calculation", "metric", metric.Type, "current", currentValue, "target", targetValue, "calculatedDesired", metricDesired)

		if metricDesired > maxDesiredReplicas {
			maxDesiredReplicas = metricDesired
		}
	}

	// Fallback to current replicas if no metrics were successfully calculated
	desiredReplicas := int32(currentReplicas)
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
	if desiredReplicas != int32(currentReplicas) {
		logger.Info("Scaling target", "from", currentReplicas, "to", desiredReplicas)

		if desiredReplicas < int32(currentReplicas) {
			// TODO: Cache-Aware Scale-Down
			// 1. Query Cache-Aware Router API
			// 2. Transition specific pod to Draining state
			// Note: When scaling down Deployments/StatefulSets, deleting a specific pod
			// requires controller logic to manage pod deletion cost annotations (for RS/Deployments)
			// or using specific controller mechanisms.
			logger.Info("Initiating Cache-Aware Scale-Down...")
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
	scaler.Status.CurrentReplicas = int32(currentReplicas)
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
		For(&autoscalingv1alpha1.LLMScaler{}).
		Named("llmscaler").
		Complete(r)
}

// fetchMetricFromCustom queries the custom metrics server API (e.g. llm-monitor /api/capacity_load)
func fetchMetricFromCustom(serverAddress string, metricType autoscalingv1alpha1.MetricType, selector map[string]string) (float64, error) {
	if serverAddress == "" {
		return 0, fmt.Errorf("serverAddress is empty, cannot fetch custom metrics")
	}

	modelName, ok := selector["model_name"]
	if !ok || modelName == "" {
		return 0, fmt.Errorf("selector must contain 'model_name' for custom metric fetching")
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}

	var endpoint string
	if metricType == autoscalingv1alpha1.MetricTypeTPMLoad {
		endpoint = "/api/tpm_load"
	} else if metricType == autoscalingv1alpha1.MetricTypeCapacityLoad {
		endpoint = "/api/capacity_load"
	} else {
		return 0, fmt.Errorf("unsupported custom metric type: %s", metricType)
	}

	// Hit the correct endpoint based on metric type
	queryURL := fmt.Sprintf("%s%s?limit=1&model=%s", strings.TrimRight(serverAddress, "/"), endpoint, url.QueryEscape(modelName))

	req, err := http.NewRequest("GET", queryURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	// Add headers from the full example
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Basic YWRtaW46NHBkYWRtaW4yMDI2IQ==")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch from custom metrics server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("invalid response from custom metrics server: %d %s", resp.StatusCode, string(bodyBytes))
	}

	var respData struct {
		Samples []struct {
			UtilPct float64 `json:"util_pct"`
		} `json:"samples"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return 0, fmt.Errorf("failed to decode JSON from custom metrics server: %w", err)
	}

	if len(respData.Samples) == 0 {
		return 0, fmt.Errorf("no samples returned from custom metrics server for model %s", modelName)
	}

	// The API returns samples in ascending order, so we take the last one
	latestSample := respData.Samples[len(respData.Samples)-1]

	// Assume util_pct corresponds to the utilization we want to scale on
	return latestSample.UtilPct, nil
}

// parseTargetValue handles parsing values like "80%" or "5" into a float64
func parseTargetValue(val string) (float64, error) {
	val = strings.TrimSpace(val)
	if before, ok :=strings.CutSuffix(val, "%"); ok  {
		numStr := before
		return strconv.ParseFloat(numStr, 64)
	}
	return strconv.ParseFloat(val, 64)
}

// fetchMetricFromUpstream queries the metrics server (e.g., Prometheus)
func fetchMetricFromUpstream(serverAddress string, metricType autoscalingv1alpha1.MetricType, selector map[string]string) (float64, error) {
	if serverAddress == "" {
		return 0, fmt.Errorf("serverAddress is empty")
	}

	// 1. Determine the metric name
	var metricName string
	switch metricType {
	case autoscalingv1alpha1.MetricTypeKVCacheUtilization:
		metricName = "vllm:gpu_cache_usage_perc"
	case autoscalingv1alpha1.MetricTypeQueueDepth:
		metricName = "vllm:num_requests_waiting"
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

	// 3. Construct the full PromQL query
	promQL := fmt.Sprintf("avg(%s%s)", metricName, selectorStr)

	// 4. Make the HTTP request
	queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", strings.TrimRight(serverAddress, "/"), url.QueryEscape(promQL))

	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(queryURL)
	if err != nil {
		return 0, fmt.Errorf("failed to query prometheus: %w", err)
	}
	defer resp.Body.Close()

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
