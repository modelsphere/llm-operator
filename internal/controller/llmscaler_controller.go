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
	"time"

	"k8s.io/apimachinery/pkg/runtime"
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *LLMScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var scaler autoscalingv1alpha1.LLMScaler
	if err := r.Get(ctx, req.NamespacedName, &scaler); err != nil {
		logger.Error(err, "unable to fetch LLMScaler")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling LLMScaler", "name", scaler.Name, "namespace", scaler.Namespace)

	// TODO: Step 1 - Fetch target LLM related metrics
	// Based on scaler.Spec.Metrics, query the metrics aggregation endpoint or Prometheus.

	// TODO: Step 2 - Make decision: SCALE-UP, SCALE-DOWN, or NO-OP
	// Compare actual replicas against MinReplicas/MaxReplicas and metric targets.

	// TODO: Step 3 - Execute Scale action
	// If SCALE-UP:
	//   Fetch the target deployment using scaler.Spec.TargetRef.
	//   Update deployment.Spec.Replicas count.
	// If SCALE-DOWN:
	//   Query the Cache-Aware Router API for the pod with least valuable cache.
	//   Transition pod to Draining state (update pod labels/annotations).
	//   Wait for the preStop hook to complete graceful termination.

	// Requeue the request after some interval to continuously monitor and scale
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LLMScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.LLMScaler{}).
		Named("llmscaler").
		Complete(r)
}
