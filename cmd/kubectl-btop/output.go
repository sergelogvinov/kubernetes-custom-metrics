package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

// Row is the flattened output shape shared by table, JSON, and YAML
// rendering: cpu and memory are sibling fields, never nested (design.md §12;
// metric-gateway.md §7.3/locked decision 11). Namespace is omitted for
// cluster-scoped resources (Nodes).
type Row struct {
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name      string `json:"name" yaml:"name"`
	CPU       string `json:"cpu" yaml:"cpu"`
	Memory    string `json:"memory" yaml:"memory"`
}

// renderRows sorts joined per o.SortBy and writes it to stdout in o.Output's
// format.
func renderRows(stdout io.Writer, o *Options, desc resourceDescriptor, joined []joinedRow) error {
	sortRows(joined, o.SortBy)
	rows := toRows(joined, desc.Namespaced)

	switch o.Output {
	case "json":
		return renderJSON(stdout, rows)
	case "yaml":
		return renderYAML(stdout, rows)
	default:
		return renderTable(stdout, o, desc, rows)
	}
}

// sortRows orders rows in place per --sort-by: cpu/memory numerically
// descending with a namespace/name tie-break, or name ascending (the
// default; metric-gateway.md §6.2).
func sortRows(rows []joinedRow, sortBy string) {
	switch sortBy {
	case "cpu": //nolint:goconst
		slices.SortFunc(rows, func(a, b joinedRow) int {
			if c := b.CPU.Cmp(a.CPU); c != 0 {
				return c
			}

			return compareIdentity(a, b)
		})
	case "memory": //nolint:goconst
		slices.SortFunc(rows, func(a, b joinedRow) int {
			if c := b.Memory.Cmp(a.Memory); c != 0 {
				return c
			}

			return compareIdentity(a, b)
		})
	default:
		slices.SortFunc(rows, compareIdentity)
	}
}

func compareIdentity(a, b joinedRow) int {
	if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}

	return cmp.Compare(a.Name, b.Name)
}

func toRows(joined []joinedRow, namespaced bool) []Row {
	rows := make([]Row, 0, len(joined))

	for _, j := range joined {
		row := Row{Name: j.Name, CPU: j.CPU.String(), Memory: j.Memory.String()}
		if namespaced {
			row.Namespace = j.Namespace
		}

		rows = append(rows, row)
	}

	return rows
}

func renderJSON(stdout io.Writer, rows []Row) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")

	return enc.Encode(rows)
}

func renderYAML(stdout io.Writer, rows []Row) error {
	out, err := yaml.Marshal(rows)
	if err != nil {
		return fmt.Errorf("rendering yaml: %w", err)
	}

	_, err = stdout.Write(out)

	return err
}

func renderTable(stdout io.Writer, o *Options, desc resourceDescriptor, rows []Row) error {
	w := tabwriter.NewWriter(stdout, 0, 4, 3, ' ', 0)

	cpuHeader := fmt.Sprintf("CPU(%s,%s)", o.Stat, o.Window)
	memHeader := fmt.Sprintf("MEMORY(%s,%s)", o.Stat, o.Window)

	var err error

	writeLine := func(format string, a ...any) {
		if err != nil {
			return
		}
		_, err = fmt.Fprintf(w, format, a...)
	}

	if !o.NoHeaders {
		if desc.Namespaced {
			writeLine("NAMESPACE\tNAME\t%s\t%s\n", cpuHeader, memHeader)
		} else {
			writeLine("NAME\t%s\t%s\n", cpuHeader, memHeader)
		}
	}

	for _, row := range rows {
		if desc.Namespaced {
			writeLine("%s\t%s\t%s\t%s\n", row.Namespace, row.Name, row.CPU, row.Memory)
		} else {
			writeLine("%s\t%s\t%s\n", row.Name, row.CPU, row.Memory)
		}
	}

	if err != nil {
		return err
	}

	return w.Flush()
}
