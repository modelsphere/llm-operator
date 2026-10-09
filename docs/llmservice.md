# LLMService

`serving.modelsphere.dev/v1alpha1` kind `LLMService` (plural `llmservices`, short name `llmsvc`) maps one namespaced object to one Helm release. `metadata.name` is the release name. The release is stored with Helm's Secret driver, so `helm list` in that namespace still shows it.

## Spec

| Field | Meaning |
| --- | --- |
| `spec.chart.name` | Chart name. |
| `spec.chart.repo` | `https://…` or `oci://…` repository. |
| `spec.chart.version` | Exact version or a semver range. Resolved once and pinned on `status.chart`. A newer chart in the repository does not move the release while the pinned version is still in range. |
| `spec.chart.credentialsRef` | Optional Secret in this namespace. `data.username` and `data.password` are basic-auth credentials for listing and pulling. Unset means anonymous. |
| `spec.layers` | Helm values documents, merged in order. Maps merge recursively, other values replace, and a JSON null deletes a key. Names are unique. An empty list leaves chart defaults. |
| `spec.model` | Descriptive only. Not rendered into the chart. |
| `spec.suspend` | Stop reconciling. Status, including a finalizer, stays as it was. A suspended object is not uninstalled while it is deleting. |

`status.appliedHash` is `sha256:` plus the hex SHA-256 of the JSON object `{chart:{name,version}, values}` with sorted keys. `status.history` is newest first.

## Reconcile

1. `spec.suspend` is true: return without writing status or calling Helm.
2. The object is deleting. `serving.modelsphere.dev/deletion-policy: Orphan` drops the finalizer and leaves the release. Otherwise the release is uninstalled (a missing release is fine) and then the finalizer is dropped.
3. Add finalizer `serving.modelsphere.dev/finalizer`.
4. Resolve `spec.chart.version` with the same rule as swiss `chart.Resolve`: an exact version, else the pinned `status.chart.version` while the range allows it, else the newest version the repository lists. A resolve error sets `phase: Failed`, condition `Applied=False` reason `ChartResolveFailed`, and retries with backoff.
5. Merge layers and hash them with the resolved chart. The same hash as `status.appliedHash` sets `phase: Applied` and `observedGeneration` when those are stale, then stops. No new history entry.
6. If a `deployed` release exists and this object has never applied (`appliedHash` empty and `history` empty), adopt it. The release chart name, chart version, and user-supplied values must equal the resolved chart and merged layers after a JSON round trip. Equal: record `appliedHash`, `Adopted=True` reason `Adopted`, `Applied=True` reason `Adopted`, a history entry, and a ControllerRevision. No Helm install or upgrade. Not equal: `phase: Failed`, `Adopted=False` reason `Drift`, message names chart and/or values. No upgrade, and no requeue; the next spec change tries again. A release that is not `deployed` is not adopted. If `Applied` is already `ApplyFailed`, the controller upgrades that release. Otherwise it sets `phase: Failed`, `Adopted=False` reason `Drift`, message `release is <status>, not deployed`, and does not requeue.
7. Otherwise set `phase: Applying` and leave `observedGeneration` at the last completed generation, then install or upgrade through the Helm SDK with server-side apply and no resource wait (`HookOnlyStrategy`, the same outcome as `wait: false`). `serving.modelsphere.dev/force-conflicts` equal to the current generation sets Helm `ForceConflicts` for that apply and records `forceConflicts` on the history entry. Any other value is ignored.

On Helm error: `phase: Failed`, `Applied=False` reason `ApplyFailed`, `message` is the error, `observedGeneration` is the generation, and the error is returned for backoff.

On success the controller writes a ControllerRevision named `<llmservice>-<8 hex chars of the hash>` in the same namespace. It is owned by the LLMService, labelled `serving.modelsphere.dev/llmservice=<name>`, and its data is JSON `{spec, annotations}` where annotations are only `swiss.modelsphere.dev/*`. Applying that spec again reuses the name and updates the revision's data and helm revision. `status.history` is prepended and capped (default 10, flag `--llmservice-history-limit`). ControllerRevisions are deleted only when no remaining history entry names them. Each history entry copies those swiss annotations, `forceConflicts`, and `swiss.modelsphere.dev/action` when set.

Events are emitted for Installed, Upgraded, Adopted, Drift, ApplyFailed, and Uninstalled.

Charts are cached under `--llmservice-chart-cache-dir` (default `$TMPDIR/llm-operator-charts`), keyed by repository, name, and version.

## Annotations

| Annotation | Effect |
| --- | --- |
| `serving.modelsphere.dev/deletion-policy: Orphan` | On delete, drop the finalizer and do not uninstall. |
| `serving.modelsphere.dev/force-conflicts: "<generation>"` | Force field-manager conflicts on the apply for that generation only. Recorded in `status.history`. |
| `swiss.modelsphere.dev/*` | Copied into each history entry and ControllerRevision. Otherwise ignored. |

## RBAC

The manager role may manage `llmservices`, their status and finalizers, `controllerrevisions`, Helm release Secrets, and `events.k8s.io` events. It also includes the rules the sglang and vllm charts render (secrets, configmaps, services, serviceaccounts, deployments, pod disruption budgets, roles, role bindings, leases, pods, servicemonitors, leaderworksets, llmscalers, llmslorequirements, modelroutes) plus `namespaces` get, create, and patch.
