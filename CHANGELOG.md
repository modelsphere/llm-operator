# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). The version is the
chart's `appVersion` and the image tag; release tags carry a `v` prefix
(`v0.4.0`).

## [Unreleased]

### Added
- Scale-to-zero: `minReplicas` may be `0`, and a fleet at zero wakes up when the
  metric turns positive (#2).
- The image and the Helm chart are published to GHCR
  (`ghcr.io/modelsphere/llm-operator`, `oci://ghcr.io/modelsphere/charts/llmscaleoperator`)
  at one version; every push to `main` publishes `<appVersion>-git<sha7>` (#4).
- A CI gate for the license text and committed credentials on every pull request.
- Dependabot for Go modules and GitHub Actions.
- `NOTICE`.

### Changed
- The default image in the Makefile, the chart, the kustomize config and
  `dist/install.yaml` is the published `docker.io/4pdosc/llm-operator`
  instead of an internal registry.
- The Dockerfile leaves `GOPROXY` and `GOSUMDB` at Go's defaults (the checksum
  database was turned off before); both stay build args.
- GitHub Actions in `release.yml` pinned to commit SHAs.

### Removed
- The internal GitLab pipeline (`.gitlab-ci.yml`).

## [0.4.0] - 2026-09-25

### Changed
- **Breaking:** the API is published under `modelsphere.dev`: `LLMScaler` is now
  `autoscaling.modelsphere.dev/v1alpha1`. Objects created under the previous
  group are not picked up. The decision document the `Custom` provider accepts
  keeps its schema version, `llmscaling.inference.x-k8s.io/v1alpha1`. Chart
  version 0.3.0.

## [0.3.2] - 2026-09-22

First tagged release.

### Added
- `LLMScaler` CRD: scales a `Deployment`, `StatefulSet` or `LeaderWorkerSet` on
  LLM signals instead of CPU and memory.
- `Prometheus` metric provider: per-replica PromQL targets, HPA-style math on
  ready replicas; a metric that cannot be evaluated is skipped, not read as zero.
- `Custom` metric provider: reads an absolute replica count from a decision
  server, still clamped to `minReplicas`/`maxReplicas`.
- Scale-down damping: a stabilization window and a per-step cap.
- Cache-aware scale-down: marks the coldest pods with
  `controller.kubernetes.io/pod-deletion-cost`, from `deletionCostQuery` or
  newest-pod-first.
- Rollout guard: scaling waits while a `Deployment` target is mid-rollout.
- Helm chart (`dist/chart`) and `dist/install.yaml`; test charts for vLLM and
  SGLang with a graceful-drain `preStop` hook and a metrics mock.
- Release workflow: a version tag builds the image for linux/amd64 and
  linux/arm64 and pushes it to Docker Hub; the tag must match the chart's
  `appVersion`.
- Apache-2.0 `LICENSE`; the Dockerfile's base images default to public images.

[Unreleased]: https://github.com/modelsphere/llm-operator/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/modelsphere/llm-operator/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/modelsphere/llm-operator/releases/tag/v0.3.2
