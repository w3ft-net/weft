package shapegen

import (
	"strings"
	"testing"
)

const hostedYAML = `
coordinates: [namespace, env, service, kind]
added:
  - name: customer_id
    type: int64
    role: partition
    pinned: true
    required: true
    go_name: Customer
`

func TestParseSpec_HostedAndDefault(t *testing.T) {
	s, err := ParseSpec([]byte(hostedYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Coordinates) != 4 || len(s.Added) != 1 || s.Added[0].GoName != "Customer" {
		t.Fatalf("spec = %+v", s)
	}
	d, err := ParseSpec([]byte("coordinates: [namespace, env, service, kind]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 0 {
		t.Fatalf("default spec has added columns: %+v", d.Added)
	}
}

// The go name of a column is its name in CamelCase unless the spec gives one.
func TestParseSpec_GoNames(t *testing.T) {
	s, err := ParseSpec([]byte(`
coordinates: [namespace, k8s_namespace]
added:
  - {name: account_id, type: int64, role: partition, pinned: true, required: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Added[0].GoName; got != "AccountId" {
		t.Errorf("added go name = %q, want AccountId", got)
	}
	if got := s.Columns()[1].GoName; got != "K8sNamespace" {
		t.Errorf("base go name = %q, want K8sNamespace", got)
	}
}

func TestParseSpec_Refusals(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no coordinates":         {"coordinates: []\n", "at least one"},
		"no namespace":           {"coordinates: [env, service]\n", "namespace"},
		"unknown base column":    {"coordinates: [namespace, tenant]\n", "tenant"},
		"duplicate coordinate":   {"coordinates: [namespace, env, env]\n", "twice"},
		"added not pinned":       {"coordinates: [namespace]\nadded:\n  - {name: a, type: int64, role: partition, required: true}\n", "pinned"},
		"added names a base":     {"coordinates: [namespace]\nadded:\n  - {name: env, type: string, role: partition, pinned: true, required: true}\n", "env"},
		"added twice":            {"coordinates: [namespace]\nadded:\n  - {name: a, type: int64, role: partition, pinned: true, required: true}\n  - {name: a, type: int64, role: partition, pinned: true, required: true}\n", "twice"},
		"unknown type":           {"coordinates: [namespace]\nadded:\n  - {name: a, type: float, role: partition, pinned: true, required: true}\n", "type"},
		"unknown role":           {"coordinates: [namespace]\nadded:\n  - {name: a, type: int64, role: shard, pinned: true, required: true}\n", "role"},
		"partition not required": {"coordinates: [namespace]\nadded:\n  - {name: a, type: int64, role: partition, pinned: true}\n", "required"},
		"unknown field":          {"coordinates: [namespace]\nbogus: 1\n", "bogus"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSpec([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}
