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

package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/chart"
	"github.com/modelsphere/llm-operator/internal/helm"
)

const (
	llmserviceLabel       = "serving.modelsphere.dev/llmservice"
	llmserviceUIDLabel    = "serving.modelsphere.dev/llmservice-uid"
	credentialUsernameKey = "username"
	credentialPasswordKey = "password"
	actionAnnotation      = servingv1alpha1.SwissAnnotationPrefix + "action"
	// statusDeployed is the helm release status adoption accepts.
	statusDeployed        = "deployed"
	statusFailed          = "failed"
	statusPendingInstall  = "pending-install"
	statusPendingUpgrade  = "pending-upgrade"
	statusPendingRollback = "pending-rollback"
	minPendingRequeue     = 15 * time.Second
	pendingForeignRequeue = time.Minute
	// DefaultPendingTimeout is how long an owned pending release is left alone
	// before recovery. It is twice the helm apply timeout.
	DefaultPendingTimeout = 2 * helm.ApplyTimeout
)

// VersionLister lists chart versions for spec.chart. Tests set it.
// A nil lister reads the repository with chart.VersionsAuth.
type VersionLister func(ctx context.Context, repo, name string, creds *helm.Credentials) ([]string, error)

// LLMServiceReconciler reconciles a LLMService object.
type LLMServiceReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Helm         helm.Helm
	Recorder     events.EventRecorder
	HistoryLimit int
	// PendingTimeout is how long an owned pending release may stay pending
	// before recovery. Zero uses DefaultPendingTimeout.
	PendingTimeout time.Duration
	ListVersions   VersionLister
	// AllowedChartRepos are the chart repositories an LLMService may install
	// from; each allows itself and the paths under it. Empty allows any.
	AllowedChartRepos []string
}

// +kubebuilder:rbac:groups=serving.modelsphere.dev,resources=llmservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.modelsphere.dev,resources=llmservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.modelsphere.dev,resources=llmservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions;deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;configmaps;services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;create;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.4pd.io;autoscaling.modelsphere.dev,resources=llmscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=inference.x-k8s.io;inference.modelsphere.dev,resources=llmslorequirements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=routing.gpucluster.io;routing.modelsphere.dev,resources=modelroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile applies spec to one helm release, or adopts a release the operator did not install.
func (r *LLMServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var obj servingv1alpha1.LLMService
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Suspend wins over deletion: a suspended object is left as it is, including its finalizer.
	if obj.Spec.Suspend {
		return ctrl.Result{}, nil
	}
	if r.Helm == nil {
		return ctrl.Result{}, fmt.Errorf("helm client is not configured")
	}
	if !obj.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &obj)
	}
	if err := r.ensureFinalizer(ctx, &obj); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileApply(ctx, &obj)
}

func (r *LLMServiceReconciler) reconcileDelete(ctx context.Context, obj *servingv1alpha1.LLMService) (ctrl.Result, error) {
	if obj.Annotations[servingv1alpha1.AnnotationDeletionPolicy] != servingv1alpha1.DeletionPolicyOrphan {
		rel, err := r.Helm.Get(ctx, obj.Namespace, obj.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		switch {
		case rel == nil:
		case installedOrAdopted(obj, rel):
			if err := r.Helm.Uninstall(ctx, obj.Namespace, obj.Name); err != nil {
				return ctrl.Result{}, err
			}
			r.event(obj, corev1.EventTypeNormal, "Uninstalled", "Uninstalled helm release")
		default:
			r.event(obj, corev1.EventTypeNormal, "KeptRelease", "Kept helm release: this LLMService never installed or adopted it")
		}
	}
	if !controllerutil.ContainsFinalizer(obj, servingv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(obj, servingv1alpha1.Finalizer)
	if err := r.Update(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *LLMServiceReconciler) ensureFinalizer(ctx context.Context, obj *servingv1alpha1.LLMService) error {
	if controllerutil.ContainsFinalizer(obj, servingv1alpha1.Finalizer) {
		return nil
	}
	controllerutil.AddFinalizer(obj, servingv1alpha1.Finalizer)
	return r.Update(ctx, obj)
}

func (r *LLMServiceReconciler) reconcileApply(ctx context.Context, obj *servingv1alpha1.LLMService) (ctrl.Result, error) {
	// Before anything is fetched or applied: whoever can create an LLMService
	// runs the chart with the operator's permissions.
	if !chartRepoAllowed(r.AllowedChartRepos, obj.Spec.Chart.Repo) {
		err := fmt.Errorf("chart repository %s is not one this operator installs from (allowed: %s)", obj.Spec.Chart.Repo, strings.Join(r.AllowedChartRepos, ", "))
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonChartRepoNotAllowed, err, false)
	}
	creds, err := r.credentials(ctx, obj)
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonChartResolveFailed, err, true)
	}
	spec, err := chart.ParseSpec(obj.Spec.Chart.Version)
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonChartResolveFailed, err, true)
	}
	keep := ""
	if obj.Status.Chart.Name == obj.Spec.Chart.Name && !reResolving(obj) {
		keep = obj.Status.Chart.Version
	}
	ver, err := chart.Resolve(ctx, spec, "", keep, func(ctx context.Context) ([]string, error) {
		return r.listVersions(ctx, obj.Spec.Chart.Repo, obj.Spec.Chart.Name, creds)
	})
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonChartResolveFailed, err, true)
	}
	values, err := mergeLayers(obj.Spec.Layers)
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonFailed, err, true)
	}
	hash, err := appliedHash(obj.Spec.Chart.Name, ver, values)
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonFailed, err, true)
	}
	if hash == obj.Status.AppliedHash {
		return r.finishApplied(ctx, obj)
	}
	rel, err := r.Helm.Get(ctx, obj.Namespace, obj.Name)
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonApplyFailed, err, true)
	}
	if rel != nil && pendingStatus(rel.Status) {
		var stop bool
		var result ctrl.Result
		rel, result, stop, err = r.reconcilePending(ctx, obj, rel)
		if err != nil || stop {
			return result, err
		}
	}
	if rel != nil && obj.Status.AppliedHash == "" && len(obj.Status.History) == 0 {
		return r.reconcileUnowned(ctx, obj, ver, values, hash, creds, rel)
	}
	return r.apply(ctx, obj, ver, values, hash, creds, rel)
}

// reconcileUnowned handles a release this object has never recorded.
// An owned release is applied. Only a deployed release can be adopted.
// Anything else is drift and is not retried.
func (r *LLMServiceReconciler) reconcileUnowned(ctx context.Context, obj *servingv1alpha1.LLMService, ver string, values map[string]any, hash string, creds *helm.Credentials, rel *helm.Release) (ctrl.Result, error) {
	if ownedBy(obj, rel) {
		return r.apply(ctx, obj, ver, values, hash, creds, rel)
	}
	if rel.Status != statusDeployed {
		return r.drift(ctx, obj, fmt.Sprintf("release is %s, not deployed", rel.Status))
	}
	return r.adopt(ctx, obj, ver, values, hash, rel)
}

// reconcilePending handles a pending-* release. stop reports that result is
// the reconcile outcome. Otherwise rel is the release apply should see: nil
// after an uninstall, or the existing release after a rollback.
func (r *LLMServiceReconciler) reconcilePending(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release) (*helm.Release, ctrl.Result, bool, error) {
	if !ownedBy(obj, rel) {
		result, err := r.pendingForeign(ctx, obj, rel)
		return nil, result, true, err
	}
	timeout := r.pendingTimeout()
	age := max(time.Since(rel.Updated), 0)
	if age < timeout {
		remaining := max(timeout-age, minPendingRequeue)
		result, err := r.operationInProgress(ctx, obj, rel, remaining)
		return nil, result, true, err
	}
	next, err := r.recoverPending(ctx, obj, rel)
	if err != nil {
		result, ferr := r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonApplyFailed, err, true)
		return nil, result, true, ferr
	}
	return next, ctrl.Result{}, false, nil
}

func (r *LLMServiceReconciler) pendingForeign(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release) (ctrl.Result, error) {
	msg := fmt.Sprintf("release is %s; another client is operating on it", rel.Status)
	obj.Status.Phase = servingv1alpha1.PhaseFailed
	obj.Status.Message = msg
	obj.Status.ObservedGeneration = obj.Generation
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionApplied,
		Status:             metav1.ConditionFalse,
		Reason:             servingv1alpha1.ReasonPendingRelease,
		Message:            msg,
		ObservedGeneration: obj.Generation,
	})
	if err := r.updateStatus(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pendingForeignRequeue}, nil
}

func (r *LLMServiceReconciler) operationInProgress(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release, after time.Duration) (ctrl.Result, error) {
	msg := fmt.Sprintf("release is %s; operation in progress", rel.Status)
	obj.Status.Phase = servingv1alpha1.PhaseApplying
	obj.Status.Message = msg
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionApplied,
		Status:             metav1.ConditionFalse,
		Reason:             servingv1alpha1.ReasonOperationInProgress,
		Message:            msg,
		ObservedGeneration: obj.Generation,
	})
	if err := r.updateStatus(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// recoverPending clears a stale owned pending release. pending-install is
// uninstalled. pending-upgrade and pending-rollback roll back to the newest
// deployed revision, or are uninstalled when history has none.
func (r *LLMServiceReconciler) recoverPending(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release) (*helm.Release, error) {
	msg := fmt.Sprintf("recovered stale %s release", rel.Status)
	obj.Status.Phase = servingv1alpha1.PhaseApplying
	obj.Status.Message = msg
	obj.Status.ReleaseUID = string(obj.UID)
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionApplied,
		Status:             metav1.ConditionFalse,
		Reason:             servingv1alpha1.ReasonRecoveredPendingRelease,
		Message:            msg,
		ObservedGeneration: obj.Generation,
	})
	if err := r.updateStatus(ctx, obj); err != nil {
		return nil, err
	}
	next, err := r.rollPending(ctx, obj, rel)
	if err != nil {
		return nil, err
	}
	r.event(obj, corev1.EventTypeNormal, servingv1alpha1.ReasonRecoveredPendingRelease, msg)
	logf.FromContext(ctx).Info("Recovered pending helm release", "status", rel.Status)
	return next, nil
}

func (r *LLMServiceReconciler) rollPending(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release) (*helm.Release, error) {
	if rel.Status == statusPendingInstall {
		if err := r.Helm.Uninstall(ctx, obj.Namespace, obj.Name); err != nil {
			return nil, err
		}
		return nil, nil
	}
	rev, err := r.deployedRevision(ctx, obj)
	if err != nil {
		return nil, err
	}
	if rev == 0 {
		if err := r.Helm.Uninstall(ctx, obj.Namespace, obj.Name); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := r.Helm.Rollback(ctx, obj.Namespace, obj.Name, rev); err != nil {
		return nil, err
	}
	return rel, nil
}

func (r *LLMServiceReconciler) adopt(ctx context.Context, obj *servingv1alpha1.LLMService, ver string, values map[string]any, hash string, rel *helm.Release) (ctrl.Result, error) {
	chartOK := rel.ChartName == obj.Spec.Chart.Name && rel.ChartVersion == ver
	valuesOK, err := jsonEqual(rel.Config, values)
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonApplyFailed, err, true)
	}
	if chartOK && valuesOK {
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
			Type:               servingv1alpha1.ConditionAdopted,
			Status:             metav1.ConditionTrue,
			Reason:             servingv1alpha1.ReasonAdopted,
			ObservedGeneration: obj.Generation,
		})
		if err := r.recordSuccess(ctx, obj, rel, obj.Spec.Chart.Name, ver, hash, servingv1alpha1.ReasonAdopted, false); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	return r.drift(ctx, obj, driftMessage(obj.Spec.Chart.Name, ver, rel, !valuesOK))
}

// drift records a terminal adoption failure and does not requeue.
func (r *LLMServiceReconciler) drift(ctx context.Context, obj *servingv1alpha1.LLMService, msg string) (ctrl.Result, error) {
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionApplied,
		Status:             metav1.ConditionFalse,
		Reason:             servingv1alpha1.ReasonDrift,
		Message:            msg,
		ObservedGeneration: obj.Generation,
	})
	return r.fail(ctx, obj, servingv1alpha1.ConditionAdopted, servingv1alpha1.ReasonDrift, errors.New(msg), false)
}

func (r *LLMServiceReconciler) apply(ctx context.Context, obj *servingv1alpha1.LLMService, ver string, values map[string]any, hash string, creds *helm.Credentials, existing *helm.Release) (ctrl.Result, error) {
	// An owned failed release with no deployed revision is installed again.
	// One that still has a deployed revision is upgraded.
	install, err := r.shouldInstall(ctx, obj, existing)
	if err != nil {
		obj.Status.ReleaseUID = string(obj.UID)
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonApplyFailed, err, true)
	}
	// Claim the release before Install, including the uninstall that precedes
	// a retry of a failed first install. observedGeneration stays at the last
	// completed generation while Applying.
	obj.Status.Phase = servingv1alpha1.PhaseApplying
	obj.Status.ReleaseUID = string(obj.UID)
	if err := r.updateStatus(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	if install && existing != nil {
		if err := r.Helm.Uninstall(ctx, obj.Namespace, obj.Name); err != nil {
			return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonApplyFailed, err, true)
		}
		existing = nil
	}
	ref := helm.ChartRef{Repo: obj.Spec.Chart.Repo, Name: obj.Spec.Chart.Name, Version: ver}
	opts := helm.ApplyOpts{
		ForceConflicts: obj.Annotations[servingv1alpha1.AnnotationForceConflicts] == strconv.FormatInt(obj.Generation, 10),
		Credentials:    creds,
		Labels:         releaseLabels(obj),
	}
	var (
		rel    *helm.Release
		reason string
	)
	if existing == nil {
		rel, err = r.Helm.Install(ctx, obj.Namespace, obj.Name, ref, values, opts)
		reason = servingv1alpha1.ReasonInstalled
	} else {
		rel, err = r.Helm.Upgrade(ctx, obj.Namespace, obj.Name, ref, values, opts)
		reason = servingv1alpha1.ReasonUpgraded
	}
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonApplyFailed, err, true)
	}
	if err := r.recordSuccess(ctx, obj, rel, obj.Spec.Chart.Name, ver, hash, reason, opts.ForceConflicts); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *LLMServiceReconciler) finishApplied(ctx context.Context, obj *servingv1alpha1.LLMService) (ctrl.Result, error) {
	cond := meta.FindStatusCondition(obj.Status.Conditions, servingv1alpha1.ConditionApplied)
	// A re-resolve that found nothing newer is honored too, or every later
	// reconcile would re-resolve and follow the repository.
	reResolve := obj.Annotations[servingv1alpha1.AnnotationReResolve]
	changed := obj.Status.Phase != servingv1alpha1.PhaseApplied ||
		obj.Status.ObservedGeneration != obj.Generation ||
		obj.Status.Message != "" ||
		obj.Status.Chart.ReResolve != reResolve ||
		cond == nil || cond.Status != metav1.ConditionTrue
	if changed {
		if cond == nil || cond.Status != metav1.ConditionTrue {
			meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
				Type:               servingv1alpha1.ConditionApplied,
				Status:             metav1.ConditionTrue,
				Reason:             servingv1alpha1.ReasonInstalled,
				ObservedGeneration: obj.Generation,
			})
		}
		obj.Status.Phase = servingv1alpha1.PhaseApplied
		obj.Status.Message = ""
		obj.Status.ObservedGeneration = obj.Generation
		obj.Status.Chart.ReResolve = reResolve
		if err := r.updateStatus(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.gcRevisions(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

type revisionSnapshot struct {
	Spec        servingv1alpha1.LLMServiceSpec `json:"spec"`
	Annotations map[string]string              `json:"annotations,omitempty"`
}

func (r *LLMServiceReconciler) recordSuccess(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release, chartName, chartVersion, hash, reason string, force bool) error {
	snap, err := json.Marshal(revisionSnapshot{Spec: obj.Spec, Annotations: swissAnnotations(obj.Annotations)})
	if err != nil {
		return err
	}
	crName := controllerRevisionName(obj.Name, hash, rel.Revision)
	if err := r.writeRevision(ctx, obj, crName, snap, rel.Revision); err != nil {
		return err
	}
	ann := swissAnnotations(obj.Annotations)
	entry := servingv1alpha1.HistoryEntry{
		Revision:           rel.Revision,
		Hash:               hash,
		AppliedAt:          metav1.Now(),
		ControllerRevision: crName,
		Annotations:        ann,
		ForceConflicts:     force,
		ReResolved:         reResolving(obj),
		Action:             obj.Annotations[actionAnnotation],
	}
	obj.Status.History = append([]servingv1alpha1.HistoryEntry{entry}, obj.Status.History...)
	if limit := r.historyLimit(); len(obj.Status.History) > limit {
		obj.Status.History = obj.Status.History[:limit]
	}
	obj.Status.AppliedHash = hash
	obj.Status.ReleaseUID = string(obj.UID)
	obj.Status.Chart = servingv1alpha1.ChartStatus{Name: chartName, Version: chartVersion, ReResolve: obj.Annotations[servingv1alpha1.AnnotationReResolve]}
	obj.Status.Helm = servingv1alpha1.HelmStatus{Revision: rel.Revision, Status: rel.Status}
	obj.Status.Phase = servingv1alpha1.PhaseApplied
	obj.Status.Message = ""
	obj.Status.ObservedGeneration = obj.Generation
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionApplied,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		ObservedGeneration: obj.Generation,
	})
	if err := r.updateStatus(ctx, obj); err != nil {
		return err
	}
	r.event(obj, corev1.EventTypeNormal, reason, reason+" helm release")
	return r.gcRevisions(ctx, obj)
}

// writeRevision records one apply's snapshot. A ControllerRevision's data is
// immutable, so each apply gets its own, named by helm revision, and none is
// ever updated. The name comes round again only when the same apply is
// recorded twice -- an adoption retried after its status update failed -- or
// when an uninstall restarted helm's numbering; the second finds a snapshot
// of this object's for a release that is gone, and replaces it.
func (r *LLMServiceReconciler) writeRevision(ctx context.Context, obj *servingv1alpha1.LLMService, name string, data []byte, rev int) error {
	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: obj.Namespace, Labels: map[string]string{llmserviceLabel: obj.Name}},
		Data:       runtime.RawExtension{Raw: append([]byte(nil), data...)},
		Revision:   int64(rev),
	}
	if err := controllerutil.SetControllerReference(obj, cr, r.Scheme); err != nil {
		return err
	}
	err := r.Create(ctx, cr)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	var have appsv1.ControllerRevision
	if err := r.Get(ctx, client.ObjectKeyFromObject(cr), &have); err != nil {
		return err
	}
	if same, err := jsonEqual(json.RawMessage(have.Data.Raw), json.RawMessage(data)); err != nil || same {
		return err
	}
	if !metav1.IsControlledBy(&have, obj) {
		return fmt.Errorf("ControllerRevision %s exists and is not this LLMService's", name)
	}
	if err := r.Delete(ctx, &have); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, cr)
}

func (r *LLMServiceReconciler) gcRevisions(ctx context.Context, obj *servingv1alpha1.LLMService) error {
	var list appsv1.ControllerRevisionList
	if err := r.List(ctx, &list, client.InNamespace(obj.Namespace), client.MatchingLabels{llmserviceLabel: obj.Name}); err != nil {
		return err
	}
	keep := map[string]struct{}{}
	for _, h := range obj.Status.History {
		keep[h.ControllerRevision] = struct{}{}
	}
	for i := range list.Items {
		item := &list.Items[i]
		if _, ok := keep[item.Name]; ok {
			continue
		}
		if err := r.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *LLMServiceReconciler) fail(ctx context.Context, obj *servingv1alpha1.LLMService, condType, reason string, cause error, requeue bool) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(cause, "Failed to reconcile LLMService", "reason", reason)
	obj.Status.Phase = servingv1alpha1.PhaseFailed
	obj.Status.Message = cause.Error()
	obj.Status.ObservedGeneration = obj.Generation
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: obj.Generation,
	})
	if err := r.updateStatus(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	if reason == servingv1alpha1.ReasonApplyFailed || reason == servingv1alpha1.ReasonDrift {
		r.event(obj, corev1.EventTypeWarning, reason, cause.Error())
	}
	if requeue {
		return ctrl.Result{}, cause
	}
	return ctrl.Result{}, nil
}

func (r *LLMServiceReconciler) updateStatus(ctx context.Context, obj *servingv1alpha1.LLMService) error {
	status := obj.Status
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest servingv1alpha1.LLMService
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), &latest); err != nil {
			return err
		}
		latest.Status = status
		return r.Status().Update(ctx, &latest)
	})
}

func (r *LLMServiceReconciler) credentials(ctx context.Context, obj *servingv1alpha1.LLMService) (*helm.Credentials, error) {
	ref := obj.Spec.Chart.CredentialsRef
	if ref == nil || ref.Name == "" {
		return nil, nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: obj.Namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("credentials secret %s: %w", ref.Name, err)
	}
	user, hasUser := secret.Data[credentialUsernameKey]
	pass, hasPass := secret.Data[credentialPasswordKey]
	if !hasUser || !hasPass {
		return nil, fmt.Errorf("credentials secret %s must contain username and password", ref.Name)
	}
	return &helm.Credentials{Username: string(user), Password: string(pass)}, nil
}

func (r *LLMServiceReconciler) listVersions(ctx context.Context, repo, name string, creds *helm.Credentials) ([]string, error) {
	if r.ListVersions != nil {
		return r.ListVersions(ctx, repo, name, creds)
	}
	var c chart.Credentials
	if creds != nil {
		c = chart.Credentials{Username: creds.Username, Password: creds.Password}
	}
	return chart.VersionsAuth(ctx, repo, "", name, c)
}

func (r *LLMServiceReconciler) historyLimit() int {
	if r.HistoryLimit <= 0 {
		return servingv1alpha1.DefaultHistoryLimit
	}
	return r.HistoryLimit
}

func (r *LLMServiceReconciler) pendingTimeout() time.Duration {
	if r.PendingTimeout <= 0 {
		return DefaultPendingTimeout
	}
	return r.PendingTimeout
}

// shouldInstall reports whether apply must install. A missing release is
// installed. An owned failed release is installed again when no revision is
// deployed, and upgraded when one is.
func (r *LLMServiceReconciler) shouldInstall(ctx context.Context, obj *servingv1alpha1.LLMService, existing *helm.Release) (bool, error) {
	if existing == nil {
		return true, nil
	}
	if existing.Status != statusFailed || !ownedBy(obj, existing) {
		return false, nil
	}
	rev, err := r.deployedRevision(ctx, obj)
	if err != nil {
		return false, err
	}
	return rev == 0, nil
}

func (r *LLMServiceReconciler) deployedRevision(ctx context.Context, obj *servingv1alpha1.LLMService) (int, error) {
	hist, err := r.Helm.History(ctx, obj.Namespace, obj.Name)
	if err != nil {
		return 0, err
	}
	rev := 0
	for _, rel := range hist {
		if rel.Status == statusDeployed && rel.Revision > rev {
			rev = rel.Revision
		}
	}
	return rev, nil
}

func (r *LLMServiceReconciler) event(obj client.Object, eventType, reason, msg string) {
	if r.Recorder == nil {
		return
	}
	// The note is a %s argument so a helm error containing % is not treated as a format.
	r.Recorder.Eventf(obj, nil, eventType, reason, reason, "%s", msg)
}

func (r *LLMServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1alpha1.LLMService{}, builder.WithPredicates(llmServicePredicate{})).
		Named("serving-llmservice").
		Complete(r)
}

// ownedBy reports whether rel belongs to obj. The release label is durable
// across status changes. status.releaseUID covers the window before Install
// stores that label, and a status that survived a lost release secret.
func ownedBy(obj *servingv1alpha1.LLMService, rel *helm.Release) bool {
	uid := string(obj.UID)
	if uid == "" {
		return false
	}
	if rel != nil && rel.Labels[llmserviceUIDLabel] == uid {
		return true
	}
	return obj.Status.ReleaseUID != "" && obj.Status.ReleaseUID == uid
}

// chartRepoAllowed reports whether repo is an allowed entry or a path under
// one: oci://ghcr.io/modelsphere allows oci://ghcr.io/modelsphere/charts, not
// oci://ghcr.io/modelsphere-x. Trailing slashes do not count. Empty allows any.
func chartRepoAllowed(allowed []string, repo string) bool {
	if len(allowed) == 0 {
		return true
	}
	repo = strings.TrimRight(repo, "/")
	for _, a := range allowed {
		a = strings.TrimRight(a, "/")
		if a != "" && (repo == a || strings.HasPrefix(repo, a+"/")) {
			return true
		}
	}
	return false
}

// installedOrAdopted reports whether deleting obj may uninstall rel. Install
// and Upgrade stamp obj's UID on the release; adoption does not relabel it and
// is recorded in status.history instead. status.releaseUID is not enough on
// its own: it is claimed before a first install that may never have stored a
// release, and the one found there since may be anybody's.
func installedOrAdopted(obj *servingv1alpha1.LLMService, rel *helm.Release) bool {
	if uid, ok := rel.Labels[llmserviceUIDLabel]; ok {
		return uid != "" && uid == string(obj.UID)
	}
	return len(obj.Status.History) > 0
}

// reResolving reports whether serving.modelsphere.dev/re-resolve holds a value
// not yet honored. Until a success records it, a retry re-resolves again.
func reResolving(obj *servingv1alpha1.LLMService) bool {
	v := obj.Annotations[servingv1alpha1.AnnotationReResolve]
	return v != "" && v != obj.Status.Chart.ReResolve
}

func releaseLabels(obj *servingv1alpha1.LLMService) map[string]string {
	return map[string]string{
		llmserviceLabel:    obj.Name,
		llmserviceUIDLabel: string(obj.UID),
	}
}

func pendingStatus(status string) bool {
	switch status {
	case statusPendingInstall, statusPendingUpgrade, statusPendingRollback:
		return true
	default:
		return false
	}
}

func swissAnnotations(in map[string]string) map[string]string {
	var out map[string]string
	for k, v := range in {
		if !strings.HasPrefix(k, servingv1alpha1.SwissAnnotationPrefix) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// controllerRevisionName is <svc>-<hash>-<helm revision>: one per apply, so a
// rollback to earlier values records a snapshot of its own.
func controllerRevisionName(svc, hash string, rev int) string {
	short := strings.TrimPrefix(hash, "sha256:")
	if len(short) > 8 {
		short = short[:8]
	}
	suffix := fmt.Sprintf("-%s-%d", short, rev)
	base := svc
	const maxLen = 253
	if len(base)+len(suffix) > maxLen {
		base = base[:maxLen-len(suffix)]
		base = strings.TrimRight(base, "-.")
	}
	return base + suffix
}

func driftMessage(name, version string, rel *helm.Release, valuesDiffer bool) string {
	var b strings.Builder
	b.WriteString("release drifted: ")
	wrote := false
	if rel.ChartName != name || rel.ChartVersion != version {
		fmt.Fprintf(&b, "chart is %s %s, release has %s %s", name, version, rel.ChartName, rel.ChartVersion)
		wrote = true
	}
	if valuesDiffer {
		if wrote {
			b.WriteString("; ")
		}
		b.WriteString("values differ")
	}
	return b.String()
}
