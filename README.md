# llmscaleoperator

A Kubernetes operator that autoscales LLM inference workloads (e.g. vLLM) on
LLM-specific signals — KV-cache utilization, queue depth, TPM/capacity load —
rather than CPU/memory. Think of it as an HPA specialized for token-serving.

![arch](./architecture.svg)

## Description

The operator introduces a single CRD, **`LLMScaler`** (`autoscaling.4pd.io`), that points at a scalable workload and drives its replica count from a metrics source.

- **Target** (`spec.targetRef`): any `Deployment`, `StatefulSet`, or `LeaderWorkerSet` (handled generically via the unstructured client).
- **Metrics** (`spec.metrics`): each entry is a **PromQL query** plus a per-replica `target`, evaluated against `spec.serverAddress`. The query **must** return a single aggregated value (e.g. wrap it in `avg(...)`), with label filters baked in — a result vector of more than one series is rejected rather than resolved to an arbitrary sample, so aggregate away every label the source varies on (`backend`, `model`, `route`, `pod`). Instant (`avg(...)`) and range (`avg(avg_over_time(...[1m]))`) queries are both valid — nothing enforces one, it's a trade-off. As a rule of thumb, a short `avg_over_time([1m])` survives a missed scrape and filters single-sample spikes while keeping scale-up responsive, provided you keep the window short (~1m for bursty queue depth, ~2m for slower-moving KV-cache); down-conservatism is better handled by `scaleDown.stabilizationWindowSeconds` than by a long metric window. **Scope the query to this deployment's pods**, not just the model — otherwise multiple deployments serving the same model get averaged together. Filter on `namespace` plus a per-deployment label such as `app` (the chart's ServiceMonitor exposes `app` via `podTargetLabels`; `namespace` is always present, and matters because release names can repeat across namespaces). Example: `query: avg(vllm:kv_cache_usage_perc{namespace="default", app="opt-125m-vllm"})`, `target: "0.8"`. The same applies to `scaleDown.deletionCostQuery`.
- **Headers** (`spec.serverHeaders`): arbitrary headers sent with every metric-fetch request (e.g. `Authorization` for a secured Prometheus).

### CRD specification

| | |
| --- | --- |
| Group / version | `autoscaling.4pd.io/v1alpha1` |
| Kind | `LLMScaler` (list `LLMScalerList`) |
| Resource | `llmscalers` (singular `llmscaler`) |
| Scope | Namespaced |
| Subresources | `status` |

The target is looked up **in the `LLMScaler`'s own namespace** — `targetRef` has no `namespace` field, so a scaler cannot drive a workload in another namespace.

#### `spec`

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `targetRef` | object | ✔ | — | Workload to scale. See below. |
| `serverAddress` | string | ✔ | — | Prometheus query endpoint, e.g. `http://prometheus-operated.monitoring.svc:9090`. Queries are issued to `{serverAddress}/api/v1/query`. |
| `serverHeaders` | map[string]string | | — | Extra HTTP headers sent with every metric fetch (e.g. `Authorization`). Values are used verbatim. |
| `minReplicas` | int32 | ✔ | — | Lower bound. Minimum `1`. |
| `maxReplicas` | int32 | ✔ | — | Upper bound. Minimum `1`. |
| `metrics` | []object | ✔ | — | Metrics driving the replica count; the **largest** recommendation across entries wins. See below. |
| `syncPeriodSeconds` | int32 | | `15` | Interval between metric evaluations (the `RequeueAfter` that paces each scale step). |
| `retryPeriodSeconds` | int32 | | `10` | Requeue interval used instead of `syncPeriodSeconds` when the target is missing or a sync errors. |
| `scaleDown` | object | | see below | Scale-down damping and cache-aware teardown. |
| `preemption` | object | | — | `enable` (bool), `priorityClass` (string). Accepted by the API but **not implemented** by the controller — setting it does nothing today. |

#### `spec.targetRef`

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `apiVersion` | string | ✔ | e.g. `apps/v1`, `leaderworkerset.x-k8s.io/v1`. |
| `kind` | string | ✔ | `Deployment`, `StatefulSet`, or `LeaderWorkerSet`. Read and written generically through the unstructured client, so any kind exposing `spec.replicas` plus `status.readyReplicas` works; the rollout guard and cache-aware victim selection are `Deployment`-only. |
| `name` | string | ✔ | Name of the target in the `LLMScaler`'s namespace. |

#### `spec.metrics[]`

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | | Identifier for this metric in logs and events. |
| `query` | string | ✔ | PromQL returning a **single** aggregated, per-replica value — more than one series is an error, not a sample to pick from. See the `spec.metrics` notes above for scoping guidance. |
| `target` | string | ✔ | Desired per-replica value, as a quoted number on the same scale as the query (`"0.8"` against a 0–1 ratio, `"80"` against a 0–100 series). Must be finite and positive; a `%` suffix is rejected rather than guessed at. |

#### `spec.scaleDown`

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `stabilizationWindowSeconds` | int32 | `0` | Hold replicas at the highest recommendation seen within this window. `0` disables it. Scale-up is unaffected. |
| `maxStepReplicas` | int32 | `0` | Maximum replicas removed per scale-down step; `0` means unlimited. Minimum `0`. Bounds the size of a step, not the interval between steps. |
| `behavior` | string | `CacheAware` | `CacheAware` biases deletion toward the coldest pods via `controller.kubernetes.io/pod-deletion-cost`; `None` leaves deletion order to the workload controller. |
| `deletionCostQuery` | string | — | PromQL returning one series per pod, each carrying a `pod` label; the sample value becomes that pod's deletion cost and the lowest is deleted first. Empty falls back to newest-pod-first. Only consulted when `behavior: CacheAware`. |

#### `status`

| Field | Type | Description |
| --- | --- | --- |
| `currentReplicas` | int32 | The target's **ready** replicas at the last sync — the figure the scaling ratio is computed from, not `spec.replicas`. |
| `desiredReplicas` | int32 | Last recommendation after clamping to min/max and scale-down stabilization, but **before** the `maxStepReplicas` cap. While a step cap is walking the fleet down it therefore reports the destination, not the replica count just written. |
| `conditions` | []metav1.Condition | Declared in the API; the controller does not populate it yet. |

`kubectl get llmscalers` prints `MinReplicas`, `MaxReplicas`, `CurrentReplicas`, `DesiredReplicas`.

#### Example

```yaml
apiVersion: autoscaling.4pd.io/v1alpha1
kind: LLMScaler
metadata:
  name: llmscaler-sample
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: vllm-opt-125m
  serverAddress: "http://prometheus-operated.monitoring.svc:9090"
  minReplicas: 1
  maxReplicas: 5
  syncPeriodSeconds: 15
  retryPeriodSeconds: 10
  metrics:
    # Scope the query to THIS deployment's pods (not just the model) so multiple
    # deployments serving the same model don't get averaged together.
    - name: kv-cache
      query: 'avg(avg_over_time(vllm:kv_cache_usage_perc{namespace="default", app="vllm-opt-125m"}[2m]))'
      target: "0.8"
  scaleDown:
    stabilizationWindowSeconds: 180
    maxStepReplicas: 1
    behavior: CacheAware
    deletionCostQuery: 'vllm:kv_cache_usage_perc{namespace="default", app="vllm-opt-125m"} * 100'
```

Kept in sync at `config/samples/autoscaling_v1alpha1_llmscaler.yaml`; the generated schema is `config/crd/bases/autoscaling.4pd.io_llmscalers.yaml`.

### Scaling algorithm

Standard HPA math, evaluated every `spec.syncPeriodSeconds`:

```
desired = ceil(readyReplicas × currentValue / targetValue)   # max across metrics
desired = clamp(desired, minReplicas, maxReplicas)
```

- Computed off the target's **ready** replicas (actual serving capacity), so it does not compound on pods that were just requested but haven't started.
- **A metric that cannot be measured is skipped, not read as zero.** A query that errors, returns an empty vector, returns more than one series, or returns `NaN`/`Inf` contributes nothing to that sync; when *every* metric is skipped the replica count is held where it is. This matters when mixing a metric that drives scale-up with a guardrail: `histogram_quantile` over a histogram with no observations in the window returns `NaN`, and were that read as 0 a guardrail could recommend `minReplicas` on its own whenever the primary metric happened to fail. For the same reason, do not append `or vector(0)` to a guardrail — reserve it for the metric that drives scale-up, where a genuine zero should scale in.
- The controller watches the `LLMScaler` with `GenerationChangedPredicate`, so its own status writes don't re-trigger reconciliation. Periodic evaluation is driven solely by `RequeueAfter(syncPeriod)`, which paces each scale step.
- **Rollout guard**: while a Deployment target is mid-rollout (spec not yet observed, or not all replicas on the new template), scaling is deferred. New pods start with cold KV caches that both distort the metric average and would be mis-picked as scale-down victims, so the controller waits for the rollout to settle before acting.
- **Scale Down**
    - **Scale-down stabilization** (`spec.scaleDown.stabilizationWindowSeconds`, 0 = off): replicas are held at the highest recommendation seen within the window, so a brief metric dip doesn't shrink the fleet.
    - **Scale-down step cap** (`spec.scaleDown.maxStepReplicas`, 0 = unlimited): at most this many replicas are removed per scale-down action, so a collapsed metric walks the fleet down (`10 → 8 → 6 → …` at `maxStepReplicas: 2`) instead of bursting from N straight to `minReplicas`. This is the knob for "don't throw away all the warm caches at once". Note it bounds the *size* of a step, not the interval between steps — that is the settle guard's job (see below).
- **Scale-up is immediate** — no scale-up stabilization window, so capacity is added as soon as the metric warrants it (the fast-up / slow-down shape). It stays bounded because `desired` is computed off *ready* replicas (no compounding) and clamped to `maxReplicas`. A scale-up rate cap for slow-starting pods is intentionally not implemented — vLLM pods are heavy, so a `1 → maxReplicas` jump can start several at once and briefly overshoot before they serve; add a step cap only if that overshoot actually hurts you.

### Cache-aware scale-down

Deleting a pod with a hot KV cache throws away a warm cache someone paid for. `spec.scaleDown.behavior`, default `CacheAware`; `None` to turn off. Two steps:

1. **Preferred victim selection** — choose the coldest pod to delete.
2. **Graceful drain** — let that pod finish what it is doing before it goes, and force it to stop if it takes too long.

Deleting a pod starts two things at the same time, and the pod only controls the one on the right:

```mermaid
flowchart TD
    M["metric below target for<br/>stabilizationWindowSeconds"] --> V["① mark coldest pod<br/>pod-deletion-cost = low"]
    V --> RS["ReplicaSet: spec.replicas -= 1"]
    RS --> D(["Pod deleted · deletionTimestamp set"])

    subgraph NET ["taking the pod out of rotation<br/>(the pod cannot see any of this)"]
        direction TB
        C1["EndpointSlice entry marked<br/>ready=false, terminating=true"]
        C2["everything watching EndpointSlice reacts:<br/>node service proxies, ingress,<br/>gateways, custom routers"]
        C3["new requests stop arriving<br/>(connections already open keep working)"]
        C1 --> C2 --> C3
    end

    subgraph POD ["② shutting the pod down<br/>(all of this must fit in terminationGracePeriodSeconds)"]
        direction TB
        K1["preStop: wait endpointSyncSeconds<br/>long enough for the left side to finish"]
        K2["preStop: wait until vLLM is idle<br/>at most drainSeconds"]
        K3["SIGTERM: vLLM finishes what is left<br/>at most --shutdown-timeout"]
        K4(["container exits"])
        K1 --> K2 --> K3 --> K4
    end

    D --> C1
    D --> K1
    POD -.->|"if that time runs out first"| KILL(["SIGKILL: pod dies,<br/>requests still running are lost"])
```

Neither side waits for the other. The kubelet starts `preStop` without knowing whether the pod has been taken out of the load balancer yet, and the pod has no way to ask — so it just waits `endpointSyncSeconds` first and lets the left side catch up. That is the one number you may need to tune.

#### 1. Preferred victim selection

Before lowering `spec.replicas`, the controller sets a low `controller.kubernetes.io/pod-deletion-cost` on the pods being removed, so the ReplicaSet deletes those first.

- Set **only when scaling down**, and **only on the pods being removed** — never on every metric check, because Kubernetes warns that frequent updates to this annotation overload the apiserver.
- It is a **hint, not a guarantee**: unready pods are still deleted first. It also works on **Deployment/ReplicaSet only** — StatefulSet and LWS delete by ordinal and ignore it.
- It only edits an annotation on the running pod, so it does **not** restart anything. (Editing the Deployment's pod *template* would start a rollout; this does not.)
- Two ways to decide which pod is coldest:
  - **Ask Prometheus** — `spec.scaleDown.deletionCostQuery` is a query returning one result per pod (with a `pod` label), run against `spec.serverAddress`. Each pod's value becomes its cost, and the lowest goes first. E.g. `vllm:kv_cache_usage_perc{namespace="default"} * 100` keeps the fuller caches.
  - **Guess** — with no query set, the newest pod is assumed to have the coldest cache.

#### 2. Graceful drain

A pod that is being deleted keeps serving until it is out of the load balancer and its remaining requests are done. `test/charts/vllm-mock` sets up the three steps on the right-hand side of the diagram:

- **Wait `endpointSyncSeconds`** — always waits the full time, even on an idle pod, because there is nothing to check. Set it to however long whatever routes traffic to this pod needs to notice it is going away: Cilium suggests ~1s for a ClusterIP Service, ~10s if an external load balancer is in front.
- **Wait for vLLM to go idle** — watches `vllm:num_requests_running` and `vllm:num_requests_waiting` and stops as soon as both are zero, waiting at most `drainSeconds`. An idle pod shuts down right away instead of waiting the whole time. If the check fails, it assumes the pod is still busy and keeps waiting.
- **`--shutdown-timeout`** (vLLM ≥ 0.18.0) — once SIGTERM arrives, vLLM's default of `0` **kills** any request still running. Setting it above zero lets those requests finish instead.

All three share the same `terminationGracePeriodSeconds` budget. When it runs out the kubelet stops waiting and sends SIGKILL, and any request still running dies with the pod — the one outcome all of this exists to avoid. It can land in any of the three steps, not just the last one, so the chart refuses to render if `endpointSyncSeconds + drainSeconds + shutdownTimeout` adds up to more than the budget.

### TODO

- [ ] **switch operator scale write to Server-Side Apply**. Move `spec.replicas` writes from the current client-side `r.Update` (PUT) to client.Apply/SSA with a stable, explicit field manager and only `spec.replicas` in the apply set, so the operator owns that field outright regardless of Helm. Recorded with the current-state details and the reason (ownership is presently incidental, so Helm could still thrash if it re-asserts the field).

### Testing the scale operation

Tiny models (e.g. `facebook/opt-125m`) never fill their KV cache enough to trip a real threshold. The `vllm-mock` chart ships an optional `metricsMock` (`--set metricsMock.enabled=true`) that returns a fixed metric value, so the scaler can be driven to scale up/down deterministically. See `test/charts/vllm-mock/values.yaml`.

