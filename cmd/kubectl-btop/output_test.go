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

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func row(namespace, name, cpu, mem string) joinedRow {
	return joinedRow{
		Namespace: namespace,
		Name:      name,
		CPU:       resource.MustParse(cpu),
		Memory:    resource.MustParse(mem),
	}
}

func names(rows []joinedRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}

	return out
}

func TestSortRows(t *testing.T) {
	tests := []struct {
		name   string
		sortBy string
		rows   []joinedRow
		want   []string
	}{
		{
			name:   "cpu descending with namespace/name tie-break",
			sortBy: "cpu",
			rows: []joinedRow{
				row("prod", "b", "100m", "1Mi"),
				row("prod", "a", "100m", "1Mi"),
				row("prod", "c", "500m", "1Mi"),
			},
			want: []string{"c", "a", "b"},
		},
		{
			name:   "memory descending",
			sortBy: "memory",
			rows: []joinedRow{
				row("prod", "a", "1m", "100Mi"),
				row("prod", "b", "1m", "500Mi"),
			},
			want: []string{"b", "a"},
		},
		{
			name:   "name ascending default",
			sortBy: "name",
			rows: []joinedRow{
				row("prod", "b", "1m", "1Mi"),
				row("prod", "a", "1m", "1Mi"),
			},
			want: []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sortRows(tt.rows, tt.sortBy)
			if got := names(tt.rows); !equalStrings(got, tt.want) {
				t.Errorf("sortRows() = %v, want %v", got, tt.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestToRows_OmitsNamespaceForClusterScoped(t *testing.T) {
	joined := []joinedRow{row("", "worker-1", "1", "1Gi")}

	rows := toRows(joined, false)
	if rows[0].Namespace != "" {
		t.Errorf("toRows() Namespace = %q, want empty for a cluster-scoped resource", rows[0].Namespace)
	}

	rows = toRows([]joinedRow{row("prod", "web-0", "1", "1Gi")}, true)
	if rows[0].Namespace != "prod" {
		t.Errorf("toRows() Namespace = %q, want prod for a namespaced resource", rows[0].Namespace)
	}
}

func TestRenderJSON_EmptyIsEmptyArray(t *testing.T) {
	var buf bytes.Buffer
	if err := renderJSON(&buf, []Row{}); err != nil {
		t.Fatalf("renderJSON() error = %v", err)
	}
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Errorf("renderJSON() = %q, want an empty array, not null", buf.String())
	}
}

func TestRenderJSON_FlattenedShape(t *testing.T) {
	var buf bytes.Buffer
	if err := renderJSON(&buf, []Row{{Namespace: "prod", Name: "web-0", CPU: "187m", Memory: "412Mi"}}); err != nil {
		t.Fatalf("renderJSON() error = %v", err)
	}

	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got[0]["cpu"] != "187m" || got[0]["memory"] != "412Mi" {
		t.Errorf("renderJSON() row = %v, want cpu/memory as flat sibling fields", got[0])
	}
}

func TestRenderJSON_NodeOmitsNamespace(t *testing.T) {
	var buf bytes.Buffer
	if err := renderJSON(&buf, []Row{{Name: "worker-1", CPU: "2.4", Memory: "12Gi"}}); err != nil {
		t.Fatalf("renderJSON() error = %v", err)
	}
	if strings.Contains(buf.String(), "namespace") {
		t.Errorf("renderJSON() = %s, want no namespace field for a node row", buf.String())
	}
}

func TestRenderTable(t *testing.T) {
	o := NewOptions()
	o.Stat = "p95"
	o.Window = "5m"

	var buf bytes.Buffer
	rows := []Row{{Namespace: "prod", Name: "web-0", CPU: "187m", Memory: "412Mi"}}
	if err := renderTable(&buf, o, resourceDescriptor{Namespaced: true}, rows); err != nil {
		t.Fatalf("renderTable() error = %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "CPU(p95,5m)") || !strings.Contains(out, "MEMORY(p95,5m)") {
		t.Errorf("renderTable() = %q, want headers annotated with stat and window", out)
	}
	if !strings.Contains(out, "web-0") {
		t.Errorf("renderTable() = %q, want the row rendered", out)
	}
}

func TestRenderTable_NoHeaders(t *testing.T) {
	o := NewOptions()
	o.NoHeaders = true

	var buf bytes.Buffer
	rows := []Row{{Namespace: "prod", Name: "web-0", CPU: "187m", Memory: "412Mi"}}
	if err := renderTable(&buf, o, resourceDescriptor{Namespaced: true}, rows); err != nil {
		t.Fatalf("renderTable() error = %v", err)
	}
	if strings.Contains(buf.String(), "NAMESPACE") {
		t.Errorf("renderTable() = %q, want no header line with --no-headers", buf.String())
	}
}

func TestRenderTable_NodesOmitNamespaceColumn(t *testing.T) {
	o := NewOptions()

	var buf bytes.Buffer
	rows := []Row{{Name: "worker-1", CPU: "2.4", Memory: "12Gi"}}
	if err := renderTable(&buf, o, resourceDescriptor{Namespaced: false}, rows); err != nil {
		t.Fatalf("renderTable() error = %v", err)
	}
	if strings.Contains(buf.String(), "NAMESPACE") {
		t.Errorf("renderTable() = %q, want no NAMESPACE column for a cluster-scoped resource", buf.String())
	}
}
