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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/helm"
	"github.com/modelsphere/llm-operator/internal/testutil/cases"
)

// serviceCase is one LLMService controller spec. HistoryLimit defaults to 10.
// PendingTimeout zero (omitted) leaves the controller default.
type serviceCase struct {
	Name           string          `json:"name,omitempty"`
	Description    string          `json:"description,omitempty"`
	HistoryLimit   int             `json:"historyLimit,omitempty"`
	PendingTimeout *cases.Duration `json:"pendingTimeout,omitempty"`
	Steps          []serviceStep   `json:"steps"`
}

type serviceStep struct {
	Action         string             `json:"action"`
	FreshNamespace bool               `json:"freshNamespace,omitempty"`
	Name           string             `json:"name,omitempty"`
	Version        string             `json:"version,omitempty"`
	Annotations    map[string]string  `json:"annotations,omitempty"`
	Finalizers     []string           `json:"finalizers,omitempty"`
	ChartVersion   *string            `json:"chartVersion,omitempty"`
	Suspend        *bool              `json:"suspend,omitempty"`
	Layers         []layerSpec        `json:"layers,omitempty"`
	Release        *releaseSpec       `json:"release,omitempty"`
	Entries        []releaseSpec      `json:"entries,omitempty"`
	Put            string             `json:"put,omitempty"`
	Versions       []string           `json:"versions,omitempty"`
	ListErr        *string            `json:"listErr,omitempty"`
	InstallErr     *string            `json:"installErr,omitempty"`
	UpgradeErr     *string            `json:"upgradeErr,omitempty"`
	GetErr         *string            `json:"getErr,omitempty"`
	KeepFailed     *bool              `json:"keepFailed,omitempty"`
	Repos          []string           `json:"repos,omitempty"`
	Status         *statusSpec        `json:"status,omitempty"`
	Capture        []string           `json:"capture,omitempty"`
	Expect         *expectSpec        `json:"expect,omitempty"`
	Deletion       *deletionSpecInput `json:"deletion,omitempty"`
	Pod            *podInput          `json:"pod,omitempty"`
	Node           *nodeInput         `json:"node,omitempty"`
	Route          *routeInput        `json:"route,omitempty"`
	Advance        *cases.Duration    `json:"advance,omitempty"`
}

type layerSpec struct {
	Name   string `json:"name"`
	Values string `json:"values,omitempty"`
}

type releaseSpec struct {
	Revision     int               `json:"revision,omitempty"`
	Status       string            `json:"status,omitempty"`
	ChartName    string            `json:"chartName,omitempty"`
	ChartVersion string            `json:"chartVersion,omitempty"`
	Config       map[string]any    `json:"config,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Owned        bool              `json:"owned,omitempty"`
	Slot         bool              `json:"slot,omitempty"`
	Updated      *cases.Duration   `json:"updated,omitempty"`
	Manifest     string            `json:"manifest,omitempty"`
}

type deletionSpecInput struct {
	MaxWait          *cases.Duration `json:"maxWait,omitempty"`
	RouteSettle      *cases.Duration `json:"routeSettle,omitempty"`
	ForceDeleteAfter *cases.Duration `json:"forceDeleteAfter,omitempty"`
}

type podInput struct {
	Name              string            `json:"name"`
	Phase             string            `json:"phase,omitempty"`
	Node              string            `json:"node,omitempty"`
	Grace             *int64            `json:"grace,omitempty"`
	DeletionTimestamp *cases.Duration   `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
}

type nodeInput struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready,omitempty"`
}

type routeInput struct {
	Name       string   `json:"name,omitempty"`
	Finalizers []string `json:"finalizers,omitempty"`
}

type statusSpec struct {
	Phase      string               `json:"phase,omitempty"`
	ChartName  string               `json:"chartName,omitempty"`
	Conditions []conditionSpec      `json:"conditions,omitempty"`
	Deletion   *deletionStatusInput `json:"deletion,omitempty"`
}

type deletionStatusInput struct {
	Stage          string          `json:"stage,omitempty"`
	Outcome        string          `json:"outcome,omitempty"`
	Message        string          `json:"message,omitempty"`
	StartedAt      *cases.Duration `json:"startedAt,omitempty"`
	RouteRemovedAt *cases.Duration `json:"routeRemovedAt,omitempty"`
	UninstalledAt  *cases.Duration `json:"uninstalledAt,omitempty"`
	PodsGoneAt     *cases.Duration `json:"podsGoneAt,omitempty"`
	PodsRemaining  *int32          `json:"podsRemaining,omitempty"`
}

type conditionSpec struct {
	Type               string `json:"type"`
	Status             string `json:"status,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	ObservedGeneration string `json:"observedGeneration,omitempty"`
	Absent             bool   `json:"absent,omitempty"`
}

type expectSpec struct {
	Mid                  *expectSpec        `json:"mid,omitempty"`
	Error                *bool              `json:"error,omitempty"`
	ErrorContains        string             `json:"errorContains,omitempty"`
	Installs             *int               `json:"installs,omitempty"`
	Upgrades             *int               `json:"upgrades,omitempty"`
	Gets                 *int               `json:"gets,omitempty"`
	Uninstalls           *int               `json:"uninstalls,omitempty"`
	Rollbacks            *int               `json:"rollbacks,omitempty"`
	LastRollback         *int               `json:"lastRollback,omitempty"`
	HelmTouched          *int               `json:"helmTouched,omitempty"`
	ListCalls            *int               `json:"listCalls,omitempty"`
	LastForce            *bool              `json:"lastForce,omitempty"`
	LastSkipSchema       *bool              `json:"lastSkipSchemaValidation,omitempty"`
	GetsUnchanged        *bool              `json:"getsUnchanged,omitempty"`
	UpgradesUnchanged    *bool              `json:"upgradesUnchanged,omitempty"`
	Phase                string             `json:"phase,omitempty"`
	ChartVersion         string             `json:"chartVersion,omitempty"`
	ReResolve            *string            `json:"reResolve,omitempty"`
	AppliedHash          *hashExpect        `json:"appliedHash,omitempty"`
	HelmRevision         *int               `json:"helmRevision,omitempty"`
	ObservedGeneration   string             `json:"observedGeneration,omitempty"`
	Generation           *int64             `json:"generation,omitempty"`
	History              *historyExpect     `json:"history,omitempty"`
	Conditions           []conditionSpec    `json:"conditions,omitempty"`
	ReleaseUID           string             `json:"releaseUID,omitempty"`
	Release              *releaseExpect     `json:"release,omitempty"`
	NotFound             *bool              `json:"notFound,omitempty"`
	Message              *messageExpect     `json:"message,omitempty"`
	DistinctRevisions    *int               `json:"distinctRevisions,omitempty"`
	Snapshots            []snapshotExpect   `json:"snapshots,omitempty"`
	Requeue              *requeueExpect     `json:"requeue,omitempty"`
	EventContains        string             `json:"eventContains,omitempty"`
	EventsContain        []string           `json:"eventsContain,omitempty"`
	Finalizer            string             `json:"finalizer,omitempty"`
	Deletion             *deletionExpect    `json:"deletion,omitempty"`
	Route                *routeExpect       `json:"route,omitempty"`
	ClusterPods          []clusterPodExpect `json:"clusterPods,omitempty"`
	PlantedRevisionUsed  *bool              `json:"plantedRevisionUsed,omitempty"`
	CapturedRevisionGone *bool              `json:"capturedRevisionGone,omitempty"`
	LatestRevisionExists *bool              `json:"latestRevisionExists,omitempty"`
}

type hashExpect struct {
	Prefix   string `json:"prefix,omitempty"`
	Empty    *bool  `json:"empty,omitempty"`
	Captured *bool  `json:"captured,omitempty"`
}

type historyExpect struct {
	Length  *int                 `json:"length,omitempty"`
	Entries []historyEntryExpect `json:"entries,omitempty"`
}

type historyEntryExpect struct {
	Index              int      `json:"index"`
	Action             string   `json:"action,omitempty"`
	Revision           *int     `json:"revision,omitempty"`
	AnnotationsPresent []string `json:"annotationsPresent,omitempty"`
	AnnotationsAbsent  []string `json:"annotationsAbsent,omitempty"`
	ReResolved         *bool    `json:"reResolved,omitempty"`
	ForceConflicts     *bool    `json:"forceConflicts,omitempty"`
	HashEqualsApplied  bool     `json:"hashEqualsApplied,omitempty"`
	HashEqualsIndex    *int     `json:"hashEqualsIndex,omitempty"`
}

type releaseExpect struct {
	Exists       *bool             `json:"exists,omitempty"`
	Status       string            `json:"status,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	LabelsAbsent []string          `json:"labelsAbsent,omitempty"`
}

type messageExpect struct {
	Contains    []string `json:"contains,omitempty"`
	Equals      string   `json:"equals,omitempty"`
	NotContains []string `json:"notContains,omitempty"`
}

type snapshotExpect struct {
	Index             int               `json:"index"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	AnnotationsAbsent []string          `json:"annotationsAbsent,omitempty"`
	ChartName         string            `json:"chartName,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	ControllerOwner   *bool             `json:"controllerOwner,omitempty"`
}

type deletionExpect struct {
	Stage          string              `json:"stage,omitempty"`
	Outcome        string              `json:"outcome,omitempty"`
	Message        *messageExpect      `json:"message,omitempty"`
	PodsRemaining  *int                `json:"podsRemaining,omitempty"`
	ForceDeleted   []string            `json:"forceDeleted,omitempty"`
	Pods           []deletionPodExpect `json:"pods,omitempty"`
	StartedAt      *bool               `json:"startedAt,omitempty"`
	Deadline       *bool               `json:"deadline,omitempty"`
	RouteRemovedAt *bool               `json:"routeRemovedAt,omitempty"`
	UninstalledAt  *bool               `json:"uninstalledAt,omitempty"`
	PodsGoneAt     *bool               `json:"podsGoneAt,omitempty"`
}

type deletionPodExpect struct {
	Name  string `json:"name"`
	Phase string `json:"phase,omitempty"`
	Node  string `json:"node,omitempty"`
	Grace *int64 `json:"grace,omitempty"`
	Stuck *bool  `json:"stuck,omitempty"`
}

type routeExpect struct {
	Name       string   `json:"name,omitempty"`
	NotFound   *bool    `json:"notFound,omitempty"`
	Deleting   *bool    `json:"deleting,omitempty"`
	Finalizers []string `json:"finalizers,omitempty"`
}

type clusterPodExpect struct {
	Name     string `json:"name"`
	NotFound *bool  `json:"notFound,omitempty"`
	Deleting *bool  `json:"deleting,omitempty"`
}

type requeueExpect struct {
	Exact   *cases.Duration `json:"exact,omitempty"`
	AtLeast *cases.Duration `json:"atLeast,omitempty"`
	Above   *cases.Duration `json:"above,omitempty"`
	AtMost  *cases.Duration `json:"atMost,omitempty"`
}

type helmSnap struct {
	gets     int
	upgrades int
}

type caseRun struct {
	tc                 serviceCase
	ns                 string
	name               string
	h                  *fakeHelm
	l                  *versionList
	r                  *LLMServiceReconciler
	events             chan string
	capturedRevision   string
	appliedHash        string
	observedGeneration int64
	planted            string
	nowOffset          time.Duration
}

func runServiceCase(tc serviceCase) {
	rec := events.NewFakeRecorder(64)
	h := &fakeHelm{}
	l := &versionList{versions: []string{ver086}}
	limit := tc.HistoryLimit
	if limit == 0 {
		limit = 10
	}
	r := newReconciler(h, l, limit)
	r.Recorder = rec
	if tc.PendingTimeout != nil {
		r.PendingTimeout = tc.PendingTimeout.Std()
	}
	run := &caseRun{
		tc:     tc,
		ns:     takeNS(),
		name:   "qwen",
		h:      h,
		l:      l,
		r:      r,
		events: rec.Events,
	}
	r.Now = run.clock
	Expect(tc.Steps).NotTo(BeEmpty(), tc.Name)
	for i, step := range tc.Steps {
		run.step(i, step)
	}
}

func (c *caseRun) step(i int, step serviceStep) {
	if step.FreshNamespace {
		c.ns = takeNS()
	}
	if step.Name != "" {
		c.name = step.Name
	}
	if step.Action == "" {
		Fail(fmt.Sprintf("%s: step %d has no action", c.tc.Name, i+1))
	}
	if stepRequiresExpect(step.Action) && step.Expect == nil {
		Fail(fmt.Sprintf("%s: step %d (%s) missing expect", c.tc.Name, i+1, step.Action))
	}
	switch step.Action {
	case "create":
		c.create(step)
	case "reconcile":
		c.reconcile(step.Expect)
	case "update":
		c.update(step)
	case "delete":
		c.deleteObj()
	case "putRelease":
		c.putRelease(step)
	case "setVersions":
		c.setVersions(step)
	case "setHelm":
		c.setHelm(step)
	case "setAllowed":
		// An omitted list is nil, and a nil allowlist permits every repository.
		if step.Repos == nil {
			Fail(fmt.Sprintf("%s: step %d setAllowed requires repos", c.tc.Name, i+1))
		}
		c.r.AllowedChartRepos = append([]string(nil), step.Repos...)
	case "deleteRelease":
		delete(c.h.releases, c.h.slot(c.ns, c.name))
	case "setHistory":
		c.setHistory(step)
	case "plantStale":
		c.plantStale()
	case "reconcileHeld":
		c.reconcileHeld(step.Expect)
	case "capture":
		c.capture(step)
	case "setStatus":
		c.setStatus(step)
	case "setStatusReleaseUID":
		c.setStatusReleaseUID()
	default:
		if !c.deletionStep(step) {
			Fail(fmt.Sprintf("%s: step %d unknown action %q", c.tc.Name, i+1, step.Action))
		}
	}
	if step.Expect != nil && step.Action != "reconcile" && step.Action != "reconcileHeld" {
		c.check(step.Expect, reconcile.Result{}, nil, helmSnap{}, false)
	}
}

// deletionStep runs the staged-deletion step actions. It reports false for
// an action it does not know.
func (c *caseRun) deletionStep(step serviceStep) bool {
	switch step.Action {
	case "addPod":
		c.addPod(step)
	case "removePod":
		c.removePod(step)
	case "setNode":
		c.setNode(step)
	case "advanceClock":
		c.advanceClock(step)
	case "addRoute":
		c.addRoute(step)
	case "clearRouteFinalizer":
		c.clearRouteFinalizer(step)
	case "annotate":
		c.annotate(step)
	case "freshReconciler":
		c.freshReconciler()
	default:
		return false
	}
	return true
}

func stepRequiresExpect(action string) bool {
	return action == "reconcile" || action == "reconcileHeld"
}

func (c *caseRun) create(step serviceStep) {
	Expect(step.Version).NotTo(BeEmpty())
	obj := newService(c.ns, c.name, step.Version)
	maps.Copy(obj.Annotations, step.Annotations)
	if step.Finalizers != nil {
		obj.Finalizers = append([]string(nil), step.Finalizers...)
	}
	applyDeletionSpec(obj, step.Deletion)
	Expect(k8sClient.Create(ctx, obj)).To(Succeed())
}

func (c *caseRun) update(step serviceStep) {
	obj := fetch(c.ns, c.name)
	if step.Annotations != nil {
		if obj.Annotations == nil {
			obj.Annotations = map[string]string{}
		}
		maps.Copy(obj.Annotations, step.Annotations)
	}
	if step.ChartVersion != nil {
		obj.Spec.Chart.Version = *step.ChartVersion
	}
	if step.Suspend != nil {
		obj.Spec.Suspend = *step.Suspend
	}
	if step.Layers != nil {
		obj.Spec.Layers = layersFrom(step.Layers)
	}
	applyDeletionSpec(obj, step.Deletion)
	Expect(k8sClient.Update(ctx, obj)).To(Succeed())
}

func (c *caseRun) annotate(step serviceStep) {
	Expect(step.Annotations).NotTo(BeEmpty())
	obj := fetch(c.ns, c.name)
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	maps.Copy(obj.Annotations, step.Annotations)
	Expect(k8sClient.Update(ctx, obj)).To(Succeed())
}

func applyDeletionSpec(obj *servingv1alpha1.LLMService, in *deletionSpecInput) {
	if in == nil {
		return
	}
	spec := &servingv1alpha1.DeletionSpec{}
	if obj.Spec.Deletion != nil {
		spec = obj.Spec.Deletion
	}
	if in.MaxWait != nil {
		spec.MaxWait = &metav1.Duration{Duration: in.MaxWait.Std()}
	}
	if in.RouteSettle != nil {
		spec.RouteSettle = &metav1.Duration{Duration: in.RouteSettle.Std()}
	}
	if in.ForceDeleteAfter != nil {
		d := metav1.Duration{Duration: in.ForceDeleteAfter.Std()}
		spec.ForceDeleteAfter = &d
	}
	obj.Spec.Deletion = spec
}

func (c *caseRun) clock() time.Time {
	return time.Now().Add(c.nowOffset)
}

func (c *caseRun) advanceClock(step serviceStep) {
	Expect(step.Advance).NotTo(BeNil())
	c.nowOffset += step.Advance.Std()
}

func (c *caseRun) freshReconciler() {
	rec := events.NewFakeRecorder(64)
	next := newReconciler(c.h, c.l, c.r.HistoryLimit)
	next.Recorder = rec
	next.PendingTimeout = c.r.PendingTimeout
	next.AllowedChartRepos = append([]string(nil), c.r.AllowedChartRepos...)
	next.Now = c.clock
	c.r = next
	c.events = rec.Events
}

func (c *caseRun) nodeName(short string) string {
	if short == "" {
		return ""
	}
	return c.ns + "-" + short
}

func (c *caseRun) addPod(step serviceStep) {
	Expect(step.Pod).NotTo(BeNil())
	Expect(step.Pod.Name).NotTo(BeEmpty())
	labels := step.Pod.Labels
	if labels == nil {
		labels = map[string]string{
			"app.kubernetes.io/instance": c.name,
			"app":                        c.name + "-sglang",
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: step.Pod.Name, Namespace: c.ns, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			NodeName:      c.nodeName(step.Pod.Node),
			Containers:    []corev1.Container{{Name: "c", Image: "pause"}},
		},
	}
	if step.Pod.Grace != nil {
		pod.Spec.TerminationGracePeriodSeconds = step.Pod.Grace
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	if step.Pod.Phase != "" {
		pod.Status.Phase = corev1.PodPhase(step.Pod.Phase)
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	if step.Pod.DeletionTimestamp != nil {
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
	}
}

func (c *caseRun) removePod(step serviceStep) {
	Expect(step.Pod).NotTo(BeNil())
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: step.Pod.Name, Namespace: c.ns}}
	Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
}

func (c *caseRun) setNode(step serviceStep) {
	Expect(step.Node).NotTo(BeNil())
	Expect(step.Node.Name).NotTo(BeEmpty())
	name := c.nodeName(step.Node.Name)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)
	if apierrors.IsNotFound(err) {
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
	} else {
		Expect(err).NotTo(HaveOccurred())
	}
	status := corev1.ConditionFalse
	if step.Node.Ready {
		status = corev1.ConditionTrue
	}
	node.Status.Conditions = []corev1.NodeCondition{{
		Type:   corev1.NodeReady,
		Status: status,
	}}
	Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
}

func (c *caseRun) addRoute(step serviceStep) {
	name := c.routeName(step)
	u := modelRouteObject(c.ns, name)
	if step.Route != nil {
		u.SetFinalizers(append([]string(nil), step.Route.Finalizers...))
	}
	u.Object["spec"] = map[string]any{"model": name}
	Expect(k8sClient.Create(ctx, u)).To(Succeed())
}

func (c *caseRun) clearRouteFinalizer(step serviceStep) {
	name := c.routeName(step)
	u := modelRouteObject(c.ns, name)
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: c.ns, Name: name}, u)).To(Succeed())
	u.SetFinalizers(nil)
	err := k8sClient.Update(ctx, u)
	if err != nil && !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

func (c *caseRun) routeName(step serviceStep) string {
	if step.Route != nil && step.Route.Name != "" {
		return step.Route.Name
	}
	return c.name
}

func modelRouteObject(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("routing.modelsphere.dev/v1alpha1")
	u.SetKind("ModelRoute")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func layersFrom(in []layerSpec) []servingv1alpha1.Layer {
	out := make([]servingv1alpha1.Layer, len(in))
	for i, l := range in {
		out[i] = servingv1alpha1.Layer{Name: l.Name}
		if l.Values != "" {
			out[i].Values = &apiextensionsv1.JSON{Raw: []byte(l.Values)}
		}
	}
	return out
}

func (c *caseRun) deleteObj() {
	Expect(k8sClient.Delete(ctx, fetch(c.ns, c.name))).To(Succeed())
}

func (c *caseRun) reconcile(exp *expectSpec) {
	before := helmSnap{gets: c.h.gets, upgrades: c.h.upgrades}
	result, err := doReconcile(c.r, c.ns, c.name)
	c.check(exp, result, err, before, true)
}

// reconcileHeld runs one reconcile that blocks inside helm, checks the
// mid-apply status, then lets the install finish.
func (c *caseRun) reconcileHeld(exp *expectSpec) {
	Expect(exp).NotTo(BeNil())
	Expect(exp.Mid).NotTo(BeNil())
	c.h.hold = make(chan struct{})
	c.h.entered = make(chan struct{}, 1)
	released := false
	release := func() {
		if !released {
			close(c.h.hold)
			released = true
		}
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		defer GinkgoRecover()
		_, recErr := doReconcile(c.r, c.ns, c.name)
		done <- recErr
	}()
	Eventually(c.h.entered).WithTimeout(5 * time.Second).Should(Receive())
	c.check(exp.Mid, reconcile.Result{}, nil, helmSnap{}, false)
	release()
	var recErr error
	Eventually(done).WithTimeout(5 * time.Second).Should(Receive(&recErr))
	Expect(recErr).NotTo(HaveOccurred())
	c.check(exp, reconcile.Result{}, recErr, helmSnap{}, true)
}

func (c *caseRun) putRelease(step serviceStep) {
	Expect(step.Release).NotTo(BeNil())
	rel := c.releaseFrom(*step.Release)
	c.h.put(c.ns, c.name, &rel)
}

func (c *caseRun) setHistory(step serviceStep) {
	Expect(step.Entries).NotTo(BeEmpty())
	hist := make([]helm.Release, len(step.Entries))
	for i, e := range step.Entries {
		hist[i] = c.releaseFrom(e)
	}
	c.h.histories = map[string][]helm.Release{c.h.slot(c.ns, c.name): hist}
	if step.Put == "" {
		return
	}
	Expect(step.Put).To(Equal("last"))
	last := hist[len(hist)-1]
	c.h.put(c.ns, c.name, &last)
}

func (c *caseRun) releaseFrom(in releaseSpec) helm.Release {
	if in.Slot {
		cur := c.h.releases[c.h.slot(c.ns, c.name)]
		Expect(cur).NotTo(BeNil())
		return *cur
	}
	if in.Owned {
		updated := time.Now()
		if in.Updated != nil {
			updated = time.Now().Add(in.Updated.Std())
		}
		rel := *ownedRel(fetch(c.ns, c.name), in.Revision, in.Status, updated)
		rel.Manifest = in.Manifest
		return rel
	}
	rel := helm.Release{
		Revision:     in.Revision,
		Status:       in.Status,
		ChartName:    in.ChartName,
		ChartVersion: in.ChartVersion,
		Config:       in.Config,
		Labels:       in.Labels,
		Manifest:     in.Manifest,
	}
	if in.Updated != nil {
		rel.Updated = time.Now().Add(in.Updated.Std())
	}
	return rel
}

func (c *caseRun) setVersions(step serviceStep) {
	if step.Versions != nil {
		c.l.versions = append([]string(nil), step.Versions...)
	}
	if step.ListErr != nil {
		c.l.err = errFrom(*step.ListErr)
	}
}

func (c *caseRun) setHelm(step serviceStep) {
	if step.InstallErr != nil {
		c.h.installErr = errFrom(*step.InstallErr)
	}
	if step.UpgradeErr != nil {
		c.h.upgradeErr = errFrom(*step.UpgradeErr)
	}
	if step.GetErr != nil {
		c.h.getErr = errFrom(*step.GetErr)
	}
	if step.KeepFailed != nil {
		c.h.keepFailed = *step.KeepFailed
	}
}

func errFrom(msg string) error {
	if msg == "" {
		return nil
	}
	return errors.New(msg)
}

func (c *caseRun) setStatus(step serviceStep) {
	Expect(step.Status).NotTo(BeNil())
	obj := fetch(c.ns, c.name)
	if step.Status.Phase != "" {
		obj.Status.Phase = servingv1alpha1.Phase(step.Status.Phase)
	}
	if step.Status.ChartName != "" {
		obj.Status.Chart.Name = step.Status.ChartName
	}
	if step.Status.Deletion != nil {
		obj.Status.Deletion = deletionStatusFrom(step.Status.Deletion)
	}
	for _, cond := range step.Status.Conditions {
		meta.SetStatusCondition(&obj.Status.Conditions, conditionFrom(obj, cond))
	}
	Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
}

func deletionStatusFrom(in *deletionStatusInput) *servingv1alpha1.DeletionStatus {
	out := &servingv1alpha1.DeletionStatus{
		Stage:   servingv1alpha1.DeletionStage(in.Stage),
		Outcome: servingv1alpha1.DeletionOutcome(in.Outcome),
		Message: in.Message,
	}
	out.StartedAt = offsetTime(in.StartedAt)
	out.RouteRemovedAt = offsetTime(in.RouteRemovedAt)
	out.UninstalledAt = offsetTime(in.UninstalledAt)
	out.PodsGoneAt = offsetTime(in.PodsGoneAt)
	if in.PodsRemaining != nil {
		out.PodsRemaining = *in.PodsRemaining
	}
	return out
}

func offsetTime(d *cases.Duration) *metav1.Time {
	if d == nil {
		return nil
	}
	t := metav1.NewTime(time.Now().Add(d.Std()))
	return &t
}

func conditionFrom(obj *servingv1alpha1.LLMService, cond conditionSpec) metav1.Condition {
	out := metav1.Condition{
		Type:    cond.Type,
		Status:  metav1.ConditionStatus(cond.Status),
		Reason:  cond.Reason,
		Message: cond.Message,
	}
	switch cond.ObservedGeneration {
	case "":
	case "current":
		out.ObservedGeneration = obj.Generation
	default:
		Fail("unsupported observedGeneration " + cond.ObservedGeneration)
	}
	return out
}

func (c *caseRun) setStatusReleaseUID() {
	obj := fetch(c.ns, c.name)
	obj.Status.ReleaseUID = string(obj.UID)
	Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
}

func (c *caseRun) capture(step serviceStep) {
	Expect(step.Capture).NotTo(BeEmpty())
	obj := fetch(c.ns, c.name)
	for _, what := range step.Capture {
		switch what {
		case "observedGeneration":
			c.observedGeneration = obj.Status.ObservedGeneration
		case "appliedHash":
			c.appliedHash = obj.Status.AppliedHash
		case "revision":
			Expect(obj.Status.History).NotTo(BeEmpty())
			c.capturedRevision = obj.Status.History[0].ControllerRevision
		default:
			Fail("unknown capture " + what)
		}
	}
}

// plantStale leaves a ControllerRevision under the name the next install
// reuses, owned by the service, so the reconcile replaces that snapshot.
func (c *caseRun) plantStale() {
	obj := fetch(c.ns, c.name)
	values, err := mergeLayers(obj.Spec.Layers)
	Expect(err).NotTo(HaveOccurred())
	hash, err := appliedHash(chartName, ver086, values)
	Expect(err).NotTo(HaveOccurred())
	stale := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerRevisionName(obj.Name, hash, 1),
			Namespace: c.ns,
		},
		Data:     runtime.RawExtension{Raw: []byte(`{"annotations":{"swiss.modelsphere.dev/action":"old"}}`)},
		Revision: 1,
	}
	Expect(controllerutil.SetControllerReference(obj, stale, k8sClient.Scheme())).To(Succeed())
	Expect(k8sClient.Create(ctx, stale)).To(Succeed())
	c.planted = stale.Name
}

func (c *caseRun) check(exp *expectSpec, result reconcile.Result, err error, before helmSnap, reconciled bool) {
	if exp == nil {
		return
	}
	c.checkErr(exp, err, reconciled)
	c.checkRequeue(exp.Requeue, result)
	c.checkHelm(exp, before)
	if exp.EventContains != "" {
		Expect(c.events).To(Receive(ContainSubstring(exp.EventContains)))
	}
	if len(exp.EventsContain) > 0 {
		c.checkEvents(exp.EventsContain)
	}
	if exp.Route != nil {
		c.checkRoute(exp.Route)
	}
	c.checkClusterPods(exp.ClusterPods)
	if exp.NotFound != nil && *exp.NotFound {
		c.checkMissing()
	}
	var got *servingv1alpha1.LLMService
	if exp.wantsObject() {
		got = fetch(c.ns, c.name)
		c.checkObject(exp, got)
	}
	if exp.Release != nil {
		c.checkRelease(exp.Release, got)
	}
}

func (c *caseRun) checkErr(exp *expectSpec, err error, reconciled bool) {
	if exp.ErrorContains != "" {
		Expect(err).To(MatchError(ContainSubstring(exp.ErrorContains)))
		return
	}
	if exp.Error != nil {
		if *exp.Error {
			Expect(err).To(HaveOccurred())
		} else {
			Expect(err).NotTo(HaveOccurred())
		}
		return
	}
	if reconciled {
		Expect(err).NotTo(HaveOccurred())
	}
}

func (c *caseRun) checkRequeue(exp *requeueExpect, result reconcile.Result) {
	if exp == nil {
		return
	}
	if exp.Exact != nil {
		Expect(result.RequeueAfter).To(Equal(exp.Exact.Std()))
	}
	if exp.AtLeast != nil {
		Expect(result.RequeueAfter).To(BeNumerically(">=", exp.AtLeast.Std()))
	}
	if exp.Above != nil {
		Expect(result.RequeueAfter).To(BeNumerically(">", exp.Above.Std()))
	}
	if exp.AtMost != nil {
		Expect(result.RequeueAfter).To(BeNumerically("<=", exp.AtMost.Std()))
	}
}

func (c *caseRun) checkHelm(exp *expectSpec, before helmSnap) {
	expectInt(c.h.installs, exp.Installs)
	expectInt(c.h.upgrades, exp.Upgrades)
	expectInt(c.h.gets, exp.Gets)
	expectInt(c.h.uninstalls, exp.Uninstalls)
	expectInt(c.h.rollbacks, exp.Rollbacks)
	expectInt(c.h.lastRollback, exp.LastRollback)
	expectInt(c.l.calls, exp.ListCalls)
	if exp.HelmTouched != nil {
		Expect(c.h.gets + c.h.installs + c.h.upgrades).To(Equal(*exp.HelmTouched))
	}
	expectBool(c.h.lastForce, exp.LastForce)
	expectBool(c.h.lastSkipSchema, exp.LastSkipSchema)
	if exp.GetsUnchanged != nil && *exp.GetsUnchanged {
		Expect(c.h.gets).To(Equal(before.gets))
	}
	if exp.UpgradesUnchanged != nil && *exp.UpgradesUnchanged {
		Expect(c.h.upgrades).To(Equal(before.upgrades))
	}
}

func expectInt(got int, want *int) {
	if want != nil {
		Expect(got).To(Equal(*want))
	}
}

func expectBool(got bool, want *bool) {
	if want == nil {
		return
	}
	if *want {
		Expect(got).To(BeTrue())
	} else {
		Expect(got).To(BeFalse())
	}
}

func (e *expectSpec) wantsObject() bool {
	if e.Phase != "" || e.ChartVersion != "" || e.ReResolve != nil || e.AppliedHash != nil {
		return true
	}
	if e.HelmRevision != nil || e.ObservedGeneration != "" || e.Generation != nil || e.History != nil {
		return true
	}
	if len(e.Conditions) > 0 || e.ReleaseUID != "" || e.Message != nil || e.Finalizer != "" || e.Deletion != nil {
		return true
	}
	if e.DistinctRevisions != nil || len(e.Snapshots) > 0 || e.PlantedRevisionUsed != nil {
		return true
	}
	return e.CapturedRevisionGone != nil || e.LatestRevisionExists != nil
}

func (c *caseRun) checkObject(exp *expectSpec, got *servingv1alpha1.LLMService) {
	if exp.Phase != "" {
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.Phase(exp.Phase)))
	}
	if exp.ChartVersion != "" {
		Expect(got.Status.Chart.Version).To(Equal(exp.ChartVersion))
	}
	if exp.ReResolve != nil {
		Expect(got.Status.Chart.ReResolve).To(Equal(*exp.ReResolve))
	}
	c.checkHash(exp.AppliedHash, got)
	if exp.HelmRevision != nil {
		Expect(got.Status.Helm.Revision).To(Equal(*exp.HelmRevision))
	}
	c.checkGeneration(exp, got)
	c.checkHistory(exp, got)
	c.checkConditions(exp.Conditions, got)
	c.checkReleaseUID(exp.ReleaseUID, got)
	c.checkMessage(exp.Message, got)
	c.checkDeletion(exp.Deletion, got)
	c.checkFinalizer(exp.Finalizer, got)
	c.checkRevisions(exp, got)
}

func (c *caseRun) checkHash(exp *hashExpect, got *servingv1alpha1.LLMService) {
	if exp == nil {
		return
	}
	if exp.Empty != nil && *exp.Empty {
		Expect(got.Status.AppliedHash).To(BeEmpty())
	}
	if exp.Prefix != "" {
		Expect(got.Status.AppliedHash).To(HavePrefix(exp.Prefix))
	}
	if exp.Captured != nil && *exp.Captured {
		Expect(got.Status.AppliedHash).To(Equal(c.appliedHash))
	}
}

func (c *caseRun) checkGeneration(exp *expectSpec, got *servingv1alpha1.LLMService) {
	if exp.Generation != nil {
		Expect(got.Generation).To(Equal(*exp.Generation))
	}
	switch exp.ObservedGeneration {
	case "":
	case "current":
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	case "captured":
		Expect(got.Status.ObservedGeneration).To(Equal(c.observedGeneration))
	case "nonzero":
		Expect(got.Status.ObservedGeneration).NotTo(BeZero())
	default:
		Fail("unsupported observedGeneration " + exp.ObservedGeneration)
	}
}

func (c *caseRun) checkHistory(exp *expectSpec, got *servingv1alpha1.LLMService) {
	if exp.History == nil {
		return
	}
	if exp.History.Length != nil {
		Expect(got.Status.History).To(HaveLen(*exp.History.Length))
	}
	for _, e := range exp.History.Entries {
		Expect(e.Index).To(BeNumerically(">=", 0))
		Expect(e.Index).To(BeNumerically("<", len(got.Status.History)))
		entry := got.Status.History[e.Index]
		if e.Action != "" {
			Expect(entry.Action).To(Equal(e.Action))
		}
		if e.Revision != nil {
			Expect(entry.Revision).To(Equal(*e.Revision))
		}
		for _, k := range e.AnnotationsPresent {
			Expect(entry.Annotations).To(HaveKey(k))
		}
		for _, k := range e.AnnotationsAbsent {
			Expect(entry.Annotations).NotTo(HaveKey(k))
		}
		expectBool(entry.ReResolved, e.ReResolved)
		expectBool(entry.ForceConflicts, e.ForceConflicts)
		if e.HashEqualsApplied {
			Expect(entry.Hash).To(Equal(got.Status.AppliedHash))
		}
		if e.HashEqualsIndex != nil {
			Expect(entry.Hash).To(Equal(got.Status.History[*e.HashEqualsIndex].Hash))
		}
	}
	if exp.DistinctRevisions != nil {
		names := map[string]struct{}{}
		for _, entry := range got.Status.History {
			names[entry.ControllerRevision] = struct{}{}
		}
		Expect(names).To(HaveLen(*exp.DistinctRevisions))
	}
}

func (c *caseRun) checkConditions(conds []conditionSpec, got *servingv1alpha1.LLMService) {
	for _, want := range conds {
		gotCond := condition(got, want.Type)
		if want.Absent {
			Expect(gotCond).To(BeNil())
			continue
		}
		Expect(gotCond).NotTo(BeNil())
		if want.Status != "" {
			Expect(gotCond.Status).To(Equal(metav1.ConditionStatus(want.Status)))
		}
		if want.Reason != "" {
			Expect(gotCond.Reason).To(Equal(want.Reason))
		}
		if want.Message != "" {
			Expect(gotCond.Message).To(Equal(want.Message))
		}
		switch want.ObservedGeneration {
		case "":
		case "current":
			Expect(gotCond.ObservedGeneration).To(Equal(got.Generation))
		default:
			Fail("unsupported observedGeneration " + want.ObservedGeneration)
		}
	}
}

func (c *caseRun) checkReleaseUID(want string, got *servingv1alpha1.LLMService) {
	switch want {
	case "":
	case "self":
		Expect(got.Status.ReleaseUID).To(Equal(string(got.UID)))
	case "empty":
		Expect(got.Status.ReleaseUID).To(BeEmpty())
	default:
		Fail("unsupported releaseUID " + want)
	}
}

func (c *caseRun) checkMessage(exp *messageExpect, got *servingv1alpha1.LLMService) {
	checkText(exp, got.Status.Message)
}

func checkText(exp *messageExpect, got string) {
	if exp == nil {
		return
	}
	if exp.Equals != "" {
		Expect(got).To(Equal(exp.Equals))
	}
	// An empty substring matches every message, so it is rejected on its own.
	for _, sub := range exp.Contains {
		if sub == "" {
			Fail("message.contains entry is empty")
		}
		Expect(got).To(ContainSubstring(sub))
	}
	for _, sub := range exp.NotContains {
		if sub == "" {
			Fail("message.notContains entry is empty")
		}
		Expect(got).NotTo(ContainSubstring(sub))
	}
}

func (c *caseRun) checkDeletion(exp *deletionExpect, got *servingv1alpha1.LLMService) {
	if exp == nil {
		return
	}
	Expect(got.Status.Deletion).NotTo(BeNil())
	del := got.Status.Deletion
	if exp.Stage != "" {
		Expect(string(del.Stage)).To(Equal(exp.Stage))
	}
	if exp.Outcome != "" {
		Expect(string(del.Outcome)).To(Equal(exp.Outcome))
	}
	checkText(exp.Message, del.Message)
	if exp.PodsRemaining != nil {
		Expect(int(del.PodsRemaining)).To(Equal(*exp.PodsRemaining))
	}
	if exp.ForceDeleted != nil {
		Expect(del.ForceDeleted).To(Equal(exp.ForceDeleted))
	}
	expectTimeSet(exp.StartedAt, del.StartedAt, "startedAt")
	expectTimeSet(exp.Deadline, del.Deadline, "deadline")
	expectTimeSet(exp.RouteRemovedAt, del.RouteRemovedAt, "routeRemovedAt")
	expectTimeSet(exp.UninstalledAt, del.UninstalledAt, "uninstalledAt")
	expectTimeSet(exp.PodsGoneAt, del.PodsGoneAt, "podsGoneAt")
	if exp.Pods == nil {
		return
	}
	Expect(del.Pods).To(HaveLen(len(exp.Pods)))
	for i, want := range exp.Pods {
		gotPod := del.Pods[i]
		Expect(gotPod.Name).To(Equal(want.Name))
		if want.Phase != "" {
			Expect(gotPod.Phase).To(Equal(want.Phase))
		}
		if want.Node != "" {
			Expect(gotPod.Node).To(Equal(c.nodeName(want.Node)))
		}
		if want.Grace != nil {
			Expect(gotPod.Grace).To(Equal(*want.Grace))
		}
		if want.Stuck != nil {
			Expect(gotPod.Stuck).To(Equal(*want.Stuck))
		}
	}
}

func expectTimeSet(want *bool, got *metav1.Time, field string) {
	if want == nil {
		return
	}
	if *want {
		Expect(got).NotTo(BeNil(), field)
		return
	}
	Expect(got).To(BeNil(), field)
}

func (c *caseRun) checkEvents(want []string) {
	got := c.drainEvents()
	for _, sub := range want {
		Expect(got).To(ContainElement(ContainSubstring(sub)), "events %v", got)
	}
}

func (c *caseRun) drainEvents() []string {
	var out []string
	for {
		select {
		case e := <-c.events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func (c *caseRun) checkRoute(exp *routeExpect) {
	name := c.name
	if exp.Name != "" {
		name = exp.Name
	}
	u := modelRouteObject(c.ns, name)
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: c.ns, Name: name}, u)
	if exp.NotFound != nil && *exp.NotFound {
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		return
	}
	Expect(err).NotTo(HaveOccurred())
	if exp.Deleting != nil {
		if *exp.Deleting {
			Expect(u.GetDeletionTimestamp().IsZero()).To(BeFalse())
		} else {
			Expect(u.GetDeletionTimestamp().IsZero()).To(BeTrue())
		}
	}
	if exp.Finalizers != nil {
		Expect(u.GetFinalizers()).To(Equal(exp.Finalizers))
	}
}

func (c *caseRun) checkClusterPods(pods []clusterPodExpect) {
	for _, want := range pods {
		pod := &corev1.Pod{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: c.ns, Name: want.Name}, pod)
		if want.NotFound != nil && *want.NotFound {
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), want.Name)
			continue
		}
		Expect(err).NotTo(HaveOccurred())
		if want.Deleting != nil {
			if *want.Deleting {
				Expect(pod.DeletionTimestamp.IsZero()).To(BeFalse())
			} else {
				Expect(pod.DeletionTimestamp.IsZero()).To(BeTrue())
			}
		}
	}
}

func (c *caseRun) checkFinalizer(want string, got *servingv1alpha1.LLMService) {
	switch want {
	case "":
	case "present":
		Expect(got.Finalizers).To(ContainElement(servingv1alpha1.Finalizer))
	case "absent":
		Expect(got.Finalizers).NotTo(ContainElement(servingv1alpha1.Finalizer))
	default:
		Fail("unsupported finalizer " + want)
	}
}

func (c *caseRun) checkRevisions(exp *expectSpec, got *servingv1alpha1.LLMService) {
	if exp.PlantedRevisionUsed != nil && *exp.PlantedRevisionUsed {
		Expect(got.Status.History).NotTo(BeEmpty())
		Expect(got.Status.History[0].ControllerRevision).To(Equal(c.planted))
	}
	if exp.CapturedRevisionGone != nil && *exp.CapturedRevisionGone {
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: c.ns, Name: c.capturedRevision}, &appsv1.ControllerRevision{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}
	if exp.LatestRevisionExists != nil && *exp.LatestRevisionExists {
		Expect(got.Status.History).NotTo(BeEmpty())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: c.ns, Name: got.Status.History[0].ControllerRevision}, &appsv1.ControllerRevision{})).To(Succeed())
	}
	for _, sn := range exp.Snapshots {
		Expect(sn.Index).To(BeNumerically("<", len(got.Status.History)))
		c.checkSnapshot(got.Status.History[sn.Index].ControllerRevision, sn)
	}
}

func (c *caseRun) checkSnapshot(name string, sn snapshotExpect) {
	var cr appsv1.ControllerRevision
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: c.ns, Name: name}, &cr)).To(Succeed())
	if sn.ControllerOwner != nil && *sn.ControllerOwner {
		Expect(cr.OwnerReferences).NotTo(BeEmpty())
		Expect(cr.OwnerReferences[0].Controller).NotTo(BeNil())
		Expect(*cr.OwnerReferences[0].Controller).To(BeTrue())
	}
	for k, v := range sn.Labels {
		Expect(cr.Labels).To(HaveKeyWithValue(k, v))
	}
	var snap revisionSnapshot
	Expect(json.Unmarshal(cr.Data.Raw, &snap)).To(Succeed())
	for k, v := range sn.Annotations {
		Expect(snap.Annotations).To(HaveKeyWithValue(k, v))
	}
	for _, k := range sn.AnnotationsAbsent {
		Expect(snap.Annotations).NotTo(HaveKey(k))
	}
	if sn.ChartName != "" {
		Expect(snap.Spec.Chart.Name).To(Equal(sn.ChartName))
	}
}

func (c *caseRun) checkRelease(exp *releaseExpect, got *servingv1alpha1.LLMService) {
	slot := c.h.slot(c.ns, c.name)
	rel := c.h.releases[slot]
	if exp.Exists != nil {
		if *exp.Exists {
			Expect(c.h.releases).To(HaveKey(slot))
		} else {
			Expect(c.h.releases).NotTo(HaveKey(slot))
		}
	}
	if exp.Status != "" {
		Expect(rel).NotTo(BeNil())
		Expect(rel.Status).To(Equal(exp.Status))
	}
	if len(exp.Labels) == 0 && len(exp.LabelsAbsent) == 0 {
		return
	}
	Expect(rel).NotTo(BeNil())
	for k, v := range exp.Labels {
		if v == "self" {
			if got == nil {
				got = fetch(c.ns, c.name)
			}
			v = string(got.UID)
		}
		Expect(rel.Labels).To(HaveKeyWithValue(k, v))
	}
	for _, k := range exp.LabelsAbsent {
		Expect(rel.Labels).NotTo(HaveKey(k))
	}
}

func (c *caseRun) checkMissing() {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: c.ns, Name: c.name}, &servingv1alpha1.LLMService{})
	Expect(apierrors.IsNotFound(err)).To(BeTrue())
}
