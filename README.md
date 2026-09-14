# Custom metrics for Kubernetes APIs

Expose historical CPU and memory usage through a Kubernetes API and a kubectl plugin.

## Motivation

The Metrics API, typically provided by metrics-server, exposes recent CPU and memory usage for Pods and Nodes and powers commands such as kubectl top. However, metrics-server keeps only the latest values in memory and does not provide historical data, long-term trends, or previous usage peaks.

In daily Kubernetes operations, historical resource usage is very useful. It helps operators make better decisions about scaling, capacity planning, resource requests and limits, and cost optimization.

Making historical metrics available through the Kubernetes API (custom.metrics.k8s.io) also allows Kubernetes addons, operators, and custom controllers to use this data directly. They can make decisions based on how workloads behaved over time, instead of using only the current resource usage.

## Goals

- Provide time windowed CPU and memory metrics such as max, average, and min, from Prometheus compatible backends.
- Support Pods and higher-level workloads like Deployments, StatefulSets, DaemonSets  CronJobs.
- Expose them via Custom Metrics API.
- Provide a kubectl plugin with a top-like feel for quick checks.

## Usage

After installing the API server and the kubectl plugin, you can use the plugin to check historical CPU and memory usage for your workloads. For example:

```sh
kubectl ctop pods -n default
```

You can also specify different time windows and metrics, such as:

```sh
kubectl ctop pods -n default --window=1h --stat=avg
kubectl ctop deployments -n default --window=24h --stat=max
```

## Installation

The project has two parts: the custom metrics API server, which runs in the cluster, and the
`kubectl ctop` client plugin, which runs on your workstation.

### Server

Install the API server with Helm from the OCI registry:

```sh
helm upgrade --install kubernetes-custom-metrics \
  oci://ghcr.io/sergelogvinov/charts/kubernetes-custom-metrics \
  --namespace kube-system --set-string args="--prometheus-url=http://prometheus-server"
```

The chart installs the `custom.metrics.k8s.io` APIService.
Configure the chart values for your Prometheus-compatible backend and catalog as needed,
see [Configuration](charts/kubernetes-custom-metrics/README.md).

### kubectl plugin

Install the `kubectl-ctop` plugin with Homebrew:

```sh
brew install sergelogvinov/tap/kubectl-ctop
```

The plugin is then available through kubectl:

```sh
kubectl ctop pods -n default
```

## Contributing

## References

* [Kubernetes Custom Metrics API](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/#custom-metrics-api)
* [Kubernetes Custom Metrics Apiserver](https://github.com/kubernetes-sigs/custom-metrics-apiserver/tree/master)
* [Prometheus](https://prometheus.io/docs/introduction/overview/)
* [VictoriaMetrics](https://victoriametrics.com/)

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
