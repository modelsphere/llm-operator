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
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/modelsphere/llm-operator/api/serving/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/helm"
	"github.com/modelsphere/llm-operator/internal/testutil/cases"
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
	histories                map[string][]helm.Release
	gets, installs, upgrades int
	uninstalls               int
	rollbacks                int
	lastRollback             int
	installErr               error
	upgradeErr               error
	getErr                   error
	lastForce                bool
	lastSkipSchema           bool
	// keepFailed stores a failed release when Install returns installErr.
	keepFailed bool
	// hold blocks Install and Upgrade until closed. entered is signaled first.
	hold, entered chan struct{}
}

func (f *fakeHelm) slot(ns, name string) string { return ns + "/" + name }

func (f *fakeHelm) Get(_ context.Context, ns, name string) (*helm.Release, error) {
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	rel := f.releases[f.slot(ns, name)]
	if rel == nil {
		return nil, nil
	}
	cp := *rel
	return &cp, nil
}

func (f *fakeHelm) History(_ context.Context, ns, name string) ([]helm.Release, error) {
	hist := f.historyOf(ns, name)
	out := make([]helm.Release, len(hist))
	copy(out, hist)
	slices.SortFunc(out, func(a, b helm.Release) int { return cmp.Compare(b.Revision, a.Revision) })
	return out, nil
}

func (f *fakeHelm) Rollback(_ context.Context, ns, name string, revision int) error {
	f.rollbacks++
	f.lastRollback = revision
	var target *helm.Release
	for _, rel := range f.historyOf(ns, name) {
		if rel.Revision == revision {
			target = &rel
			break
		}
	}
	if target == nil {
		return fmt.Errorf("release has no %d version", revision)
	}
	next := revision + 1
	if prev := f.releases[f.slot(ns, name)]; prev != nil && prev.Revision >= next {
		next = prev.Revision + 1
	}
	rel := *target
	rel.Revision = next
	rel.Status = deployed
	rel.Updated = time.Now()
	f.put(ns, name, &rel)
	return nil
}

func (f *fakeHelm) historyOf(ns, name string) []helm.Release {
	if f.histories != nil {
		if hist, ok := f.histories[f.slot(ns, name)]; ok {
			return hist
		}
	}
	if rel := f.releases[f.slot(ns, name)]; rel != nil {
		return []helm.Release{*rel}
	}
	return nil
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
	f.lastSkipSchema = opts.SkipSchemaValidation
	f.gate()
	if f.installErr != nil {
		if f.keepFailed {
			f.put(ns, name, &helm.Release{
				Revision: 1, Status: statusFailed, ChartName: ch.Name, ChartVersion: ch.Version, Config: values,
				Labels: opts.Labels, Updated: time.Now(),
			})
		}
		return nil, f.installErr
	}
	rel := &helm.Release{
		Revision: 1, Status: deployed, ChartName: ch.Name, ChartVersion: ch.Version, Config: values,
		Labels: opts.Labels, Updated: time.Now(),
	}
	f.put(ns, name, rel)
	cp := *rel
	return &cp, nil
}

func (f *fakeHelm) Upgrade(_ context.Context, ns, name string, ch helm.ChartRef, values map[string]any, opts helm.ApplyOpts) (*helm.Release, error) {
	f.upgrades++
	f.lastForce = opts.ForceConflicts
	f.lastSkipSchema = opts.SkipSchemaValidation
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
	rel := &helm.Release{
		Revision: rev, Status: deployed, ChartName: ch.Name, ChartVersion: ch.Version, Config: values,
		Labels: opts.Labels, Updated: time.Now(),
	}
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
	for _, f := range cases.MustLoad[serviceCase]("testdata/controller") {
		tc := f.Case
		It(tc.Description, func() {
			runServiceCase(tc)
		})
	}
})

func ownedRel(obj *servingv1alpha1.LLMService, rev int, status string, updated time.Time) *helm.Release {
	rel := ownedRelVal(obj, rev, status, updated)
	return &rel
}

func ownedRelVal(obj *servingv1alpha1.LLMService, rev int, status string, updated time.Time) helm.Release {
	return helm.Release{
		Revision:     rev,
		Status:       status,
		ChartName:    chartName,
		ChartVersion: ver086,
		Config:       map[string]any{replicasKey: float64(1)},
		Labels: map[string]string{
			llmserviceLabel:    obj.Name,
			llmserviceUIDLabel: string(obj.UID),
		},
		Updated: updated,
	}
}
