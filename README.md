# Custom metrics for Kubernetes APIs

Expose historical CPU and memory usage through a Kubernetes API and a kubectl plugin.

## Motivation

Kubernetes shows current usage but not historical behavior or peaks, which makes right sizing hard.

## Goals

- Provide time windowed CPU and memory metrics such as max, average, and min, from Prometheus compatible backends.
- Support Pods and higher-level workloads.
- Expose them via Custom Metrics API.
- Provide a kubectl plugin with a top-like feel for quick checks.
