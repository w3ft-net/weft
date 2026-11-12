package coordinator

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

// The exact backends over the pinned coordinates must admit the same rows:
// DuckDB running the planned SQL, and the live RowMatcher, both held against an
// oracle that reads the source and the scope directly. The superset backend
// (shard routing) must never drop a row they admit. See docs/plans/tenant-scope.md,
// "Coordinates are IR" and contract 2.
func TestCoordinateBackendsAgree(t *testing.T) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Skipf("duckdb unavailable: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	namespaces := []string{"a", "b", "c"}
	envs := []string{"", "prod", "dev"}
	services := []string{"api", "web"}
	kinds := []string{"main", "metrics"}

	// Every combination, for two customers, so a miss in any column shows.
	type row struct {
		id                 string
		cust               int64
		ns, env, svc, kind string
	}
	var rows []row
	for _, cust := range []int64{1, 2} {
		for _, ns := range namespaces {
			for _, env := range envs {
				for _, svc := range services {
					for _, kind := range kinds {
						rows = append(rows, row{fmt.Sprintf("r%d", len(rows)), cust, ns, env, svc, kind})
					}
				}
			}
		}
	}
	if _, err := db.Exec(`CREATE TABLE logs (raw VARCHAR, ts TIMESTAMP, event_id UUID, line_no BIGINT,
		customer_id BIGINT, namespace VARCHAR, env VARCHAR, service VARCHAR, kind VARCHAR)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO logs VALUES (?, now(), uuid(), 1, ?, ?, ?, ?, ?)`, r.id, r.cust, r.ns, r.env, r.svc, r.kind); err != nil {
			t.Fatal(err)
		}
	}

	rng := rand.New(rand.NewSource(7))
	pick := func(vals []string) string { return vals[rng.Intn(len(vals))] }
	cell := func(vals []string, allowNull bool) shape.Cell {
		switch n := rng.Intn(6); {
		case n <= 2:
			return shape.Any()
		case n == 3 && allowNull:
			return shape.Null()
		}
		return shape.Value(pick(vals))
	}

	checked, denied := 0, 0
	for i := 0; i < 1500; i++ {
		// The scope: one to three tuples of customer 1.
		var scope shape.Set
		for n := 1 + rng.Intn(3); n > 0; n-- {
			scope = append(scope, shapetest.In(1, shape.Coord{
				Namespace: cell(namespaces, false),
				Env:       cell(envs[1:], true),
				Service:   cell(services, false),
				Kind:      cell(kinds, false),
			}))
		}
		// The source: one to three files. A pipeline has one env, so the files
		// share it, as the parser requires.
		env := pick(envs)
		var paths []string
		var source shape.Set
		for n := 1 + rng.Intn(3); n > 0; n-- {
			ns, svc, kind := pick(namespaces), pick(services), pick(kinds)
			p := fmt.Sprintf("/log/%s/%s/%s.log", ns, svc, kind)
			t := shapetest.In(1, shape.Coord{Namespace: shape.Value(ns), Env: shape.Any(), Service: shape.Value(svc), Kind: shape.Value(kind)})
			if env != "" {
				p = fmt.Sprintf("/log/%s/%s/%s/%s.log", ns, env, svc, kind)
				t.Env = shape.Value(env)
			}
			paths = append(paths, p)
			source = append(source, t)
		}
		pipeline := "cat " + strings.Join(paths, " ")

		plan, err := planner.Plan(pipeline, scope, "", time.Time{}, time.Time{})
		someTupleEmpty := false
		for _, s := range source {
			if len(shape.Intersect(shape.Set{s}, scope)) == 0 {
				someTupleEmpty = true
			}
		}
		if someTupleEmpty {
			if !errors.Is(err, planner.ErrNoSource) {
				t.Fatalf("%s over %+v: err = %v, want a refusal (a named source the scope leaves nothing of)", pipeline, scope, err)
			}
			denied++
			continue
		}
		if err != nil {
			t.Fatalf("%s over %+v: %v", pipeline, scope, err)
		}
		checked++

		var want []string
		for _, r := range rows {
			if source.Matches(shapetest.KeyInPtr(r.cust, shape.Key{Namespace: r.ns, Env: r.env, Service: r.svc, Kind: r.kind})) && scope.Matches(shapetest.KeyInPtr(r.cust, shape.Key{Namespace: r.ns, Env: r.env, Service: r.svc, Kind: r.kind})) {
				want = append(want, r.id)
			}
		}
		slices.Sort(want)

		// SQL backend.
		res, err := db.Query(plan.ShardSQL, plan.Args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", pipeline, err, plan.ShardSQL)
		}
		var gotSQL []string
		for res.Next() {
			var raw string
			var ts time.Time
			var ev sql.NullString
			var ln sql.NullInt64
			if err := res.Scan(&raw, &ts, &ev, &ln); err != nil {
				t.Fatal(err)
			}
			gotSQL = append(gotSQL, raw)
		}
		res.Close()
		slices.Sort(gotSQL)

		// Live matcher.
		m := &RowMatcher{Coords: plan.Coords}
		var gotLive []string
		for _, r := range rows {
			if m.Match(Row{LogRow: &pb.LogRow{Namespace: r.ns, Env: r.env, Service: r.svc, Kind: r.kind}, Added: shapetest.Added(r.cust)}) {
				gotLive = append(gotLive, r.id)
			}
		}
		slices.Sort(gotLive)

		if !slices.Equal(gotSQL, want) {
			t.Fatalf("SQL backend disagrees with the oracle\n%s\nover scope %+v\nSQL: %s %v\ngot  %v\nwant %v", pipeline, scope, plan.ShardSQL, plan.Args, gotSQL, want)
		}
		if !slices.Equal(gotLive, want) {
			t.Fatalf("live matcher disagrees with the oracle\n%s\nover scope %+v\ngot  %v\nwant %v", pipeline, scope, gotLive, want)
		}
		// Superset backend: routing may include shards it need not, never skip one
		// that holds an admitted row.
		for _, id := range want {
			var r row
			for _, c := range rows {
				if c.id == id {
					r = c
				}
			}
			if plan.SourceNamespace != "" && r.ns != plan.SourceNamespace {
				t.Fatalf("%s: routed by namespace %q but admits a row in %q", pipeline, plan.SourceNamespace, r.ns)
			}
			if plan.SourceService != "" && r.svc != plan.SourceService {
				t.Fatalf("%s: routed by service %q but admits a row in %q", pipeline, plan.SourceService, r.svc)
			}
		}
	}
	if checked < 300 || denied < 50 {
		t.Fatalf("generator is degenerate: %d admitted, %d denied", checked, denied)
	}
}
