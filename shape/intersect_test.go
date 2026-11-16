package shape

import (
	"reflect"
	"strconv"
	"testing"
)

// These tests are written for whichever shape the build has: a tuple of group
// n is the open tuple placed in partition n, so they name no group column.
// Where a case is about two groups it needs a shape that has one.

func partitionN(n int64) Coord {
	vals := make([]string, len(BuildLayout().Columns))
	for i := range vals {
		vals[i] = strconv.FormatInt(n, 10)
	}
	c, err := CoordFromPartition(vals)
	if err != nil {
		panic(err)
	}
	return c
}

func hasGroup() bool { return len(BuildLayout().Columns) > 0 }

func TestIntersect(t *testing.T) {
	c := func(cust int64, ns, env, svc, kind Cell) Coord {
		return Coord{Namespace: ns, Env: env, Service: svc, Kind: kind}.WithPartition(partitionN(cust))
	}
	a, b := Value("a"), Value("b")
	tests := []struct {
		name          string
		needsGroup    bool
		source, scope Set
		want          Set
	}{
		{"scope narrows an open source", false,
			Set{c(1, Any(), Any(), Any(), Any())},
			Set{c(1, a, Any(), Any(), Any())},
			Set{c(1, a, Any(), Any(), Any())}},
		{"source narrows an open scope", false,
			Set{c(1, a, Any(), Value("api"), Value("main"))},
			Set{c(1, Any(), Any(), Any(), Any())},
			Set{c(1, a, Any(), Value("api"), Value("main"))}},
		{"disjoint values are a deny", false,
			Set{c(1, a, Any(), Any(), Any())},
			Set{c(1, b, Any(), Any(), Any())},
			nil},
		{"another customer is a deny", true,
			Set{c(2, Any(), Any(), Any(), Any())},
			Set{c(1, Any(), Any(), Any(), Any())},
			nil},
		{"empty scope is a deny", false,
			Set{c(1, Any(), Any(), Any(), Any())},
			nil,
			nil},
		{"empty source is a deny", false,
			nil,
			Set{c(1, Any(), Any(), Any(), Any())},
			nil},
		{"null env meets null env", false,
			Set{c(1, a, Null(), Any(), Any())},
			Set{c(1, a, Null(), Any(), Any())},
			Set{c(1, a, Null(), Any(), Any())}},
		{"null env never meets a named env", false,
			Set{c(1, a, Null(), Any(), Any())},
			Set{c(1, a, Value("prod"), Any(), Any())},
			nil},
		{"any env meets null env", false,
			Set{c(1, a, Any(), Any(), Any())},
			Set{c(1, a, Null(), Any(), Any())},
			Set{c(1, a, Null(), Any(), Any())}},
		{"tuples stay tuples: a/api + b/web does not admit a/web", false,
			Set{c(1, a, Any(), Value("api"), Any()), c(1, b, Any(), Value("web"), Any())},
			Set{c(1, Any(), Any(), Any(), Any())},
			Set{c(1, a, Any(), Value("api"), Any()), c(1, b, Any(), Value("web"), Any())}},
		{"each source tuple meets each scope tuple", false,
			Set{c(1, Any(), Any(), Value("api"), Any())},
			Set{c(1, a, Any(), Any(), Any()), c(1, b, Any(), Any(), Any())},
			Set{c(1, a, Any(), Value("api"), Any()), c(1, b, Any(), Value("api"), Any())}},
		{"duplicates collapse", false,
			Set{c(1, a, Any(), Any(), Any()), c(1, a, Any(), Any(), Any())},
			Set{c(1, Any(), Any(), Any(), Any())},
			Set{c(1, a, Any(), Any(), Any())}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.needsGroup && !hasGroup() {
				t.Skip("the shape has no group")
			}
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
	in := partitionN(1)
	all := Set{Coord{}.WithPartition(in)}
	a := Set{Coord{Namespace: Value("a")}.WithPartition(in)}
	key := func(k Key) *Key { k = k.InPartition(in); return &k }
	if !Covers(all, a) || Covers(a, all) {
		t.Error("an open scope covers a named source, and a named scope does not cover an open one")
	}
	if Covers(nil, a) || Covers(a, nil) {
		t.Error("nothing covers an empty source, and an empty scope covers nothing")
	}
	if !a.Matches(key(Key{Namespace: "a", Env: "prod", Service: "api", Kind: "main"})) ||
		a.Matches(key(Key{Namespace: "b"})) {
		t.Error("Matches disagrees with the tuple")
	}
	if hasGroup() {
		other := Key{Namespace: "a"}.InPartition(partitionN(2))
		if a.Matches(&other) {
			t.Error("a row of another group matched")
		}
	}
	nullEnv := Set{Coord{Env: Null()}.WithPartition(in)}
	if !nullEnv.Matches(key(Key{Namespace: "x", Service: "s", Kind: "k"})) ||
		nullEnv.Matches(key(Key{Namespace: "x", Env: "prod", Service: "s", Kind: "k"})) {
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
	in := partitionN(1)
	scope := Set{
		Coord{Namespace: Value("a"), Service: Value("api")}.WithPartition(in),
		Coord{Namespace: Value("b")}.WithPartition(in),
	}
	src := func(ns, svc string) Coord {
		return Coord{Namespace: Value(ns), Service: Value(svc)}.WithPartition(in)
	}
	if !scope.Overlaps(src("a", "api")) || scope.Overlaps(src("a", "web")) || !scope.Overlaps(src("b", "web")) || scope.Overlaps(src("c", "api")) {
		t.Error("a source is visible exactly when the scope admits some row of it")
	}
	names, all := scope.NamespaceSuperset(in)
	if all || !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Errorf("NamespaceSuperset = %v %v, want [a b] not all", names, all)
	}
	if _, all := (Set{in}).NamespaceSuperset(in); !all {
		t.Error("a tuple with no namespace opens every namespace")
	}
}
