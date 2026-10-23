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
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
)

// podAccount is the release pods still in the API, ignoring Succeeded and Failed.
type podAccount struct {
	remaining int
	pods      []servingv1alpha1.DeletionPod
}

// accountPods applies the stuck rules. Within grace a pod always counts.
// Past grace, a pod on a NotReady or missing node is gone and stuck. Any
// other pod is gone after grace plus 5m. When nodesOK is false only the
// time rule applies. pods are sorted by name.
func accountPods(pods []corev1.Pod, nodes map[string]bool, nodesOK bool, now time.Time) podAccount {
	var rows []servingv1alpha1.DeletionPod
	remaining := 0
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		grace := podGrace(pod)
		stuck := podStuck(pod, grace, nodes, nodesOK, now)
		if !stuck {
			remaining++
		}
		row := servingv1alpha1.DeletionPod{
			Name:              pod.Name,
			Phase:             string(pod.Status.Phase),
			Node:              pod.Spec.NodeName,
			DeletionTimestamp: pod.DeletionTimestamp,
			Grace:             grace,
			Stuck:             stuck,
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b servingv1alpha1.DeletionPod) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return podAccount{remaining: remaining, pods: rows}
}

func podGrace(pod *corev1.Pod) int64 {
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		return *pod.Spec.TerminationGracePeriodSeconds
	}
	return defaultPodGraceSeconds
}

// podStuck reports whether pod no longer blocks the finalizer.
func podStuck(pod *corev1.Pod, grace int64, nodes map[string]bool, nodesOK bool, now time.Time) bool {
	if pod.DeletionTimestamp.IsZero() {
		return false
	}
	deadline := graceDeadline(pod, grace)
	if now.Before(deadline) {
		return false
	}
	if nodesOK && !podNodeReady(pod, nodes) {
		return true
	}
	return !now.Before(deadline.Add(stuckPastGrace))
}

// graceDeadline is when the pod's termination grace ends.
// The API sets deletionTimestamp to that instant and records the window in
// deletionGracePeriodSeconds. A timestamp without that field is the moment
// deletion was requested, and the grace comes from the pod spec.
func graceDeadline(pod *corev1.Pod, grace int64) time.Time {
	if pod.DeletionGracePeriodSeconds != nil {
		return pod.DeletionTimestamp.Time
	}
	return pod.DeletionTimestamp.Add(time.Duration(grace) * time.Second)
}

// terminationStarted is when the pod was marked for deletion.
func terminationStarted(pod *corev1.Pod) time.Time {
	if pod.DeletionGracePeriodSeconds != nil {
		return pod.DeletionTimestamp.Add(-time.Duration(*pod.DeletionGracePeriodSeconds) * time.Second)
	}
	return pod.DeletionTimestamp.Time
}

func podNodeReady(pod *corev1.Pod, nodes map[string]bool) bool {
	if pod.Spec.NodeName == "" {
		return false
	}
	ready, ok := nodes[pod.Spec.NodeName]
	return ok && ready
}

func podWaitMessage(remaining int, pods []servingv1alpha1.DeletionPod) string {
	noun := "pods"
	if remaining == 1 {
		noun = "pod"
	}
	msg := fmt.Sprintf("waiting for %d %s", remaining, noun)
	note := stuckNote(pods)
	if note == "" {
		return msg
	}
	return msg + " (" + note + ")"
}

func stuckNote(pods []servingv1alpha1.DeletionPod) string {
	byNode := map[string]int{}
	missing := 0
	for _, pod := range pods {
		if !pod.Stuck {
			continue
		}
		if pod.Node == "" {
			missing++
			continue
		}
		byNode[pod.Node]++
	}
	if len(byNode) == 0 && missing == 0 {
		return ""
	}
	var parts []string
	names := slices.Sorted(maps.Keys(byNode))
	switch len(names) {
	case 0:
	case 1:
		parts = append(parts, fmt.Sprintf("%d stuck on NotReady node %s", byNode[names[0]], names[0]))
	default:
		total := 0
		for _, name := range names {
			total += byNode[name]
		}
		parts = append(parts, fmt.Sprintf("%d stuck on NotReady nodes %s", total, strings.Join(names, ", ")))
	}
	if missing > 0 {
		parts = append(parts, fmt.Sprintf("%d stuck on a missing node", missing))
	}
	return strings.Join(parts, ", ")
}

func capPods(pods []servingv1alpha1.DeletionPod) []servingv1alpha1.DeletionPod {
	if len(pods) > deletionListCap {
		return pods[:deletionListCap]
	}
	return pods
}

func appendUnique(in []string, name string) []string {
	if slices.Contains(in, name) || len(in) >= deletionListCap {
		return in
	}
	return append(in, name)
}

func (r *LLMServiceReconciler) listReleasePods(ctx context.Context, obj *servingv1alpha1.LLMService) ([]corev1.Pod, error) {
	selectors := []client.MatchingLabels{
		{"app.kubernetes.io/instance": obj.Name},
	}
	if obj.Status.Chart.Name != "" {
		selectors = append(selectors, client.MatchingLabels{"app": obj.Name + "-" + obj.Status.Chart.Name})
	}
	seen := map[types.UID]struct{}{}
	var out []corev1.Pod
	for _, sel := range selectors {
		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(obj.Namespace), sel); err != nil {
			return nil, err
		}
		for _, pod := range list.Items {
			if _, ok := seen[pod.UID]; ok {
				continue
			}
			seen[pod.UID] = struct{}{}
			out = append(out, pod)
		}
	}
	return out, nil
}

// readyNodes maps a node name to whether its Ready condition is True.
// The bool is false when the node list cannot be read.
func (r *LLMServiceReconciler) readyNodes(ctx context.Context) (map[string]bool, bool) {
	var list corev1.NodeList
	if err := r.List(ctx, &list); err != nil {
		logf.FromContext(ctx).Error(err, "Could not list Nodes")
		return nil, false
	}
	out := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = nodeReady(&list.Items[i])
	}
	return out, true
}

func nodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// dueForcePods are non-terminal pods whose deletionTimestamp is at least
// after old. Pods that are not deleting are never due.
func dueForcePods(pods []corev1.Pod, after time.Duration, now time.Time) []corev1.Pod {
	var due []corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.DeletionTimestamp.IsZero() {
			continue
		}
		if now.Before(terminationStarted(pod).Add(after)) {
			continue
		}
		due = append(due, *pod)
	}
	slices.SortFunc(due, func(a, b corev1.Pod) int { return cmp.Compare(a.Name, b.Name) })
	return due
}

// forceDeletePod deletes pod with grace 0. deleted is false when it was already gone.
// A grace-0 delete of a pod that is already terminating only shortens the grace
// period. The second delete removes the API object once that grace is 0.
func (r *LLMServiceReconciler) forceDeletePod(ctx context.Context, obj *servingv1alpha1.LLMService, pod *corev1.Pod) (bool, error) {
	uid := pod.UID
	existed, err := r.deletePodNow(ctx, pod, uid)
	if err != nil || !existed {
		return false, err
	}
	if _, err := r.deletePodNow(ctx, pod, uid); err != nil {
		return false, err
	}
	r.event(obj, corev1.EventTypeWarning, servingv1alpha1.ReasonPodForceDeleted,
		fmt.Sprintf("Force-deleted pod %s", pod.Name))
	return true, nil
}

func (r *LLMServiceReconciler) deletePodNow(ctx context.Context, pod *corev1.Pod, uid types.UID) (bool, error) {
	err := r.Delete(ctx, pod, client.GracePeriodSeconds(0), client.Preconditions{UID: &uid})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// metav1Now keeps the monotonic instant so a wait computed in this pass is exact.
func metav1Now(now time.Time) *metav1.Time {
	t := metav1.Time{Time: now}
	return &t
}
