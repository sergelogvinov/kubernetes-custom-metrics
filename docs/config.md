# Configuring the custom metrics gateway

## What this file does

`custom-metrics` does not hard-code "CPU" and "memory" as PromQL queries. 
Instead, it reads a small YAML file called the catalog. 
The catalog defines:

- which Kubernetes-facing metrics it should offer (`cpu`, `memory`, `node_cpu`, and `node_memory`)
- which Prometheus metric supplies each one
- how to combine values from multiple Pods into one value for a Deployment, StatefulSet, or CronJob.

The gateway reads this file once at startup. 
The path comes from `--catalog-path` (or the `CATALOG_PATH` environment variable) and defaults to:

```
/etc/custom-metrics/catalog.yaml
```

In the Helm chart, the file comes from a ConfigMap (`configmap.yaml`) generated from the chart's
`.Values.config` value.

## The default catalog

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

Everything lives under one top-level key, `bases`.
Each entry—`cpu`, `memory`, `node_cpu`, or `node_memory`—is a **base metric**: a metric that
`kubectl btop` and the `custom.metrics.k8s.io` API can report.

## A Prometheus primer

- **Metric / series**: a named, timestamped number that Prometheus stores, for example:
  `container_cpu_usage_seconds_total{pod="web-0", container="app"}`. The part in `{}` is made of
  **labels**—extra tags that identify the Pod, container, or other source of the number.
- **Gauge**: a value that can go up or down and is meaningful to read directly, such as the
  number of bytes currently in use. You can add (`sum()`) multiple gauges and get a meaningful
  result.
- **Counter**: a value that only goes up, such as the total CPU-seconds consumed since a
  container started. A counter is not current CPU usage by itself. Use PromQL's `rate()` function
  to calculate how quickly it is increasing and turn it into a CPU-cores-style gauge over a
  trailing window. Summing or averaging a raw counter produces a meaningless result.
- **Aggregation**: `sum-then-stat`, `stat-then-sum`, or `raw`. This determines how multiple Pod
  or Node series are combined into one value for the base metric.

Each base has exactly four fields, all required:

| Field | Meaning |
| :--- | :--- |
| `series` | The Prometheus metric name to read. Must be a bare metric name (`pod_cpu_usage_cores`) — never a PromQL expression like `rate(...)` or `sum(...)`. |
| `unit` | `cores` or `bytes`. Fixed per base name — see the table below. |
| `scope` | `pod` (applies to Pods + Deployments/StatefulSets/DaemonSets/Jobs/CronJobs) or `node` (applies to Nodes only). Fixed per base name. |
| `aggregation` | `sum-then-stat`, `stat-then-sum`, or `raw`. Your one real choice — see the next section. |

### The four fixed bases: what you can and can't change

| Base name | Unit | Scope | What it measures |
| :--- | :--- | :--- | :--- |
| `cpu` | `cores` | `pod` | Pod-level CPU usage |
| `memory` | `bytes` | `pod` | Pod-level memory usage |
| `node_cpu` | `cores` | `node` | Node-level CPU usage |
| `node_memory` | `bytes` | `node` | Node-level memory usage |

- **You cannot invent a new base name.** The catalog loader rejects anything other than these
  four exact names.
- **You cannot change a base's `unit` or `scope`.** They're fixed by the table above; the loader
  rejects a mismatch (e.g. `cpu` declared with `unit: bytes`).
- **You *can* rename `series`**: point `cpu` at a different metric name if your recording rules
  use different naming, as long as it's still a normalized cores/bytes gauge with the pod/node
  identity labels this project expects (see `monitoring/rules.yaml` for the exact contract).
  This does *not* apply when `aggregation: raw`—see the [raw](#3-raw) section below. `series` is
  still required, but its value is ignored in that mode.
- **You can drop a base entirely** by omitting it from `bases:`. That metric simply won't be
  offered any more (e.g. remove `node_cpu`/`node_memory` if you only care about Pods). At least
  one base must remain — an empty catalog is rejected.

## Aggregation types, the important part

This is the one field where you're actually making a decision, so it gets a full example.

**The scenario:** Imagine a Deployment with two Pods, `web-0` and `web-1`. 
Every minute (the "grid step"), each Pod reports its current CPU usage in cores. 
Over a four-minute window, the raw values look like this:

| Minute | web-0 | web-1 |
| :---: | :---: | :---: |
| 1 | 0.2 | 0.1 |
| 2 | **0.9** | 0.1 |
| 3 | 0.1 | **0.9** |
| 4 | 0.1 | 0.2 |

`web-0` had a brief spike at minute 2, and `web-1` had its own spike at minute 3. 
They never spiked *at the same time*. 
Now see what `--stat=max` reports for this Deployment under each aggregation strategy.

### 1. `sum-then-stat`

**Add the Pods together at each minute, then take the maximum of the combined line.**

| Minute | web-0 + web-1 |
| :---: | :---: |
| 1 | 0.3 |
| 2 | 1.0 |
| 3 | 1.0 |
| 4 | 0.3 |

Result: **max = 1.0 core.** This is the highest total CPU that the Deployment actually used at
any one instant—a true combined peak. This approach also handles Pods scaling up or down during
the window: a Pod that was not running yet simply contributes nothing for those minutes.

This is the **default and recommended choice** for almost everything — it's what "how much CPU is
this Deployment using" normally means. 
It's what the default catalog above uses for `cpu`, `memory`, and `node_cpu`.

### 2. `stat-then-sum`

**Take the maximum of each Pod's line first, then add those maxima together.**

`max(web-0) = 0.9`, `max(web-1) = 0.9` → Result: **max = 1.8 cores.**

That number never actually occurred because the two Pods peaked at different minutes. However,
`stat-then-sum` deliberately answers a different, more pessimistic question: *"if every Pod's
worst moment happened at once, how bad would it get?"* That's a useful, conservative number for
capacity planning (e.g. sizing a Node or a ResourceQuota so it survives Pods' peaks lining up
worst-case), even though it isn't a number that was ever actually observed.

One extra rule applies here specifically: if a Pod was confirmed *not running* during part of the
window (scaled down, not yet started), it counts as `0` for that part rather than being left out
entirely — so a partially-alive Pod doesn't artificially raise or lower the sum depending on when
it happened to report data.

The default catalog uses `stat-then-sum` for `node_memory` (summed per-node peaks are a more
useful "worst combined memory pressure" signal for cluster capacity than a single instant's total).

### Why `avg` doesn't care

For `--stat=avg` specifically, both orderings give the **same answer** — check the numbers above:
`avg(web-0)=0.325`, `avg(web-1)=0.325`, summed = `0.65`; and the averaged combined line
`(0.3+1.0+1.0+0.3)/4` is also `0.65`. Averaging and summing don't interfere with each other. The
aggregation choice only changes the result for non-linear statistics: `max`, `min`, and the
percentiles (`p50`/`p90`/`p95`/`p99`).

### 3. `raw`

The two modes above read from the pre-computed, "normalized" gauge series produced by the
recording rules in `monitoring/rules.yaml` (see the [Prometheus primer](#a-prometheus-primer)
above). `raw` skips that step: it queries Kubernetes' well-known cAdvisor and node-exporter
metrics directly at request time (`container_cpu_usage_seconds_total`,
`container_memory_working_set_bytes`, `node_cpu_seconds_total`, and
`node_memory_MemTotal_bytes`/`MemAvailable_bytes`) and computes the `rate()` and sum on the fly.

```yaml
bases:
  cpu:
    series: pod_cpu_usage_cores   # required field, but its value is IGNORED in raw mode
    unit: cores
    scope: pod
    aggregation: raw
```

Use `raw` when:

- you're evaluating this project and haven't deployed `monitoring/rules.yaml` yet, or
- your cluster genuinely can't run recording rules.

Trade-off: `raw` skips every one of this project's data-quality checks — it does not verify that
every expected Pod actually reported a sample, that the sample is recent, or that every container
inside a Pod was counted. A scrape gap or a kubelet restart can silently produce a slightly wrong
number instead of a clear error. The two normalized modes actively check for and reject exactly
that kind of gap. **Treat `raw` as a quick-start bridge, not the long-term production setting** —
switch to `sum-then-stat`/`stat-then-sum` once the recording rules are running.

### Decision cheat-sheet

| Situation | Pick |
| :--- | :--- |
| Just trying this out, no recording rules deployed yet | `raw` |
| Recording rules are running (normal production setup) | `sum-then-stat` |
| You specifically want a conservative "if every Pod's peak lined up" number | `stat-then-sum` |
| Unsure | `sum-then-stat` — it's what "total usage" means to most people |

## How this fits together with `--stat` and `--window`

The aggregation strategy in the catalog only controls the *Pod-combining* axis. Two more choices
happen later, per request, and are not part of this file at all:

- **`--stat`** (`avg`, `max`, `min`, `p50`, `p90`, `p95`, `p99`, `stddev`) — which statistic to
  apply, e.g. in the worked example above we used `max`.
- **`--window`** (`5m`, `1h`, …) — how far back in time to look.

`window` accepts any duration of `1m` or longer that `time.ParseDuration` can parse; it is not a
fixed enum. The metric-name parser validates the duration by parsing it and checking the minimum.

Temporal `sum` is deferred: `sum_over_time` adds samples and depends on resolution. It is neither
CPU-seconds nor a meaningful memory-usage quantity. Spatial sums across containers and Pods remain
part of aggregation.

Examples: `cpu_avg_5m`, `memory_max_1h`, `node_cpu_p95_12h`, `node_memory_min_24h`.

## Applying a change

1. Edit the catalog YAML, either in the chart's `values.yaml` under `config:` or in your deployed
   configuration.
2. Roll out the change with `helm upgrade`.
   Remember: the gateway only reads this file at startup, so a ConfigMap update alone does nothing
   until the Pods restart.
3. Check `kubectl btop deployments web -n prod` (or your equivalent) to confirm that the expected
   metric names are listed.
