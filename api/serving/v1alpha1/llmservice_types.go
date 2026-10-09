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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// ConditionApplied is true once the resolved chart and merged layers are
	// applied to the helm release.
	ConditionApplied = "Applied"
	// ConditionAdopted is true once an existing helm release has been adopted.
	ConditionAdopted = "Adopted"

	// ReasonDrift means adoption refused because the release does not match
	// the resolved chart and merged layers. No upgrade is attempted.
	ReasonDrift = "Drift"
	// ReasonInstalled means the helm release was installed.
	ReasonInstalled = "Installed"
	// ReasonUpgraded means the helm release was upgraded.
	ReasonUpgraded = "Upgraded"
	// ReasonAdopted means an existing helm release was adopted without a helm call.
	ReasonAdopted = "Adopted"
	// ReasonFailed means reconciliation failed.
	ReasonFailed = "Failed"
	// ReasonApplyFailed means helm install or upgrade failed.
	ReasonApplyFailed = "ApplyFailed"
	// ReasonChartResolveFailed means spec.chart.version could not be resolved.
	ReasonChartResolveFailed = "ChartResolveFailed"
	// ReasonSuspended means spec.suspend stopped reconciliation.
	ReasonSuspended = "Suspended"

	// Finalizer keeps the LLMService until the helm release is uninstalled,
	// unless AnnotationDeletionPolicy is DeletionPolicyOrphan.
	Finalizer = "serving.modelsphere.dev/finalizer"

	// AnnotationDeletionPolicy requests how deletion treats the helm release.
	// The only value is DeletionPolicyOrphan.
	AnnotationDeletionPolicy = "serving.modelsphere.dev/deletion-policy"
	// DeletionPolicyOrphan drops the finalizer without uninstalling the release.
	DeletionPolicyOrphan = "Orphan"

	// AnnotationForceConflicts, set to a generation, lets that generation's
	// apply take fields other managers own. Any other generation ignores it.
	AnnotationForceConflicts = "serving.modelsphere.dev/force-conflicts"

	// SwissAnnotationPrefix selects annotations copied into each history entry.
	// The operator otherwise ignores them.
	SwissAnnotationPrefix = "swiss.modelsphere.dev/"

	// DefaultHistoryLimit is how many status.history entries are kept, newest first.
	DefaultHistoryLimit = 10
)

// Phase is the high-level reconciliation state of an LLMService.
type Phase string

const (
	// PhasePending means the spec has not been applied yet.
	PhasePending Phase = "Pending"
	// PhaseApplying means a helm install or upgrade is in progress.
	PhaseApplying Phase = "Applying"
	// PhaseApplied means the helm release matches the resolved spec.
	PhaseApplied Phase = "Applied"
	// PhaseFailed means the last reconcile stopped on an error.
	PhaseFailed Phase = "Failed"
)

// LocalObjectReference names an object in the LLMService's namespace.
// credentialsRef uses it for the Secret that holds chart-pull credentials.
type LocalObjectReference struct {
	// name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ChartSpec selects the helm chart installed for this release.
type ChartSpec struct {
	// name is the chart name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// repo is the chart repository URL. It must start with https:// or oci://.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(https://|oci://).+`
	Repo string `json:"repo"`

	// version is an exact chart version or a semver range. The operator
	// resolves it once per spec change and pins the result on status.chart.
	// While the pinned version still satisfies the range, a newer chart in
	// the repository does not move the release.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// credentialsRef optionally names a Secret in this namespace used to pull
	// the chart and to list its versions. The Secret data must contain the
	// keys username and password. When unset, the pull and the listing are anonymous.
	// +optional
	CredentialsRef *LocalObjectReference `json:"credentialsRef,omitempty"`
}

// Layer is one values document. Layers merge in list order, last writer wins.
type Layer struct {
	// name identifies this layer. Names must be unique within spec.layers.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// values is a schemaless object of helm values contributed by this layer.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
}

// ModelSpec describes the model a service is serving. It is descriptive only:
// labels, printer columns, and catalog checks. It is not used to render the chart.
type ModelSpec struct {
	// name of the model.
	// +optional
	Name string `json:"name,omitempty"`

	// version of the model.
	// +optional
	Version string `json:"version,omitempty"`

	// variant is the serving variant, such as a parallelism and hardware profile.
	// +optional
	Variant string `json:"variant,omitempty"`

	// hf is the Hugging Face repository id.
	// +optional
	HF string `json:"hf,omitempty"`

	// engine is the inference engine, such as sglang or vllm.
	// +optional
	Engine string `json:"engine,omitempty"`
}

// LLMServiceSpec defines the desired state of LLMService.
type LLMServiceSpec struct {
	// chart selects the helm chart for this release.
	// +required
	Chart ChartSpec `json:"chart"`

	// layers are helm values documents merged in order, last writer wins.
	// Names are unique so each layer can be owned separately under
	// server-side apply. Order is the meaning of the list, so one writer
	// should own the whole list. An empty list is valid and leaves the
	// chart defaults in place.
	// +listType=map
	// +listMapKey=name
	// +optional
	Layers []Layer `json:"layers,omitempty"`

	// model is descriptive metadata for the served model. It is not used to
	// render the chart.
	// +optional
	Model *ModelSpec `json:"model,omitempty"`

	// suspend stops reconciliation. The status keeps the last outcome.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ChartStatus is the chart spec.chart.version resolved to.
type ChartStatus struct {
	// name is the chart name that was resolved.
	// +optional
	Name string `json:"name,omitempty"`

	// version is the exact chart version resolved from spec.chart.version.
	// +optional
	Version string `json:"version,omitempty"`
}

// HelmStatus is the helm release last observed for this LLMService.
type HelmStatus struct {
	// revision is the helm release revision.
	// +optional
	Revision int `json:"revision,omitempty"`

	// status is the helm release status, such as deployed.
	// +optional
	Status string `json:"status,omitempty"`
}

// HistoryEntry records one applied revision. status.history is newest first.
type HistoryEntry struct {
	// revision is the helm release revision this entry records.
	Revision int `json:"revision"`

	// hash is the appliedHash at this revision.
	Hash string `json:"hash"`

	// appliedAt is when this revision was recorded.
	AppliedAt metav1.Time `json:"appliedAt"`

	// controllerRevision is the name of the ControllerRevision that stores
	// the spec snapshot for this revision.
	ControllerRevision string `json:"controllerRevision"`

	// annotations are the swiss.modelsphere.dev/* annotations copied from the
	// LLMService when this revision was recorded.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// forceConflicts records that this apply honored
	// serving.modelsphere.dev/force-conflicts for its generation.
	// +optional
	ForceConflicts bool `json:"forceConflicts,omitempty"`

	// action is the swiss action copied for this revision (install, upgrade,
	// rollback, or migrate), when one was set.
	// +optional
	Action string `json:"action,omitempty"`
}

// LLMServiceStatus defines the observed state of LLMService.
type LLMServiceStatus struct {
	// observedGeneration is the metadata.generation last reconciled to a
	// terminal outcome.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is the current reconciliation phase: Pending, Applying, Applied,
	// or Failed.
	// +kubebuilder:validation:Enum=Pending;Applying;Applied;Failed
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// conditions represent the current state of the LLMService.
	// Types used by the operator are Applied and Adopted.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// message is the helm error when phase is Failed, and empty otherwise.
	// +optional
	Message string `json:"message,omitempty"`

	// chart is the chart name and the exact version spec.chart.version
	// resolved to.
	// +optional
	Chart ChartStatus `json:"chart,omitempty"`

	// appliedHash is a digest over status.chart and the merged layers.
	// +optional
	AppliedHash string `json:"appliedHash,omitempty"`

	// helm is the release revision and status last observed.
	// +optional
	Helm HelmStatus `json:"helm,omitempty"`

	// history records applies, newest first. The controller keeps the most
	// recent DefaultHistoryLimit (10) entries.
	// +optional
	History []HistoryEntry `json:"history,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=llmservices,scope=Namespaced,shortName=llmsvc
// +kubebuilder:printcolumn:name="Chart",type="string",JSONPath=".spec.chart.name",description="Chart name"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.chart.version",description="Resolved chart version"
// +kubebuilder:printcolumn:name="Model",type="string",JSONPath=".spec.model.name",description="Model name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Reconciliation phase"
// +kubebuilder:printcolumn:name="Revision",type="integer",JSONPath=".status.helm.revision",description="Helm release revision"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// LLMService is the Schema for the llmservices API. One LLMService maps to one
// helm release in the same namespace; metadata.name is the release name.
type LLMService struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of LLMService
	// +required
	Spec LLMServiceSpec `json:"spec"`

	// status defines the observed state of LLMService
	// +optional
	Status LLMServiceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// LLMServiceList contains a list of LLMService
type LLMServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []LLMService `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &LLMService{}, &LLMServiceList{})
		return nil
	})
}
