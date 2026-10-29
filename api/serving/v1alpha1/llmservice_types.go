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
	// ReasonChartRepoNotAllowed means spec.chart.repo is not one the operator
	// was started to install from. Nothing was fetched or applied.
	ReasonChartRepoNotAllowed = "ChartRepoNotAllowed"
	// ReasonPendingRelease means a pending helm release belongs to another
	// client. The controller does not write to it.
	ReasonPendingRelease = "PendingRelease"
	// ReasonOperationInProgress means this object owns a pending helm release
	// that is not yet old enough to recover.
	ReasonOperationInProgress = "OperationInProgress"
	// ReasonRecoveredPendingRelease means a stale pending helm release owned
	// by this object was rolled back or uninstalled so apply could continue.
	ReasonRecoveredPendingRelease = "RecoveredPendingRelease"
	// ReasonSuspended means spec.suspend stopped reconciliation.
	ReasonSuspended = "Suspended"

	// Finalizer keeps the LLMService until the helm release is uninstalled,
	// unless AnnotationDeletionPolicy is DeletionPolicyOrphan. Only a release
	// the LLMService installed or adopted is uninstalled; any other is kept.
	Finalizer = "serving.modelsphere.dev/finalizer"

	// AnnotationDeletionPolicy requests how deletion treats the helm release.
	// The only value is DeletionPolicyOrphan.
	AnnotationDeletionPolicy = "serving.modelsphere.dev/deletion-policy"
	// DeletionPolicyOrphan drops the finalizer without uninstalling the release.
	// The ModelRoute and the pods are left as they are.
	DeletionPolicyOrphan = "Orphan"

	// AnnotationDeletionMaxWait overrides spec.deletion.maxWait for one object.
	// It is read on every reconcile while the object is deleting.
	AnnotationDeletionMaxWait = "serving.modelsphere.dev/deletion-max-wait"
	// AnnotationRouteSettle overrides spec.deletion.routeSettle.
	AnnotationRouteSettle = "serving.modelsphere.dev/route-settle"
	// AnnotationForceDeleteAfter overrides spec.deletion.forceDeleteAfter.
	// A value of 0s force-deletes a pod as soon as it has a deletionTimestamp.
	AnnotationForceDeleteAfter = "serving.modelsphere.dev/force-delete-after"

	// ConditionDeleting is true while a deleting LLMService still has its finalizer.
	ConditionDeleting = "Deleting"

	// ReasonRouteRemoving means the ModelRoute from the release is being deleted.
	ReasonRouteRemoving = "RouteRemoving"
	// ReasonRouteSettling means the ModelRoute is gone and routes are settling.
	ReasonRouteSettling = "RouteSettling"
	// ReasonRouteStuck means the ModelRoute was still present when the budget elapsed.
	// Its finalizer is left in place.
	ReasonRouteStuck = "RouteStuck"
	// ReasonNoRoute means the release manifest had no ModelRoute.
	ReasonNoRoute = "NoRoute"
	// ReasonUninstalled means the helm release was uninstalled.
	ReasonUninstalled = "Uninstalled"
	// ReasonWaitingForPods means release pods are still present.
	ReasonWaitingForPods = "WaitingForPods"
	// ReasonPodsForceDeleted means at least one pod was force-deleted.
	ReasonPodsForceDeleted = "PodsForceDeleted"
	// ReasonTimedOut means the deletion budget elapsed while pods were still present.
	ReasonTimedOut = "TimedOut"
	// ReasonReleased means staged deletion finished and the finalizer can drop.
	ReasonReleased = "Released"
	// ReasonInvalidAnnotation means a deletion annotation could not be parsed.
	ReasonInvalidAnnotation = "InvalidAnnotation"
	// ReasonPodForceDeleted means one pod was deleted with a zero grace period.
	ReasonPodForceDeleted = "PodForceDeleted"
	// ReasonKeptRelease means deletion left a helm release this object does not own.
	ReasonKeptRelease = "KeptRelease"

	// AnnotationForceConflicts, set to a generation, lets that generation's
	// apply take fields other managers own. Any other generation ignores it.
	AnnotationForceConflicts = "serving.modelsphere.dev/force-conflicts"

	// AnnotationReResolve, set to a value status.chart.reResolve does not hold,
	// drops the pinned status.chart.version once, so a range resolves to the
	// newest chart the repository holds. Any later new value does it again.
	AnnotationReResolve = "serving.modelsphere.dev/re-resolve"

	// AnnotationSkipSchemaValidation, set to "true", installs and upgrades
	// without checking the merged values against the chart's
	// values.schema.json, as helm's --skip-schema-validation does. Any other
	// value leaves the check on. Not part of the applied hash: turning it on
	// re-applies only a release whose last apply did not succeed.
	AnnotationSkipSchemaValidation = "serving.modelsphere.dev/skip-schema-validation"

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
	// PhaseDeleting means the object is deleting and staged teardown is in progress.
	PhaseDeleting Phase = "Deleting"
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
	// resolves it on first apply and pins the result on status.chart. While
	// the pinned version still satisfies the range it is kept, through other
	// spec changes and newer charts in the repository alike; a new
	// serving.modelsphere.dev/re-resolve value moves it to the newest in range.
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
	// Suspend wins over deletion: a suspended object that is deleting is left
	// as it is, including its finalizer.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// deletion tunes staged teardown. Annotations on the object override these
	// fields, and an unset field uses the built-in default. An explicit 0s is
	// a real zero (routeSettle 0s skips the settle wait).
	// +optional
	Deletion *DeletionSpec `json:"deletion,omitempty"`
}

// DeletionSpec tunes staged deletion of an LLMService.
type DeletionSpec struct {
	// maxWait is the budget for holding the finalizer, measured from
	// status.deletion.startedAt. Past it, remaining waits are skipped and the
	// finalizer is dropped. The annotation
	// serving.modelsphere.dev/deletion-max-wait overrides this on every pass.
	// +kubebuilder:default="1h"
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="maxWait must be a non-negative duration"
	// +optional
	MaxWait *metav1.Duration `json:"maxWait,omitempty"`

	// routeSettle is how long to wait after the ModelRoute is gone so the
	// gateway can drop it before the Services disappear. 0, via the
	// annotation serving.modelsphere.dev/route-settle, skips the wait. The
	// built-in default is 60s.
	// +kubebuilder:default="60s"
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="routeSettle must be a non-negative duration"
	// +optional
	RouteSettle *metav1.Duration `json:"routeSettle,omitempty"`

	// forceDeleteAfter force-deletes pods that have been terminating for this
	// long, with gracePeriodSeconds 0. The wait starts when deletion was
	// requested. Unset leaves the pods to the kubelet. The annotation
	// serving.modelsphere.dev/force-delete-after overrides this.
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="forceDeleteAfter must be a non-negative duration"
	// +optional
	ForceDeleteAfter *metav1.Duration `json:"forceDeleteAfter,omitempty"`
}

// DeletionStage is the monotonic stage of a staged deletion.
type DeletionStage string

const (
	// DeletionStageRouteRemoving deletes the ModelRoute and waits for it to go.
	DeletionStageRouteRemoving DeletionStage = "RouteRemoving"
	// DeletionStageRouteSettling waits after the ModelRoute is NotFound.
	DeletionStageRouteSettling DeletionStage = "RouteSettling"
	// DeletionStageUninstalling uninstalls the helm release.
	DeletionStageUninstalling DeletionStage = "Uninstalling"
	// DeletionStagePodsTerminating waits for the release's pods to go.
	DeletionStagePodsTerminating DeletionStage = "PodsTerminating"
	// DeletionStageForceDeleting force-deletes pods past forceDeleteAfter.
	DeletionStageForceDeleting DeletionStage = "ForceDeleting"
	// DeletionStageReleased means the finalizer may be dropped.
	DeletionStageReleased DeletionStage = "Released"
)

// DeletionOutcome is how staged deletion finished.
type DeletionOutcome string

const (
	// DeletionOutcomeClean means every stage finished inside the budget.
	DeletionOutcomeClean DeletionOutcome = "Clean"
	// DeletionOutcomeNoRoute means the release manifest had no ModelRoute.
	DeletionOutcomeNoRoute DeletionOutcome = "NoRoute"
	// DeletionOutcomeRouteStuck means the ModelRoute was still present at the deadline.
	DeletionOutcomeRouteStuck DeletionOutcome = "RouteStuck"
	// DeletionOutcomePodsForceDeleted means at least one pod was force-deleted.
	DeletionOutcomePodsForceDeleted DeletionOutcome = "PodsForceDeleted"
	// DeletionOutcomeTimedOut means the budget elapsed while pods were still present.
	DeletionOutcomeTimedOut DeletionOutcome = "TimedOut"
)

// ChartStatus is the chart spec.chart.version resolved to.
type ChartStatus struct {
	// name is the chart name that was resolved.
	// +optional
	Name string `json:"name,omitempty"`

	// version is the exact chart version resolved from spec.chart.version.
	// +optional
	Version string `json:"version,omitempty"`

	// reResolve is the serving.modelsphere.dev/re-resolve value already
	// honored. Only a different non-empty value re-resolves.
	// +optional
	ReResolve string `json:"reResolve,omitempty"`
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

	// reResolved records that this apply honored a new
	// serving.modelsphere.dev/re-resolve value rather than the pinned version.
	// +optional
	ReResolved bool `json:"reResolved,omitempty"`

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
	// Failed, or Deleting.
	// +kubebuilder:validation:Enum=Pending;Applying;Applied;Failed;Deleting
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

	// releaseUID is the UID of the LLMService that claimed this helm release.
	// The controller writes it before install, and again on adopt and on every
	// successful apply. Ownership is this value equal to metadata.uid, or the
	// release label serving.modelsphere.dev/llmservice-uid equal to that uid.
	// A later condition change does not drop the claim.
	// +optional
	ReleaseUID string `json:"releaseUID,omitempty"`

	// helm is the release revision and status last observed.
	// +optional
	Helm HelmStatus `json:"helm,omitempty"`

	// history records applies, newest first. The controller keeps the most
	// recent DefaultHistoryLimit (10) entries.
	// +optional
	History []HistoryEntry `json:"history,omitempty"`

	// deletion is the staged teardown observed while the object is deleting.
	// +optional
	Deletion *DeletionStatus `json:"deletion,omitempty"`
}

// DeletionStatus is the observed staged teardown of an LLMService.
type DeletionStatus struct {
	// stage is the current teardown stage. It only moves forward.
	// +kubebuilder:validation:Enum=RouteRemoving;RouteSettling;Uninstalling;PodsTerminating;ForceDeleting;Released
	// +optional
	Stage DeletionStage `json:"stage,omitempty"`

	// outcome is how teardown finished, or the non-clean result already known.
	// +kubebuilder:validation:Enum=Clean;NoRoute;RouteStuck;PodsForceDeleted;TimedOut
	// +optional
	Outcome DeletionOutcome `json:"outcome,omitempty"`

	// startedAt is when staged deletion began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// deadline is startedAt plus the effective maxWait. It is recomputed when
	// the annotation or the spec changes.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// routeRemovedAt is when the ModelRoute was first observed NotFound.
	// +optional
	RouteRemovedAt *metav1.Time `json:"routeRemovedAt,omitempty"`

	// uninstalledAt is when the helm uninstall succeeded.
	// +optional
	UninstalledAt *metav1.Time `json:"uninstalledAt,omitempty"`

	// podsGoneAt is when no release pods were left to wait for.
	// +optional
	PodsGoneAt *metav1.Time `json:"podsGoneAt,omitempty"`

	// podsRemaining is how many release pods still block the finalizer.
	// +optional
	PodsRemaining int32 `json:"podsRemaining,omitempty"`

	// pods are the release pods still in the API, ignoring Succeeded and Failed.
	// The list is capped at 20.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	Pods []DeletionPod `json:"pods,omitempty"`

	// forceDeleted are pod names deleted with gracePeriodSeconds 0. Capped at 20.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	ForceDeleted []string `json:"forceDeleted,omitempty"`

	// message is a human-readable line, for example
	// "waiting for 3 pods (2 stuck on NotReady node gpu-7)".
	// +optional
	Message string `json:"message,omitempty"`
}

// DeletionPod is one release pod observed during teardown.
type DeletionPod struct {
	// name of the pod.
	Name string `json:"name"`

	// phase is the pod phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// node is the node the pod is assigned to.
	// +optional
	Node string `json:"node,omitempty"`

	// deletionTimestamp is the pod's metadata.deletionTimestamp.
	// +optional
	DeletionTimestamp *metav1.Time `json:"deletionTimestamp,omitempty"`

	// grace is the pod's terminationGracePeriodSeconds. 30 when the pod does not set one.
	// +optional
	Grace int64 `json:"grace,omitempty"`

	// stuck is true when the pod is past its grace on a NotReady or missing
	// node, or past grace plus 5m, and no longer blocks the finalizer.
	// +optional
	Stuck bool `json:"stuck,omitempty"`
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
