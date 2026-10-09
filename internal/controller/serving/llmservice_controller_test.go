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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/helm"
)

const (
	ver086      = "0.8.6"
	deployed    = "deployed"
	chartName   = "sglang"
	replicasKey = "replicaCount"
)

var nsSeq int

func takeNS() string {
	nsSeq++
	name := fmt.Sprintf("ns-%d", nsSeq)
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	return name
}

func layer(body string) servingv1alpha1.Layer {
	return servingv1alpha1.Layer{Name: "site", Values: &apiextensionsv1.JSON{Raw: []byte(body)}}
}

func newService(ns, name, version string) *servingv1alpha1.LLMService {
	return &servingv1alpha1.LLMService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				"swiss.modelsphere.dev/action": "install",
				"swiss.modelsphere.dev/note":   "n",
				"unrelated":                    "x",
			},
		},
		Spec: servingv1alpha1.LLMServiceSpec{
			Chart: servingv1alpha1.ChartSpec{
				Name:    chartName,
				Repo:    "oci://ghcr.io/modelsphere/charts",
				Version: version,
			},
			Layers: []servingv1alpha1.Layer{layer(`{"replicaCount":1}`)},
		},
	}
}

type fakeHelm struct {
	releases                 map[string]*helm.Release
	gets, installs, upgrades int
	uninstalls               int
	installErr               error
	upgradeErr               error
	lastForce                bool
	// keepFailed stores a failed release when Install returns installErr.
	keepFailed bool
	// hold blocks Install and Upgrade until closed. entered is signaled first.
	hold, entered chan struct{}
}

func (f *fakeHelm) slot(ns, name string) string { return ns + "/" + name }

func (f *fakeHelm) Get(_ context.Context, ns, name string) (*helm.Release, error) {
	f.gets++
	rel := f.releases[f.slot(ns, name)]
	if rel == nil {
		return nil, nil
	}
	cp := *rel
	return &cp, nil
}

func (f *fakeHelm) gate() {
	if f.hold == nil {
		return
	}
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	<-f.hold
}

func (f *fakeHelm) Install(_ context.Context, ns, name string, ch helm.ChartRef, values map[string]any, opts helm.ApplyOpts) (*helm.Release, error) {
	f.installs++
	f.lastForce = opts.ForceConflicts
	f.gate()
	if f.installErr != nil {
		if f.keepFailed {
			f.put(ns, name, &helm.Release{
				Revision: 1, Status: "failed", ChartName: ch.Name, ChartVersion: ch.Version, Config: values,
			})
		}
		return nil, f.installErr
	}
	rel := &helm.Release{Revision: 1, Status: deployed, ChartName: ch.Name, ChartVersion: ch.Version, Config: values}
	f.put(ns, name, rel)
	cp := *rel
	return &cp, nil
}

func (f *fakeHelm) Upgrade(_ context.Context, ns, name string, ch helm.ChartRef, values map[string]any, opts helm.ApplyOpts) (*helm.Release, error) {
	f.upgrades++
	f.lastForce = opts.ForceConflicts
	f.gate()
	if f.upgradeErr != nil {
		return nil, f.upgradeErr
	}
	if f.installErr != nil {
		return nil, f.installErr
	}
	rev := 1
	if prev := f.releases[f.slot(ns, name)]; prev != nil {
		rev = prev.Revision + 1
	}
	rel := &helm.Release{Revision: rev, Status: deployed, ChartName: ch.Name, ChartVersion: ch.Version, Config: values}
	f.put(ns, name, rel)
	cp := *rel
	return &cp, nil
}

func (f *fakeHelm) Uninstall(_ context.Context, ns, name string) error {
	f.uninstalls++
	delete(f.releases, f.slot(ns, name))
	return nil
}

func (f *fakeHelm) put(ns, name string, rel *helm.Release) {
	if f.releases == nil {
		f.releases = map[string]*helm.Release{}
	}
	f.releases[f.slot(ns, name)] = rel
}

type versionList struct {
	versions []string
	calls    int
	err      error
}

func (v *versionList) list(context.Context, string, string, *helm.Credentials) ([]string, error) {
	v.calls++
	if v.err != nil {
		return nil, v.err
	}
	return append([]string(nil), v.versions...), nil
}

func newReconciler(h *fakeHelm, l *versionList, limit int) *LLMServiceReconciler {
	return &LLMServiceReconciler{
		Client:       k8sClient,
		Scheme:       k8sClient.Scheme(),
		Helm:         h,
		Recorder:     events.NewFakeRecorder(32),
		HistoryLimit: limit,
		ListVersions: l.list,
	}
}

func doReconcile(r *LLMServiceReconciler, ns, name string) (reconcile.Result, error) {
	return r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
}

func fetch(ns, name string) *servingv1alpha1.LLMService {
	obj := &servingv1alpha1.LLMService{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj)).To(Succeed())
	return obj
}

func condition(obj *servingv1alpha1.LLMService, typ string) *metav1.Condition {
	return meta.FindStatusCondition(obj.Status.Conditions, typ)
}

var _ = Describe("LLMService controller", func() {
	It("installs and records a ControllerRevision", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		obj := newService(ns, "qwen", ">=0.8.0")
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())

		_, err := doReconcile(newReconciler(h, l, 10), ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.installs).To(Equal(1))
		Expect(h.upgrades).To(Equal(0))

		got := fetch(ns, "qwen")
		Expect(got.Finalizers).To(ContainElement(servingv1alpha1.Finalizer))
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseApplied))
		Expect(got.Status.Chart.Version).To(Equal(ver086))
		Expect(got.Status.AppliedHash).To(HavePrefix("sha256:"))
		Expect(got.Status.Helm.Revision).To(Equal(1))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		Expect(got.Status.History).To(HaveLen(1))
		Expect(got.Status.History[0].Action).To(Equal("install"))
		Expect(got.Status.History[0].Annotations).To(HaveKey("swiss.modelsphere.dev/note"))
		Expect(got.Status.History[0].Annotations).NotTo(HaveKey("unrelated"))
		Expect(condition(got, servingv1alpha1.ConditionApplied).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(got, servingv1alpha1.ConditionApplied).Reason).To(Equal(servingv1alpha1.ReasonInstalled))

		var cr appsv1.ControllerRevision
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: got.Status.History[0].ControllerRevision}, &cr)).To(Succeed())
		Expect(cr.Labels).To(HaveKeyWithValue(llmserviceLabel, "qwen"))
		Expect(cr.OwnerReferences).NotTo(BeEmpty())
		Expect(cr.OwnerReferences[0].Controller).NotTo(BeNil())
		Expect(*cr.OwnerReferences[0].Controller).To(BeTrue())
		var snap revisionSnapshot
		Expect(json.Unmarshal(cr.Data.Raw, &snap)).To(Succeed())
		Expect(snap.Annotations).To(HaveKeyWithValue("swiss.modelsphere.dev/action", "install"))
		Expect(snap.Annotations).NotTo(HaveKey("unrelated"))
		Expect(snap.Spec.Chart.Name).To(Equal("sglang"))
	})

	It("does not call helm when the spec is unchanged", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ver086))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())

		got := fetch(ns, "qwen")
		got.Annotations["swiss.modelsphere.dev/note"] = "later"
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.installs).To(Equal(1))
		Expect(h.upgrades).To(Equal(0))
		Expect(h.gets).To(Equal(1))
	})

	It("upgrades and bounds history", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 2)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ver086))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		first := fetch(ns, "qwen").Status.History[0].ControllerRevision

		for _, body := range []string{`{"replicaCount":2}`, `{"replicaCount":3}`} {
			got := fetch(ns, "qwen")
			got.Spec.Layers = []servingv1alpha1.Layer{layer(body)}
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			_, err = doReconcile(r, ns, "qwen")
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(h.upgrades).To(Equal(2))
		got := fetch(ns, "qwen")
		Expect(got.Status.History).To(HaveLen(2))
		Expect(got.Status.History[0].Revision).To(Equal(3))
		Expect(got.Status.History[1].Revision).To(Equal(2))
		Expect(got.Status.Helm.Revision).To(Equal(3))
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: first}, &appsv1.ControllerRevision{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: got.Status.History[0].ControllerRevision}, &appsv1.ControllerRevision{})).To(Succeed())
	})

	It("keeps a resolved version while the range allows it", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ">=0.8.0"))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch(ns, "qwen").Status.Chart.Version).To(Equal(ver086))
		Expect(l.calls).To(Equal(1))

		l.versions = []string{ver086, "0.9.0"}
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch(ns, "qwen").Status.Chart.Version).To(Equal(ver086))
		Expect(l.calls).To(Equal(1))
		Expect(h.upgrades).To(Equal(0))

		got := fetch(ns, "qwen")
		got.Spec.Chart.Version = ">=0.9.0"
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(fetch(ns, "qwen").Status.Chart.Version).To(Equal("0.9.0"))
		Expect(l.calls).To(Equal(2))
		Expect(h.upgrades).To(Equal(1))
	})

	It("adopts a matching release and refuses drift", func() {
		ns := takeNS()
		h := &fakeHelm{}
		h.put(ns, "qwen", &helm.Release{
			Revision: 7, Status: deployed, ChartName: chartName, ChartVersion: ver086,
			Config: map[string]any{replicasKey: float64(1)},
		})
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ">=0.8.0"))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.installs).To(Equal(0))
		Expect(h.upgrades).To(Equal(0))
		got := fetch(ns, "qwen")
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseApplied))
		Expect(condition(got, servingv1alpha1.ConditionAdopted).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(got, servingv1alpha1.ConditionAdopted).Reason).To(Equal(servingv1alpha1.ReasonAdopted))
		Expect(condition(got, servingv1alpha1.ConditionApplied).Reason).To(Equal(servingv1alpha1.ReasonAdopted))
		Expect(got.Status.History[0].Revision).To(Equal(7))

		ns2 := takeNS()
		h.put(ns2, "qwen", &helm.Release{
			Revision: 3, Status: deployed, ChartName: "other", ChartVersion: "0.1.0",
			Config: map[string]any{replicasKey: float64(9)},
		})
		Expect(k8sClient.Create(ctx, newService(ns2, "qwen", ">=0.8.0"))).To(Succeed())
		_, err = doReconcile(r, ns2, "qwen")
		Expect(err).NotTo(HaveOccurred())
		_, err = doReconcile(r, ns2, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.installs).To(Equal(0))
		Expect(h.upgrades).To(Equal(0))
		drifted := fetch(ns2, "qwen")
		Expect(drifted.Status.Phase).To(Equal(servingv1alpha1.PhaseFailed))
		Expect(drifted.Status.ObservedGeneration).To(Equal(drifted.Generation))
		Expect(condition(drifted, servingv1alpha1.ConditionAdopted).Status).To(Equal(metav1.ConditionFalse))
		Expect(condition(drifted, servingv1alpha1.ConditionAdopted).Reason).To(Equal(servingv1alpha1.ReasonDrift))
		Expect(drifted.Status.Message).To(ContainSubstring("chart"))
		Expect(drifted.Status.Message).To(ContainSubstring("values"))
	})

	It("orphans a delete and uninstalls otherwise", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "keep", ver086))).To(Succeed())
		_, err := doReconcile(r, ns, "keep")
		Expect(err).NotTo(HaveOccurred())
		got := fetch(ns, "keep")
		got.Annotations[servingv1alpha1.AnnotationDeletionPolicy] = servingv1alpha1.DeletionPolicyOrphan
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Expect(k8sClient.Delete(ctx, got)).To(Succeed())
		gets := h.gets
		_, err = doReconcile(r, ns, "keep")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.uninstalls).To(Equal(0))
		Expect(h.gets).To(Equal(gets))
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "keep"}, &servingv1alpha1.LLMService{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		Expect(k8sClient.Create(ctx, newService(ns, "drop", ver086))).To(Succeed())
		_, err = doReconcile(r, ns, "drop")
		Expect(err).NotTo(HaveOccurred())
		drop := fetch(ns, "drop")
		Expect(k8sClient.Delete(ctx, drop)).To(Succeed())
		_, err = doReconcile(r, ns, "drop")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.uninstalls).To(Equal(1))
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "drop"}, &servingv1alpha1.LLMService{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("uninstalls when the release is already absent", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		obj := newService(ns, "gone", ver086)
		obj.Finalizers = []string{servingv1alpha1.Finalizer}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
		_, err := doReconcile(newReconciler(h, l, 10), ns, "gone")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.gets).To(Equal(0))
		Expect(h.installs).To(Equal(0))
		Expect(h.upgrades).To(Equal(0))
		Expect(h.uninstalls).To(Equal(1))
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gone"}, &servingv1alpha1.LLMService{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("leaves status unchanged while suspended", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ver086))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		before := fetch(ns, "qwen")
		before.Spec.Suspend = true
		before.Spec.Layers = []servingv1alpha1.Layer{layer(`{"replicaCount":9}`)}
		Expect(k8sClient.Update(ctx, before)).To(Succeed())
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.installs).To(Equal(1))
		Expect(h.upgrades).To(Equal(0))
		got := fetch(ns, "qwen")
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseApplied))
		Expect(got.Status.AppliedHash).To(Equal(before.Status.AppliedHash))
		Expect(got.Status.Helm.Revision).To(Equal(1))
		Expect(got.Status.ObservedGeneration).To(Equal(before.Status.ObservedGeneration))
	})

	It("honors force-conflicts only for the current generation", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		obj := newService(ns, "qwen", ver086)
		obj.Annotations[servingv1alpha1.AnnotationForceConflicts] = "1"
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.lastForce).To(BeTrue())
		Expect(fetch(ns, "qwen").Status.History[0].ForceConflicts).To(BeTrue())

		got := fetch(ns, "qwen")
		got.Spec.Layers = []servingv1alpha1.Layer{layer(`{"replicaCount":2}`)}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Expect(fetch(ns, "qwen").Generation).To(Equal(int64(2)))
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.lastForce).To(BeFalse())
		Expect(fetch(ns, "qwen").Status.History[0].ForceConflicts).To(BeFalse())
	})

	It("records a helm failure", func() {
		ns := takeNS()
		h := &fakeHelm{installErr: errors.New("boom")}
		l := &versionList{versions: []string{ver086}}
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ver086))).To(Succeed())
		_, err := doReconcile(newReconciler(h, l, 10), ns, "qwen")
		Expect(err).To(MatchError(ContainSubstring("boom")))
		got := fetch(ns, "qwen")
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseFailed))
		Expect(got.Status.Message).To(ContainSubstring("boom"))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		Expect(condition(got, servingv1alpha1.ConditionApplied).Reason).To(Equal(servingv1alpha1.ReasonApplyFailed))
		Expect(condition(got, servingv1alpha1.ConditionApplied).Status).To(Equal(metav1.ConditionFalse))
	})

	It("upgrades a failed release from our own install", func() {
		ns := takeNS()
		h := &fakeHelm{installErr: errors.New("boom"), keepFailed: true}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ver086))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).To(MatchError(ContainSubstring("boom")))
		got := fetch(ns, "qwen")
		Expect(got.Status.AppliedHash).To(BeEmpty())
		Expect(got.Status.History).To(BeEmpty())
		Expect(condition(got, servingv1alpha1.ConditionApplied).Reason).To(Equal(servingv1alpha1.ReasonApplyFailed))

		h.installErr = nil
		h.upgradeErr = errors.New("has no deployed releases")
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).To(MatchError(ContainSubstring("has no deployed releases")))
		Expect(h.installs).To(Equal(1))
		Expect(h.upgrades).To(Equal(1))
		got = fetch(ns, "qwen")
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseFailed))
		Expect(got.Status.Message).To(ContainSubstring("has no deployed releases"))
		Expect(condition(got, servingv1alpha1.ConditionApplied).Reason).To(Equal(servingv1alpha1.ReasonApplyFailed))
		Expect(condition(got, servingv1alpha1.ConditionAdopted)).To(BeNil())

		h.upgradeErr = nil
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.installs).To(Equal(1))
		Expect(h.upgrades).To(Equal(2))
		got = fetch(ns, "qwen")
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseApplied))
		Expect(condition(got, servingv1alpha1.ConditionApplied).Reason).To(Equal(servingv1alpha1.ReasonUpgraded))
	})

	It("does not adopt a release that is not deployed", func() {
		ns := takeNS()
		h := &fakeHelm{}
		h.put(ns, "qwen", &helm.Release{
			Revision: 1, Status: "failed", ChartName: chartName, ChartVersion: ver086,
			Config: map[string]any{replicasKey: float64(1)},
		})
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ver086))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		Expect(h.installs).To(Equal(0))
		Expect(h.upgrades).To(Equal(0))
		got := fetch(ns, "qwen")
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseFailed))
		Expect(got.Status.Message).To(Equal("release is failed, not deployed"))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		Expect(condition(got, servingv1alpha1.ConditionAdopted).Status).To(Equal(metav1.ConditionFalse))
		Expect(condition(got, servingv1alpha1.ConditionAdopted).Reason).To(Equal(servingv1alpha1.ReasonDrift))
	})

	It("keeps observedGeneration while install is in progress", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ver086))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())
		prev := fetch(ns, "qwen").Status.ObservedGeneration
		Expect(prev).NotTo(BeZero())

		delete(h.releases, h.slot(ns, "qwen"))
		got := fetch(ns, "qwen")
		got.Spec.Layers = []servingv1alpha1.Layer{layer(`{"replicaCount":4}`)}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())

		h.hold = make(chan struct{})
		h.entered = make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			_, recErr := doReconcile(r, ns, "qwen")
			done <- recErr
		}()
		Eventually(h.entered).WithTimeout(5 * time.Second).Should(Receive())
		mid := fetch(ns, "qwen")
		Expect(mid.Status.Phase).To(Equal(servingv1alpha1.PhaseApplying))
		Expect(mid.Status.ObservedGeneration).To(Equal(prev))
		close(h.hold)
		Eventually(done).WithTimeout(5 * time.Second).Should(Receive(BeNil()))
		Expect(h.installs).To(Equal(2))
		Expect(h.upgrades).To(Equal(0))
		doneObj := fetch(ns, "qwen")
		Expect(doneObj.Status.Phase).To(Equal(servingv1alpha1.PhaseApplied))
		Expect(doneObj.Status.ObservedGeneration).To(Equal(doneObj.Generation))
	})

	It("reuses a ControllerRevision when the same spec is applied again", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{versions: []string{ver086}}
		r := newReconciler(h, l, 10)
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ver086))).To(Succeed())
		_, err := doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())

		got := fetch(ns, "qwen")
		got.Spec.Layers = []servingv1alpha1.Layer{layer(`{"replicaCount":2}`)}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())

		got = fetch(ns, "qwen")
		got.Spec.Layers = []servingv1alpha1.Layer{layer(`{"replicaCount":1}`)}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		_, err = doReconcile(r, ns, "qwen")
		Expect(err).NotTo(HaveOccurred())

		got = fetch(ns, "qwen")
		Expect(got.Status.History).To(HaveLen(3))
		Expect(got.Status.History[0].Hash).To(Equal(got.Status.History[2].Hash))
		Expect(got.Status.History[0].Hash).NotTo(Equal(got.Status.History[1].Hash))
		Expect(got.Status.History[0].ControllerRevision).To(Equal(got.Status.History[2].ControllerRevision))
		Expect(got.Status.History[0].ControllerRevision).NotTo(Equal(got.Status.History[1].ControllerRevision))
		Expect(got.Status.History[0].Revision).To(Equal(3))
		Expect(got.Status.History[1].Revision).To(Equal(2))
		Expect(got.Status.History[2].Revision).To(Equal(1))

		var crA, crB appsv1.ControllerRevision
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: got.Status.History[0].ControllerRevision}, &crA)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: got.Status.History[1].ControllerRevision}, &crB)).To(Succeed())
		Expect(crA.Revision).To(Equal(int64(got.Status.History[0].Revision)))
		Expect(crB.Revision).To(Equal(int64(2)))
		var snap revisionSnapshot
		Expect(json.Unmarshal(crA.Data.Raw, &snap)).To(Succeed())
		Expect(snap.Spec.Layers).To(HaveLen(1))
		var values map[string]any
		Expect(json.Unmarshal(snap.Spec.Layers[0].Values.Raw, &values)).To(Succeed())
		Expect(values[replicasKey]).To(Equal(float64(1)))
	})

	It("fails when the chart version cannot be resolved", func() {
		ns := takeNS()
		h := &fakeHelm{}
		l := &versionList{err: errors.New("registry down")}
		Expect(k8sClient.Create(ctx, newService(ns, "qwen", ">=0.8.0"))).To(Succeed())
		_, err := doReconcile(newReconciler(h, l, 10), ns, "qwen")
		Expect(err).To(HaveOccurred())
		got := fetch(ns, "qwen")
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseFailed))
		Expect(got.Status.Message).To(ContainSubstring("registry down"))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
		Expect(condition(got, servingv1alpha1.ConditionApplied).Reason).To(Equal(servingv1alpha1.ReasonChartResolveFailed))
		Expect(h.installs).To(Equal(0))
	})
})
