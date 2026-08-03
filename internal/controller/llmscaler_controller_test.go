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
	"net/http"
	"net/http/httptest"

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

		BeforeEach(func() {
			mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{
					"status": "success",
					"data": {
						"result": [
							{
								"value": [ 1234567890, "0.85" ]
							}
						]
					}
				}`))
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
	})
})
