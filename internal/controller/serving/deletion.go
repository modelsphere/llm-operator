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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/helm"
)

// stageInput is the observation nextStage turns into one forward move.
type stageInput struct {
	Stage         servingv1alpha1.DeletionStage
	HasRoute      bool
	RouteGone     bool
	SettleDone    bool
	Uninstalled   bool
	PodsRemaining int
	PastDeadline  bool
	ForceDue      bool
}

// nextStage is the monotonic teardown transition. It never moves backward.
func nextStage(in stageInput) servingv1alpha1.DeletionStage {
	want := in.Stage
	switch in.Stage {
	case "":
		want = servingv1alpha1.DeletionStageUninstalling
		if in.HasRoute {
			want = servingv1alpha1.DeletionStageRouteRemoving
		}
	case servingv1alpha1.DeletionStageRouteRemoving:
		want = routeRemovingNext(in)
	case servingv1alpha1.DeletionStageRouteSettling:
		want = servingv1alpha1.DeletionStageRouteSettling
		if in.SettleDone || in.PastDeadline {
			want = servingv1alpha1.DeletionStageUninstalling
		}
	case servingv1alpha1.DeletionStageUninstalling:
		want = servingv1alpha1.DeletionStageUninstalling
		if in.Uninstalled {
			want = servingv1alpha1.DeletionStagePodsTerminating
		}
	case servingv1alpha1.DeletionStagePodsTerminating, servingv1alpha1.DeletionStageForceDeleting:
		want = podsNext(in)
	case servingv1alpha1.DeletionStageReleased:
		want = servingv1alpha1.DeletionStageReleased
	}
	if stageRank(want) < stageRank(in.Stage) {
		return in.Stage
	}
	return want
}

func routeRemovingNext(in stageInput) servingv1alpha1.DeletionStage {
	if !in.RouteGone {
		if in.PastDeadline {
			return servingv1alpha1.DeletionStageUninstalling
		}
		return servingv1alpha1.DeletionStageRouteRemoving
	}
	if !in.SettleDone {
		return servingv1alpha1.DeletionStageRouteSettling
	}
	return servingv1alpha1.DeletionStageUninstalling
}

func podsNext(in stageInput) servingv1alpha1.DeletionStage {
	if in.PodsRemaining == 0 || in.PastDeadline {
		return servingv1alpha1.DeletionStageReleased
	}
	if in.ForceDue {
		return servingv1alpha1.DeletionStageForceDeleting
	}
	return in.Stage
}

func stageRank(stage servingv1alpha1.DeletionStage) int {
	switch stage {
	case servingv1alpha1.DeletionStageRouteRemoving:
		return 1
	case servingv1alpha1.DeletionStageRouteSettling:
		return 2
	case servingv1alpha1.DeletionStageUninstalling:
		return 3
	case servingv1alpha1.DeletionStagePodsTerminating:
		return 4
	case servingv1alpha1.DeletionStageForceDeleting:
		return 5
	case servingv1alpha1.DeletionStageReleased:
		return 6
	default:
		return 0
	}
}

func (r *LLMServiceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *LLMServiceReconciler) reconcileStagedDelete(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release) (ctrl.Result, error) {
	now := r.now()
	knobs := deletionKnobsFrom(obj)
	r.warnInvalidKnobs(obj, knobs)
	del := cloneDeletion(obj)
	stampDeletionClock(del, now, knobs.maxWait)
	past := !now.Before(del.Deadline.Time)

	if del.Stage == servingv1alpha1.DeletionStageReleased {
		return r.finishReleased(ctx, obj, del)
	}
	if del.Stage == "" || del.Stage == servingv1alpha1.DeletionStageRouteRemoving || del.Stage == servingv1alpha1.DeletionStageRouteSettling {
		if rel == nil {
			// The release secret is already gone, so its manifest cannot name a route.
			del.Stage = servingv1alpha1.DeletionStageUninstalling
			if del.UninstalledAt == nil {
				del.UninstalledAt = metav1Now(now)
			}
		} else {
			res, cont, err := r.advanceRoutes(ctx, obj, rel, del, knobs, now, past)
			if err != nil || !cont {
				return res, err
			}
		}
	}
	if del.Stage == servingv1alpha1.DeletionStageUninstalling {
		if err := r.advanceUninstall(ctx, obj, del, now); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.advancePods(ctx, obj, del, knobs, now, past)
}

// deletionInProgress reports whether a previous pass already recorded a stage.
// The helm release is gone after uninstall, and that must not skip the pod wait.
func deletionInProgress(obj *servingv1alpha1.LLMService) bool {
	return obj.Status.Deletion != nil && obj.Status.Deletion.Stage != ""
}

func cloneDeletion(obj *servingv1alpha1.LLMService) *servingv1alpha1.DeletionStatus {
	if obj.Status.Deletion == nil {
		return &servingv1alpha1.DeletionStatus{}
	}
	return obj.Status.Deletion.DeepCopy()
}

func stampDeletionClock(del *servingv1alpha1.DeletionStatus, now time.Time, maxWait time.Duration) {
	if del.StartedAt == nil {
		del.StartedAt = metav1Now(now)
	}
	deadline := del.StartedAt.Add(maxWait)
	del.Deadline = metav1Now(deadline)
}

func (r *LLMServiceReconciler) advanceRoutes(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release, del *servingv1alpha1.DeletionStatus, knobs deletionKnobs, now time.Time, past bool) (ctrl.Result, bool, error) {
	if del.Stage == "" {
		routes, err := modelRoutesFromManifest(rel.Manifest, obj.Namespace)
		if err != nil {
			return ctrl.Result{}, false, err
		}
		del.Stage = nextStage(stageInput{HasRoute: len(routes) > 0})
		if del.Stage == servingv1alpha1.DeletionStageUninstalling {
			setOutcome(del, servingv1alpha1.DeletionOutcomeNoRoute)
			return ctrl.Result{}, true, nil
		}
	}
	if del.Stage == servingv1alpha1.DeletionStageRouteRemoving {
		res, err := r.advanceRouteRemoving(ctx, obj, rel, del, knobs, now, past)
		if err != nil || del.Stage == servingv1alpha1.DeletionStageRouteRemoving {
			return res, false, err
		}
	}
	if del.Stage == servingv1alpha1.DeletionStageRouteSettling {
		res, err := r.advanceRouteSettling(ctx, obj, del, knobs, now, past)
		if err != nil || del.Stage == servingv1alpha1.DeletionStageRouteSettling {
			return res, false, err
		}
	}
	return ctrl.Result{}, true, nil
}

func (r *LLMServiceReconciler) advanceRouteRemoving(ctx context.Context, obj *servingv1alpha1.LLMService, rel *helm.Release, del *servingv1alpha1.DeletionStatus, knobs deletionKnobs, now time.Time, past bool) (ctrl.Result, error) {
	routes, err := modelRoutesFromManifest(rel.Manifest, obj.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	msg := routeRemovingMessage(routes)
	if err := r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonRouteRemoving, msg, corev1.EventTypeNormal); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteModelRoutes(ctx, routes); err != nil {
		return ctrl.Result{}, err
	}
	gone, err := r.modelRoutesGone(ctx, routes)
	if err != nil {
		return ctrl.Result{}, err
	}
	del.Stage = nextStage(stageInput{
		Stage:        servingv1alpha1.DeletionStageRouteRemoving,
		HasRoute:     true,
		RouteGone:    gone,
		SettleDone:   gone && settleDone(del, knobs, now, past),
		PastDeadline: past,
	})
	if !gone && !past {
		return ctrl.Result{RequeueAfter: deletionPoll}, nil
	}
	if gone && del.RouteRemovedAt == nil {
		del.RouteRemovedAt = metav1Now(now)
	}
	if !gone && past {
		setOutcome(del, servingv1alpha1.DeletionOutcomeRouteStuck)
	}
	return ctrl.Result{}, nil
}

func (r *LLMServiceReconciler) advanceRouteSettling(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus, knobs deletionKnobs, now time.Time, past bool) (ctrl.Result, error) {
	if del.RouteRemovedAt == nil {
		del.RouteRemovedAt = metav1Now(now)
	}
	if !settleDone(del, knobs, now, past) {
		if err := r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonRouteSettling, msgSettle, corev1.EventTypeNormal); err != nil {
			return ctrl.Result{}, err
		}
		ready := del.RouteRemovedAt.Add(knobs.routeSettle)
		return ctrl.Result{RequeueAfter: ready.Sub(now)}, nil
	}
	del.Stage = nextStage(stageInput{
		Stage:        servingv1alpha1.DeletionStageRouteSettling,
		SettleDone:   true,
		PastDeadline: past,
	})
	return ctrl.Result{}, nil
}

func settleDone(del *servingv1alpha1.DeletionStatus, knobs deletionKnobs, now time.Time, past bool) bool {
	if past || del.RouteRemovedAt == nil {
		return past
	}
	return !now.Before(del.RouteRemovedAt.Add(knobs.routeSettle))
}

func (r *LLMServiceReconciler) advanceUninstall(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus, now time.Time) error {
	if del.UninstalledAt != nil {
		del.Stage = nextStage(stageInput{Stage: servingv1alpha1.DeletionStageUninstalling, Uninstalled: true})
		return nil
	}
	if err := r.writePreUninstall(ctx, obj, del); err != nil {
		return err
	}
	if err := r.Helm.Uninstall(ctx, obj.Namespace, obj.Name); err != nil {
		return err
	}
	del.UninstalledAt = metav1Now(now)
	del.Stage = nextStage(stageInput{Stage: servingv1alpha1.DeletionStageUninstalling, Uninstalled: true})
	return r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonUninstalled, msgUninstalled, corev1.EventTypeNormal)
}

func (r *LLMServiceReconciler) writePreUninstall(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus) error {
	switch del.Outcome {
	case servingv1alpha1.DeletionOutcomeNoRoute:
		return r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonNoRoute, msgNoRoute, corev1.EventTypeNormal)
	case servingv1alpha1.DeletionOutcomeRouteStuck:
		return r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonRouteStuck, msgRouteStuck, corev1.EventTypeWarning)
	default:
		return r.putDeletion(ctx, obj, del, "", "", "")
	}
}

func (r *LLMServiceReconciler) advancePods(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus, knobs deletionKnobs, now time.Time, past bool) (ctrl.Result, error) {
	pods, nodes, nodesOK, err := r.observePods(ctx, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	forced, err := r.forceDuePods(ctx, obj, del, knobs, pods, now, past)
	if err != nil {
		return ctrl.Result{}, err
	}
	if forced {
		pods, nodes, nodesOK, err = r.observePods(ctx, obj)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	acct := accountPods(pods, nodes, nodesOK, now)
	applyPodStatus(del, acct)
	if acct.remaining == 0 {
		return r.releasePods(ctx, obj, del, now)
	}
	if past {
		return r.releasePastDeadline(ctx, obj, del)
	}
	return r.waitForPods(ctx, obj, del, acct)
}

func (r *LLMServiceReconciler) observePods(ctx context.Context, obj *servingv1alpha1.LLMService) ([]corev1.Pod, map[string]bool, bool, error) {
	pods, err := r.listReleasePods(ctx, obj)
	if err != nil {
		return nil, nil, false, err
	}
	nodes, ok := r.readyNodes(ctx)
	return pods, nodes, ok, nil
}

func (r *LLMServiceReconciler) forceDuePods(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus, knobs deletionKnobs, pods []corev1.Pod, now time.Time, past bool) (bool, error) {
	if past || knobs.forceAfter == nil {
		return false, nil
	}
	due := dueForcePods(pods, *knobs.forceAfter, now)
	if len(due) == 0 {
		return false, nil
	}
	del.Stage = servingv1alpha1.DeletionStageForceDeleting
	setOutcome(del, servingv1alpha1.DeletionOutcomePodsForceDeleted)
	acct := accountPods(pods, nil, false, now)
	applyPodStatus(del, acct)
	del.Message = podWaitMessage(acct.remaining, acct.pods)
	if err := r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonPodsForceDeleted, del.Message, corev1.EventTypeWarning); err != nil {
		return false, err
	}
	deleted := false
	for i := range due {
		ok, err := r.forceDeletePod(ctx, obj, &due[i])
		if err != nil {
			return deleted, err
		}
		if !ok {
			continue
		}
		deleted = true
		del.ForceDeleted = appendUnique(del.ForceDeleted, due[i].Name)
		if err := r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonPodsForceDeleted, del.Message, corev1.EventTypeWarning); err != nil {
			return true, err
		}
	}
	return true, nil
}

func applyPodStatus(del *servingv1alpha1.DeletionStatus, acct podAccount) {
	del.PodsRemaining = int32(acct.remaining)
	del.Pods = capPods(acct.pods)
}

func (r *LLMServiceReconciler) releasePods(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus, now time.Time) (ctrl.Result, error) {
	if del.PodsGoneAt == nil {
		del.PodsGoneAt = metav1Now(now)
	}
	if del.Outcome == "" {
		setOutcome(del, servingv1alpha1.DeletionOutcomeClean)
	}
	del.Stage = nextStage(stageInput{Stage: del.Stage, PodsRemaining: 0})
	del.Message = releaseMessage(del)
	return r.finishReleased(ctx, obj, del)
}

func (r *LLMServiceReconciler) releasePastDeadline(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus) (ctrl.Result, error) {
	del.Stage = servingv1alpha1.DeletionStageReleased
	if del.Outcome != servingv1alpha1.DeletionOutcomeRouteStuck {
		setOutcome(del, servingv1alpha1.DeletionOutcomeTimedOut)
		del.Message = msgTimedOut
		if err := r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonTimedOut, del.Message, corev1.EventTypeWarning); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		del.Message = msgRouteStuck
	}
	return r.finishReleased(ctx, obj, del)
}

func (r *LLMServiceReconciler) waitForPods(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus, acct podAccount) (ctrl.Result, error) {
	if del.Stage != servingv1alpha1.DeletionStageForceDeleting {
		del.Stage = nextStage(stageInput{Stage: servingv1alpha1.DeletionStagePodsTerminating, PodsRemaining: acct.remaining})
	}
	reason := servingv1alpha1.ReasonWaitingForPods
	eventType := corev1.EventTypeNormal
	if del.Stage == servingv1alpha1.DeletionStageForceDeleting {
		reason = servingv1alpha1.ReasonPodsForceDeleted
		eventType = corev1.EventTypeWarning
	}
	del.Message = podWaitMessage(acct.remaining, acct.pods)
	if err := r.putDeletion(ctx, obj, del, reason, del.Message, eventType); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: deletionPoll}, nil
}

func (r *LLMServiceReconciler) finishReleased(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus) (ctrl.Result, error) {
	if del.Message == "" {
		del.Message = releaseMessage(del)
	}
	del.Stage = servingv1alpha1.DeletionStageReleased
	if err := r.putDeletion(ctx, obj, del, servingv1alpha1.ReasonReleased, del.Message, corev1.EventTypeNormal); err != nil {
		return ctrl.Result{}, err
	}
	var latest servingv1alpha1.LLMService
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), &latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return r.dropFinalizer(ctx, &latest)
}

func releaseMessage(del *servingv1alpha1.DeletionStatus) string {
	switch del.Outcome {
	case servingv1alpha1.DeletionOutcomeTimedOut:
		return msgTimedOut
	case servingv1alpha1.DeletionOutcomeRouteStuck:
		return msgRouteStuck
	case servingv1alpha1.DeletionOutcomePodsForceDeleted:
		return msgForceGone
	case servingv1alpha1.DeletionOutcomeNoRoute:
		return msgNoRoute
	default:
		return msgPodsGone
	}
}

func setOutcome(del *servingv1alpha1.DeletionStatus, next servingv1alpha1.DeletionOutcome) {
	if next == servingv1alpha1.DeletionOutcomeTimedOut && del.Outcome == servingv1alpha1.DeletionOutcomeRouteStuck {
		return
	}
	if outcomeRank(next) < outcomeRank(del.Outcome) {
		return
	}
	del.Outcome = next
}

func outcomeRank(outcome servingv1alpha1.DeletionOutcome) int {
	switch outcome {
	case servingv1alpha1.DeletionOutcomeClean:
		return 1
	case servingv1alpha1.DeletionOutcomeNoRoute:
		return 2
	case servingv1alpha1.DeletionOutcomeRouteStuck:
		return 3
	case servingv1alpha1.DeletionOutcomePodsForceDeleted:
		return 4
	case servingv1alpha1.DeletionOutcomeTimedOut:
		return 5
	default:
		return 0
	}
}

// putDeletion writes phase Deleting and status.deletion before the action
// the reason names. An unchanged status is left alone so a status write does
// not retrigger the reconcile. reason empty keeps the current Deleting
// condition and does not emit an event.
func (r *LLMServiceReconciler) putDeletion(ctx context.Context, obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus, reason, message, eventType string) error {
	if message != "" {
		del.Message = message
	}
	condReason, condMessage := reason, message
	if reason == "" {
		condReason, condMessage = deletingCondition(obj)
		if condReason == "" {
			condReason = servingv1alpha1.ReasonUninstalled
		}
	}
	if deletionUnchanged(obj, del, condReason, condMessage) {
		return nil
	}
	prev, _ := deletingCondition(obj)
	if reason != "" && prev != reason {
		if eventType != "" {
			r.event(obj, eventType, reason, message)
		}
		logf.FromContext(ctx).Info("Updated LLMService deletion", "stage", string(del.Stage), "reason", reason)
	}
	obj.Status.Phase = servingv1alpha1.PhaseDeleting
	obj.Status.Deletion = del.DeepCopy()
	meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionDeleting,
		Status:             metav1.ConditionTrue,
		Reason:             condReason,
		Message:            condMessage,
		ObservedGeneration: obj.Generation,
	})
	return r.updateStatus(ctx, obj)
}

func deletionUnchanged(obj *servingv1alpha1.LLMService, del *servingv1alpha1.DeletionStatus, reason, message string) bool {
	if obj.Status.Phase != servingv1alpha1.PhaseDeleting {
		return false
	}
	gotReason, gotMessage := deletingCondition(obj)
	if gotReason != reason || gotMessage != message {
		return false
	}
	return equality.Semantic.DeepEqual(obj.Status.Deletion, del)
}

func deletingCondition(obj *servingv1alpha1.LLMService) (string, string) {
	cond := meta.FindStatusCondition(obj.Status.Conditions, servingv1alpha1.ConditionDeleting)
	if cond == nil {
		return "", ""
	}
	return cond.Reason, cond.Message
}
