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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// TargetRef defines the target resource to scale (e.g., Deployment)
type TargetRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// Metric providers accepted by LLMScalerSpec.MetricProvider.
const (
	// MetricProviderPrometheus evaluates spec.metrics as PromQL against
	// spec.serverAddress. This is the default.
	MetricProviderPrometheus = "Prometheus"

	// MetricProviderCustom reads a replica recommendation straight off a
	// decision server; see CustomProviderSpec.
	MetricProviderCustom = "Custom"
)

// CustomProviderSpec configures the "Custom" metric provider: an external
// decision server that replaces Prometheus and hands back a replica count
// directly, rather than a measurement to divide by a target.
//
// spec.serverAddress becomes that server's base URL and spec.serverHeaders are
// sent with the request, exactly as for Prometheus. The request is
//
//	GET {serverAddress}{path}?serviceId={serviceId}
//
// and the reply is expected to carry a recognised apiVersion — the schema is
// keyed off that field, so an unknown one is refused rather than parsed
// hopefully. Of the payload only decisions[].replicas.active is read, and it is
// used as the recommendation itself: it is an absolute replica count, not a
// per-replica value, so readyReplicas plays no part in it. Everything after
// that is unchanged — the count is clamped to [minReplicas, maxReplicas], damped
// by scaleDown.stabilizationWindowSeconds, and paced by
// scaleDown.maxStepReplicas.
//
// A request that fails, a reply in an unknown schema, and a reply with no
// matching decision are all treated the way an unevaluable metric is: that sync
// is skipped and the replica count is held where it is, never read as zero.
type CustomProviderSpec struct {
	// serviceId names the service whose decision to read. It is sent as the
	// serviceId query parameter and is also matched against
	// decisions[].serviceId in the reply, since the server is free to answer
	// with more than it was asked for.
	ServiceID string `json:"serviceId"`

	// namespace optionally narrows the match to decisions carrying this
	// namespace, for a server that reports the same serviceId in several. When
	// empty, serviceId alone selects the decision — and a serviceId matching
	// more than one decision is an error rather than a choice, so set this if
	// the server ever reports duplicates.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// path is the request path on serverAddress. Defaults to /decisions.
	// +kubebuilder:default="/decisions"
	// +optional
	Path string `json:"path,omitempty"`
}

// MetricSpec defines a Prometheus query and the target value to scale on.
type MetricSpec struct {
	// name is an optional identifier for this metric, used in logs and events.
	// +optional
	Name string `json:"name,omitempty"`

	// query is a PromQL instant query returning the current metric value as a
	// single (averaged) scalar. Scaling compares it to target as
	//   desiredReplicas = ceil(readyReplicas * value / target)
	// so it should be a per-replica value — e.g. wrap it in avg(...). Bake any
	// label filters into the query. Example:
	//   avg(vllm:kv_cache_usage_perc{model_name="opt-125m"})
	// A result of more than one series is an error, not a sample to pick from:
	// aggregate away every label the source varies on (backend, model, route,
	// pod). A query that errors, returns no data, or returns NaN/Inf is skipped
	// for that sync rather than read as zero; if every metric is skipped the
	// replica count is held where it is.
	Query string `json:"query"`

	// target is the desired per-replica value of the query result. When the
	// measured value exceeds target, replicas scale up proportionally. It must be
	// a finite positive number on the same scale as the query — a "%" suffix is
	// rejected, since "80%" cannot be resolved without knowing whether the query
	// returns a 0-1 fraction (write "0.8") or a 0-100 percentage (write "80").
	Target string `json:"target"`
}

// ScaleDownSpec defines cache-aware scale down behavior
type ScaleDownSpec struct {
	// stabilizationWindowSeconds dampens scale-down: replicas are held at the
	// highest recommendation seen within this many seconds, so a brief metric dip
	// doesn't shrink the fleet. Scale-up is immediate. 0 disables stabilization.
	// +optional
	StabilizationWindowSeconds int32 `json:"stabilizationWindowSeconds,omitempty"`

	// maxStepReplicas caps how many replicas a single scale-down step may remove,
	// so the fleet shrinks gradually instead of dropping straight from N to
	// minReplicas when the metric collapses. It bounds the size of a step, not how
	// often one happens: the next scale-down also waits for the previous one to
	// settle, so steps are at least syncPeriodSeconds apart and can be further
	// apart while the target is still converging. A full descent therefore takes
	// roughly ceil((current-minReplicas)/maxStepReplicas) steps at that spacing —
	// on a slow-settling workload prefer a larger step. It rate-limits the write
	// only:
	// status.desiredReplicas keeps reporting the uncapped recommendation, and
	// subsequent syncs keep stepping toward it until they meet. Scaling never goes
	// below minReplicas, and scale-up is unaffected. 0 (default) means unlimited —
	// scale down straight to the recommendation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxStepReplicas int32 `json:"maxStepReplicas,omitempty"`

	// behavior controls how pods are chosen when scaling down.
	// "CacheAware" (default) biases removal toward the coldest-cache pods by
	// setting a low controller.kubernetes.io/pod-deletion-cost on the pods being
	// removed, so the ReplicaSet deletes them first. This is best-effort and only
	// applies to Deployment/ReplicaSet targets — StatefulSet/LWS scale-down is
	// ordinal-based and ignores the hint. "None" opts out and uses the workload
	// controller's default deletion order.
	// +kubebuilder:validation:Enum=CacheAware;None
	// +kubebuilder:default="CacheAware"
	Behavior string `json:"behavior,omitempty"`

	// deletionCostQuery is an optional PromQL instant query used to compute each
	// pod's controller.kubernetes.io/pod-deletion-cost at scale-down time (only
	// applies when behavior is CacheAware). It is evaluated against
	// spec.serverAddress and must return one series per pod
	// carrying a "pod" label; the sample value becomes that pod's deletion cost.
	// Lower cost is deleted first, so the expression should yield lower numbers
	// for colder / less valuable pods (e.g. "vllm:kv_cache_usage_perc * 100").
	// When empty, a newest-pod-first heuristic is used instead. It is PromQL, so
	// it is only consulted under the Prometheus metric provider — under Custom,
	// spec.serverAddress is a decision server with no query API, and the
	// heuristic is used regardless of what is set here.
	// +optional
	DeletionCostQuery string `json:"deletionCostQuery,omitempty"`
}

// PreemptionSpec defines preemption capabilities
type PreemptionSpec struct {
	Enable        bool   `json:"enable,omitempty"`
	PriorityClass string `json:"priorityClass,omitempty"`
}

// LLMScalerSpec defines the desired state of LLMScaler
// +kubebuilder:validation:XValidation:rule="(has(self.metricProvider) && self.metricProvider == 'Custom') || (has(self.metrics) && size(self.metrics) > 0)",message="spec.metrics must be non-empty unless spec.metricProvider is Custom"
// +kubebuilder:validation:XValidation:rule="!(has(self.metricProvider) && self.metricProvider == 'Custom') || has(self.customProvider)",message="spec.customProvider is required when spec.metricProvider is Custom"
type LLMScalerSpec struct {
	// targetRef points to the resource (e.g., Deployment) to scale
	TargetRef TargetRef `json:"targetRef"`

	// metricProvider selects where the scaling signal comes from.
	// "Prometheus" (default) evaluates spec.metrics as PromQL against
	// spec.serverAddress and derives replicas from them. "Custom" ignores
	// spec.metrics entirely and reads a replica count straight off the decision
	// server described by spec.customProvider — the two are alternatives, not
	// layers, and there is no fallback from one to the other.
	// +kubebuilder:validation:Enum=Prometheus;Custom
	// +kubebuilder:default=Prometheus
	// +optional
	MetricProvider string `json:"metricProvider,omitempty"`

	// customProvider configures the Custom metric provider. Required when
	// metricProvider is Custom, ignored otherwise.
	// +optional
	CustomProvider *CustomProviderSpec `json:"customProvider,omitempty"`

	// serverAddress is the metric source endpoint: the Prometheus query API
	// (e.g. http://prometheus:9090) under the Prometheus provider, or the
	// decision server's base URL (e.g. http://decisions:80) under Custom.
	ServerAddress string `json:"serverAddress"`

	// serverHeaders are additional HTTP headers sent with every metric-fetch
	// request to the metric source (e.g. Authorization for a secured endpoint).
	// Values are used verbatim.
	// +optional
	ServerHeaders map[string]string `json:"serverHeaders,omitempty"`

	// syncPeriodSeconds is the interval at which the autoscaler evaluates metrics and scales. Defaults to 15.
	// +kubebuilder:default=15
	// +optional
	SyncPeriodSeconds int32 `json:"syncPeriodSeconds,omitempty"`

	// retryPeriodSeconds is the interval at which the autoscaler retries when an error occurs or target is not found. Defaults to 10.
	// +kubebuilder:default=10
	// +optional
	RetryPeriodSeconds int32 `json:"retryPeriodSeconds,omitempty"`

	// minReplicas is the lower limit for the number of replicas to which the autoscaler can scale down
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas"`

	// maxReplicas is the upper limit for the number of replicas to which the autoscaler can scale up
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// metrics contains the specifications for which to use to calculate the
	// desired replica count. Required (and non-empty) under the Prometheus
	// provider; ignored under Custom, which gets its recommendation from the
	// decision server instead.
	// +optional
	Metrics []MetricSpec `json:"metrics,omitempty"`

	// scaleDown defines the behavior for scaling down, e.g. cache-aware teardown
	// +optional
	ScaleDown ScaleDownSpec `json:"scaleDown,omitempty"`

	// preemption defines whether priority-based preemption is enabled
	// +optional
	Preemption PreemptionSpec `json:"preemption,omitempty"`
}

// LLMScalerStatus defines the observed state of LLMScaler.
type LLMScalerStatus struct {
	// currentReplicas is the current number of replicas of pods managed by this autoscaler
	// +optional
	CurrentReplicas int32 `json:"currentReplicas"`

	// desiredReplicas is the desired number of replicas of pods managed by this autoscaler
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas"`

	// conditions represent the current state of the LLMScaler resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MinReplicas",type="integer",JSONPath=".spec.minReplicas",description="The lower limit for the number of replicas"
// +kubebuilder:printcolumn:name="MaxReplicas",type="integer",JSONPath=".spec.maxReplicas",description="The upper limit for the number of replicas"
// +kubebuilder:printcolumn:name="CurrentReplicas",type="integer",JSONPath=".status.currentReplicas",description="The current number of replicas"
// +kubebuilder:printcolumn:name="DesiredReplicas",type="integer",JSONPath=".status.desiredReplicas",description="The desired number of replicas"

// LLMScaler is the Schema for the llmscalers API
type LLMScaler struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of LLMScaler
	// +required
	Spec LLMScalerSpec `json:"spec"`

	// status defines the observed state of LLMScaler
	// +optional
	Status LLMScalerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LLMScalerList contains a list of LLMScaler
type LLMScalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LLMScaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &LLMScaler{}, &LLMScalerList{})
		return nil
	})
}
