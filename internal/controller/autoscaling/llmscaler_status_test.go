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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/modelsphere/llm-operator/api/autoscaling/v1alpha1"
)

// TestTargetWriteFailedRecordsReady covers the path envtest cannot force: the
// target update returns an error. AbleToScale and Ready record
// TargetWriteFailed, and observedGeneration is recorded. The deferral and
// the replica counts already in status are left alone, and the original
// error is returned.
func TestTargetWriteFailedRecordsReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"result":[{"value":[0,"%s"]}]}}`, "0.85")
	}))
	defer srv.Close()

	deferred := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	scaler := prometheusScaler(srv.URL)
	scaler.Status = autoscalingv1alpha1.LLMScalerStatus{
		CurrentReplicas: 4,
		DesiredReplicas: 4,
		AppliedReplicas: 4,
		DeferredSince:   &deferred,
	}
	meta.SetStatusCondition(&scaler.Status.Conditions, metav1.Condition{
		Type:               autoscalingv1alpha1.ConditionAbleToScale,
		Status:             metav1.ConditionTrue,
		Reason:             autoscalingv1alpha1.ReasonRolloutInProgress,
		Message:            "seeded",
		ObservedGeneration: 2,
	})

	scheme := statusScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rolledDeployment(), scaler).
		WithStatusSubresource(&autoscalingv1alpha1.LLMScaler{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*unstructured.Unstructured); ok {
					return fmt.Errorf("injected write failure")
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()

	result, err := (&LLMScalerReconciler{Client: c, Scheme: scheme}).Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: scaler.Namespace, Name: scaler.Name},
	})
	if err == nil || !strings.Contains(err.Error(), "injected write failure") {
		t.Fatalf("Reconcile error = %v, want the injected write failure", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want 0", result.RequeueAfter)
	}

	got := &autoscalingv1alpha1.LLMScaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(scaler), got); err != nil {
		t.Fatal(err)
	}
	if got.Generation == 0 {
		t.Fatal("generation = 0, the fixture did not stick")
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want generation %d", got.Status.ObservedGeneration, got.Generation)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("Ready condition missing")
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != autoscalingv1alpha1.ReasonTargetWriteFailed {
		t.Errorf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, autoscalingv1alpha1.ReasonTargetWriteFailed)
	}
	if !strings.Contains(ready.Message, "injected write failure") {
		t.Errorf("Ready message = %q, want the injected error", ready.Message)
	}
	if ready.ObservedGeneration != got.Generation {
		t.Errorf("Ready.observedGeneration = %d, want %d", ready.ObservedGeneration, got.Generation)
	}
	if got.Status.LastScaleTime != nil {
		t.Errorf("lastScaleTime = %s, want unset", got.Status.LastScaleTime.Time)
	}
	if got.Status.CurrentReplicas != 4 || got.Status.DesiredReplicas != 4 || got.Status.AppliedReplicas != 4 {
		t.Errorf("replicas current/desired/applied = %d/%d/%d, want 4/4/4",
			got.Status.CurrentReplicas, got.Status.DesiredReplicas, got.Status.AppliedReplicas)
	}
	if got.Status.DeferredSince == nil || !got.Status.DeferredSince.Time.Equal(deferred.Time) {
		t.Errorf("deferredSince = %v, want %s", got.Status.DeferredSince, deferred.Time)
	}
	able := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionAbleToScale)
	if able == nil {
		t.Fatal("AbleToScale condition missing")
	}
	if able.Status != metav1.ConditionFalse || able.Reason != autoscalingv1alpha1.ReasonTargetWriteFailed {
		t.Errorf("AbleToScale = %s/%s, want False/%s", able.Status, able.Reason, autoscalingv1alpha1.ReasonTargetWriteFailed)
	}
	if !strings.Contains(able.Message, "injected write failure") {
		t.Errorf("AbleToScale message = %q, want the injected error", able.Message)
	}
	if able.ObservedGeneration != got.Generation {
		t.Errorf("AbleToScale.observedGeneration = %d, want %d", able.ObservedGeneration, got.Generation)
	}

	deploy := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "d"}, deploy); err != nil {
		t.Fatal(err)
	}
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
		t.Errorf("deployment replicas = %v, want 1", deploy.Spec.Replicas)
	}
}

// TestTargetUnreadableRecordsAbleToScale covers a target Get that fails for a
// reason other than NotFound. AbleToScale and Ready are False with
// TargetUnreadable. deferredSince and the replica counts stay as seeded.
func TestTargetUnreadableRecordsAbleToScale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"result":[{"value":[0,"%s"]}]}}`, "0.85")
	}))
	defer srv.Close()

	deferred := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	scaler := prometheusScaler(srv.URL)
	scaler.Status = autoscalingv1alpha1.LLMScalerStatus{
		CurrentReplicas: 4,
		DesiredReplicas: 4,
		AppliedReplicas: 4,
		DeferredSince:   &deferred,
	}

	scheme := statusScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rolledDeployment(), scaler).
		WithStatusSubresource(&autoscalingv1alpha1.LLMScaler{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*unstructured.Unstructured); ok {
					return fmt.Errorf("injected read failure")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	result, err := (&LLMScalerReconciler{Client: c, Scheme: scheme}).Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: scaler.Namespace, Name: scaler.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("RequeueAfter = %s, want 10s", result.RequeueAfter)
	}

	got := &autoscalingv1alpha1.LLMScaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(scaler), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want generation %d", got.Status.ObservedGeneration, got.Generation)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionReady)
	if ready == nil {
		t.Fatal("Ready condition missing")
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != autoscalingv1alpha1.ReasonTargetUnreadable {
		t.Errorf("Ready = %s/%s, want False/%s", ready.Status, ready.Reason, autoscalingv1alpha1.ReasonTargetUnreadable)
	}
	if !strings.Contains(ready.Message, "injected read failure") {
		t.Errorf("Ready message = %q, want the injected error", ready.Message)
	}
	able := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionAbleToScale)
	if able == nil {
		t.Fatal("AbleToScale condition missing")
	}
	if able.Status != metav1.ConditionFalse || able.Reason != autoscalingv1alpha1.ReasonTargetUnreadable {
		t.Errorf("AbleToScale = %s/%s, want False/%s", able.Status, able.Reason, autoscalingv1alpha1.ReasonTargetUnreadable)
	}
	if able.Message != ready.Message {
		t.Errorf("AbleToScale message = %q, want Ready's %q", able.Message, ready.Message)
	}
	if able.ObservedGeneration != got.Generation {
		t.Errorf("AbleToScale.observedGeneration = %d, want %d", able.ObservedGeneration, got.Generation)
	}
	if got.Status.CurrentReplicas != 4 || got.Status.DesiredReplicas != 4 || got.Status.AppliedReplicas != 4 {
		t.Errorf("replicas current/desired/applied = %d/%d/%d, want 4/4/4",
			got.Status.CurrentReplicas, got.Status.DesiredReplicas, got.Status.AppliedReplicas)
	}
	if got.Status.DeferredSince == nil || !got.Status.DeferredSince.Time.Equal(deferred.Time) {
		t.Errorf("deferredSince = %v, want %s", got.Status.DeferredSince, deferred.Time)
	}
	if got.Status.LastScaleTime != nil {
		t.Errorf("lastScaleTime = %s, want unset", got.Status.LastScaleTime.Time)
	}
}

// TestMissingCustomProviderRecordsInvalidSpec runs the admission-rejected
// pairing through the controller. The fleet stays where it is, and Ready
// records InvalidSpec without a DecisionAvailable condition.
func TestMissingCustomProviderRecordsInvalidSpec(t *testing.T) {
	scaler := prometheusScaler("http://127.0.0.1:9")
	scaler.Spec.MetricProvider = autoscalingv1alpha1.MetricProviderCustom
	scaler.Spec.CustomProvider = nil
	scaler.Spec.Metrics = nil

	scheme := statusScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rolledDeployment(), scaler).
		WithStatusSubresource(&autoscalingv1alpha1.LLMScaler{}).
		Build()

	result, err := (&LLMScalerReconciler{Client: c, Scheme: scheme}).Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: scaler.Namespace, Name: scaler.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("RequeueAfter = %s, want 15s", result.RequeueAfter)
	}

	got := &autoscalingv1alpha1.LLMScaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(scaler), got); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != autoscalingv1alpha1.ReasonInvalidSpec {
		t.Fatalf("Ready = %+v, want False/InvalidSpec", ready)
	}
	if ready.Message != "spec.customProvider is not set" {
		t.Errorf("Ready message = %q", ready.Message)
	}
	if meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionDecisionAvailable) != nil {
		t.Error("DecisionAvailable was set; a missing customProvider does not query")
	}
	able := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionAbleToScale)
	if able == nil {
		t.Fatal("AbleToScale condition missing")
	}
	if able.Status != metav1.ConditionTrue || able.Reason != autoscalingv1alpha1.ReasonSynced {
		t.Errorf("AbleToScale = %s/%s, want True/Synced", able.Status, able.Reason)
	}
	if able.Message != "replica count is already at the step target" {
		t.Errorf("AbleToScale message = %q", able.Message)
	}
	if able.ObservedGeneration != got.Generation {
		t.Errorf("AbleToScale.observedGeneration = %d, want %d", able.ObservedGeneration, got.Generation)
	}
	if got.Status.DesiredReplicas != 1 || got.Status.AppliedReplicas != 1 || got.Status.CurrentReplicas != 1 {
		t.Errorf("replicas desired/applied/current = %d/%d/%d, want 1/1/1",
			got.Status.DesiredReplicas, got.Status.AppliedReplicas, got.Status.CurrentReplicas)
	}
	if got.Status.LastScaleTime != nil {
		t.Errorf("lastScaleTime = %s, want unset", got.Status.LastScaleTime.Time)
	}
	if got.Generation == 0 || got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, generation = %d", got.Status.ObservedGeneration, got.Generation)
	}

	deploy := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "d"}, deploy); err != nil {
		t.Fatal(err)
	}
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
		t.Errorf("deployment replicas = %v, want 1", deploy.Spec.Replicas)
	}
}

func statusScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := autoscalingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func prometheusScaler(serverURL string) *autoscalingv1alpha1.LLMScaler {
	return &autoscalingv1alpha1.LLMScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns", Generation: 7},
		Spec: autoscalingv1alpha1.LLMScalerSpec{
			TargetRef: autoscalingv1alpha1.TargetRef{
				APIVersion: "apps/v1",
				Kind:       deploymentKind,
				Name:       "d",
			},
			ServerAddress: serverURL,
			MinReplicas:   1,
			MaxReplicas:   5,
			Metrics: []autoscalingv1alpha1.MetricSpec{{
				Query:  "avg(vllm:kv_cache_usage_perc)",
				Target: "0.5",
			}},
		},
	}
}

func rolledDeployment() *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: "d"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{appLabelKey: "d"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx"}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			ReadyReplicas:      1,
			AvailableReplicas:  1,
		},
	}
}
