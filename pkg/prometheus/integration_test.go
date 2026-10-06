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

	"github.com/sergelogvinov/kubernetes-custom-metrics/pkg/catalog"
	clocktesting "k8s.io/utils/clock/testing"
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

// TestEvaluate_RealPrometheus checks real numbers against a real Prometheus
// server, including the difference between the two aggregation orders.
//
// It backdates synthetic pod_cpu_usage_cores/pod_active/pod_cpu_complete
// samples directly into TSDB blocks (via `promtool tsdb
// create-blocks-from openmetrics`) instead of waiting on real-time
// scraping, then runs a real `prometheus` server against those blocks and
// issues Client.Evaluate's actual rendered PromQL through its real query
// engine — proving the rendered queries execute correctly, not just that
// they parse.
//
// Fixture: two pods with a single clean flip at the window's midpoint —
// web-0 hot (1.0 core) for the first two 60s grid points then cold (0.1),
// web-1 the exact reverse. Because max is nonlinear (unlike avg, which
// commutes with sum and would show no distinction here), sum-then-stat
// (max of the combined per-step sum, which is 1.1 at every step since
// exactly one pod is hot at a time) and stat-then-sum (each pod's own
// max — 1.0 apiece — summed) diverge cleanly: 1.1 vs 2.0. It also checks
// that one batch query answers several targets against their own identity
// sets, and that raw input sums every selected Pod's containers.
func TestEvaluate_RealPrometheus(t *testing.T) {
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

	client, err := NewClient(ClientConfig{
		URL:     baseURL,
		Cluster: "test",
		Timeout: 5 * time.Second,
		Clock:   clocktesting.NewFakePassiveClock(steps[len(steps)-1].Add(30 * time.Second)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	evaluate := func(base catalog.Base, stat catalog.Stat, targets ...Target) []Sample {
		t.Helper()

		eval, err := client.Evaluate(context.Background(), Computation{
			Base:      base,
			Stat:      stat,
			Window:    240 * time.Second,
			Namespace: "prod",
			Targets:   targets,
		})
		if err != nil {
			t.Fatalf("Evaluate(%s, %s): %v", base.Aggregation, stat, err)
		}
		if !eval.Time.Equal(steps[len(steps)-1]) {
			t.Errorf("Time = %s, want the last grid step %s", eval.Time, steps[len(steps)-1])
		}

		return eval.Samples
	}

	both := Target{Name: "web", Pods: []string{"web-0", "web-1"}}
	cpu := catalog.Base{Series: "pod_cpu_usage_cores", Unit: catalog.UnitCores, Scope: catalog.ScopePod}

	cpu.Aggregation = catalog.AggregationSumThenStat
	sumThenStat := evaluate(cpu, catalog.StatMax, both)[0]

	cpu.Aggregation = catalog.AggregationStatThenSum
	statThenSum := evaluate(cpu, catalog.StatMax, both)[0]

	if !sumThenStat.Present || math.Abs(sumThenStat.Value-1.1) > 0.01 {
		t.Errorf("sum-then-stat = %+v, want ~1.1", sumThenStat)
	}
	if !statThenSum.Present || math.Abs(statThenSum.Value-2.0) > 0.01 {
		t.Errorf("stat-then-sum = %+v, want ~2.0", statThenSum)
	}

	// One batch evaluates several targets through the real query engine,
	// each against its own identity set.
	cpu.Aggregation = catalog.AggregationSumThenStat
	batch := evaluate(cpu, catalog.StatMax,
		Target{Name: "web-0", Pods: []string{"web-0"}},
		both,
		Target{Name: "web-1", Pods: []string{"web-1"}},
		Target{Name: "gone", Pods: []string{"gone-0"}},
	)
	for i, want := range []float64{1.0, 1.1, 1.0} {
		if !batch[i].Present || math.Abs(batch[i].Value-want) > 0.01 {
			t.Errorf("batch[%d] = %+v, want ~%v", i, batch[i], want)
		}
	}
	if batch[3].Present {
		t.Errorf("batch[3] = %+v, want absent (never active)", batch[3])
	}

	// Raw input sums every selected Pod into one value rather than
	// returning one series per Pod.
	rawMemory := catalog.Base{Unit: catalog.UnitBytes, Scope: catalog.ScopePod, Aggregation: catalog.AggregationRaw}
	raw := evaluate(rawMemory, catalog.StatAvg, both)[0]
	if !raw.Present || math.Abs(raw.Value-300) > 0.01 {
		t.Errorf("raw memory = %+v, want 300 (100 + 200)", raw)
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

	b.WriteString("# TYPE container_memory_working_set_bytes gauge\n")
	for pod, value := range map[string]int{"web-0": 100, "web-1": 200} {
		for _, ts := range steps {
			fmt.Fprintf(&b, "container_memory_working_set_bytes{cluster=\"test\",namespace=\"prod\",pod=\"%s\",container=\"app\",image=\"app:v1\"} %d %d\n", pod, value, ts.Unix())
		}
	}

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
