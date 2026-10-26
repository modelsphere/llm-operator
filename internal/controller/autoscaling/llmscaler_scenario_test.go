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

package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/modelsphere/llm-operator/api/autoscaling/v1alpha1"
)

// Literals shared across the fixtures in this package's tests.
const (
	appLabelKey  = "app"
	fixtureImage = "nginx"

	// decisionNamespace is the namespace the fake decision server reports in
	// its decisions.
	decisionNamespace = "modelforge"

	// mockMetricValue is the sample the fake Prometheus serves; over the
	// 0.5 target it always recommends scaling up.
	mockMetricValue = "0.85"

	fixtureNamespace   = "default"
	promScalerName     = "test-scaler"
	promDeployName     = "test-deployment"
	customScalerName   = "custom-provider-scaler"
	customDeployName   = "custom-provider-deployment"
	fakeRecorderBuffer = 32
)

// deferralStart is the time source for the deferral tests. Second-aligned
// so status.deferredSince round-trips through the apiserver unchanged
// (metav1.Time is stored as RFC3339, which has no fractional seconds).
var deferralStart = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// promFix and customFix are the envtest fixtures for the spec currently
// running. BeforeEach installs one; the runners read it from here so case
// entries stay data.
var (
	promFix   *promFixture
	customFix *customFixture
)

func ptr[T any](v T) *T { return &v }

// sinceKind says how a step checks status.deferredSince. The zero value
// leaves the field unchecked.
type sinceKind int

const (
	sinceUnchecked sinceKind = iota
	sinceNone
	sinceOffset
	sinceSeeded
)

// sinceExpect compares status.deferredSince to the fake clock's origin.
// sinceOffset is deferralStart+offset. sinceSeeded is the value written by
// scalerScenario.seedDeferredSince (a restarted reconciler). sinceNone expects
// the field to be cleared.
type sinceExpect struct {
	kind   sinceKind
	offset time.Duration
}

// scalerSpecPatch is applied to the fixture LLMScaler before the steps.
// A nil field is left at the fixture value. DeferTimeoutSeconds is a pointer
// on the API, so a pointer to 0 is "never force" and nil means "don't patch".
type scalerSpecPatch struct {
	minReplicas                *int32
	maxStepReplicas            *int32
	stabilizationWindowSeconds *int32
	deferTimeoutSeconds        *int32
	syncPeriodSeconds          *int32
}

func (p scalerSpecPatch) empty() bool {
	return p.minReplicas == nil && p.maxStepReplicas == nil &&
		p.stabilizationWindowSeconds == nil && p.deferTimeoutSeconds == nil &&
		p.syncPeriodSeconds == nil
}

// deployState is a hand-written Deployment spec and status. envtest runs no
// Deployment controller: a spec write bumps generation, and status has to be
// filled in here or the rollout guard sees a phantom rollout.
//
// available must be <= ready. The apiserver rejects availableReplicas greater
// than readyReplicas. readyReplicas 0 is omitted on the wire and reads back
// as a missing field, which is what the settle guard treats as "not ready".
//
// spec nil leaves spec.replicas alone. Otherwise spec is written first, then
// status with observedGeneration set to the post-write generation.
// terminating nil clears the field; the settle guard treats an absent value
// as "not reported" and does not wait on it.
type deployState struct {
	spec        *int32
	replicas    int32
	updated     int32
	ready       int32
	available   int32
	terminating *int32
}

// scenarioStep is one beat of a reconcile scenario. by is the step's By text.
// Mutations run before the reconcile. at is an absolute offset from
// deferralStart and is applied only when the reconciler has a fake clock.
// Zero is deferralStart itself, so a later step that should stay put must
// repeat its offset rather than leave at unset.
type scenarioStep struct {
	by             string
	at             time.Duration
	metric         string
	deploy         *deployState
	observeSpec    *int32
	rollout        bool
	restoreStatus  bool
	skipReconcile  bool
	snapshotStatus bool
	deleteDeploy   bool
	apiVersion     string
	maxReplicas    *int32
	closeServer    bool
	expect         stepExpect
}

// lastScaleExpect says how a step checks status.lastScaleTime. The zero value
// leaves the field unchecked. set means this sync wrote it (and, when the
// step reconciled on the fake clock, that it equals that instant). unset
// means nil. unchanged means equal to the value from before the reconcile.
type lastScaleExpect int

const (
	lastScaleSkip lastScaleExpect = iota
	lastScaleSet
	lastScaleUnset
	lastScaleUnchanged
)

func parseLastScale(s string) lastScaleExpect {
	switch s {
	case "":
		return lastScaleSkip
	case "set":
		return lastScaleSet
	case "unset":
		return lastScaleUnset
	case "unchanged":
		return lastScaleUnchanged
	default:
		Fail(fmt.Sprintf("lastScaleTime %q", s))
		return lastScaleSkip
	}
}

// stepExpect is the assertions for one step. Nil pointers and the zero
// sinceExpect are skipped, so a step only states what that beat checked.
type stepExpect struct {
	replicas                            *int32
	desired                             *int32
	applied                             *int32
	current                             *int32
	deferredSince                       sinceExpect
	condition                           *condExpect
	requeueAfter                        *time.Duration
	events                              []eventExpect
	noMoreEvents                        bool
	forcedWrite                         *bool
	forcedWriteAt                       *time.Duration
	active                              *condExpect
	decision                            *condExpect
	limited                             *condExpect
	observedGenerationCurrent           bool
	conditionsObservedGenerationCurrent bool
	generationGreaterThan               int64
	lastScaleTime                       lastScaleExpect
}

// condExpect is one status condition. messageContains are substrings that
// must all appear in the message. observedGenerationCurrent requires the
// condition's ObservedGeneration to equal the scaler's generation.
type condExpect struct {
	status                    metav1.ConditionStatus
	reason                    string
	messageContains           []string
	observedGenerationCurrent bool
	// observedGenerationStale requires ObservedGeneration to differ from the
	// scaler's generation: a later sync left a condition written earlier alone.
	observedGenerationStale bool
	// absent requires the condition to be missing. The other fields must be
	// unset; a present condition fails the step.
	absent bool
}

// eventExpect is one event in the order the recorder received it.
// prefix is a HavePrefix check (empty skips it); contains are substrings.
type eventExpect struct {
	prefix   string
	contains []string
}

// scalerScenario is one envtest reconcile spec. The fixture (1 replica, fully
// rolled out, metric 0.85, min 1, max 5) is already in place; these fields
// adjust it, then steps run in order.
type scalerScenario struct {
	// prelude is By text for the one-time setup, emitted before spec/deploy.
	prelude                  string
	spec                     scalerSpecPatch
	deploy                   *deployState
	rollout                  bool
	metric                   string
	seedDeferredSince        *time.Duration
	checkTerminating         *bool
	plainReconciler          bool
	expectNilRecommendations bool
	closeServer              bool
	steps                    []scenarioStep
}

// promFixture is the Prometheus-provider world for one spec: names, the fake
// Prometheus sample, and the client context.
type promFixture struct {
	ctx    context.Context
	scaler types.NamespacedName
	deploy types.NamespacedName
	server *httptest.Server
	metric string
}

func (f *promFixture) setMetric(v string) { f.metric = v }

func startPromFixture() *promFixture {
	f := &promFixture{
		ctx:    context.Background(),
		metric: mockMetricValue,
		scaler: types.NamespacedName{Name: promScalerName, Namespace: fixtureNamespace},
		deploy: types.NamespacedName{Name: promDeployName, Namespace: fixtureNamespace},
	}
	// Published before the creates so a failed Expect still gets cleaned up.
	promFix = f
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"result":[{"value":[0,"%s"]}]}}`, f.metric)
	}))
	createRolledOutDeployment(f.ctx, f.deploy, "test", 1)
	createPrometheusScaler(f.ctx, f.scaler, f.deploy.Name, f.server.URL)
	return f
}

func (f *promFixture) stop() {
	if f == nil {
		return
	}
	if f.server != nil {
		f.server.Close()
	}
	deleteScalerAndDeployment(f.ctx, f.scaler, f.deploy)
}

// customFixture is the decision-server world. The scaler is not created here:
// each case builds its own so admission cases can submit an invalid spec.
type customFixture struct {
	ctx    context.Context
	scaler types.NamespacedName
	deploy types.NamespacedName
	server *httptest.Server
	active string
}

func startCustomFixture() *customFixture {
	f := &customFixture{
		ctx:    context.Background(),
		active: "4",
		scaler: types.NamespacedName{Name: customScalerName, Namespace: fixtureNamespace},
		deploy: types.NamespacedName{Name: customDeployName, Namespace: fixtureNamespace},
	}
	customFix = f
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Expect(r.URL.Path).To(Equal(defaultDecisionsPath))
		Expect(r.URL.Query().Get("serviceId")).To(Equal(sampleServiceID))
		_, _ = fmt.Fprintf(w,
			`{"apiVersion":%q,"decisions":[{"namespace":%q,"serviceId":%q,"replicas":{"active":%s}}]}`,
			decisionsAPIVersion, decisionNamespace, sampleServiceID, f.active)
	}))
	createRolledOutDeployment(f.ctx, f.deploy, "custom", 1)
	return f
}

func (f *customFixture) stop() {
	if f == nil {
		return
	}
	if f.server != nil {
		f.server.Close()
	}
	deleteScalerAndDeployment(f.ctx, f.scaler, f.deploy)
}

// newScaler builds a Custom-provider scaler. Callers mutate it before create
// when they want a different corner of the CRD union rules.
func (f *customFixture) newScaler() *autoscalingv1alpha1.LLMScaler {
	return &autoscalingv1alpha1.LLMScaler{
		ObjectMeta: metav1.ObjectMeta{Name: f.scaler.Name, Namespace: f.scaler.Namespace},
		Spec: autoscalingv1alpha1.LLMScalerSpec{
			TargetRef: autoscalingv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       deploymentKind,
				Name:       f.deploy.Name,
			},
			MetricProvider: autoscalingv1alpha1.MetricProviderCustom,
			CustomProvider: &autoscalingv1alpha1.CustomProviderSpec{
				ServiceID: sampleServiceID,
				Namespace: decisionNamespace,
			},
			ServerAddress: f.server.URL,
			MinReplicas:   1,
			MaxReplicas:   5,
		},
	}
}

func createRolledOutDeployment(ctx context.Context, nn types.NamespacedName, app string, replicas int32) {
	By("creating a dummy Deployment to act as the target")
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: app}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabelKey: app}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: fixtureImage, Image: fixtureImage}}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, deploy)).To(Succeed())

	// envtest runs no Deployment controller, so populate status to look
	// fully rolled out; otherwise the rollout guard defers scaling.
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = replicas
	deploy.Status.UpdatedReplicas = replicas
	deploy.Status.ReadyReplicas = replicas
	deploy.Status.AvailableReplicas = replicas
	Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())
}

func createPrometheusScaler(ctx context.Context, nn types.NamespacedName, deployName, serverURL string) {
	By("creating the LLMScaler resource")
	scaler := &autoscalingv1alpha1.LLMScaler{
		ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
		Spec: autoscalingv1alpha1.LLMScalerSpec{
			TargetRef: autoscalingv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       deploymentKind,
				Name:       deployName,
			},
			ServerAddress: serverURL,
			MinReplicas:   1,
			MaxReplicas:   5,
			Metrics: []autoscalingv1alpha1.MetricSpec{{
				Query:  `avg(vllm:kv_cache_usage_perc)`,
				Target: "0.5",
			}},
		},
	}
	Expect(k8sClient.Create(ctx, scaler)).To(Succeed())
}

func deleteScalerAndDeployment(ctx context.Context, scalerNN, deployNN types.NamespacedName) {
	By("Cleanup the LLMScaler")
	scaler := &autoscalingv1alpha1.LLMScaler{}
	_ = k8sClient.Get(ctx, scalerNN, scaler)
	_ = k8sClient.Delete(ctx, scaler)

	By("Cleanup the Deployment")
	deploy := &appsv1.Deployment{}
	_ = k8sClient.Get(ctx, deployNN, deploy)
	_ = k8sClient.Delete(ctx, deploy)
}

func applySpecPatch(ctx context.Context, nn types.NamespacedName, patch scalerSpecPatch) {
	if patch.empty() {
		return
	}
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(ctx, nn, scaler)).To(Succeed())
	if patch.minReplicas != nil {
		scaler.Spec.MinReplicas = *patch.minReplicas
	}
	if patch.maxStepReplicas != nil {
		scaler.Spec.ScaleDown.MaxStepReplicas = *patch.maxStepReplicas
	}
	if patch.stabilizationWindowSeconds != nil {
		scaler.Spec.ScaleDown.StabilizationWindowSeconds = *patch.stabilizationWindowSeconds
	}
	if patch.deferTimeoutSeconds != nil {
		scaler.Spec.DeferTimeoutSeconds = patch.deferTimeoutSeconds
	}
	if patch.syncPeriodSeconds != nil {
		scaler.Spec.SyncPeriodSeconds = *patch.syncPeriodSeconds
	}
	Expect(k8sClient.Update(ctx, scaler)).To(Succeed())
}

func applyDeployState(ctx context.Context, nn types.NamespacedName, st deployState) {
	// availableReplicas > readyReplicas is rejected by the apiserver. Fail here
	// so the message names the rule instead of the status update.
	Expect(st.available).To(BeNumerically("<=", st.ready))

	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, nn, deploy)).To(Succeed())
	if st.spec != nil {
		deploy.Spec.Replicas = st.spec
		Expect(k8sClient.Update(ctx, deploy)).To(Succeed())
	}
	// A spec write bumps generation. Observing that generation is what keeps
	// a pure replica change from looking like a rollout.
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = st.replicas
	deploy.Status.UpdatedReplicas = st.updated
	deploy.Status.ReadyReplicas = st.ready
	deploy.Status.AvailableReplicas = st.available
	deploy.Status.TerminatingReplicas = st.terminating
	Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())
}

// markRollout leaves the Deployment mid-rollout: not every replica is on the
// new template yet. It does not touch observedGeneration.
func markRollout(ctx context.Context, nn types.NamespacedName) {
	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, nn, deploy)).To(Succeed())
	deploy.Status.UpdatedReplicas = 0
	Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())
}

// observeDeployment marks the Deployment observed at its current spec with the
// given ready count. A replica write bumps generation, and envtest runs no
// Deployment controller; without this the next sync sees a rollout.
// ready 0 is omitted by the apiserver and reads back as a missing field.
func observeDeployment(ctx context.Context, nn types.NamespacedName, ready int32) {
	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, nn, deploy)).To(Succeed())
	n := int32(1)
	if deploy.Spec.Replicas != nil {
		n = *deploy.Spec.Replicas
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = n
	deploy.Status.UpdatedReplicas = n
	deploy.Status.ReadyReplicas = ready
	deploy.Status.AvailableReplicas = ready
	Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())
}

func seedDeferredSince(ctx context.Context, nn types.NamespacedName, offset time.Duration) {
	// Whole seconds only: RFC3339 drops fractions, so a sub-second offset
	// would not match the value read back.
	stamped := metav1.NewTime(deferralStart.Add(offset))
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(ctx, nn, scaler)).To(Succeed())
	scaler.Status.DeferredSince = &stamped
	Expect(k8sClient.Status().Update(ctx, scaler)).To(Succeed())
}

func deploymentReplicas(ctx context.Context, nn types.NamespacedName) int32 {
	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, nn, deploy)).To(Succeed())
	Expect(deploy.Spec.Replicas).NotTo(BeNil())
	return *deploy.Spec.Replicas
}

type scenarioRun struct {
	fix    *promFixture
	r      *LLMScalerReconciler
	clk    *testingclock.FakeClock
	rec    *events.FakeRecorder
	result reconcile.Result
	held   *autoscalingv1alpha1.LLMScalerStatus
	seed   *time.Duration
}

func newScenarioRun(f *promFixture, sc scalerScenario) *scenarioRun {
	run := &scenarioRun{fix: f, seed: sc.seedDeferredSince}
	if sc.plainReconciler {
		run.r = &LLMScalerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		return run
	}
	// The buffer is larger than any spec asserts, so an event nobody receives
	// cannot fill it and block the reconciler.
	run.clk = testingclock.NewFakeClock(deferralStart)
	run.rec = events.NewFakeRecorder(fakeRecorderBuffer)
	run.r = newScalerReconciler(run.clk, run.rec)
	return run
}

func applyScenarioSetup(f *promFixture, sc scalerScenario) {
	applySpecPatch(f.ctx, f.scaler, sc.spec)
	if sc.deploy != nil {
		applyDeployState(f.ctx, f.deploy, *sc.deploy)
	}
	if sc.rollout {
		markRollout(f.ctx, f.deploy)
	}
	if sc.metric != "" {
		f.setMetric(sc.metric)
	}
	if sc.seedDeferredSince != nil {
		seedDeferredSince(f.ctx, f.scaler, *sc.seedDeferredSince)
	}
}

// runScalerScenario is the only runner for Prometheus-provider reconcile specs.
func runScalerScenario(sc scalerScenario) {
	f := promFix
	Expect(f).NotTo(BeNil())

	if sc.checkTerminating != nil {
		restore := checkTerminatingReplicas
		checkTerminatingReplicas = *sc.checkTerminating
		DeferCleanup(func() { checkTerminatingReplicas = restore })
	}
	if sc.prelude != "" {
		By(sc.prelude)
	}
	applyScenarioSetup(f, sc)

	run := newScenarioRun(f, sc)
	if sc.expectNilRecommendations {
		Expect(run.r.recommendations).To(BeNil())
	}
	if sc.closeServer {
		f.server.Close()
	}
	for _, step := range sc.steps {
		runScenarioStep(run, step)
	}
}

func runScenarioStep(run *scenarioRun, step scenarioStep) {
	if step.by != "" {
		By(step.by)
	}
	f := run.fix
	if step.metric != "" {
		f.setMetric(step.metric)
	}
	// deploy before rollout/observe: a full status write would undo them.
	if step.deploy != nil {
		applyDeployState(f.ctx, f.deploy, *step.deploy)
	}
	if step.observeSpec != nil {
		observeDeployment(f.ctx, f.deploy, *step.observeSpec)
	}
	if step.rollout {
		markRollout(f.ctx, f.deploy)
	}
	if step.restoreStatus {
		restoreScalerStatus(f.ctx, f.scaler, run.held)
	}
	if step.deleteDeploy {
		deleteDeployment(f.ctx, f.deploy)
	}
	if step.apiVersion != "" {
		setTargetAPIVersion(f.ctx, f.scaler, step.apiVersion)
	}
	if step.maxReplicas != nil {
		patchMaxReplicas(f.ctx, f.scaler, *step.maxReplicas)
	}
	if step.closeServer {
		f.server.Close()
	}
	var prevLastScale *metav1.Time
	if step.expect.lastScaleTime == lastScaleUnchanged {
		prevLastScale = scalerLastScaleTime(f.ctx, f.scaler)
	}
	if step.skipReconcile {
		assertStep(run, step, prevLastScale)
		return
	}
	if run.clk != nil {
		run.clk.SetTime(deferralStart.Add(step.at))
	}
	var err error
	run.result, err = run.r.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.scaler})
	Expect(err).NotTo(HaveOccurred())
	assertStep(run, step, prevLastScale)
	if step.snapshotStatus {
		run.held = snapshotScalerStatus(f.ctx, f.scaler)
	}
}

func snapshotScalerStatus(ctx context.Context, nn types.NamespacedName) *autoscalingv1alpha1.LLMScalerStatus {
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(ctx, nn, scaler)).To(Succeed())
	return scaler.Status.DeepCopy()
}

func restoreScalerStatus(ctx context.Context, nn types.NamespacedName, held *autoscalingv1alpha1.LLMScalerStatus) {
	Expect(held).NotTo(BeNil())
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(ctx, nn, scaler)).To(Succeed())
	scaler.Status = *held
	Expect(k8sClient.Status().Update(ctx, scaler)).To(Succeed())
}

func assertStep(run *scenarioRun, step scenarioStep, prevLastScale *metav1.Time) {
	exp := step.expect
	if exp.replicas != nil {
		Expect(deploymentReplicas(run.fix.ctx, run.fix.deploy)).To(Equal(*exp.replicas))
	}
	assertScalerStatus(run, step, prevLastScale)
	if exp.requeueAfter != nil {
		Expect(run.result.RequeueAfter).To(Equal(*exp.requeueAfter))
	}
	assertEvents(run, exp)
	assertForcedWrite(run, exp)
}

func assertScalerStatus(run *scenarioRun, step scenarioStep, prevLastScale *metav1.Time) {
	exp := step.expect
	if exp.desired == nil && exp.applied == nil && exp.current == nil &&
		exp.deferredSince.kind == sinceUnchecked && exp.condition == nil &&
		exp.active == nil && exp.decision == nil && exp.limited == nil &&
		!exp.observedGenerationCurrent && !exp.conditionsObservedGenerationCurrent &&
		exp.generationGreaterThan == 0 && exp.lastScaleTime == lastScaleSkip {
		return
	}
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(run.fix.ctx, run.fix.scaler, scaler)).To(Succeed())
	if exp.desired != nil {
		Expect(scaler.Status.DesiredReplicas).To(Equal(*exp.desired))
	}
	if exp.applied != nil {
		Expect(scaler.Status.AppliedReplicas).To(Equal(*exp.applied))
	}
	if exp.current != nil {
		Expect(scaler.Status.CurrentReplicas).To(Equal(*exp.current))
	}
	assertDeferredSince(scaler, exp.deferredSince, run.seed)
	if exp.condition != nil {
		assertCondition(scaler, autoscalingv1alpha1.ConditionAbleToScale, *exp.condition)
	}
	if exp.active != nil {
		assertCondition(scaler, autoscalingv1alpha1.ConditionScalingActive, *exp.active)
	}
	if exp.decision != nil {
		assertCondition(scaler, autoscalingv1alpha1.ConditionScalingActive, *exp.decision)
	}
	expectDecisionMirrorsActive(scaler)
	if exp.limited != nil {
		assertCondition(scaler, autoscalingv1alpha1.ConditionScalingLimited, *exp.limited)
	}
	if exp.observedGenerationCurrent {
		Expect(scaler.Status.ObservedGeneration).To(Equal(scaler.Generation))
	}
	if exp.generationGreaterThan > 0 {
		Expect(scaler.Generation).To(BeNumerically(">", exp.generationGreaterThan))
	}
	if exp.conditionsObservedGenerationCurrent {
		Expect(scaler.Status.Conditions).NotTo(BeEmpty())
		for i := range scaler.Status.Conditions {
			Expect(scaler.Status.Conditions[i].ObservedGeneration).To(Equal(scaler.Generation),
				"condition %s observedGeneration", scaler.Status.Conditions[i].Type)
		}
	}
	assertLastScaleTime(scaler.Status.LastScaleTime, exp.lastScaleTime, prevLastScale, deferralStart.Add(step.at), run.clk != nil && !step.skipReconcile)
}

func assertDeferredSince(scaler *autoscalingv1alpha1.LLMScaler, exp sinceExpect, seed *time.Duration) {
	switch exp.kind {
	case sinceUnchecked:
		return
	case sinceNone:
		Expect(scaler.Status.DeferredSince).To(BeNil())
	case sinceOffset:
		Expect(scaler.Status.DeferredSince).NotTo(BeNil())
		Expect(scaler.Status.DeferredSince.Time).To(BeTemporally("==", deferralStart.Add(exp.offset)))
	case sinceSeeded:
		Expect(seed).NotTo(BeNil())
		Expect(scaler.Status.DeferredSince).NotTo(BeNil())
		Expect(scaler.Status.DeferredSince.Time).To(BeTemporally("==", deferralStart.Add(*seed)))
	default:
		Fail(fmt.Sprintf("unknown deferredSince check %d", exp.kind))
	}
}

func assertCondition(scaler *autoscalingv1alpha1.LLMScaler, condType string, exp condExpect) {
	cond := meta.FindStatusCondition(scaler.Status.Conditions, condType)
	if exp.absent {
		if exp.status != "" || exp.reason != "" || len(exp.messageContains) > 0 ||
			exp.observedGenerationCurrent || exp.observedGenerationStale {
			Fail("absent condition also sets fields")
		}
		Expect(cond).To(BeNil())
		return
	}
	Expect(cond).NotTo(BeNil())
	if exp.status != "" {
		Expect(cond.Status).To(Equal(exp.status))
	}
	if exp.reason != "" {
		Expect(cond.Reason).To(Equal(exp.reason))
	}
	// An empty substring matches every message, so it is rejected on its own.
	for _, sub := range exp.messageContains {
		if sub == "" {
			Fail("messageContains entry is empty")
		}
		Expect(cond.Message).To(ContainSubstring(sub))
	}
	if exp.observedGenerationCurrent && exp.observedGenerationStale {
		Fail("observedGenerationCurrent and observedGenerationStale are both set")
	}
	if exp.observedGenerationCurrent {
		Expect(cond.ObservedGeneration).To(Equal(scaler.Generation))
	}
	if exp.observedGenerationStale {
		Expect(cond.ObservedGeneration).NotTo(Equal(scaler.Generation))
	}
}

func assertLastScaleTime(got *metav1.Time, exp lastScaleExpect, prev *metav1.Time, when time.Time, checkWhen bool) {
	switch exp {
	case lastScaleSkip:
		return
	case lastScaleUnset:
		Expect(got).To(BeNil())
	case lastScaleSet:
		Expect(got).NotTo(BeNil())
		if checkWhen {
			Expect(got.Time).To(BeTemporally("==", when))
		}
	case lastScaleUnchanged:
		if prev == nil {
			Expect(got).To(BeNil())
			return
		}
		Expect(got).NotTo(BeNil())
		Expect(got.Time).To(BeTemporally("==", prev.Time))
	default:
		Fail(fmt.Sprintf("unknown lastScaleTime check %d", exp))
	}
}

func assertEvents(run *scenarioRun, exp stepExpect) {
	if len(exp.events) == 0 && !exp.noMoreEvents {
		return
	}
	Expect(run.rec).NotTo(BeNil())
	for _, want := range exp.events {
		// An empty substring matches every event, so it is rejected on its own.
		for _, sub := range want.contains {
			if sub == "" {
				Fail("events.contains entry is empty")
			}
		}
		var got string
		Expect(run.rec.Events).To(Receive(&got))
		if want.prefix != "" {
			Expect(got).To(HavePrefix(want.prefix))
		}
		for _, sub := range want.contains {
			Expect(got).To(ContainSubstring(sub))
		}
	}
	if exp.noMoreEvents {
		Expect(run.rec.Events).NotTo(Receive())
	}
}

func assertForcedWrite(run *scenarioRun, exp stepExpect) {
	if exp.forcedWrite == nil && exp.forcedWriteAt == nil {
		return
	}
	got, ok := run.r.lastForcedWriteAt(run.fix.scaler)
	if exp.forcedWrite != nil {
		Expect(ok).To(Equal(*exp.forcedWrite))
	}
	if exp.forcedWriteAt != nil {
		Expect(ok).To(BeTrue())
		Expect(got).To(BeTemporally("==", deferralStart.Add(*exp.forcedWriteAt)))
	}
}

func newScalerReconciler(clk *testingclock.FakeClock, rec events.EventRecorder) *LLMScalerReconciler {
	return &LLMScalerReconciler{
		Client:   k8sClient,
		Scheme:   k8sClient.Scheme(),
		Clock:    clk,
		Recorder: rec,
	}
}

func deleteDeployment(ctx context.Context, nn types.NamespacedName) {
	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, nn, deploy)).To(Succeed())
	Expect(k8sClient.Delete(ctx, deploy)).To(Succeed())
}

func patchMaxReplicas(ctx context.Context, nn types.NamespacedName, maxReplicas int32) {
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(ctx, nn, scaler)).To(Succeed())
	scaler.Spec.MaxReplicas = maxReplicas
	Expect(k8sClient.Update(ctx, scaler)).To(Succeed())
}

func setTargetAPIVersion(ctx context.Context, nn types.NamespacedName, apiVersion string) {
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(ctx, nn, scaler)).To(Succeed())
	scaler.Spec.TargetRef.APIVersion = apiVersion
	Expect(k8sClient.Update(ctx, scaler)).To(Succeed())
}

func scalerLastScaleTime(ctx context.Context, nn types.NamespacedName) *metav1.Time {
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(ctx, nn, scaler)).To(Succeed())
	if scaler.Status.LastScaleTime == nil {
		return nil
	}
	return scaler.Status.LastScaleTime.DeepCopy()
}

// customReconcileCase drives one decision-server reconcile. The *By fields are
// By texts for each phase; empty strings skip the label.
type customReconcileCase struct {
	createBy                  string
	deploy                    *deployState
	deployBy                  string
	closeServer               bool
	closeBy                   string
	reconcileBy               string
	replicasBy                string
	wantReplicas              int32
	statusBy                  string
	wantDesired               *int32
	decisionStatus            metav1.ConditionStatus
	decisionReason            string
	decisionMessageNonEmpty   bool
	decisionMessageContains   []string
	active                    *condExpect
	observedGenerationCurrent bool
	lastScaleTime             lastScaleExpect
}

func runCustomProviderReconcile(tc customReconcileCase) {
	f := customFix
	Expect(f).NotTo(BeNil())

	if tc.createBy != "" {
		By(tc.createBy)
	}
	Expect(k8sClient.Create(f.ctx, f.newScaler())).To(Succeed())

	if tc.deploy != nil {
		if tc.deployBy != "" {
			By(tc.deployBy)
		}
		applyDeployState(f.ctx, f.deploy, *tc.deploy)
	}
	if tc.closeServer {
		if tc.closeBy != "" {
			By(tc.closeBy)
		}
		f.server.Close()
	}
	if tc.reconcileBy != "" {
		By(tc.reconcileBy)
	}
	reconciler := &LLMScalerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	_, err := reconciler.Reconcile(f.ctx, reconcile.Request{NamespacedName: f.scaler})
	Expect(err).NotTo(HaveOccurred())

	if tc.replicasBy != "" {
		By(tc.replicasBy)
	}
	Expect(deploymentReplicas(f.ctx, f.deploy)).To(Equal(tc.wantReplicas))

	needStatus := tc.wantDesired != nil || tc.decisionStatus != "" || tc.active != nil ||
		tc.observedGenerationCurrent || tc.lastScaleTime != lastScaleSkip ||
		len(tc.decisionMessageContains) > 0
	if !needStatus {
		return
	}
	if tc.statusBy != "" {
		By(tc.statusBy)
	}
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(f.ctx, f.scaler, scaler)).To(Succeed())
	if tc.wantDesired != nil {
		Expect(scaler.Status.DesiredReplicas).To(Equal(*tc.wantDesired))
	}
	if tc.observedGenerationCurrent {
		Expect(scaler.Status.ObservedGeneration).To(Equal(scaler.Generation))
	}
	assertLastScaleTime(scaler.Status.LastScaleTime, tc.lastScaleTime, nil, time.Time{}, false)
	var active *metav1.Condition
	if tc.decisionStatus != "" || len(tc.decisionMessageContains) > 0 {
		active = meta.FindStatusCondition(scaler.Status.Conditions, autoscalingv1alpha1.ConditionScalingActive)
		Expect(active).NotTo(BeNil())
	}
	if tc.decisionStatus != "" {
		Expect(active.Status).To(Equal(tc.decisionStatus))
		if tc.decisionReason != "" {
			Expect(active.Reason).To(Equal(tc.decisionReason))
		}
		if tc.decisionMessageNonEmpty {
			Expect(active.Message).NotTo(BeEmpty())
		}
	}
	for _, sub := range tc.decisionMessageContains {
		if sub == "" {
			Fail("decisionMessageContains entry is empty")
		}
		Expect(active.Message).To(ContainSubstring(sub))
	}
	if tc.active != nil {
		assertCondition(scaler, autoscalingv1alpha1.ConditionScalingActive, *tc.active)
	}
	expectDecisionMirrorsActive(scaler)
}

// expectDecisionMirrorsActive checks that the deprecated DecisionAvailable
// condition matches ScalingActive whenever either one is present.
func expectDecisionMirrorsActive(scaler *autoscalingv1alpha1.LLMScaler) {
	active := meta.FindStatusCondition(scaler.Status.Conditions, autoscalingv1alpha1.ConditionScalingActive)
	decision := meta.FindStatusCondition(scaler.Status.Conditions, autoscalingv1alpha1.ConditionDecisionAvailable) //nolint:staticcheck // deprecated mirror of ScalingActive, still written
	if active == nil && decision == nil {
		return
	}
	Expect(active).NotTo(BeNil())
	Expect(decision).NotTo(BeNil())
	Expect(decision.Status).To(Equal(active.Status))
	Expect(decision.Reason).To(Equal(active.Reason))
	Expect(decision.Message).To(Equal(active.Message))
	Expect(decision.ObservedGeneration).To(Equal(active.ObservedGeneration))
}

// customAdmissionCase submits one scaler the CRD should reject.
type customAdmissionCase struct {
	metricProvider     string
	omitCustomProvider bool
	wantErr            string
}

func runCustomProviderAdmission(tc customAdmissionCase) {
	f := customFix
	Expect(f).NotTo(BeNil())
	scaler := f.newScaler()
	if tc.omitCustomProvider {
		scaler.Spec.CustomProvider = nil
	}
	if tc.metricProvider != "" {
		scaler.Spec.MetricProvider = tc.metricProvider
	}
	err := k8sClient.Create(f.ctx, scaler)
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(ContainSubstring(tc.wantErr))
}
