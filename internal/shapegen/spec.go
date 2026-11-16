// Package shapegen turns a shape spec into the Go source of the coordinator's
// coordinate vocabulary: the columns that locate a log row, chosen when the
// coordinator is built.
package shapegen

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is a shape: the base columns it selects from the row vocabulary, and the
// columns it adds.
type Spec struct {
	// Coordinates are base columns, by name. They are LogRow fields that weftd
	// and the ingest adapters fill in.
	Coordinates []string `yaml:"coordinates"`
	// Added columns never appear in a LogRow. The coordinator stamps them from
	// the ingest token, so they are always pinned.
	Added []Added `yaml:"added"`
}

// Added is one column a shape adds to the base ones.
type Added struct {
	Name string `yaml:"name"`
	// Type is the column's storage type: int64 or string.
	Type string `yaml:"type"`
	// Role is what the column is for. Only partition columns own storage:
	// caches, per-key tables, hive paths, metering and retention key on them.
	Role string `yaml:"role"`
	// Pinned columns are stamped from the ingest token, never agent-supplied.
	Pinned bool `yaml:"pinned"`
	// Required columns are never a wildcard in a scope tuple or a stamp.
	Required bool `yaml:"required"`
	// GoName is the Go field name; it defaults to the column's name in CamelCase.
	GoName string `yaml:"go_name"`
}

// Column is one column of a shape as the generator sees it, in order: the base
// columns, then the added ones.
type Column struct {
	Name   string
	GoName string
	// Added is true for a column the shape adds; a base column is a LogRow field.
	Added    bool
	Type     string // "int64" or "string"
	Required bool   // a concrete value, never a wildcard
	// Partition is true when the column owns storage.
	Partition bool
}

// baseColumns is the row vocabulary every build shares: the LogRow fields a
// shape may select as coordinates.
var baseColumns = []string{
	"namespace", "env", "service", "kind", "region", "cluster",
	"k8s_namespace", "deployment", "version", "pod", "container", "node",
}

// ParseSpec reads and validates a shape spec.
func ParseSpec(b []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("shape spec: %w", err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("shape spec: %w", err)
	}
	for i := range s.Added {
		if s.Added[i].GoName == "" {
			s.Added[i].GoName = camel(s.Added[i].Name)
		}
	}
	return &s, nil
}

func (s *Spec) validate() error {
	if len(s.Coordinates) == 0 {
		return errors.New("coordinates needs at least one column")
	}
	if !slices.Contains(s.Coordinates, "namespace") {
		return errors.New("coordinates must include namespace: the unit of ownership of data and artifacts")
	}
	seen := map[string]bool{}
	for _, c := range s.Coordinates {
		if !slices.Contains(baseColumns, c) {
			return fmt.Errorf("coordinate %q is not a base column (the base columns are %s)", c, strings.Join(baseColumns, ", "))
		}
		if seen[c] {
			return fmt.Errorf("coordinate %q is listed twice", c)
		}
		seen[c] = true
	}
	for _, a := range s.Added {
		switch {
		case a.Name == "":
			return errors.New("an added column has no name")
		case slices.Contains(baseColumns, a.Name):
			return fmt.Errorf("added column %q is a base column; list it under coordinates", a.Name)
		case seen[a.Name]:
			return fmt.Errorf("column %q is listed twice", a.Name)
		case a.Type != "int64" && a.Type != "string":
			return fmt.Errorf("added column %q: type %q is not int64 or string", a.Name, a.Type)
		case a.Role != "partition":
			return fmt.Errorf("added column %q: role %q is not partition", a.Name, a.Role)
		case !a.Pinned:
			return fmt.Errorf("added column %q must be pinned: a column an agent had to fill in would need a custom weftd", a.Name)
		case !a.Required:
			return fmt.Errorf("added column %q is a partition column, so it must be required: a partition has a value, never a wildcard", a.Name)
		}
		seen[a.Name] = true
	}
	return nil
}

// Columns lists the shape's columns in order: the base columns, then the added
// ones.
func (s *Spec) Columns() []Column {
	var out []Column
	for _, c := range s.Coordinates {
		out = append(out, Column{Name: c, GoName: camel(c), Type: "string"})
	}
	for _, a := range s.Added {
		out = append(out, Column{Name: a.Name, GoName: a.GoName, Added: true, Type: a.Type, Required: a.Required, Partition: a.Role == "partition"})
	}
	return out
}

// camel is a column name in CamelCase: account_id is AccountId.
func camel(name string) string {
	var b strings.Builder
	for _, part := range strings.Split(name, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}
