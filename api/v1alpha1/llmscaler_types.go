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

type MetricType string

const (
	MetricTypeKVCacheUtilization MetricType = "KVCacheUtilization"
	MetricTypeQueueDepth         MetricType = "QueueDepth"
)

// MetricSpec defines the metric to monitor for scaling decisions
type MetricSpec struct {
	Type               MetricType `json:"type"`
	TargetAverageValue string     `json:"targetAverageValue"`
}

// ScaleDownSpec defines cache-aware scale down behavior
type ScaleDownSpec struct {
	StabilizationWindowSeconds int32 `json:"stabilizationWindowSeconds,omitempty"`
	// +kubebuilder:default="TargetedDeletion"
	Behavior string `json:"behavior,omitempty"`
}

// PreemptionSpec defines preemption capabilities
type PreemptionSpec struct {
	Enable        bool   `json:"enable,omitempty"`
	PriorityClass string `json:"priorityClass,omitempty"`
}

// LLMScalerSpec defines the desired state of LLMScaler
type LLMScalerSpec struct {
	// targetRef points to the resource (e.g., Deployment) to scale
	TargetRef TargetRef `json:"targetRef"`

	// minReplicas is the lower limit for the number of replicas to which the autoscaler can scale down
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas"`

	// maxReplicas is the upper limit for the number of replicas to which the autoscaler can scale up
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// metrics contains the specifications for which to use to calculate the desired replica count
	Metrics []MetricSpec `json:"metrics"`

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
