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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "gitlab.4pd.io/inference-production-stack/llm-operator/api/v1alpha1"
)

var _ = Describe("LLMScaler Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			scalerName       = "test-scaler"
			scalerNamespace  = "default"
			targetDeployName = "test-deployment"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      scalerName,
			Namespace: scalerNamespace,
		}

		var mockServer *httptest.Server
		// mockValue is the metric value the fake Prometheus returns; tests set it
		// before reconciling.
		var mockValue string

		BeforeEach(func() {
			mockValue = "0.85"
			mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(fmt.Appendf(nil,
					`{"status":"success","data":{"result":[{"value":[0,"%s"]}]}}`, mockValue))
			}))

			By("creating a dummy Deployment to act as the target")
			var replicas int32 = 1
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      targetDeployName,
					Namespace: scalerNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "test"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "test"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "nginx",
									Image: "nginx",
								},
							},
						},
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

			By("creating the LLMScaler resource")
			llmscaler := &autoscalingv1alpha1.LLMScaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      scalerName,
					Namespace: scalerNamespace,
				},
				Spec: autoscalingv1alpha1.LLMScalerSpec{
					TargetRef: autoscalingv1alpha1.TargetRef{
						APIVersion: "apps/v1",
						Kind:       deploymentKind,
						Name:       targetDeployName,
					},
					ServerAddress: mockServer.URL,
					MinReplicas:   1,
					MaxReplicas:   5,
					Metrics: []autoscalingv1alpha1.MetricSpec{
						{
							Query:  `avg(vllm:kv_cache_usage_perc)`,
							Target: "0.5",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, llmscaler)).To(Succeed())
		})

		AfterEach(func() {
			if mockServer != nil {
				mockServer.Close()
			}

			By("Cleanup the LLMScaler")
			scaler := &autoscalingv1alpha1.LLMScaler{}
			_ = k8sClient.Get(ctx, typeNamespacedName, scaler)
			_ = k8sClient.Delete(ctx, scaler)

			By("Cleanup the Deployment")
			deploy := &appsv1.Deployment{}
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)
			_ = k8sClient.Delete(ctx, deploy)
		})

		It("should successfully scale the deployment based on metrics", func() {
			By("running the Reconciler")
			controllerReconciler := &LLMScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Call Reconcile once
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("checking if the Deployment replicas increased")
			deploy := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)
			Expect(err).NotTo(HaveOccurred())

			// We expect the Reconciler to scale from 1 to 2 because:
			// the mock Prometheus returns 0.85 for the query, target is 0.5,
			// ceil(1 * 0.85/0.5) = ceil(1.7) = 2
			Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
		})

		It("should defer scaling while the Deployment is rolling out", func() {
			By("marking the Deployment as mid-rollout")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			// Not all replicas are on the new template yet -> rollout in progress.
			deploy.Status.UpdatedReplicas = 0
			Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())

			By("running the Reconciler")
			controllerReconciler := &LLMScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("checking the Deployment was NOT scaled")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			// The metric would compute desired=2, but the rollout guard defers
			// scaling, so replicas stay at 1.
			Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))
		})

		It("should scale down to minReplicas when the metric is zero", func() {
			By("setting the Deployment to 5 replicas and the metric to 0 (idle)")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			var five int32 = 5
			deploy.Spec.Replicas = &five
			Expect(k8sClient.Update(ctx, deploy)).To(Succeed())
			// Fully rolled out at 5 so the rollout guard doesn't defer.
			deploy.Status.ObservedGeneration = deploy.Generation
			deploy.Status.Replicas = five
			deploy.Status.UpdatedReplicas = five
			deploy.Status.ReadyReplicas = five
			deploy.Status.AvailableReplicas = five
			Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())

			mockValue = "0" // empty queue

			By("running the Reconciler")
			controllerReconciler := &LLMScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("checking it scaled down to minReplicas")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			// desired = ceil(5 * 0/5) = 0, clamped up to minReplicas (1).
			Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))
		})

		// unsettledAtFive puts the Deployment at 5 replicas, fully rolled out (so the
		// rollout guard doesn't fire and mask the settle guard), with the given
		// readyReplicas and terminatingReplicas, and an idle metric.
		unsettledAtFive := func(ready int32, terminating *int32) *appsv1.Deployment {
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			var five int32 = 5
			deploy.Spec.Replicas = &five
			Expect(k8sClient.Update(ctx, deploy)).To(Succeed())
			deploy.Status.ObservedGeneration = deploy.Generation
			deploy.Status.Replicas = five
			deploy.Status.UpdatedReplicas = five
			deploy.Status.ReadyReplicas = ready
			deploy.Status.TerminatingReplicas = terminating
			Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())

			mockValue = "0" // empty queue -> wants minReplicas
			return deploy
		}

		replicasNow := func() int32 {
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			return *deploy.Spec.Replicas
		}

		It("should defer a scale-down until readyReplicas is back at specReplicas", func() {
			By("setting the Deployment to 5 replicas with only 3 ready")
			unsettledAtFive(3, nil)

			controllerReconciler := &LLMScalerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking the scale-down was deferred")
			Expect(replicasNow()).To(Equal(int32(5)))

			By("letting the fleet converge")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			deploy.Status.ReadyReplicas = 5
			Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking the scale-down now proceeds")
			Expect(replicasNow()).To(Equal(int32(1)))
		})

		It("should ignore still-draining pods while checkTerminatingReplicas is off", func() {
			// The fallback path. readyReplicas excludes terminating pods, so the
			// fleet reads as settled the moment the previous step's pods are marked
			// for deletion — the descent is then paced by syncPeriodSeconds, not by
			// the drain.
			restore := checkTerminatingReplicas
			checkTerminatingReplicas = false
			DeferCleanup(func() { checkTerminatingReplicas = restore })

			By("setting 5 ready with 2 still draining")
			two := int32(2)
			unsettledAtFive(5, &two)

			controllerReconciler := &LLMScalerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking the scale-down was not held back by the drain")
			Expect(replicasNow()).To(Equal(int32(1)))
		})

		It("should also wait for the drain when checkTerminatingReplicas is on", func() {
			restore := checkTerminatingReplicas
			checkTerminatingReplicas = true
			DeferCleanup(func() { checkTerminatingReplicas = restore })

			By("setting 5 ready with 2 still draining")
			two := int32(2)
			unsettledAtFive(5, &two)

			controllerReconciler := &LLMScalerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking it is deferred; readyReplicas alone would have said settled")
			Expect(replicasNow()).To(Equal(int32(5)))

			By("letting the drain finish")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			var zero int32 = 0
			deploy.Status.TerminatingReplicas = &zero
			Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking the scale-down now proceeds")
			Expect(replicasNow()).To(Equal(int32(1)))
		})

		It("should not gate scale-up on the previous scale-down settling", func() {
			By("setting 3 replicas with 2 ready and 1 still terminating, under load")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			var three int32 = 3
			deploy.Spec.Replicas = &three
			Expect(k8sClient.Update(ctx, deploy)).To(Succeed())
			deploy.Status.ObservedGeneration = deploy.Generation
			deploy.Status.Replicas = three
			deploy.Status.UpdatedReplicas = three
			deploy.Status.ReadyReplicas = 2
			terminating := int32(1)
			deploy.Status.TerminatingReplicas = &terminating
			Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())

			mockValue = "0.85" // over the 0.5 target -> wants more replicas

			controllerReconciler := &LLMScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking capacity was added despite the unsettled fleet")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			// ceil(2 * 0.85/0.5) = 4; the settle guard only gates scale-down.
			Expect(*deploy.Spec.Replicas).To(Equal(int32(4)))
		})

		It("should hold for the stabilization window between scale-down steps", func() {
			By("enabling a 300s window and a 2-replica step cap")
			scaler := &autoscalingv1alpha1.LLMScaler{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, scaler)).To(Succeed())
			scaler.Spec.ScaleDown.StabilizationWindowSeconds = 300
			scaler.Spec.ScaleDown.MaxStepReplicas = 2
			Expect(k8sClient.Update(ctx, scaler)).To(Succeed())

			By("starting settled at 5 with an idle metric")
			unsettledAtFive(5, nil)

			controllerReconciler := &LLMScalerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking the first step landed")
			Expect(replicasNow()).To(Equal(int32(3)))

			By("settling the fleet at 3 so neither the rollout nor the settle guard fires")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			deploy.Status.ObservedGeneration = deploy.Generation
			deploy.Status.Replicas = 3
			deploy.Status.UpdatedReplicas = 3
			deploy.Status.ReadyReplicas = 3
			Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("checking the next step is held; without the pin it would drop to 1")
			Expect(replicasNow()).To(Equal(int32(3)))
		})

		It("should walk down one step at a time when maxStepReplicas is set", func() {
			By("capping the scale-down step at 2 replicas")
			scaler := &autoscalingv1alpha1.LLMScaler{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, scaler)).To(Succeed())
			scaler.Spec.ScaleDown.MaxStepReplicas = 2
			Expect(k8sClient.Update(ctx, scaler)).To(Succeed())

			setReplicas := func(n int32) {
				deploy := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
				deploy.Spec.Replicas = &n
				Expect(k8sClient.Update(ctx, deploy)).To(Succeed())
				// Fully rolled out at n so the rollout guard doesn't defer.
				deploy.Status.ObservedGeneration = deploy.Generation
				deploy.Status.Replicas = n
				deploy.Status.UpdatedReplicas = n
				deploy.Status.ReadyReplicas = n
				deploy.Status.AvailableReplicas = n
				Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())
			}

			By("setting the Deployment to 5 replicas and the metric to 0 (idle)")
			setReplicas(5)
			mockValue = "0" // empty queue

			controllerReconciler := &LLMScalerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("running the Reconciler; raw recommendation is minReplicas but the step is capped")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			// desired = 1 (clamped to minReplicas), capped to 5 - 2 = 3.
			Expect(*deploy.Spec.Replicas).To(Equal(int32(3)))

			By("checking status reports the uncapped recommendation, not the step")
			Expect(k8sClient.Get(ctx, typeNamespacedName, scaler)).To(Succeed())
			// The cap rate-limits the write; the recommendation is still 1.
			Expect(scaler.Status.DesiredReplicas).To(Equal(int32(1)))

			By("running the Reconciler again; it takes the next step down")
			setReplicas(3) // simulate the Deployment controller settling at 3
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: targetDeployName, Namespace: scalerNamespace}, deploy)).To(Succeed())
			// 3 - 2 = 1, which is minReplicas, so it lands there rather than below.
			Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))
		})
	})
})

// TestStabilizeDesired unit-tests the scale-down stabilization helper directly
// (deterministic, no envtest needed).
func TestStabilizeDesired(t *testing.T) {
	r := &LLMScalerReconciler{}
	key := types.NamespacedName{Namespace: "ns", Name: "s"}
	t0 := time.Now()
	window := 60 * time.Second

	if got := r.stabilizeDesired(key, 5, window, t0); got != 5 {
		t.Fatalf("first recommendation: got %d, want 5", got)
	}
	// Metric dipped to 1, but the peak of 5 is still within the window -> hold.
	if got := r.stabilizeDesired(key, 1, window, t0.Add(10*time.Second)); got != 5 {
		t.Fatalf("within window: got %d, want 5 (held at peak)", got)
	}
	// After the window the peak has expired -> the low recommendation wins.
	if got := r.stabilizeDesired(key, 1, window, t0.Add(61*time.Second)); got != 1 {
		t.Fatalf("after window: got %d, want 1", got)
	}
	// Scale-up is immediate regardless of history.
	if got := r.stabilizeDesired(key, 9, window, t0.Add(61*time.Second)); got != 9 {
		t.Fatalf("scale-up: got %d, want 9", got)
	}
}

// TestHoldAfterScaleDown checks that pinning the written count after a scale-down
// paces the next one a full stabilization window later, rather than letting the
// whole descent resolve off one collapsed reading.
func TestHoldAfterScaleDown(t *testing.T) {
	r := &LLMScalerReconciler{}
	key := types.NamespacedName{Namespace: "ns", Name: "s"}
	t0 := time.Now()
	window := 300 * time.Second

	// Fleet of 8, metric collapses; the peak is still in the window, so it holds.
	if got := r.stabilizeDesired(key, 8, window, t0); got != 8 {
		t.Fatalf("peak: got %d, want 8", got)
	}
	// A window later the peak has expired and the low recommendation wins: the
	// controller writes 6 (an 8 -> 1 recommendation capped to a 2-replica step)
	// and pins it.
	if got := r.stabilizeDesired(key, 1, window, t0.Add(301*time.Second)); got != 1 {
		t.Fatalf("first step: got %d, want 1", got)
	}
	r.holdAfterScaleDown(key, 6, window, t0.Add(301*time.Second))

	// Within the window the fleet is held at 6 even though the metric still reads
	// idle. Before the pin, these evaluations would each have shrunk it again.
	for _, dt := range []time.Duration{311, 400, 600} {
		if got := r.stabilizeDesired(key, 1, window, t0.Add(dt*time.Second)); got != 6 {
			t.Errorf("held at +%s: got %d, want 6", dt*time.Second, got)
		}
	}
	// Once the pin ages out, the next step is allowed.
	if got := r.stabilizeDesired(key, 1, window, t0.Add(602*time.Second)); got != 1 {
		t.Fatalf("after window: got %d, want 1", got)
	}

	// Scale-up is never blocked by the pin.
	r.holdAfterScaleDown(key, 6, window, t0.Add(700*time.Second))
	if got := r.stabilizeDesired(key, 9, window, t0.Add(710*time.Second)); got != 9 {
		t.Fatalf("scale-up during hold: got %d, want 9", got)
	}

	// With stabilization off there is nothing to pace with, so the pin is a no-op.
	off := &LLMScalerReconciler{}
	off.holdAfterScaleDown(key, 6, 0, t0)
	if got := off.stabilizeDesired(key, 1, 0, t0); got != 1 {
		t.Fatalf("window disabled: got %d, want 1", got)
	}
}

// TestParseTargetValue covers the target parser, which is the denominator of the
// scaling ratio and so must never yield zero, a negative, or a non-finite value.
func TestParseTargetValue(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    float64
		wantErr bool
	}{
		{"plain integer", "5", 5, false},
		{"ratio", "0.8", 0.8, false},
		{"surrounding whitespace", "  2.5 ", 2.5, false},
		{"percent suffix is rejected, not stripped", "80%", 0, true},
		{"zero would make the ratio infinite", "0", 0, true},
		{"negative would invert scaling", "-1", 0, true},
		{"NaN parses as a float but is not usable", "NaN", 0, true},
		{"Inf parses as a float but is not usable", "+Inf", 0, true},
		{"not a number", "abc", 0, true},
		{"empty", "", 0, true},
	}
	for _, tt := range tests {
		got, err := parseTargetValue(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: parseTargetValue(%q) error = %v, wantErr %v", tt.name, tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("%s: parseTargetValue(%q) = %v, want %v", tt.name, tt.in, got, tt.want)
		}
	}
}

// TestQueryPrometheusScalar checks how the result vector is resolved: exactly one
// series is a value, and anything else is an error the caller skips the metric
// on, rather than a silently-chosen sample or an implied zero.
func TestQueryPrometheusScalar(t *testing.T) {
	tests := []struct {
		name    string
		result  string
		want    float64
		wantErr string
	}{
		{
			name:   "single series resolves",
			result: `[{"metric":{},"value":[0,"1.5"]}]`,
			want:   1.5,
		},
		{
			name:    "empty vector is not zero",
			result:  `[]`,
			wantErr: "returned no data",
		},
		{
			name: "an under-aggregated query is rejected, naming the series",
			result: `[{"metric":{"__name__":"m","model":"qwen","backend":"10.0.0.1:8000"},"value":[0,"1"]},
			          {"metric":{"__name__":"m","model":"qwen","backend":"10.0.0.2:8000"},"value":[0,"9"]}]`,
			wantErr: `returned 2 series`,
		},
	}
	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(fmt.Appendf(nil, `{"status":"success","data":{"result":%s}}`, tt.result))
		}))

		got, err := queryPrometheusScalar(srv.URL, "some_query", nil)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", tt.name, err)
		case tt.wantErr == "" && got != tt.want:
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		case tt.wantErr != "" && err == nil:
			t.Errorf("%s: expected error containing %q, got value %v", tt.name, tt.wantErr, got)
		case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
			t.Errorf("%s: error = %q, want it to contain %q", tt.name, err, tt.wantErr)
		}
		srv.Close()
	}

	// The multi-series error has to point at the label to aggregate away, so it
	// quotes both series with their labels sorted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[
			{"metric":{"model":"qwen","backend":"10.0.0.1:8000"},"value":[0,"1"]},
			{"metric":{"model":"qwen","backend":"10.0.0.2:8000"},"value":[0,"9"]}]}}`))
	}))
	defer srv.Close()
	_, err := queryPrometheusScalar(srv.URL, "some_query", nil)
	if err == nil {
		t.Fatal("expected a multi-series error")
	}
	for _, want := range []string{`{backend="10.0.0.1:8000",model="qwen"}`, `{backend="10.0.0.2:8000",model="qwen"}`, "avg(...)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("multi-series error = %q, want it to contain %q", err, want)
		}
	}
}

// TestComputeDesiredFromMetricsNonFinite pins the rule that a NaN or +Inf sample
// is a failed evaluation, not a zero. Without it haveMetric goes true while
// maxDesired stays 0, which reads as "scale to minReplicas" — the failure mode a
// latency guardrail hits routinely, since histogram_quantile over a histogram
// with no observations in the window returns NaN.
func TestComputeDesiredFromMetricsNonFinite(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "-Inf"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(fmt.Appendf(nil,
				`{"status":"success","data":{"result":[{"metric":{},"value":[0,"%s"]}]}}`, value))
		}))

		scaler := &autoscalingv1alpha1.LLMScaler{
			Spec: autoscalingv1alpha1.LLMScalerSpec{
				ServerAddress: srv.URL,
				Metrics: []autoscalingv1alpha1.MetricSpec{
					{Name: "guardrail", Query: "histogram_quantile(0.95, x)", Target: "2"},
				},
			},
		}
		r := &LLMScalerReconciler{}
		desired, haveMetric := r.computeDesiredFromMetrics(context.Background(), scaler, 4)
		if haveMetric {
			t.Errorf("value %s: haveMetric = true, want false (a non-finite sample is not a measurement)", value)
		}
		if desired != 0 {
			t.Errorf("value %s: desired = %d, want 0", value, desired)
		}
		srv.Close()
	}
}

// TestCapScaleDownStep unit-tests the scale-down rate limiter.
func TestCapScaleDownStep(t *testing.T) {
	tests := []struct {
		name                      string
		current, desired, maxStep int32
		want                      int32
	}{
		{"unlimited when maxStep is 0", 10, 1, 0, 1},
		{"caps a burst scale-down", 10, 1, 2, 8},
		{"passes through a step within the cap", 10, 9, 2, 9},
		{"passes through a step exactly at the cap", 10, 8, 2, 8},
		{"leaves scale-up untouched", 2, 10, 1, 10},
		{"leaves a no-op untouched", 5, 5, 1, 5},
	}
	for _, tt := range tests {
		if got := capScaleDownStep(tt.current, tt.desired, tt.maxStep); got != tt.want {
			t.Errorf("%s: capScaleDownStep(%d, %d, %d) = %d, want %d", tt.name, tt.current, tt.desired, tt.maxStep, got, tt.want)
		}
	}
}
