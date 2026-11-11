package shape

import (
	"reflect"
	"testing"
)

func TestIntersect(t *testing.T) {
	c := func(cust int64, ns, env, svc, kind Cell) Coord {
		return Coord{Customer: cust, Namespace: ns, Env: env, Service: svc, Kind: kind}
	}
	a, b := Value("a"), Value("b")
	tests := []struct {
		name          string
		source, scope Set
		want          Set
	}{
		{"scope narrows an open source",
			Set{c(1, Any(), Any(), Any(), Any())},
			Set{c(1, a, Any(), Any(), Any())},
			Set{c(1, a, Any(), Any(), Any())}},
		{"source narrows an open scope",
			Set{c(1, a, Any(), Value("api"), Value("main"))},
			Set{c(1, Any(), Any(), Any(), Any())},
			Set{c(1, a, Any(), Value("api"), Value("main"))}},
		{"disjoint values are a deny",
			Set{c(1, a, Any(), Any(), Any())},
			Set{c(1, b, Any(), Any(), Any())},
			nil},
		{"another customer is a deny",
			Set{c(2, Any(), Any(), Any(), Any())},
			Set{c(1, Any(), Any(), Any(), Any())},
			nil},
		{"empty scope is a deny",
			Set{c(1, Any(), Any(), Any(), Any())},
			nil,
			nil},
		{"empty source is a deny",
			nil,
			Set{c(1, Any(), Any(), Any(), Any())},
			nil},
		{"null env meets null env",
			Set{c(1, a, Null(), Any(), Any())},
			Set{c(1, a, Null(), Any(), Any())},
			Set{c(1, a, Null(), Any(), Any())}},
		{"null env never meets a named env",
			Set{c(1, a, Null(), Any(), Any())},
			Set{c(1, a, Value("prod"), Any(), Any())},
			nil},
		{"any env meets null env",
			Set{c(1, a, Any(), Any(), Any())},
			Set{c(1, a, Null(), Any(), Any())},
			Set{c(1, a, Null(), Any(), Any())}},
		{"tuples stay tuples: a/api + b/web does not admit a/web",
			Set{c(1, a, Any(), Value("api"), Any()), c(1, b, Any(), Value("web"), Any())},
			Set{c(1, Any(), Any(), Any(), Any())},
			Set{c(1, a, Any(), Value("api"), Any()), c(1, b, Any(), Value("web"), Any())}},
		{"each source tuple meets each scope tuple",
			Set{c(1, Any(), Any(), Value("api"), Any())},
			Set{c(1, a, Any(), Any(), Any()), c(1, b, Any(), Any(), Any())},
			Set{c(1, a, Any(), Value("api"), Any()), c(1, b, Any(), Value("api"), Any())}},
		{"duplicates collapse",
			Set{c(1, a, Any(), Any(), Any()), c(1, a, Any(), Any(), Any())},
			Set{c(1, Any(), Any(), Any(), Any())},
			Set{c(1, a, Any(), Any(), Any())}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Intersect(tt.source, tt.scope)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Intersect = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCoversAndMatches(t *testing.T) {
	all := Set{{Customer: 1}}
	a := Set{{Customer: 1, Namespace: Value("a")}}
	if !Covers(all, a) || Covers(a, all) {
		t.Error("an open scope covers a named source, and a named scope does not cover an open one")
	}
	if Covers(nil, a) || Covers(a, nil) {
		t.Error("nothing covers an empty source, and an empty scope covers nothing")
	}
	if !a.Matches(1, "a", "prod", "api", "main") || a.Matches(1, "b", "", "", "") || a.Matches(2, "a", "", "", "") {
		t.Error("Matches disagrees with the tuple")
	}
	nullEnv := Set{{Customer: 1, Env: Null()}}
	if !nullEnv.Matches(1, "x", "", "s", "k") || nullEnv.Matches(1, "x", "prod", "s", "k") {
		t.Error("Null env matches only a row with no env")
	}
	if v, ok := Shared(Set{{Namespace: Value("a")}, {Namespace: Value("a")}}, func(c Coord) Cell { return c.Namespace }); !ok || v != "a" {
		t.Errorf("Shared = %q %v", v, ok)
	}
	if _, ok := Shared(Set{{Namespace: Value("a")}, {Namespace: Value("b")}}, func(c Coord) Cell { return c.Namespace }); ok {
		t.Error("two namespaces share none")
	}
}

func TestOverlapsAndNamespaceSuperset(t *testing.T) {
	scope := Set{
		{Customer: 1, Namespace: Value("a"), Service: Value("api")},
		{Customer: 1, Namespace: Value("b")},
	}
	src := func(ns, svc string) Coord {
		return Coord{Customer: 1, Namespace: Value(ns), Service: Value(svc)}
	}
	if !scope.Overlaps(src("a", "api")) || scope.Overlaps(src("a", "web")) || !scope.Overlaps(src("b", "web")) || scope.Overlaps(src("c", "api")) {
		t.Error("a source is visible exactly when the scope admits some row of it")
	}
	names, all := scope.NamespaceSuperset(1)
	if all || !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Errorf("NamespaceSuperset = %v %v, want [a b] not all", names, all)
	}
	if _, all := (Set{{Customer: 1}}).NamespaceSuperset(1); !all {
		t.Error("a tuple with no namespace opens every namespace")
	}
}
