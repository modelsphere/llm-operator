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
	"math"
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
		// Fetch current metric value from upstream server
		currentValue, err := fetchMetricFromUpstream(scaler.Spec.ServerAddress, metric.Type, scaler.Spec.Selector)
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

// fetchMetricFromUpstream is a mock function to query the metrics server (e.g., Prometheus)
func fetchMetricFromUpstream(serverAddress string, metricType autoscalingv1alpha1.MetricType, selector map[string]string) (float64, error) {
	if serverAddress == "" {
		return 0, fmt.Errorf("serverAddress is empty")
	}
	// TODO: Implement actual HTTP request to Prometheus API using the serverAddress and selector.
	// For example:
	// query := buildPromQL(metricType, selector)
	// resp := http.Get(fmt.Sprintf("%s/api/v1/query?query=%s", serverAddress, url.QueryEscape(query)))

	// Mocking a response for demonstration purposes:
	switch metricType {
	case autoscalingv1alpha1.MetricTypeKVCacheUtilization:
		// e.g., 85% cache utilized
		return 85.0, nil
	case autoscalingv1alpha1.MetricTypeQueueDepth:
		// e.g., 10 requests in queue per pod
		return 10.0, nil
	default:
		return 0, fmt.Errorf("unknown metric type")
	}
}

// parseTargetValue handles parsing values like "80%" or "5" into a float64
func parseTargetValue(val string) (float64, error) {
	val = strings.TrimSpace(val)
	if strings.HasSuffix(val, "%") {
		numStr := strings.TrimSuffix(val, "%")
		return strconv.ParseFloat(numStr, 64)
	}
	return strconv.ParseFloat(val, 64)
}
