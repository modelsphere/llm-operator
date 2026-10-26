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
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/modelsphere/llm-operator/api/autoscaling/v1alpha1"
	"github.com/modelsphere/llm-operator/internal/testutil/cases"
)

// sinceCheck is how one step reads status.deferredSince.
// Omitted means do not check. JSON null means the field was cleared.
// "seed" is the instant written by seedDeferredSince. Any other string is a
// Go duration offset from deferralStart, including "0s".
type sinceCheck struct {
	kind   sinceKind
	offset time.Duration
}

func (s *sinceCheck) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		s.kind = sinceNone
		return nil
	}
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("deferredSince: %w", err)
	}
	if raw == "seed" {
		s.kind = sinceSeeded
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("deferredSince %q: %w", raw, err)
	}
	s.kind = sinceOffset
	s.offset = d
	return nil
}

func (s sinceCheck) expect() sinceExpect { return sinceExpect(s) }

type specPatchInput struct {
	MinReplicas                *int32 `json:"minReplicas"`
	MaxStepReplicas            *int32 `json:"maxStepReplicas"`
	StabilizationWindowSeconds *int32 `json:"stabilizationWindowSeconds"`
	DeferTimeoutSeconds        *int32 `json:"deferTimeoutSeconds"`
	SyncPeriodSeconds          *int32 `json:"syncPeriodSeconds"`
}

func (p specPatchInput) patch() scalerSpecPatch {
	return scalerSpecPatch{
		minReplicas:                p.MinReplicas,
		maxStepReplicas:            p.MaxStepReplicas,
		stabilizationWindowSeconds: p.StabilizationWindowSeconds,
		deferTimeoutSeconds:        p.DeferTimeoutSeconds,
		syncPeriodSeconds:          p.SyncPeriodSeconds,
	}
}

type deployInput struct {
	Spec        *int32 `json:"spec"`
	Replicas    int32  `json:"replicas"`
	Updated     int32  `json:"updated"`
	Ready       int32  `json:"ready"`
	Available   int32  `json:"available"`
	Terminating *int32 `json:"terminating"`
}

func (d deployInput) state() deployState {
	return deployState{
		spec:        d.Spec,
		replicas:    d.Replicas,
		updated:     d.Updated,
		ready:       d.Ready,
		available:   d.Available,
		terminating: d.Terminating,
	}
}

type stepInput struct {
	By             string         `json:"by"`
	At             cases.Duration `json:"at"`
	Metric         string         `json:"metric"`
	Deploy         *deployInput   `json:"deploy"`
	ObserveSpec    *int32         `json:"observeSpec"`
	Rollout        bool           `json:"rollout"`
	RestoreStatus  bool           `json:"restoreStatus"`
	SkipReconcile  bool           `json:"skipReconcile"`
	SnapshotStatus bool           `json:"snapshotStatus"`
	DeleteDeploy   bool           `json:"deleteDeploy"`
	APIVersion     string         `json:"apiVersion"`
	MaxReplicas    *int32         `json:"maxReplicas"`
}

type condInput struct {
	Status                    string   `json:"status"`
	Reason                    string   `json:"reason"`
	MessageContains           []string `json:"messageContains"`
	ObservedGenerationCurrent bool     `json:"observedGenerationCurrent"`
	ObservedGenerationStale   bool     `json:"observedGenerationStale"`
}

func (c condInput) expect() condExpect {
	return condExpect{
		status:                    metav1.ConditionStatus(c.Status),
		reason:                    c.Reason,
		messageContains:           c.MessageContains,
		observedGenerationCurrent: c.ObservedGenerationCurrent,
		observedGenerationStale:   c.ObservedGenerationStale,
	}
}

type eventInput struct {
	Prefix   string   `json:"prefix"`
	Contains []string `json:"contains"`
}

func (e eventInput) expect() eventExpect {
	return eventExpect{prefix: e.Prefix, contains: e.Contains}
}

type stepExpected struct {
	Replicas                            *int32          `json:"replicas"`
	Desired                             *int32          `json:"desired"`
	Applied                             *int32          `json:"applied"`
	Current                             *int32          `json:"current"`
	DeferredSince                       sinceCheck      `json:"deferredSince"`
	Condition                           *condInput      `json:"condition"`
	RequeueAfter                        *cases.Duration `json:"requeueAfter"`
	Events                              []eventInput    `json:"events"`
	NoMoreEvents                        bool            `json:"noMoreEvents"`
	ForcedWrite                         *bool           `json:"forcedWrite"`
	ForcedWriteAt                       *cases.Duration `json:"forcedWriteAt"`
	Ready                               *condInput      `json:"ready"`
	Decision                            *condInput      `json:"decision"`
	ObservedGenerationCurrent           bool            `json:"observedGenerationCurrent"`
	ConditionsObservedGenerationCurrent bool            `json:"conditionsObservedGenerationCurrent"`
	GenerationGreaterThan               int64           `json:"generationGreaterThan"`
	LastScaleTime                       string          `json:"lastScaleTime"`
}

func (e stepExpected) expect() stepExpect {
	out := stepExpect{
		replicas:                            e.Replicas,
		desired:                             e.Desired,
		applied:                             e.Applied,
		current:                             e.Current,
		deferredSince:                       e.DeferredSince.expect(),
		requeueAfter:                        durPtr(e.RequeueAfter),
		noMoreEvents:                        e.NoMoreEvents,
		forcedWrite:                         e.ForcedWrite,
		forcedWriteAt:                       durPtr(e.ForcedWriteAt),
		observedGenerationCurrent:           e.ObservedGenerationCurrent,
		conditionsObservedGenerationCurrent: e.ConditionsObservedGenerationCurrent,
		generationGreaterThan:               e.GenerationGreaterThan,
		lastScaleTime:                       parseLastScale(e.LastScaleTime),
	}
	if e.Condition != nil {
		cond := e.Condition.expect()
		out.condition = &cond
	}
	if e.Ready != nil {
		ready := e.Ready.expect()
		out.ready = &ready
	}
	if e.Decision != nil {
		decision := e.Decision.expect()
		out.decision = &decision
	}
	if len(e.Events) > 0 {
		out.events = make([]eventExpect, len(e.Events))
		for i, ev := range e.Events {
			out.events[i] = ev.expect()
		}
	}
	return out
}

func (s stepInput) step(exp stepExpected) scenarioStep {
	out := scenarioStep{
		by:             s.By,
		at:             s.At.Std(),
		metric:         s.Metric,
		observeSpec:    s.ObserveSpec,
		rollout:        s.Rollout,
		restoreStatus:  s.RestoreStatus,
		skipReconcile:  s.SkipReconcile,
		snapshotStatus: s.SnapshotStatus,
		deleteDeploy:   s.DeleteDeploy,
		apiVersion:     s.APIVersion,
		maxReplicas:    s.MaxReplicas,
		expect:         exp.expect(),
	}
	if s.Deploy != nil {
		st := s.Deploy.state()
		out.deploy = &st
	}
	return out
}

func durPtr(d *cases.Duration) *time.Duration {
	if d == nil {
		return nil
	}
	v := d.Std()
	return &v
}

type scalerInputs struct {
	Prelude                  string          `json:"prelude"`
	Spec                     specPatchInput  `json:"spec"`
	Deploy                   *deployInput    `json:"deploy"`
	Rollout                  bool            `json:"rollout"`
	Metric                   string          `json:"metric"`
	SeedDeferredSince        *cases.Duration `json:"seedDeferredSince"`
	CheckTerminating         *bool           `json:"checkTerminating"`
	PlainReconciler          bool            `json:"plainReconciler"`
	ExpectNilRecommendations bool            `json:"expectNilRecommendations"`
	CloseServer              bool            `json:"closeServer"`
	Steps                    []stepInput     `json:"steps"`
}

type scalerExpected struct {
	Steps []stepExpected `json:"steps"`
}

// scalerCase is one Prometheus-provider reconcile spec.
// inputs.steps and expected.steps are zipped by index.
type scalerCase struct {
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Inputs      scalerInputs   `json:"inputs"`
	Expected    scalerExpected `json:"expected"`
}

func (c scalerCase) scenario() scalerScenario {
	in := c.Inputs
	if len(in.Steps) != len(c.Expected.Steps) {
		Fail(fmt.Sprintf("%s: %d input steps and %d expected steps", c.Description, len(in.Steps), len(c.Expected.Steps)))
	}
	steps := make([]scenarioStep, len(in.Steps))
	for i := range in.Steps {
		steps[i] = in.Steps[i].step(c.Expected.Steps[i])
	}
	sc := scalerScenario{
		prelude:                  in.Prelude,
		spec:                     in.Spec.patch(),
		rollout:                  in.Rollout,
		metric:                   in.Metric,
		seedDeferredSince:        durPtr(in.SeedDeferredSince),
		checkTerminating:         in.CheckTerminating,
		plainReconciler:          in.PlainReconciler,
		expectNilRecommendations: in.ExpectNilRecommendations,
		closeServer:              in.CloseServer,
		steps:                    steps,
	}
	if in.Deploy != nil {
		st := in.Deploy.state()
		sc.deploy = &st
	}
	return sc
}

func runScalerCase(c scalerCase) {
	runScalerScenario(c.scenario())
}

type customReconcileFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		CreateBy    string       `json:"createBy"`
		Deploy      *deployInput `json:"deploy"`
		DeployBy    string       `json:"deployBy"`
		CloseServer bool         `json:"closeServer"`
		CloseBy     string       `json:"closeBy"`
		ReconcileBy string       `json:"reconcileBy"`
		ReplicasBy  string       `json:"replicasBy"`
		StatusBy    string       `json:"statusBy"`
	} `json:"inputs"`
	Expected struct {
		Replicas                  int32      `json:"replicas"`
		Desired                   *int32     `json:"desired"`
		DecisionStatus            string     `json:"decisionStatus"`
		DecisionReason            string     `json:"decisionReason"`
		DecisionMessageNonEmpty   bool       `json:"decisionMessageNonEmpty"`
		DecisionMessageContains   []string   `json:"decisionMessageContains"`
		Ready                     *condInput `json:"ready"`
		ObservedGenerationCurrent bool       `json:"observedGenerationCurrent"`
		LastScaleTime             string     `json:"lastScaleTime"`
		MirrorDecisionMessage     bool       `json:"mirrorDecisionMessage"`
	} `json:"expected"`
}

func runCustomReconcileFile(c customReconcileFile) {
	exp := c.Expected
	tc := customReconcileCase{
		createBy:                  c.Inputs.CreateBy,
		deployBy:                  c.Inputs.DeployBy,
		closeServer:               c.Inputs.CloseServer,
		closeBy:                   c.Inputs.CloseBy,
		reconcileBy:               c.Inputs.ReconcileBy,
		replicasBy:                c.Inputs.ReplicasBy,
		wantReplicas:              exp.Replicas,
		statusBy:                  c.Inputs.StatusBy,
		wantDesired:               exp.Desired,
		decisionStatus:            metav1.ConditionStatus(exp.DecisionStatus),
		decisionReason:            exp.DecisionReason,
		decisionMessageNonEmpty:   exp.DecisionMessageNonEmpty,
		decisionMessageContains:   exp.DecisionMessageContains,
		observedGenerationCurrent: exp.ObservedGenerationCurrent,
		lastScaleTime:             parseLastScale(exp.LastScaleTime),
		mirrorDecisionMessage:     exp.MirrorDecisionMessage,
	}
	if exp.Ready != nil {
		ready := exp.Ready.expect()
		tc.ready = &ready
	}
	if c.Inputs.Deploy != nil {
		st := c.Inputs.Deploy.state()
		tc.deploy = &st
	}
	runCustomProviderReconcile(tc)
}

type customAdmissionFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		MetricProvider     string `json:"metricProvider"`
		OmitCustomProvider bool   `json:"omitCustomProvider"`
		PinProvider        bool   `json:"pinProvider"`
	} `json:"inputs"`
	Expected struct {
		ErrorContains string `json:"errorContains"`
	} `json:"expected"`
}

func runCustomAdmissionFile(c customAdmissionFile) {
	if c.Inputs.PinProvider && c.Inputs.MetricProvider != autoscalingv1alpha1.MetricProviderPrometheus {
		Fail(fmt.Sprintf("metricProvider %q, want %q", c.Inputs.MetricProvider, autoscalingv1alpha1.MetricProviderPrometheus))
	}
	// An empty substring matches every error.
	if c.Expected.ErrorContains == "" {
		Fail(c.Description + ": errorContains is required")
	}
	runCustomProviderAdmission(customAdmissionCase{
		metricProvider:     c.Inputs.MetricProvider,
		omitCustomProvider: c.Inputs.OmitCustomProvider,
		wantErr:            c.Expected.ErrorContains,
	})
}

type deferDefaultFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
	} `json:"inputs"`
	Expected struct {
		Seconds int32 `json:"seconds"`
	} `json:"expected"`
}

func runDeferDefault(tc deferDefaultFile) {
	if tc.Expected.Seconds != autoscalingv1alpha1.DefaultDeferTimeoutSeconds {
		Fail(fmt.Sprintf("seconds %d, want DefaultDeferTimeoutSeconds %d",
			tc.Expected.Seconds, autoscalingv1alpha1.DefaultDeferTimeoutSeconds))
	}
	scaler := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(promFix.ctx, promFix.scaler, scaler)).To(Succeed())
	// The fixture leaves the field unset; the CRD default fills it.
	Expect(scaler.Spec.DeferTimeoutSeconds).NotTo(BeNil())
	Expect(*scaler.Spec.DeferTimeoutSeconds).To(Equal(tc.Expected.Seconds))
}

type customDefaultFile struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Inputs      struct {
		CreateBy  string `json:"createBy"`
		SwitchBy  string `json:"switchBy"`
		Query     string `json:"query"`
		Target    string `json:"target"`
		ServiceID string `json:"serviceId"`
	} `json:"inputs"`
	Expected struct {
		MetricProvider string `json:"metricProvider"`
		Path           string `json:"path"`
	} `json:"expected"`
}

func runCustomDefault(tc customDefaultFile) {
	if tc.Expected.MetricProvider != autoscalingv1alpha1.MetricProviderPrometheus {
		Fail(fmt.Sprintf("metricProvider %q, want %q", tc.Expected.MetricProvider, autoscalingv1alpha1.MetricProviderPrometheus))
	}
	if tc.Expected.Path != defaultDecisionsPath {
		Fail(fmt.Sprintf("path %q, want %q", tc.Expected.Path, defaultDecisionsPath))
	}
	if tc.Inputs.ServiceID != sampleServiceID {
		Fail(fmt.Sprintf("serviceId %q, want %q", tc.Inputs.ServiceID, sampleServiceID))
	}

	By(tc.Inputs.CreateBy)
	scaler := customFix.newScaler()
	scaler.Spec.MetricProvider = ""
	scaler.Spec.CustomProvider = nil
	scaler.Spec.Metrics = []autoscalingv1alpha1.MetricSpec{{Query: tc.Inputs.Query, Target: tc.Inputs.Target}}
	Expect(k8sClient.Create(customFix.ctx, scaler)).To(Succeed())

	stored := &autoscalingv1alpha1.LLMScaler{}
	Expect(k8sClient.Get(customFix.ctx, customFix.scaler, stored)).To(Succeed())
	Expect(stored.Spec.MetricProvider).To(Equal(tc.Expected.MetricProvider))

	By(tc.Inputs.SwitchBy)
	stored.Spec.MetricProvider = autoscalingv1alpha1.MetricProviderCustom
	stored.Spec.CustomProvider = &autoscalingv1alpha1.CustomProviderSpec{ServiceID: tc.Inputs.ServiceID}
	Expect(k8sClient.Update(customFix.ctx, stored)).To(Succeed())

	Expect(k8sClient.Get(customFix.ctx, customFix.scaler, stored)).To(Succeed())
	Expect(stored.Spec.CustomProvider.Path).To(Equal(tc.Expected.Path))
}
