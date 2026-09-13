/*
Copyright 2026 Kubernetes Authors.

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

package prometheus

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireBinary skips the test when name is not on PATH, so environments
// without a Prometheus toolchain (e.g. minimal CI images) skip gracefully
// rather than failing, matching how the repository's `make test-rules`
// already treats promtool as an optional, explicitly-invoked tool.
func requireBinary(t *testing.T, name string) {
	t.Helper()

	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not found on PATH, skipping real-Prometheus fixture", name)
	}
}

// TestClient_Query_RealPrometheus_AggregationOrderDistinction is the
// plan.md T4 Done-criterion "numeric fixtures against a real Prometheus
// instance validate at least one aggregation-order distinction end-to-end."
//
// It backdates synthetic pod_cpu_usage_cores/pod_active/pod_cpu_complete
// samples directly into TSDB blocks (via `promtool tsdb
// create-blocks-from openmetrics`) instead of waiting on real-time
// scraping, then runs a real `prometheus` server against those blocks and
// issues Client.Query's actual rendered PromQL through its real query
// engine — proving the rendered queries execute correctly, not just that
// they parse.
//
// Fixture: two pods with a single clean flip at the window's midpoint —
// web-0 hot (1.0 core) for the first two 60s grid points then cold (0.1),
// web-1 the exact reverse. Because max is nonlinear (unlike avg, which
// commutes with sum and would show no distinction here), sum-then-stat
// (max of the combined per-step sum, which is 1.1 at every step since
// exactly one pod is hot at a time) and stat-then-sum (each pod's own
// max — 1.0 apiece — summed) diverge cleanly: 1.1 vs 2.0.
func TestClient_Query_RealPrometheus_AggregationOrderDistinction(t *testing.T) {
	requireBinary(t, "promtool")
	requireBinary(t, "prometheus")

	dir := t.TempDir()

	t0 := time.Now().Add(-15 * time.Minute).Truncate(60 * time.Second)
	steps := []time.Time{
		t0,
		t0.Add(60 * time.Second),
		t0.Add(120 * time.Second),
		t0.Add(180 * time.Second),
		t0.Add(240 * time.Second),
	}
	web0 := []float64{1.0, 1.0, 0.1, 0.1, 0.1}
	web1 := []float64{0.1, 0.1, 1.0, 1.0, 1.0}

	inputPath := filepath.Join(dir, "input.om")
	if err := os.WriteFile(inputPath, []byte(buildOpenMetrics(steps, web0, web1)), 0o600); err != nil {
		t.Fatalf("writing openmetrics input: %v", err)
	}

	dataDir := filepath.Join(dir, "data")
	out, err := exec.CommandContext(context.Background(), "promtool", "tsdb", "create-blocks-from", "openmetrics", inputPath, dataDir).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool tsdb create-blocks-from: %v\n%s", err, out)
	}

	port := freePort(t)
	cfgPath := filepath.Join(dir, "prometheus.yml")
	if err := os.WriteFile(cfgPath, []byte("global:\n  scrape_interval: 60s\n"), 0o600); err != nil {
		t.Fatalf("writing prometheus.yml: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	promCmd := exec.CommandContext(ctx, "prometheus",
		"--config.file="+cfgPath,
		"--storage.tsdb.path="+dataDir,
		"--storage.tsdb.retention.time=100y",
		fmt.Sprintf("--web.listen-address=127.0.0.1:%d", port),
		"--log.level=error",
	)
	var stderr strings.Builder
	promCmd.Stderr = &stderr

	if err := promCmd.Start(); err != nil {
		t.Fatalf("starting prometheus: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = promCmd.Wait()
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForReady(t, baseURL, &stderr)

	client, err := NewClient(ClientConfig{URL: baseURL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req := Request{
		Cluster:   "test",
		Series:    "pod_cpu_usage_cores",
		Scope:     ScopePod,
		Quantity:  QuantityCPU,
		Stat:      StatMax,
		Window:    240 * time.Second,
		Namespace: "prod",
		Names:     []string{"web-0", "web-1"},
		QueryTime: steps[len(steps)-1],
	}

	req.Aggregation = AggregationSumThenStat
	sumThenStat, ok, err := client.Query(context.Background(), req)
	if err != nil || !ok {
		t.Fatalf("sum-then-stat Query: ok=%v err=%v", ok, err)
	}

	req.Aggregation = AggregationStatThenSum
	statThenSum, ok, err := client.Query(context.Background(), req)
	if err != nil || !ok {
		t.Fatalf("stat-then-sum Query: ok=%v err=%v", ok, err)
	}

	if math.Abs(sumThenStat.Value-1.1) > 0.01 {
		t.Errorf("sum-then-stat value = %v, want ~1.1", sumThenStat.Value)
	}
	if math.Abs(statThenSum.Value-2.0) > 0.01 {
		t.Errorf("stat-then-sum value = %v, want ~2.0", statThenSum.Value)
	}
	if statThenSum.Value-sumThenStat.Value < 0.5 {
		t.Errorf("expected a clear aggregation-order distinction: sum-then-stat=%v stat-then-sum=%v",
			sumThenStat.Value, statThenSum.Value)
	}
}

func buildOpenMetrics(steps []time.Time, web0, web1 []float64) string {
	var b strings.Builder

	writeUsage := func(pod, uid string, values []float64) {
		for i, ts := range steps {
			fmt.Fprintf(&b, "pod_cpu_usage_cores{cluster=\"test\",namespace=\"prod\",pod=\"%s\",uid=\"%s\"} %s %d\n",
				pod, uid, strconv.FormatFloat(values[i], 'f', -1, 64), ts.Unix())
		}
	}
	writeConstant := func(name, pod, uid string) {
		for _, ts := range steps {
			fmt.Fprintf(&b, "%s{cluster=\"test\",namespace=\"prod\",pod=\"%s\",uid=\"%s\"} 1 %d\n", name, pod, uid, ts.Unix())
		}
	}

	b.WriteString("# TYPE pod_cpu_usage_cores gauge\n")
	writeUsage("web-0", "uid-web-0", web0)
	writeUsage("web-1", "uid-web-1", web1)

	b.WriteString("# TYPE pod_active gauge\n")
	writeConstant("pod_active", "web-0", "uid-web-0")
	writeConstant("pod_active", "web-1", "uid-web-1")

	b.WriteString("# TYPE pod_cpu_complete gauge\n")
	writeConstant("pod_cpu_complete", "web-0", "uid-web-0")
	writeConstant("pod_cpu_complete", "web-1", "uid-web-1")

	b.WriteString("# EOF\n")

	return b.String()
}

func freePort(t *testing.T) int {
	t.Helper()

	var lc net.ListenConfig

	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer func() { _ = l.Close() }()

	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", l.Addr())
	}

	return addr.Port
}

func waitForReady(t *testing.T, baseURL string, stderr fmt.Stringer) {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/-/ready", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("prometheus did not become ready in time; stderr:\n%s", stderr.String())
}
