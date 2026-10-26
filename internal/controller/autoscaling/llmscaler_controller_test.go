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
	. "github.com/onsi/ginkgo/v2"

	"github.com/modelsphere/llm-operator/internal/testutil/cases"
	"github.com/modelsphere/llm-operator/internal/testutil/cases/ginkgocases"
)

// The reconcile specs are one YAML file each under testdata/controller.
// The runner and the fixtures live in llmscaler_scenario_test.go.
// Helper tests that do not need envtest are plain testing funcs in
// llmscaler_helpers_test.go.
var _ = Describe("LLMScaler Controller", func() {
	Context("When reconciling a resource", func() {
		BeforeEach(func() {
			promFix = startPromFixture()
		})
		AfterEach(func() {
			promFix.stop()
			promFix = nil
		})

		DescribeTable("metric scaling", runScalerCase,
			ginkgocases.Entries("testdata/controller/metric-scaling", scalerDesc))
		DescribeTable("rollout guard (G8)", runScalerCase,
			ginkgocases.Entries("testdata/controller/rollout-guard", scalerDesc))
		DescribeTable("settle guard (G7)", runScalerCase,
			ginkgocases.Entries("testdata/controller/settle-guard", scalerDesc))
		DescribeTable("stabilization and max step", runScalerCase,
			ginkgocases.Entries("testdata/controller/stabilization-and-step", scalerDesc))
		DescribeTable("scaling limited", runScalerCase,
			ginkgocases.Entries("testdata/controller/scaling-limited", scalerDesc))
		DescribeTable("deferral timeout", runScalerCase,
			ginkgocases.Entries("testdata/controller/deferral-timeout", scalerDesc))
		DescribeTable("deferral status", runScalerCase,
			ginkgocases.Entries("testdata/controller/deferral-status", scalerDesc))
		DescribeTable("forced-write pacing", runScalerCase,
			ginkgocases.Entries("testdata/controller/forced-write-pacing", scalerDesc))
		DescribeTable("ready status", runScalerCase,
			ginkgocases.Entries("testdata/controller/ready-status", scalerDesc))

		for _, f := range cases.MustLoad[deferDefaultFile]("testdata/controller/crd-default") {
			tc := f.Case
			It(tc.Description, func() { runDeferDefault(tc) })
		}
	})

	// These run against the envtest apiserver so the CRD's own union rules are
	// exercised, not just the controller's reading of a well-formed spec.
	Context("When the metric provider is Custom", func() {
		BeforeEach(func() {
			customFix = startCustomFixture()
		})
		AfterEach(func() {
			customFix.stop()
			customFix = nil
		})

		DescribeTable("decision server", runCustomReconcileFile,
			ginkgocases.Entries("testdata/controller/custom-reconcile", customReconcileDesc))
		DescribeTable("admission", runCustomAdmissionFile,
			ginkgocases.Entries("testdata/controller/custom-admission", customAdmissionDesc))

		for _, f := range cases.MustLoad[customDefaultFile]("testdata/controller/custom-defaults") {
			tc := f.Case
			It(tc.Description, func() { runCustomDefault(tc) })
		}
	})
})

func scalerDesc(c scalerCase) string { return c.Description }

func customReconcileDesc(c customReconcileFile) string { return c.Description }

func customAdmissionDesc(c customAdmissionFile) string { return c.Description }
