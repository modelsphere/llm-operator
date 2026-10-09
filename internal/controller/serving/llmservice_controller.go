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
	credentialUsernameKey = "username"
	credentialPasswordKey = "password"
	actionAnnotation      = servingv1alpha1.SwissAnnotationPrefix + "action"
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
	ListVersions VersionLister
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
		if err := r.Helm.Uninstall(ctx, obj.Namespace, obj.Name); err != nil {
			return ctrl.Result{}, err
		}
		r.event(obj, corev1.EventTypeNormal, "Uninstalled", "Uninstalled helm release")
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
	creds, err := r.credentials(ctx, obj)
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonChartResolveFailed, err, true)
	}
	spec, err := chart.ParseSpec(obj.Spec.Chart.Version)
	if err != nil {
		return r.fail(ctx, obj, servingv1alpha1.ConditionApplied, servingv1alpha1.ReasonChartResolveFailed, err, true)
	}
	keep := ""
	if obj.Status.Chart.Name == obj.Spec.Chart.Name {
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
	if rel != nil && obj.Status.AppliedHash == "" && len(obj.Status.History) == 0 {
		return r.adopt(ctx, obj, ver, values, hash, rel)
	}
	return r.apply(ctx, obj, ver, values, hash, creds, rel)
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
	msg := driftMessage(obj.Spec.Chart.Name, ver, rel, !valuesOK)
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionApplied,
		Status:             metav1.ConditionFalse,
		Reason:             servingv1alpha1.ReasonDrift,
		Message:            msg,
		ObservedGeneration: obj.Generation,
	})
	// Drift does not requeue. The next spec change reconciles again.
	return r.fail(ctx, obj, servingv1alpha1.ConditionAdopted, servingv1alpha1.ReasonDrift, errors.New(msg), false)
}

func (r *LLMServiceReconciler) apply(ctx context.Context, obj *servingv1alpha1.LLMService, ver string, values map[string]any, hash string, creds *helm.Credentials, existing *helm.Release) (ctrl.Result, error) {
	obj.Status.Phase = servingv1alpha1.PhaseApplying
	if err := r.updateStatus(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	ref := helm.ChartRef{Repo: obj.Spec.Chart.Repo, Name: obj.Spec.Chart.Name, Version: ver}
	opts := helm.ApplyOpts{
		ForceConflicts: obj.Annotations[servingv1alpha1.AnnotationForceConflicts] == strconv.FormatInt(obj.Generation, 10),
		Credentials:    creds,
	}
	var (
		rel    *helm.Release
		err    error
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
	changed := obj.Status.Phase != servingv1alpha1.PhaseApplied ||
		obj.Status.ObservedGeneration != obj.Generation ||
		obj.Status.Message != "" ||
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
	crName := controllerRevisionName(obj.Name, hash)
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
		Action:             obj.Annotations[actionAnnotation],
	}
	obj.Status.History = append([]servingv1alpha1.HistoryEntry{entry}, obj.Status.History...)
	if limit := r.historyLimit(); len(obj.Status.History) > limit {
		obj.Status.History = obj.Status.History[:limit]
	}
	obj.Status.AppliedHash = hash
	obj.Status.Chart = servingv1alpha1.ChartStatus{Name: chartName, Version: chartVersion}
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

func (r *LLMServiceReconciler) writeRevision(ctx context.Context, obj *servingv1alpha1.LLMService, name string, data []byte, rev int) error {
	cr := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: obj.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cr, func() error {
		if cr.Labels == nil {
			cr.Labels = map[string]string{}
		}
		cr.Labels[llmserviceLabel] = obj.Name
		cr.Data = runtime.RawExtension{Raw: append([]byte(nil), data...)}
		cr.Revision = int64(rev)
		return controllerutil.SetControllerReference(obj, cr, r.Scheme)
	})
	return err
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

func controllerRevisionName(svc, hash string) string {
	short := strings.TrimPrefix(hash, "sha256:")
	if len(short) > 8 {
		short = short[:8]
	}
	suffix := "-" + short
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
