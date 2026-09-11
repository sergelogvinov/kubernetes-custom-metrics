# Metrics Gateway — v1 Specification (Revised)

Status: revised design, not yet implemented. This revision supersedes the previously frozen v3 draft. Authorization to a custom-metrics endpoint grants access to the corresponding metrics without requiring the caller to read the underlying Kubernetes resources. The contracts and limitations below must be covered by integration tests before release.

---

## 1. Locked Decisions (cumulative)

| # | Decision | Value |
| :--- | :--- | :--- |
| 1 | Plugin name | **`btop`** (invoked as `kubectl btop`) |
| 2 | Workload return shape | **Single aggregated value** per object |
| 3 | Node wildcard return shape | **Per-node list** (one row per node) |
| 4 | Base metrics | `cpu`, `memory`, `node_cpu`, `node_memory` — no `http_rps` |
| 5 | Cache TTL | `window ≤ 1h → 15s`, `window > 1h → 10m` |
| 6 | Resource scope | Pods, Nodes, Deployments, StatefulSets, DaemonSets, Jobs, CronJobs |
| 7 | CronJob with no active Jobs | **Fallback lookup over 1-day window**; if still empty → `404` |
| 8 | Watch mode | In scope for v1 |
| 9 | Configuration | CLI flags + env vars |
| 10 | API groups | `custom.metrics.k8s.io` only |
| 11 | `btop` JSON output | **Flattened** `{name, namespace, cpu, memory}` shape |
| 12 | Distribution | **Raw binary** (no Krew/Homebrew in v1) |
| 13 | Authorization | **Apiserver authorizes metric requests**; trusted-proxy-only gateway; ServiceAccount resource reads; no gateway SubjectAccessReview or impersonation |
| 14 | Historical membership | Retained objects selected at query time, not a complete historical ownership ledger |
| 15 | Deferred features | Temporal `sum`, per-container output, and server-side watch |

---

## 2. Metric Name Grammar

```
<base>_<stat>_<window>
```

| Segment | Values |
| :--- | :--- |
| `base` | `cpu`, `memory`, `node_cpu`, `node_memory` |
| `stat` | `avg`, `max`, `min`, `p50`, `p90`, `p95`, `p99`, `stddev` |
| `window` | `1m`, `5m`, `15m`, `1h`, `6h`, `12h`, `24h` |

There are **4 × 8 × 7 = 224 distinct metric names**. `cpu` and `memory` apply to the six namespaced resources; `node_cpu` and `node_memory` apply only to Nodes. Discovery therefore contains **(6 × 2 + 1 × 2) × 8 × 7 = 784 resource/metric entries**. The discovery cap counts entries, not distinct names. Unsupported base/resource combinations are not advertised and return `404`.

Temporal `sum` is deferred: `sum_over_time` adds samples and depends on resolution; it is neither CPU-seconds nor a meaningful memory-usage quantity. Spatial sums across containers/pods remain part of aggregation.

Examples: `cpu_avg_5m`, `memory_max_1h`, `node_cpu_p95_12h`, `node_memory_min_24h`.

---

## 3. Resource Scope & Aggregation

### 3.1 Return Shapes

| Resource | Single-object path | Wildcard path (`*`) |
| :--- | :--- | :--- |
| Pod | 1 value | List of per-pod values |
| **Node** | 1 value | **List of per-node values** |
| Deployment | 1 aggregated value | List of per-Deployment aggregated values |
| StatefulSet | 1 aggregated value | List |
| DaemonSet | 1 aggregated value | List |
| Job | 1 aggregated value | List |
| CronJob | 1 aggregated value | List |

### 3.2 Workload Aggregation

Default strategy: **sum-then-stat** — at each evaluation step, sum all selected pod usage into one workload value, then apply the temporal statistic. Each wildcard object is evaluated independently.

Illustrative query against the normalized series contract in §8 (coverage validation is additional):

```promql
avg_over_time((sum(pod_cpu_usage_cores{cluster="example",namespace="prod",uid=~"uid-a|uid-b"}))[1h:15s])
```

Do not retain `pod` in the workload sum, and do not sum per-pod quantiles afterward. For **stat-then-sum**, apply the temporal statistic separately to each pod, then sum those values; it is deliberately a different statistic. Pod inputs already sum application containers. Node inputs already aggregate CPU cores/memory into one value per node.

Temporal definitions:

- CPU input is a five-minute rate expressed in cores; memory input is a gauge expressed in bytes. A `1m` CPU window describes five-minute-smoothed rates sampled over the last minute, not instantaneous peaks.
- Normalized recording rules and subqueries use a fixed 15-second grid. Source scrape interval must be at most 15 seconds. CPU needs at least five minutes of source history before the requested window; backend retention must cover at least 24h plus that warm-up.
- `avg`, `max`, `min`, and population `stddev` use the corresponding Prometheus `_over_time` functions. Quantiles use `quantile_over_time`; short-window p99 is an interpolated small-sample estimate, not a high-confidence tail estimate.
- All supported statistics retain the input unit. CPU quantities use DecimalSI, rounded up to millicores; memory uses BinarySI, rounded up to whole bytes. Reject negative, nonfinite, and overflowing results rather than clamping them.
- Query time is captured once per computation and aligned to the last fully completed grid step. All subqueries and coverage checks for that computation use this time.
- Statistics cover the interval when at least one selected member is known to be active. Confirmed inactive members contribute zero when other members are active; fully inactive intervals are excluded, not silently zero-filled. This is an active-period statistic, not amortized usage over wall-clock time.
- Both aggregation strategies use this same union-active grid. In `stat-then-sum`, evaluate each member with explicit zeros at its confirmed-inactive points on that grid before summing member statistics. Do not use separate per-member active-only grids. Include staggered-lifetime fixtures.

Per-base override remains available in the catalog (`aggregation: sum-then-stat | stat-then-sum`). Numeric fixture tests must distinguish the strategies using alternating pod peaks.

### 3.3 Resource Resolution Table

| Resource | Selector source | Notes |
| :--- | :--- | :--- |
| Pod | API GET/list → UID | Names identify API objects; UIDs identify metric series |
| Node | API GET/list → UID | Explicit node-exporter to Kubernetes identity mapping |
| Deployment | `apps/v1` full `spec.selector` → retained Pods | Includes `matchExpressions` |
| StatefulSet | `apps/v1` full `spec.selector` → retained Pods | Includes `matchExpressions` |
| DaemonSet | `apps/v1` full `spec.selector` → retained Pods | Includes `matchExpressions` |
| Job | `batch/v1` full `spec.selector` → retained Pods | Includes `matchExpressions` |
| **CronJob** | **Active Jobs → their selectors** | See §3.4 |

All lookups use the gateway ServiceAccount. Selectors are evaluated by Kubernetes when listing Pods, not inserted as arbitrary labels into resource-metric PromQL. Union results by Pod UID, so overlapping selectors never double count. Constrain every backend query by the configured cluster, namespace where applicable, and exact escaped UIDs; an empty selection must never become an unrestricted query.

v1 answers for **retained membership at query time**. It does not reconstruct deleted Pods, previous label assignments, or previous objects with the same name. Retained completed Pods can contribute historical samples. Operators requiring historical Job metrics must retain Jobs and Pods for the required lookback; Kubernetes Job/Pod cleanup can make data inaccessible even if Prometheus retains it. Completeness checks cover the selected retained set, not deleted membership. A complete historical ownership ledger is out of scope.

### 3.4 CronJob Fallback Logic

A CronJob has no pod selector of its own. Resolution order:

1. **Active Jobs** — list retained Jobs with a controller ownerReference matching the CronJob UID and `status.active > 0`; union their full pod selectors.
2. **Recent Jobs (1-day fallback)** — only if no active Jobs exist, select retained Jobs with the same controller owner UID and `status.completionTime` or `status.startTime` within the **last 24 hours**; union their full selectors.
3. **Not found** — if neither yields any Jobs, return **`404 Not Found`** with body:
   ```json
   {
     "kind": "Status",
     "apiVersion": "v1",
     "status": "Failure",
     "reason": "NotFound",
    "code": 404,
     "message": "no active or recent (24h) Jobs for CronJob prod/nightly-batch"
   }
   ```

This intentionally means **active runs, otherwise recent retained runs**, not all runs in the metric window. Starting a run changes the selected set and can exclude completed runs. The fallback does not guarantee 24-hour history: deleted metadata, no eligible retained Pods, or verified inactivity throughout the metric window can produce `404`. Missing telemetry for expected-active members instead produces `503`, even if no usage series exists. The metric window and Job fallback window are separate filters. Keep this limitation visible in command documentation.

### 3.5 API Paths (v1beta2)

| Scope | Path |
| :--- | :--- |
| Pod | `/namespaces/{ns}/pods/{name}/{metric}` |
| Pods (all) | `/namespaces/{ns}/pods/*/{metric}?labelSelector=…` |
| Node | `/nodes/{name}/{metric}` |
| Nodes (all) | `/nodes/*/{metric}` |
| Deployment | `/namespaces/{ns}/deployments.apps/{name}/{metric}` |
| StatefulSet | `/namespaces/{ns}/statefulsets.apps/{name}/{metric}` |
| DaemonSet | `/namespaces/{ns}/daemonsets.apps/{name}/{metric}` |
| Job | `/namespaces/{ns}/jobs.batch/{name}/{metric}` |
| CronJob | `/namespaces/{ns}/cronjobs.batch/{name}/{metric}` |

Wildcard `*` on workloads returns a list of single aggregated values.

### 3.6 Wire and error contract

- Prefix all paths with `/apis/custom.metrics.k8s.io/v1beta2`. Both named and wildcard successful HTTP responses are `MetricValueList`; a named response has exactly one item. A provider's single `MetricValue` is wrapped at the HTTP edge.
- Each item includes `describedObject` with API version, kind, name, UID and namespace where applicable, `metricName`, `timestamp` (evaluation time), `windowSeconds` (statistic window), and `value`. The five-minute CPU smoothing interval is documented separately; it does not replace `windowSeconds`.
- `labelSelector` selects API objects for wildcard requests. Nonempty object selectors on named requests are rejected with `400`. `metricLabelSelector` is a separate selector channel; nonempty values are unsupported in v1 and return `400`, never silently ignored.
- Malformed metric syntax/selectors → `400`; unknown or unsupported metric/resource, missing named object, or named object with no eligible retained members or verified inactivity throughout the window → `404`. Invalid/untrusted authentication → `401`; denied requests are rejected by kube-apiserver with `403` before forwarding. Gateway ServiceAccount permission/configuration failures → `503`, not caller `403`.
- Backend/protocol failures, partial backend responses, stale active input or incomplete expected coverage → `503`; exceeded per-query or total computation deadline → `504`; admission saturation → `429` with `Retry-After`; local count/size limits in §6.3 → `413`. All errors use Kubernetes `Status` objects with matching HTTP/status codes and sanitized messages. A backend's own sample-limit failure is a backend error (`503`), not proof of a local request-size violation.
- Wildcards omit objects with no eligible retained members or verified inactivity throughout the window, return the remaining items sorted by namespace/name/UID, and return an empty list when all are absent. Missing usage for expected-active members or unknown lifecycle/completeness is always `503`, even when every usage series is missing. Wildcards fail as a whole on these or other operational errors; do not return a misleading partial success. A successful list describes available measurements, not an inventory of all objects.

### 3.7 Coverage and freshness

The normalized input contract (§8) includes historical expected-member and completeness signals, not just usage. Require one valid sample for every expected active member at every 15-second evaluation point in the requested window. Distinguish explicitly inactive members from missing telemetry; missing completeness signals are failures, not inactivity. Reject windows with gaps rather than summing a silently reduced set. Validate historical coverage before aggregation, including container coverage inside normalized pod values.

An active member's underlying source sample must be no older than 30 seconds at the relevant grid point. Recording-rule evaluation time alone is not source freshness; normalization must gate on source timestamps and successful scrapes. Completed retained members may legitimately have only historical active intervals. If no members were active anywhere in the window, the metric is absent (`404` for named requests).

---

## 4. Cache Design

### Tier 1 — Response Cache

| Window | TTL |
| :--- | :--- |
| `≤ 1h` | **15s** |
| `> 1h` | **10m** |

- Key: `(catalogRevision, verb, namespace, groupResource, objectName, metricName, objectSelectorHash, metricSelectorHash)`. Hash canonical selector encodings; reject unsupported selectors before lookup. Cluster/backend configuration is immutable per process.
- Value: immutable `MetricValueList` data including original evaluation timestamp and object UIDs. Serialize per request at the HTTP edge, avoiding content-negotiation-dependent byte caches.
- Eviction: LRU bounded by entries (`--cache-size`, default 10000) and accounted bytes (`--cache-max-bytes`, default 64 MiB). Entries larger than 1 MiB are returned but not cached; the separate response limit still applies.
- Invalidation: TTL or process/catalog rollout. Freshness/coverage checks apply at computation time; cached values can then be as old as the TTL. Do not rewrite timestamps on hits. Deletion/recreation can leave old UIDs visible until expiry; this is an explicit bounded-staleness contract.
- Caller identity is intentionally absent: endpoint authorization grants the corresponding metric data, and resource resolution always uses the same ServiceAccount. Authentication still runs on every request, before cache lookup. Upstream authorization still runs on every proxied request. No caller-specific resource permissions are cached or checked.
- Cache successful complete results only, including valid empty wildcard lists. Do not cache errors.

### Tier 2 — Singleflight

`golang.org/x/sync/singleflight` collapses concurrent identical queries. Critical for `btop --watch` fan-out and HPA storms.

Each waiter can cancel independently. Shared work has a server-owned context bounded by `--request-timeout` and server shutdown, not the first caller's context; one disconnected caller must not cancel other waiters. Bound shared work and waiting requests separately (§6.3). Cache and singleflight are process-local; replicas can compute independently and return different evaluation timestamps within the TTL contract.

### Cache Metrics

```
gateway_cache_hits_total{resource,metric}
gateway_cache_misses_total{resource,metric}
gateway_singleflight_collapsed_total
gateway_prom_query_duration_seconds_bucket{le}
gateway_prom_query_duration_seconds_sum
gateway_prom_query_duration_seconds_count
gateway_query_errors_total{reason}
gateway_cronjob_fallback_total
gateway_cronjob_notfound_total
```

---

## 5. Authorization Model

**Authorization to a custom-metrics request is sufficient to receive its corresponding metric data.** Callers do not need `get`/`list` access to the underlying Pods, Nodes or workloads. This does not grant them those Kubernetes API permissions or unrestricted access to other custom-metrics endpoints; kube-apiserver still authorizes each path, namespace and resource/subresource.

- kube-apiserver authenticates and authorizes every request before proxying it. Operators grant the appropriate resource/metric subresources in `custom.metrics.k8s.io`, rather than treating API discovery access as permission to read all metrics.
- The gateway performs no SubjectAccessReview and no caller impersonation. It uses its own ServiceAccount for all object resolution. Shared metric caching is valid under this identity-independent policy.
- Resource routes accept **only authenticated aggregation-proxy requests**. Validate the proxy client certificate against the request-header CA and allowed proxy CNs before accepting configured identity headers (normally `X-Remote-User`, `X-Remote-Group`, and extras). `Impersonate-*` headers are not the inbound identity protocol. Use the authenticated request context for audit attribution, never unvalidated raw headers.
- Read and watch `kube-system/extension-apiserver-authentication` through the standard authentication-reader RoleBinding. Use its request-header configuration, require a nonempty allowed-proxy-name list, and reload CA/name changes. Fail closed when the trust configuration is unavailable or invalid. Do not confuse the serving-certificate CA, the ordinary Kubernetes client CA, and the front-proxy CA.
- No ordinary bearer-token, end-user client-certificate, or anonymous fallback is permitted on resource routes. A NetworkPolicy limits access to the control plane as defense in depth, but cannot replace proxy certificate validation. Anyone controlling a trusted proxy credential is inside this trust boundary.
- The exact `/healthz`, `/livez`, and `/readyz` paths may be anonymous and must return no sensitive data. `/metrics` uses a separate configured monitoring-client CA and allowed CNs; monitoring credentials cannot access API resource routes, and proxy identity headers have no effect on monitoring authorization. No other unauthenticated endpoints are installed.
- No `--rbac-enforce` flag exists. Generic-apiserver defaults must not silently enable direct access or delegated SAR. The no-SAR mode must be an explicit, tested proxy-only configuration.

**RBAC:** gateway ServiceAccount needs `get`/`list` on Pods, Nodes and supported workloads, plus the authentication-reader binding. It needs no `impersonate`, TokenReview, or SubjectAccessReview permission. Supporting direct authenticated clients later requires a separate authorization design, not relaxing proxy-only checks.

---

## 6. Configuration

### 6.1 Gateway Flags / Env Vars

| Flag | Env Var | Default | Purpose |
| :--- | :--- | :--- | :--- |
| `--prometheus-url` | `GATEWAY_PROMETHEUS_URL` | — | Backend endpoint |
| `--prometheus-timeout` | `GATEWAY_PROM_TIMEOUT` | `10s` | Per-query timeout |
| `--prometheus-max-conns` | `GATEWAY_PROM_MAX_CONNS` | `100` | Connection pool |
| `--cluster` | `GATEWAY_CLUSTER` | — | Required exact backend cluster label |
| `--prometheus-ca-file` | `GATEWAY_PROM_CA` | system roots | Backend TLS trust |
| `--prometheus-token-file` | `GATEWAY_PROM_TOKEN_FILE` | — | Optional bearer-token file; never log its contents |
| `--request-timeout` | `GATEWAY_REQUEST_TIMEOUT` | `30s` | Total computation deadline |
| `--max-inflight-requests` | `GATEWAY_MAX_INFLIGHT` | `128` | Admitted metric requests including waiters |
| `--max-shared-computations` | `GATEWAY_MAX_COMPUTATIONS` | `32` | Live distinct computations, including queued work |
| `--max-concurrent-queries` | `GATEWAY_MAX_QUERIES` | `16` | Global backend query concurrency |
| `--cache-ttl-short` | `GATEWAY_CACHE_TTL_SHORT` | `15s` | TTL for `window ≤ 1h` |
| `--cache-ttl-long` | `GATEWAY_CACHE_TTL_LONG` | `10m` | TTL for `window > 1h` |
| `--cache-size` | `GATEWAY_CACHE_SIZE` | `10000` | LRU cap |
| `--cache-max-bytes` | `GATEWAY_CACHE_MAX_BYTES` | `67108864` | Accounted cache byte cap |
| `--catalog-path` | `GATEWAY_CATALOG_PATH` | `/etc/gateway/catalog.yaml` | Catalog mount |
| `--cronjob-fallback-window` | `GATEWAY_CRONJOB_FALLBACK` | `24h` | Recent-Job lookback |
| `--tls-cert-file` | `GATEWAY_TLS_CERT` | — | Serving cert |
| `--tls-private-key-file` | `GATEWAY_TLS_KEY` | — | Serving key |
| `--metrics-client-ca-file` | `GATEWAY_METRICS_CLIENT_CA` | — | Optional dedicated monitoring-client CA |
| `--metrics-client-names` | `GATEWAY_METRICS_CLIENT_NAMES` | — | Allowed monitoring certificate CNs; required with monitoring CA |
| `--secure-port` | `GATEWAY_SECURE_PORT` | `6443` | Listen port |
| `--log-level` | `GATEWAY_LOG_LEVEL` | `info` | |
| `--discovery-max-metrics` | `GATEWAY_DISCOVERY_MAX` | `1000` | Resource/metric entry cap |

Request-header trust comes from the authentication ConfigMap, not a general `--client-ca-file`. Without monitoring credentials configured, `/metrics` denies access; the two monitoring flags must be supplied together. TLS verification is never disabled for Prometheus; reject URLs containing userinfo and do not forward credentials across redirects.

### 6.2 btop Flags / Env Vars

| Flag | Env Var | Default | Purpose |
| :--- | :--- | :--- | :--- |
| `--window` | `btop_WINDOW` | `5m` | Statistical window |
| `--stat` | `btop_STAT` | `avg` | Statistic |
| `--selector` / `-l` | `btop_SELECTOR` | — | Label selector |
| `--namespace` / `-n` | `btop_NAMESPACE` | ctx ns | |
| `--no-headers` | `btop_NO_HEADERS` | `false` | |
| `--sort-by` | `btop_SORT_BY` | `name` | `cpu`, `memory`, `name` |
| `--output` / `-o` | `btop_OUTPUT` | `table` | `table`, `json`, `yaml` |
| `--watch` / `-w` | `btop_WATCH` | `false` | |
| `--watch-interval` | `btop_WATCH_INTERVAL` | `5s` | |
| `--request-timeout` | `btop_REQUEST_TIMEOUT` | `35s` | Bound discovery and metric HTTP requests |
| `--kubeconfig` | `KUBECONFIG` | — | |
| `--context` | `btop_CONTEXT` | — | |

Resolve explicit flags before parsing overridden environment values: an invalid env value must not defeat a valid explicit flag. Reject malformed effective values. `KUBECONFIG` retains client-go's path-list merging behavior; only an explicit `--kubeconfig` becomes an explicit single-file override. Reject namespace flags on nodes and `--selector` together with a named object. `--containers` is deferred and rejected in v1; aggregate pod metrics cannot be decomposed by the client.

### 6.3 Work and memory budgets

- Cap admitted metric requests at 128 by default; reject excess immediately with `429` and `Retry-After: 1`. Health/discovery handling does not wait on expensive metric work.
- Independently cap live distinct shared computations at 32 by default, including queued work. Hold that slot until computation ends, even if all callers disconnect; joining an existing flight needs no new computation slot. Reject creation of an excess flight with `429` and `Retry-After: 1`.
- By default at most 16 backend queries run concurrently per process; at most four per shared computation is a fixed v1 limit. All queueing, Kubernetes listing, coverage checks and backend queries count toward the total deadline (default 30 seconds). The per-query timeout (default 10 seconds) is additionally enforced. No automatic backend retries in v1.
- Fixed v1 safety limits: 500 target objects, 5,000 selected Pods, 1 MiB rendered query, 16 MiB decompressed backend response and 8 MiB encoded API response. Enforce limits while reading/listing, not after unbounded accumulation. Over-budget requests fail with `413` (`RequestEntityTooLarge`) rather than truncating results. Configure the backend with a query sample limit as well; normalized input and concurrency limits do not bound Prometheus's internal execution memory.
- Kubernetes lists use bounded pages and propagate contexts. A single oversized selection is not split into independently aggregated results: doing so would corrupt quantiles and other nonlinear statistics.
- No server watch flag exists: polling is ordinary GET traffic. `btop` enforces a minimum interval of two seconds locally, but server admission limits apply to every client, including HPA.

---

## 7. `btop` Plugin

### 7.1 Command Surface

```
kubectl btop pods         [NAME] [-n NS] [-l SEL] [--window] [--stat] [-w]
kubectl btop nodes        [NAME]        [-l SEL] [--window] [--stat] [-w]
kubectl btop deployments  [NAME] [-n NS] [-l SEL] [--window] [--stat] [-w]
kubectl btop statefulsets [NAME] [-n NS] [-l SEL] [--window] [--stat] [-w]
kubectl btop daemonsets   [NAME] [-n NS] [-l SEL] [--window] [--stat] [-w]
kubectl btop jobs         [NAME] [-n NS] [-l SEL] [--window] [--stat] [-w]
kubectl btop cronjobs     [NAME] [-n NS] [-l SEL] [--window] [--stat] [-w]
```

### 7.2 Table Output

```
$ kubectl btop pods --window=5m --stat=p95
NAMESPACE   NAME      CPU(p95,5m)   MEMORY(p95,5m)
prod        web-0     187m          412Mi
prod        web-1     203m          428Mi
prod        api-0     88m           156Mi

$ kubectl btop deployments -n prod --window=1h --stat=max
NAMESPACE   NAME   CPU(max,1h)   MEMORY(max,1h)
prod        web    412m          512Mi
prod        api    210m          256Mi

$ kubectl btop nodes --window=5m --stat=avg
NAME           CPU(avg,5m)   MEMORY(avg,5m)
worker-1       2.4           12Gi
worker-2       1.8           9Gi
```

### 7.3 Flattened JSON Output

`-o json` returns a flat array — one object per row, `cpu` and `memory` as sibling fields:

```json
[
  {"namespace": "prod", "name": "web-0", "cpu": "187m", "memory": "412Mi"},
  {"namespace": "prod", "name": "web-1", "cpu": "203m", "memory": "428Mi"},
  {"namespace": "prod", "name": "api-0", "cpu": "88m",  "memory": "156Mi"}
]
```

For nodes, `namespace` is omitted. For workloads, `name` is the workload name.

YAML output mirrors the same shape.

Rows include only objects with both CPU and memory values. Join by group/resource, namespace, name and UID; different incarnations must never be paired. A mismatched set fails a non-watch command without writing a partial document. Both empty lists produce `[]`. Sort CPU/memory numerically descending, with namespace/name tie-breaks; `name` sorts ascending. Diagnostics go to stderr. Exit codes: `0` success or clean watch quit, `2` usage/configuration error, `1` API/data/output error.

Retain timestamps internally. Different CPU/memory timestamps are allowed because the API provides no atomic multi-metric snapshot; report their evaluation range/age on stderr (and in the watch status) rather than presenting a refresh time as measurement time. Do not rewrite timestamps or bypass the cache by varying selectors. A conforming external custom-metrics server must also expose this project's metric naming/unit contract for `btop` to work; protocol conformance alone is insufficient.

### 7.4 Watch Mode

`kubectl btop pods -w` renders a live-updating table:

- Poll at `--watch-interval` (default 5s, minimum 2s); never overlap refreshes, skip missed ticks, and respect `Retry-After`.
- TTY table output uses an ANSI alternate screen with terminal state restored on every exit. Header includes refresh time and the measurement evaluation range/age. `q` / `Ctrl-C` quits cleanly.
- Unix resize handling uses SIGWINCH; Windows uses the terminal library's resize support. Platform-specific signal code must be build-tagged. If terminal features are unavailable, fall back to non-TTY rendering.
- Non-TTY table mode emits timestamped table blocks per refresh, with no cursor controls. JSON emits one compact array per line (NDJSON snapshots); YAML emits `---`-separated documents. Structured output never enters alternate-screen mode or writes status text to stdout.
- On refresh failure, retain but visibly mark the last TTY snapshot stale; non-TTY modes emit a stderr diagnostic and no data snapshot for that refresh. Clear stale state only after a successful refresh. Failures before the first snapshot exit `1`; subsequent transient `429`/`503`/`504` errors may retry at the next eligible interval. Permanent errors exit `1`.

```
btop — pods (avg, 5m) — ns: prod — 10:42:03
──────────────────────────────────────────────
NAMESPACE   NAME      CPU          MEMORY
prod        web-0     187m         412Mi
prod        web-1     203m         428Mi
prod        api-0     88m          156Mi
──────────────────────────────────────────────
refreshing every 5s · q to quit
```

### 7.5 Distribution

**Raw binary only in v1.** Release artifacts:
- `btop-linux-amd64`, `btop-linux-arm64`
- `btop-darwin-amd64`, `btop-darwin-arm64`
- `btop-windows-amd64.exe`
- SHA256 checksums + cosign signature

Install: place on `$PATH` as `kubectl-btop`. Krew/Homebrew deferred to v2.

---

## 8. Deployment Topology

| Component | Kind | Notes |
| :--- | :--- | :--- |
| `metrics-gateway` | Deployment (2–3 replicas) | Stateless |
| `metrics-gateway` | Service (ClusterIP :443 → :6443) | |
| `metrics-gateway-certs` | cert-manager `Certificate` | SAN `metrics-gateway.<ns>.svc` |
| `v1beta2.custom.metrics.k8s.io` | `APIService` | `caBundle` from cert-manager |
| `metrics-gateway` | ServiceAccount + ClusterRole | Get/list Pods, Nodes and workloads; no impersonation |
| `metrics-gateway-auth-reader` | RoleBinding in `kube-system` | Bind `extension-apiserver-authentication-reader` to gateway ServiceAccount |
| `metrics-gateway` | NetworkPolicy | Control-plane API ingress, monitoring ingress, required egress |
| `metrics-gateway-catalog` | ConfigMap | Base metric definitions |
| `metrics-gateway` | PodDisruptionBudget | `minAvailable: 1` |
| `metrics-gateway` | ServiceMonitor | Optional; dedicated monitoring client certificate and serving-CA verification |

cert-manager and the Prometheus Operator CRDs are optional integrations, not unconditional install prerequisites. Provide ordinary Secret/TLS and scraping examples too. Monitor and reload serving certificates before expiry; set termination grace time greater than the request deadline and drain on SIGTERM. Only one APIService can own a group/version: installation must detect an existing custom-metrics adapter and refuse to replace it silently. Discovery stays independent of Prometheus availability; readiness reflects backend checks, liveness does not.

### Normalized Prometheus input contract

v1 consumes deployment-supplied normalized recording rules, not arbitrary raw cAdvisor/node-exporter layouts. Shipping tested rules/fixtures for at least one supported scrape setup is a release prerequisite; the catalog below is not a complete monitoring installation.

- All series carry a required `cluster` label. Pod series carry `namespace`, `pod`, `uid`; node series carry `node`, `uid`. UID must identify the actual Kubernetes incarnation. Rules must preserve historical identity at recording time, not join historical name-only usage to today's metadata.
- Emit exactly one usage series per identity and metric at each 15-second step. Select a single scrape source or explicitly deduplicate HA replicas before aggregation. Exclude root cgroups, empty container identities and pause containers. Include running application containers, native sidecars, init and ephemeral containers when they consume resources; sum only once per actual container identity.
- `pod_cpu_usage_cores` is the sum of five-minute per-container counter rates (rate before sum); `pod_memory_working_set_bytes` is summed container working set.
- `node_cpu_usage_cores` sums five-minute `node_cpu_seconds_total` rates over CPU cores and modes `user`, `nice`, `system`, `irq`, `softirq`, `steal`. Exclude idle, iowait and guest/guest_nice to avoid idle accounting and guest double counting. `node_memory_used_bytes` is `MemTotal - MemAvailable`, not kubelet working set. Node and pod memory therefore have explicitly different definitions.
- Emit `pod_active` / `node_active` and `pod_cpu_complete`, `pod_memory_complete`, `node_cpu_complete`, `node_memory_complete`, with the corresponding identity labels. Active is 0/1 and complete is 1 only when expected-container coverage and source freshness hold; emit 0 for incomplete coverage. Missing signals are unknown, never implicitly inactive. Lifecycle signals cover each retained object's lifetime, including completed intervals; pre-creation times are known inactive from the object's creation timestamp. Missing post-creation lifecycle history makes the window unavailable.
- Validate these signals over the same historical grid as usage before computing a statistic. Backend warnings/partial responses, duplicate identities, and stale or missing expected inputs fail closed. Normalization must not mask missing containers by merely summing whichever samples are present.

### Catalog ConfigMap

```yaml
bases:
  cpu:
    series: pod_cpu_usage_cores
    unit: cores
    scope: pod
    aggregation: sum-then-stat
  memory:
    series: pod_memory_working_set_bytes
    unit: bytes
    scope: pod
    aggregation: sum-then-stat
  node_cpu:
    series: node_cpu_usage_cores
    unit: cores
    scope: node
    aggregation: sum-then-stat
  node_memory:
    series: node_memory_used_bytes
    unit: bytes
    scope: node
    aggregation: stat-then-sum
```

All inputs are normalized gauges; raw counter handling belongs in recording rules. The four base names have fixed units/scopes; series names may be changed only to equivalent normalized inputs. Reject unknown YAML fields, arbitrary PromQL expressions, invalid metric identifiers, incompatible units/scopes, unknown bases, and an empty catalog. Removing a base removes its endpoints from discovery. Parse metric names by matching a known base then validating stat/window, not by splitting every underscore. Completeness/lifecycle series names and label schema are fixed in v1.

---

## 9. Request Flow — Workload Aggregated Value

```
btop deployments web --window=1h --stat=p95 -n prod
        │
        ▼
GET /apis/custom.metrics.k8s.io/v1beta2/namespaces/prod/deployments.apps/web/cpu_p95_1h
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ apiserver: authn + authz against custom.metrics.k8s.io      │
└──────────────────────┬──────────────────────────────────────┘
                       ▼
┌─────────────────────────────────────────────────────────────┐
│ Gateway                                                     │
│  1. Verify trusted proxy, then parse metric                │
│  2. Cache lookup (TTL 15s)  ──hit──▶ return                 │
│  3. ServiceAccount: Deployment → full selector → Pod UIDs   │
│  4. Validate normalized input coverage for selected UIDs   │
│  5. Sum pod usage at each step, then p95 → single value     │
│  6. Cache store (TTL 15s)                                   │
│  7. Return one-item MetricValueList                         │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
{
  "kind": "MetricValueList",
  "apiVersion": "custom.metrics.k8s.io/v1beta2",
  "metadata": {},
  "items": [{
    "metricName": "cpu_p95_1h",
    "value": "412m",
    "timestamp": "2026-09-11T10:42:00Z",
    "windowSeconds": 3600,
    "describedObject": {
      "apiVersion": "apps/v1", "kind": "Deployment",
      "name": "web", "namespace": "prod", "uid": "deployment-uid"
    }
  }]
}
```

---

## 10. CronJob Request Flow

```
btop cronjobs nightly-batch --window=6h --stat=avg -n prod
        │
        ▼
GET /apis/custom.metrics.k8s.io/v1beta2/namespaces/prod/cronjobs.batch/nightly-batch/cpu_avg_6h
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ Gateway                                                     │
│  1. Verify trusted proxy, then parse metric                │
│  2. Cache lookup (TTL 10m)                                  │
│  3. Resolve CronJob → active Jobs (status.active > 0)       │
│     ├── found ──▶ full selectors → retained Pod UID union  │
│     └── none ──▶ Jobs started or completed within 24h       │
│                  ├── found ──▶ retained Pod UID union       │
│                  └── none ──▶ 404 NotFound                  │
│  4. Validate coverage; sum-then-stat over selected UIDs     │
│  5. Cache store (TTL 10m)                                   │
│  6. Return one-item MetricValueList                         │
└─────────────────────────────────────────────────────────────┘
```

---

## 11. v1 Scope

**In scope:**
- Bases: `cpu`, `memory`, `node_cpu`, `node_memory`
- Stats: `avg`, `max`, `min`, `p50`, `p90`, `p95`, `p99`, `stddev`
- Windows: `1m`, `5m`, `15m`, `1h`, `6h`, `12h`, `24h`
- Resources: pods, nodes, deployments, statefulsets, daemonsets, jobs, cronjobs
- Single aggregated value per workload; per-node list for node wildcards
- CronJob 1-day fallback with 404 on empty
- Tier-1 + Tier-2 cache with window-driven TTL
- `btop` plugin with watch mode and flattened JSON
- Raw binary distribution
- Apiserver endpoint authorization, trusted-proxy authentication, ServiceAccount resolution
- Normalized recording-rule inputs with identity, coverage and freshness validation
- Prometheus self-metrics
- CLI/env configuration

---

## 12. Next Artifacts

Before release, deliver and test:

1. **API contract document** — exact request/response schemas, error codes, discovery payload.
2. **`btop` command reference** — every flag, every subcommand, exit codes, examples.
3. **Sequence diagrams** — pod, node, workload, CronJob-fallback, cache-hit, cache-miss.
4. **Catalog schema** — formal YAML schema for base metric definitions.
5. **RBAC manifests** — gateway read-only ClusterRole, authentication-reader binding, and example HPA metric-reader bindings (no caller resource-read requirement).
6. **Normalized recording rules and numerical fixtures** — at least one supported scrape setup, including node mapping, lifecycle, completeness, historical gaps and duplicate-source tests.
7. **Security integration tests** — direct-access/header-spoof rejection, proxy CA/name rotation, monitoring isolation, and successful metric reads by users lacking underlying resource permissions on cache hits and misses.

Deferred: temporal `sum`, per-container API/output, complete historical ownership reconstruction, direct-client gateway authorization, and server-side watch. These require explicit contracts rather than silent approximations.
