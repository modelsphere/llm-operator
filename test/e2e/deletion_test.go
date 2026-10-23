//go:build e2e

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

package e2e

import (
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/modelsphere/llm-operator/test/utils"
)

const (
	deletionNamespace = "llmop-deletion-e2e"
	routeFinalizer    = "routing.modelsphere.dev/cleanup"
	chartRepo         = "oci://example.com/charts"
)

// kubectlMu keeps utils.Run's process-wide chdir from racing the autoconfig poll.
var kubectlMu sync.Mutex

func runCmd(cmd *exec.Cmd) (string, error) {
	kubectlMu.Lock()
	defer kubectlMu.Unlock()
	return utils.Run(cmd)
}

func kubectl(args ...string) (string, error) {
	return runCmd(exec.Command("kubectl", args...))
}

type routeCleaner struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
}

func startRouteCleaner() *routeCleaner {
	c := &routeCleaner{stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			clearDeletingRouteFinalizers()
			select {
			case <-c.stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
	return c
}

func (c *routeCleaner) stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() { close(c.stopCh) })
	<-c.done
}

func clearDeletingRouteFinalizers() {
	out, err := kubectl("get", "modelroutes", "-n", deletionNamespace, "-o", "json")
	if err != nil {
		return
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string   `json:"name"`
				DeletionTimestamp *string  `json:"deletionTimestamp"`
				Finalizers        []string `json:"finalizers"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return
	}
	for _, item := range list.Items {
		if item.Metadata.DeletionTimestamp == nil || len(item.Metadata.Finalizers) == 0 {
			continue
		}
		kept := make([]string, 0, len(item.Metadata.Finalizers))
		for _, f := range item.Metadata.Finalizers {
			if f != routeFinalizer {
				kept = append(kept, f)
			}
		}
		if len(kept) == len(item.Metadata.Finalizers) {
			continue
		}
		body, err := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": kept}})
		if err != nil {
			continue
		}
		_, _ = kubectl("patch", "modelroute", item.Metadata.Name, "-n", deletionNamespace, "--type=merge", "-p", string(body))
	}
}

func registerDeletionTests() {
	var (
		release string
		cleaner *routeCleaner
	)

	Context("staged deletion", Ordered, func() {
		BeforeAll(func() {
			By("installing helm 4 when the CLI is missing")
			_, err := runCmd(exec.Command("make", "install-helm"))
			Expect(err).NotTo(HaveOccurred())

			By("creating the deletion namespace without a restricted policy")
			_, err = kubectl("get", "ns", deletionNamespace)
			if err != nil {
				_, err = kubectl("create", "ns", deletionNamespace)
				Expect(err).NotTo(HaveOccurred())
			}

			By("installing the ModelRoute CRD")
			_, err = kubectl("apply", "-f", "test/e2e/testdata/modelroute-crd.yaml")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				out, err := kubectl("get", "crd", "modelroutes.routing.modelsphere.dev",
					"-o", "jsonpath={.status.conditions[?(@.type=='Established')].status}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("True"))
			}, time.Minute, time.Second).Should(Succeed())
		})

		AfterAll(func() {
			_, _ = kubectl("delete", "ns", deletionNamespace, "--wait=false", "--ignore-not-found")
			_, _ = kubectl("delete", "-f", "test/e2e/testdata/modelroute-crd.yaml", "--ignore-not-found", "--wait=false")
		})

		AfterEach(func() {
			if cleaner != nil {
				cleaner.stop()
				cleaner = nil
			}
			if release == "" {
				return
			}
			_, _ = kubectl("patch", "modelroute", release, "-n", deletionNamespace, "--type=json",
				"-p", `[{"op":"remove","path":"/metadata/finalizers"}]`)
			_, _ = runCmd(exec.Command("helm", "uninstall", release, "-n", deletionNamespace))
			_, _ = kubectl("delete", "llmsvc", release, "-n", deletionNamespace, "--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "modelroute", release, "-n", deletionNamespace, "--ignore-not-found", "--wait=false")
			release = ""
		})

		It("removes the ModelRoute before the Deployment", func() {
			release = "del-route"
			cleaner = startRouteCleaner()
			installChart(release, "test/e2e/testdata/deletion-route-values.yaml")
			adoptDeletingService(release, map[string]string{
				"serving.modelsphere.dev/route-settle": "8s",
			})

			Eventually(func(g Gomega) {
				g.Expect(modelRouteExists(release)).To(BeFalse())
				g.Expect(deploymentExists(release)).To(BeTrue())
			}, 2*time.Minute, time.Second).Should(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(deploymentExists(release)).To(BeFalse())
				g.Expect(serviceExists(release)).To(BeFalse())
			}, 2*time.Minute, time.Second).Should(Succeed())
		})

		It("keeps the service in PodsTerminating through a 20s preStop", func() {
			release = "del-prestop"
			installChart(release, "test/e2e/testdata/deletion-prestop-values.yaml")
			waitPodRunning(release)
			adoptDeletingService(release, nil)

			Eventually(func(g Gomega) {
				g.Expect(servicePhase(release)).To(Equal("Deleting"))
				g.Expect(deletionStage(release)).To(Equal("PodsTerminating"))
			}, time.Minute, time.Second).Should(Succeed())

			Consistently(func(g Gomega) {
				g.Expect(serviceExists(release)).To(BeTrue())
				g.Expect(deletionStage(release)).To(Equal("PodsTerminating"))
			}, 12*time.Second, time.Second).Should(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(serviceExists(release)).To(BeFalse())
			}, 2*time.Minute, time.Second).Should(Succeed())
		})

		It("force-deletes a pod that ignores SIGTERM", func() {
			release = "del-force"
			installChart(release, "test/e2e/testdata/deletion-force-values.yaml")
			waitPodRunning(release)
			adoptDeletingService(release, map[string]string{
				"serving.modelsphere.dev/force-delete-after": "5s",
			})

			Eventually(func(g Gomega) {
				g.Expect(podNames(release)).To(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(eventsContain(release, "PodForceDeleted")).To(BeTrue())
			}, time.Minute, time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(serviceExists(release)).To(BeFalse())
			}, 2*time.Minute, time.Second).Should(Succeed())
		})

		It("reaches RouteStuck when the route finalizer is never cleared", func() {
			release = "del-stuck"
			installChart(release, "test/e2e/testdata/deletion-routestuck-values.yaml")
			adoptDeletingService(release, map[string]string{
				"serving.modelsphere.dev/deletion-max-wait": "15s",
			})

			seen := false
			Eventually(func(g Gomega) {
				outcome, exists := deletionOutcome(release)
				if exists && outcome == "RouteStuck" {
					seen = true
				}
				g.Expect(seen).To(BeTrue())
			}, 2*time.Minute, 200*time.Millisecond).Should(Succeed())

			Eventually(func(g Gomega) {
				g.Expect(serviceExists(release)).To(BeFalse())
			}, 2*time.Minute, time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(modelRouteFinalizers(release)).To(ContainElement(routeFinalizer))
			}, time.Minute, time.Second).Should(Succeed())
		})
	})
}

func installChart(name, values string) {
	By("installing " + name)
	_, err := runCmd(exec.Command("helm", "upgrade", "--install", name, "test/charts/sglang-mock",
		"-n", deletionNamespace, "-f", values))
	Expect(err).NotTo(HaveOccurred())
}

func adoptDeletingService(name string, annotations map[string]string) {
	By("adopting " + name + " and deleting it while suspended")
	meta := map[string]any{
		"name":       name,
		"namespace":  deletionNamespace,
		"finalizers": []string{"serving.modelsphere.dev/finalizer"},
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	obj := map[string]any{
		"apiVersion": "serving.modelsphere.dev/v1alpha1",
		"kind":       "LLMService",
		"metadata":   meta,
		"spec": map[string]any{
			"suspend": true,
			"chart": map[string]any{
				"name":    "sglang",
				"repo":    chartRepo,
				"version": "0.1.0",
			},
		},
	}
	raw, err := json.Marshal(obj)
	Expect(err).NotTo(HaveOccurred())
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(raw))
	_, err = runCmd(cmd)
	Expect(err).NotTo(HaveOccurred())

	status := map[string]any{
		"status": map[string]any{
			"chart": map[string]any{"name": "sglang", "version": "0.1.0"},
			"history": []map[string]any{{
				"revision":           1,
				"hash":               "sha256:e2e",
				"appliedAt":          "2026-01-02T03:04:05Z",
				"controllerRevision": "e2e",
			}},
		},
	}
	body, err := json.Marshal(status)
	Expect(err).NotTo(HaveOccurred())
	_, err = kubectl("patch", "llmsvc", name, "-n", deletionNamespace,
		"--subresource=status", "--type=merge", "-p", string(body))
	Expect(err).NotTo(HaveOccurred())

	_, err = kubectl("delete", "llmsvc", name, "-n", deletionNamespace, "--wait=false")
	Expect(err).NotTo(HaveOccurred())
	_, err = kubectl("patch", "llmsvc", name, "-n", deletionNamespace, "--type=merge", "-p", `{"spec":{"suspend":false}}`)
	Expect(err).NotTo(HaveOccurred())
}

func waitPodRunning(name string) {
	Eventually(func(g Gomega) {
		g.Expect(podPhase(name)).To(Equal("Running"))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

func podPhase(name string) string {
	out, err := kubectl("get", "pods", "-n", deletionNamespace, "-l", "app="+name+"-sglang",
		"-o", "jsonpath={.items[0].status.phase}")
	if err != nil {
		return ""
	}
	return out
}

func podNames(name string) string {
	out, err := kubectl("get", "pods", "-n", deletionNamespace, "-l", "app="+name+"-sglang",
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return "error"
	}
	return strings.TrimSpace(out)
}

func deploymentExists(name string) bool {
	_, err := kubectl("get", "deploy", name+"-sglang", "-n", deletionNamespace)
	return err == nil
}

func modelRouteExists(name string) bool {
	_, err := kubectl("get", "modelroute", name, "-n", deletionNamespace)
	return err == nil
}

func modelRouteFinalizers(name string) []string {
	out, err := kubectl("get", "modelroute", name, "-n", deletionNamespace, "-o", "json")
	if err != nil {
		return nil
	}
	var obj struct {
		Metadata struct {
			Finalizers []string `json:"finalizers"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(out), &obj) != nil {
		return nil
	}
	return obj.Metadata.Finalizers
}

func serviceExists(name string) bool {
	_, exists := serviceField(name, "{.metadata.name}")
	return exists
}

func servicePhase(name string) string {
	out, exists := serviceField(name, "{.status.phase}")
	if !exists {
		return ""
	}
	return out
}

func deletionStage(name string) string {
	out, exists := serviceField(name, "{.status.deletion.stage}")
	if !exists {
		return ""
	}
	return out
}

func deletionOutcome(name string) (string, bool) {
	return serviceField(name, "{.status.deletion.outcome}")
}

func serviceField(name, path string) (string, bool) {
	out, err := kubectl("get", "llmsvc", name, "-n", deletionNamespace, "-o", "jsonpath="+path)
	if err != nil {
		return "", false
	}
	return out, true
}

func eventsContain(name, reason string) bool {
	out, err := kubectl("get", "events", "-n", deletionNamespace,
		"--field-selector", "involvedObject.name="+name, "-o", "json")
	if err != nil {
		return false
	}
	return strings.Contains(out, reason)
}
