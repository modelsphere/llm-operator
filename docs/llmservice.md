# LLMService

`serving.modelsphere.dev/v1alpha1` kind `LLMService` installs one Helm release per object and keeps it matching the spec.

- **Release**: one LLMService is one Helm release in the same namespace, named `metadata.name`. Releases are stored with Helm's Secret driver, so `helm list -n <namespace>` shows them.
- **Chart** (`spec.chart`): name, `https://` or `oci://` repository, and an exact version or a [semver range](https://github.com/Masterminds/semver/tree/v3.5.0#checking-version-constraints). A range is resolved once and pinned on `status.chart.version`; see [Chart version](#chart-version).
- **Layers** (`spec.layers`): Helm values documents merged in order, last writer wins. See [Layer merge](#layer-merge).
- **History** (`status.history`): every successful apply is recorded, newest first, with a ControllerRevision holding the spec it applied. See [Revisions](#revisions).

## CRD specification

| | |
| --- | --- |
| Group / version | `serving.modelsphere.dev/v1alpha1` |
| Kind | `LLMService` (list `LLMServiceList`) |
| Resource | `llmservices` (singular `llmservice`, short name `llmsvc`) |
| Scope | Namespaced |
| Subresources | `status` |

### `spec`

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `chart` | object | ✔ | — | The chart to install. See below. |
| `layers` | []object | | — | Helm values documents, merged in order. Empty or unset leaves the chart defaults. See below. |
| `model` | object | | — | Descriptive metadata. Not rendered into the chart. See below. |
| `suspend` | bool | | `false` | Stop reconciling: no status write and no Helm call. Suspend wins over deletion: a suspended object that is deleting is not uninstalled and keeps its finalizer. |
| `deletion` | object | | — | Staged teardown budget. See [Staged deletion](#staged-deletion). |

### `spec.chart`

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | ✔ | Chart name. |
| `repo` | string | ✔ | Repository URL, matching `^(https://\|oci://).+`. When `--llmservice-allowed-chart-repos` is set, it must be one of those repositories or a path under one. |
| `version` | string | ✔ | Exact version or a semver range; see [Chart version](#chart-version). A range is resolved on first apply and pinned on `status.chart.version`. While the range allows it, the pin is kept through other spec changes and newer charts alike; a new `serving.modelsphere.dev/re-resolve` value moves it to the newest version in range. |
| `credentialsRef` | object | | `name` of a Secret in this namespace whose `data.username` and `data.password` are basic-auth credentials for listing and pulling the chart. Unset means anonymous. |

### `spec.layers[]`

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | ✔ | Unique within the list. It is the list-map key, so each layer can be owned by a different field manager under server-side apply. |
| `values` | object | | A Helm values document. Schemaless. |

The order of the list is its meaning, so one writer should own the whole list.

### `spec.model`

All fields are optional strings. They are descriptive only: labels, printer columns, catalog checks. None is rendered into the chart.

| Field | Description |
| --- | --- |
| `name` | Model name. Printed by `kubectl get llmsvc`. |
| `version` | Model version. |
| `variant` | Serving variant, such as a parallelism and hardware profile. |
| `hf` | Hugging Face repository id. |
| `engine` | Inference engine, such as `sglang` or `vllm`. |

### `status`

| Field | Type | Description |
| --- | --- | --- |
| `phase` | string | `Applying`, `Applied`, `Failed`, or `Deleting`. The enum also allows `Pending`, which the controller does not set. |
| `observedGeneration` | int64 | `metadata.generation` last reconciled to a terminal outcome. It stays at the previous generation while `Applying`. |
| `conditions` | []metav1.Condition | `Applied` and `Adopted`. See [Conditions and events](#conditions-and-events). |
| `message` | string | The error when `phase` is `Failed`; empty otherwise. |
| `chart.name`, `chart.version` | string | The chart and the exact version the release is pinned to. |
| `chart.reResolve` | string | The `serving.modelsphere.dev/re-resolve` value already honored. Only a different non-empty value re-resolves. |
| `appliedHash` | string | `sha256:` plus the hex SHA-256 of the JSON object `{chart:{name,version}, values}` with sorted keys. |
| `releaseUID` | string | UID of the LLMService that claimed the release. See [Ownership](#ownership). |
| `helm.revision`, `helm.status` | int, string | The Helm release revision and status last observed. |
| `history` | []object | Successful applies, newest first, capped by `--llmservice-history-limit`. See below. |
| `deletion` | object | Staged teardown while the object is deleting. See [Staged deletion](#staged-deletion). |

### `status.history[]`

| Field | Type | Description |
| --- | --- | --- |
| `revision` | int | The Helm release revision this entry records. |
| `hash` | string | `appliedHash` at this revision. |
| `appliedAt` | time | When the entry was recorded. |
| `controllerRevision` | string | The ControllerRevision holding the spec snapshot. See [Revisions](#revisions). |
| `annotations` | map[string]string | The `swiss.modelsphere.dev/*` annotations on the LLMService at the time. |
| `forceConflicts` | bool | This apply honored `serving.modelsphere.dev/force-conflicts`. |
| `reResolved` | bool | This apply honored a new `serving.modelsphere.dev/re-resolve` value instead of the pinned version. |
| `action` | string | `swiss.modelsphere.dev/action` (`install`, `upgrade`, `rollback`, or `migrate`) when set. |

`kubectl get llmsvc` prints `Chart`, `Version` (`status.chart.version`), `Model`, `Phase`, `Revision` (`status.helm.revision`), and `Age`.

### Example

```yaml
apiVersion: serving.modelsphere.dev/v1alpha1
kind: LLMService
metadata:
  name: qwen
  namespace: models
  annotations:
    swiss.modelsphere.dev/plan-hash: sha256:9f2c4a1b8e0d
    swiss.modelsphere.dev/catalog: /models/index.json
    swiss.modelsphere.dev/catalog-name: public
    swiss.modelsphere.dev/catalog-ref: sha256:c0ffee
    swiss.modelsphere.dev/entry-digest: sha256:a1b2c3
    swiss.modelsphere.dev/profile: prod
    swiss.modelsphere.dev/action: upgrade
    swiss.modelsphere.dev/note: "bump to 1.1.0"
spec:
  chart:
    name: sglang
    repo: oci://ghcr.io/modelsphere/charts
    version: ">=0.8.0"
    # Secret data keys: username, password. Omit credentialsRef for an anonymous pull.
    credentialsRef:
      name: chart-pull
  layers:
    - name: catalog
      values:
        model: qwen3.6-35b-a3b
        engine: sglang
    - name: site
      values:
        replicaCount: 2
        resources:
          limits:
            nvidia.com/gpu: "1"
    - name: derived
      values:
        service:
          port: 30000
  model:
    name: qwen3.6-35b-a3b
    version: "1.1.0"
    variant: sglang-tp2-h100
    hf: Qwen/Qwen3.6-35B-A3B
    engine: sglang
  suspend: false
```

Kept in sync at `config/samples/serving_v1alpha1_llmservice.yaml`; the generated schema is `config/crd/bases/serving.modelsphere.dev_llmservices.yaml`.

The Secret `credentialsRef` names:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: chart-pull
  namespace: models
stringData:
  username: <registry user>
  password: <registry token>
```

## Chart version

`spec.chart.version` uses the constraint syntax of [Masterminds/semver v3](https://github.com/Masterminds/semver/tree/v3.5.0#checking-version-constraints), the library Helm uses for dependency versions. A range resolves to the newest version in it that the repository lists, on first apply and on each new `serving.modelsphere.dev/re-resolve` value; otherwise the pinned `status.chart.version` is kept.

The last column assumes a repository holding `0.7.0 0.7.1 0.7.5 0.8.0-rc.1 0.8.0 0.8.2 0.9.0 1.0.0 1.2.0 2.0.0`.

| `spec.chart.version` | Allows | Resolves to |
| --- | --- | --- |
| `0.7.1` | Exactly `0.7.1`. The only form that is not a range: the repository is never listed. | `0.7.1` |
| `=0.7.1` | Exactly `0.7.1`, but as a range: the repository is listed on first apply. | `0.7.1` |
| `0.7`, `0.7.x` | `>=0.7.0 <0.8.0` | `0.7.5` |
| `~0.7.1` | `>=0.7.1 <0.8.0`: patch releases. | `0.7.5` |
| `^0.7.1` | `>=0.7.1 <0.8.0`. Below `1.0.0` the minor version is the compatibility level, so `^` matches `~`. | `0.7.5` |
| `^1.0.0` | `>=1.0.0 <2.0.0`: minor and patch releases. | `1.2.0` |
| `>=0.7.1 <0.9.0`, `>=0.7.1, <0.9.0` | Both bounds. A space or a comma is AND. | `0.8.2` |
| `0.7.1 - 0.8.2` | `>=0.7.1 <=0.8.2`. The spaces around `-` are required. | `0.8.2` |
| `^0.7.1 \|\| ^1.0.0` | Either range. `\|\|` is OR. | `1.2.0` |
| `>=0.7.1` | `0.7.1` or newer, with no upper bound, so a major release is picked too. | `2.0.0` |
| `^0.8.0-0` | `>=0.8.0-0 <0.9.0`, prereleases included. A prerelease such as `0.8.0-rc.1` only matches a range that names one; `-0` is the lowest. | `0.8.2` |

To move a pinned range to the newest version in range without changing the spec, set a new re-resolve value:

```sh
kubectl annotate llmsvc qwen -n models serving.modelsphere.dev/re-resolve="$(date +%s)" --overwrite
```

| Situation | Result |
| --- | --- |
| A newer version is in range | Upgrade to it. `status.chart.reResolve` records the value; the history entry has `reResolved: true`. |
| Nothing newer | No upgrade. The value is still recorded, so a later reconcile keeps the pin. |
| The upgrade fails | The value is not recorded; the retry re-resolves again. |
| The same value is set again | Nothing. Only a different non-empty value re-resolves. |
| `spec.chart.version` is exact | Nothing to resolve. The value is recorded. |

## Layer merge

Layers merge in list order into one values document. For each key a layer sets:

| The layer's value | Effect |
| --- | --- |
| A map, onto a map | Merged recursively. |
| Anything else: a scalar, a list, or a map onto a non-map | Replaces the earlier value. Lists are not merged. |
| `null` | Deletes the key. |

A layer with no `values`, or `values: null`, contributes nothing.

## Annotations

| Annotation | Effect |
| --- | --- |
| `serving.modelsphere.dev/deletion-policy: Orphan` | On delete, drop the finalizer and do not uninstall. |
| `serving.modelsphere.dev/deletion-max-wait` | Overrides `spec.deletion.maxWait`. Read again on every pass. `0s` stops waiting. |
| `serving.modelsphere.dev/route-settle` | Overrides `spec.deletion.routeSettle`. `0s` skips the settle wait. |
| `serving.modelsphere.dev/force-delete-after` | Overrides `spec.deletion.forceDeleteAfter`. `0s` force-deletes a terminating pod on the next pass. |
| `serving.modelsphere.dev/force-conflicts: "<generation>"` | Force field-manager conflicts on the apply for that generation only. Any other value is ignored. Recorded as `forceConflicts` in `status.history`. |
| `serving.modelsphere.dev/re-resolve: "<any new value>"` | Drop the pinned chart version once and resolve `spec.chart.version` to the newest version in range. See [Chart version](#chart-version). |
| `serving.modelsphere.dev/skip-schema-validation: "true"` | Install and upgrade without checking the merged values against the chart's `values.schema.json` (helm's `--skip-schema-validation`), for every apply while set. Any other value leaves the check on. An escape hatch for values the schema refuses. Not part of the applied hash: turning it on re-applies only a release whose last apply did not succeed. |
| `swiss.modelsphere.dev/*` | Copied into each history entry and ControllerRevision. Otherwise ignored. |

Annotation changes trigger a reconcile, as do generation changes and deletion. Status-only updates do not.

## Ownership

| Mark | On | Written |
| --- | --- | --- |
| Label `serving.modelsphere.dev/llmservice-uid=<uid>` | The Helm release | By Install and Upgrade. Adoption does not relabel. |
| Label `serving.modelsphere.dev/llmservice=<name>` | The Helm release | By Install and Upgrade. |
| `status.releaseUID` | The LLMService | Before Install, on adopt, and on every successful apply. |

- **Owned**: the release's uid label, or `status.releaseUID`, equals the object's UID. It decides whether a pending release is recovered or left alone, and whether a release this object never recorded is applied or adopted. Condition reasons are not ownership.
- **Uninstalled on delete**: the release's uid label equals the object's UID; or, when the release has no uid label, the object has `status.history`. `status.releaseUID` alone is not enough, since it is claimed before a first install that may never have stored a release.

A chart-list, credentials, or Helm read error retries with backoff and does not decide Drift.

## Reconcile

1. `spec.suspend` is true: return without writing status or calling Helm.
2. The object is deleting. See [Staged deletion](#staged-deletion). Orphan and a release this object never installed or adopted still skip that and drop the finalizer immediately. A kept release emits `KeptRelease`.
3. Add finalizer `serving.modelsphere.dev/finalizer`.
4. `spec.chart.repo` is not allowed by `--llmservice-allowed-chart-repos`: `phase: Failed`, `Applied=False` reason `ChartRepoNotAllowed`. Nothing is fetched or applied, and nothing is retried until the spec or annotations change.
5. Read the credentials Secret, then resolve `spec.chart.version` with the same rule as swiss `chart.Resolve`: an exact version; else the pinned `status.chart.version` while the range allows it, unless a new `serving.modelsphere.dev/re-resolve` value skips the pin; else the newest version the repository lists. An error sets `phase: Failed`, `Applied=False` reason `ChartResolveFailed`, and retries with backoff.
6. Merge layers and hash them with the resolved chart. The same hash as `status.appliedHash` sets `phase: Applied`, `observedGeneration`, and `status.chart.reResolve` when those are stale, then stops, with no new history entry.
7. Get the release. An error sets `phase: Failed`, `Applied=False` reason `ApplyFailed`, and retries with backoff.
8. A `pending-install`, `pending-upgrade`, or `pending-rollback` release is not upgraded in place. Its age is the release's last-deployed time.
   - Not owned: `phase: Failed`, `Applied=False` reason `PendingRelease`, requeue after 1m. No Helm write.
   - Owned and younger than `--llmservice-pending-timeout`: `phase: Applying`, `Applied=False` reason `OperationInProgress`, requeue after the time remaining, at least 15s. No Helm write.
   - Owned and older: recover, emit `RecoveredPendingRelease`, then apply in the same reconcile. `pending-install` is uninstalled. `pending-upgrade` and `pending-rollback` roll back to the newest `deployed` revision, or are uninstalled when history has none.
9. `appliedHash` and `history` are empty and a release exists:
   - Owned: apply (step 10). This is an install whose status was wiped.
   - Not owned and `deployed`: adopt. The release's chart name, chart version, and user-supplied values must equal the resolved chart and merged layers after a JSON round trip. Equal: record the apply as `Adopted` with no Helm call. Not equal: `Drift`, naming the chart and/or values; no retry until the spec or annotations change.
   - Not owned and anything else: `Drift`, `release is <status>, not deployed`; no retry until the spec or annotations change.
10. Apply: set `phase: Applying` and write `status.releaseUID`, then install or upgrade. An owned `failed` release is upgraded when some revision is `deployed`, otherwise uninstalled and installed. Helm uses server-side apply as field manager `llm-operator-helm`, a 5m timeout, and no resource wait (`HookOnlyStrategy`, the same outcome as `wait: false`). A Helm error sets `phase: Failed`, `Applied=False` reason `ApplyFailed`, and retries with backoff.

A successful install, upgrade, or adoption writes a ControllerRevision, prepends a `status.history` entry, and sets `status.chart`, `appliedHash`, `releaseUID`, `helm`, `phase: Applied`, and `observedGeneration`.

## Staged deletion

An LLMService that installed or adopted its release keeps `serving.modelsphere.dev/finalizer` and tears the release down in order. Each pass requeues after 5 seconds and writes status before the step it records. The stage only moves forward, and a restart resumes from the recorded stage and timestamps.

1. Delete every `ModelRoute` in the Helm release manifest (`routing.modelsphere.dev` or `routing.gpucluster.io`) and wait until it is NotFound. The operator does not remove the route finalizer.
2. Wait `routeSettle` so the gateway can drop the route.
3. Uninstall the Helm release (`Uninstalled`).
4. Wait until the release pods are gone. The selectors are `app.kubernetes.io/instance=<release>` and `app=<release>-<status.chart.name>`. Succeeded and Failed pods are ignored. A pod inside its grace period always counts. Past grace, a pod on a NotReady or missing node counts as gone (`stuck: true`). Any other pod counts as gone after grace plus 5 minutes. If the node list cannot be read, only the time rule applies.
5. Drop the finalizer.

No ModelRoute in the manifest skips straight to uninstall with outcome `NoRoute`. Past `maxWait`, the finalizer is dropped. The outcome is `TimedOut` when pods are still present, unless it is already `RouteStuck`: the route was still there at the deadline, its finalizer stays, and the release is uninstalled anyway. `forceDeleteAfter`, when set, deletes a pod that has been terminating that long with `gracePeriodSeconds: 0` and a UID precondition.

`spec.suspend` still wins: the finalizer stays and nothing is deleted. `serving.modelsphere.dev/deletion-policy: Orphan` skips every stage and leaves the release. A release this object never installed or adopted is kept, with `KeptRelease`, and its ModelRoute is not deleted.

### Knobs

Precedence is annotation, then `spec.deletion`, then the built-in default. Annotations are read on every pass, including ones added after the delete. Annotation changes during deletion, such as lowering `serving.modelsphere.dev/deletion-max-wait`, setting `serving.modelsphere.dev/force-delete-after: 0s`, or adding `serving.modelsphere.dev/deletion-policy: Orphan`, take effect on the next pass. `deadline` is `startedAt` plus the effective `maxWait`. An invalid annotation emits Warning `InvalidAnnotation` and falls back to the spec or the default. An explicit `0s`, in the spec or an annotation, is a real zero: `routeSettle: 0s` skips the settle wait and `maxWait: 0s` releases on the next pass.

| Spec | Annotation | Default | Effect |
| --- | --- | --- | --- |
| `maxWait` | `serving.modelsphere.dev/deletion-max-wait` | `1h` | How long the finalizer may be held. Past it, remaining waits are skipped. |
| `routeSettle` | `serving.modelsphere.dev/route-settle` | `60s` | Wait after the ModelRoute is NotFound. `0s` skips it. |
| `forceDeleteAfter` | `serving.modelsphere.dev/force-delete-after` | unset (off) | Force-delete pods that have been terminating this long. `0s` does it on the next pass. |

### `status.deletion`

`status.phase` is `Deleting` while the finalizer is held. `status.message` stays the Helm error field; the human line is `status.deletion.message`.

| Field | Meaning |
| --- | --- |
| `stage` | `RouteRemoving`, `RouteSettling`, `Uninstalling`, `PodsTerminating`, `ForceDeleting`, `Released`. |
| `outcome` | `Clean`, `NoRoute`, `RouteStuck`, `PodsForceDeleted`, `TimedOut`. |
| `startedAt` | When staged deletion began. |
| `deadline` | `startedAt` plus the effective `maxWait`, recomputed each pass. |
| `routeRemovedAt` | When every ModelRoute was first observed NotFound. |
| `uninstalledAt` | When the Helm uninstall succeeded. |
| `podsGoneAt` | When no release pods were left to wait for. |
| `podsRemaining` | How many pods still block the finalizer. |
| `pods` | Up to 20 pods still in the API, ignoring Succeeded and Failed. Each has `name`, `phase`, `node`, `deletionTimestamp`, `grace`, and `stuck`. |
| `forceDeleted` | Up to 20 pod names deleted with a zero grace period. |
| `message` | For example `waiting for 3 pods (2 stuck on NotReady node gpu-7)`. |

Condition `Deleting` uses the same reasons as the events: `RouteRemoving`, `RouteSettling`, `RouteStuck`, `NoRoute`, `Uninstalled`, `WaitingForPods`, `PodsForceDeleted`, `TimedOut`, `Released`. `PodForceDeleted` is one Warning per pod. `InvalidAnnotation` is a Warning whenever an annotation value cannot be parsed.

## Conditions and events

| Reason | Condition | Phase | Retry | Event |
| --- | --- | --- | --- | --- |
| `Installed` | `Applied=True` | `Applied` | — | `Installed` |
| `Upgraded` | `Applied=True` | `Applied` | — | `Upgraded` |
| `Adopted` | `Applied=True`, `Adopted=True` | `Applied` | — | `Adopted` |
| `ChartRepoNotAllowed` | `Applied=False` | `Failed` | On the next spec or annotation change | — |
| `ChartResolveFailed` | `Applied=False` | `Failed` | Backoff | — |
| `Failed` | `Applied=False` | `Failed` | Backoff | — |
| `ApplyFailed` | `Applied=False` | `Failed` | Backoff | `ApplyFailed` (Warning) |
| `Drift` | `Applied=False`, `Adopted=False` | `Failed` | On the next spec or annotation change | `Drift` (Warning) |
| `PendingRelease` | `Applied=False` | `Failed` | After 1m | — |
| `OperationInProgress` | `Applied=False` | `Applying` | After the time remaining, at least 15s | — |
| `RecoveredPendingRelease` | `Applied=False` | `Applying` | Applies in the same reconcile | `RecoveredPendingRelease` |

`Failed` means the layers could not be merged or hashed. Deletion also emits the reasons in [Staged deletion](#staged-deletion). `Uninstalled` and `KeptRelease` are unchanged.

## Revisions

Each successful apply writes one ControllerRevision:

| | |
| --- | --- |
| Name | `<llmservice>-<first 8 hex of appliedHash>-<helm revision>`, the LLMService name shortened to fit 253 characters |
| Namespace | The LLMService's |
| Owner | The LLMService (controller reference) |
| Label | `serving.modelsphere.dev/llmservice=<name>` |
| `revision` | The Helm revision |
| `data` | JSON `{spec, annotations}`, with only the `swiss.modelsphere.dev/*` annotations |

A ControllerRevision is never updated. A name that comes round again, from the same apply recorded twice or from Helm numbering restarted by an uninstall, is replaced. ControllerRevisions are deleted once no `status.history` entry names them.

## Operator flags

| Flag | Default | Effect |
| --- | --- | --- |
| `--llmservice-allowed-chart-repos` | empty | Comma-separated chart repositories an LLMService may install from. Each allows itself and the paths under it: `oci://ghcr.io/modelsphere` allows `oci://ghcr.io/modelsphere/charts`, not `oci://ghcr.io/modelsphere-x`. Empty allows any, which lets whoever can create an LLMService run any chart with the operator's permissions. |
| `--llmservice-history-limit` | `10` | `status.history` entries kept per LLMService. |
| `--llmservice-pending-timeout` | `10m` | How long an owned pending release is left alone before recovery. Twice the 5m Helm apply timeout. |
| `--llmservice-chart-cache-dir` | `$TMPDIR/llm-operator-charts` | Chart cache, one `chart.tgz` per repository, name, and version. |

## RBAC

The manager role may manage `llmservices`, their status and finalizers, `controllerrevisions`, Helm release Secrets, and `events.k8s.io` events. It also includes the rules the sglang and vllm charts render (secrets, configmaps, services, serviceaccounts, deployments, pod disruption budgets, roles, role bindings, leases, pods, servicemonitors, leaderworkersets, llmscalers, llmslorequirements, modelroutes) plus `namespaces` get, create, and patch. Pods include `delete`, used by force-delete. Nodes are `get`, `list`, and `watch`, used to tell a NotReady node from a slow pod.
