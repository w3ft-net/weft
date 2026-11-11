package coordinator

import (
	"database/sql"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
)

// Cold parquet pruning by coordinates is the third superset backend over the
// pinned coordinates (docs/plans/tenant-scope.md, "Coordinates are IR"). It
// needs no code of its own: an archive's key directories are hive partitions,
// and DuckDB drops the ones the planned SQL cannot match, including a union of
// tuples with env (not a partition) inside its branches. This pins that, so a
// change to the SQL's shape that defeats it fails here: the query must open
// exactly the key directories the coordinates admit, no more.
func TestColdArchiveOpensOnlyAdmittedKeys(t *testing.T) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Skipf("duckdb unavailable: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	namespaces := []string{"a", "b", "c"}
	services := []string{"api", "web"}
	kinds := []string{"main", "metrics"}
	envs := []string{"prod", "dev"}

	dir := t.TempDir()
	if _, err := db.Exec(`CREATE TABLE src AS SELECT 1::BIGINT AS customer_id, ns AS namespace, svc AS service, k AS kind,
		e AS env, 60 AS _w, TIMESTAMP '2026-01-01 00:00:00' AS _part, TIMESTAMP '2026-01-01 00:10:00' AS ts,
		'r' AS raw, uuid() AS event_id, 1::BIGINT AS line_no
		FROM (VALUES ('a'),('b'),('c')) n(ns), (VALUES ('api'),('web')) s(svc),
		     (VALUES ('main'),('metrics')) kk(k), (VALUES ('prod'),('dev')) ee(e)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`COPY src TO '%s/sized' (FORMAT PARQUET, PARTITION_BY (%s))`, dir, "customer_id, namespace, service, kind, _w, _part")); err != nil {
		t.Fatal(err)
	}
	read := shard.ParquetReadExpr(dir + "/sized/**/*.parquet")
	filesRead := regexp.MustCompile(`Total Files Read: (\d+)`)

	rng := rand.New(rand.NewSource(11))
	pick := func(vals []string) string { return vals[rng.Intn(len(vals))] }
	cell := func(vals []string) shape.Cell {
		if rng.Intn(5) < 3 {
			return shape.Any()
		}
		return shape.Value(pick(vals))
	}
	checked := 0
	for i := 0; i < 800; i++ {
		var scope shape.Set
		for n := 1 + rng.Intn(2); n > 0; n-- {
			scope = append(scope, shape.Coord{Customer: 1, Namespace: cell(namespaces), Service: cell(services), Kind: cell(kinds),
				Env: cell(envs)})
		}
		env := ""
		if rng.Intn(2) == 0 {
			env = pick(envs)
		}
		var paths []string
		var source shape.Set
		for n := 1 + rng.Intn(3); n > 0; n-- {
			ns, svc, kind := pick(namespaces), pick(services), pick(kinds)
			tup := shape.Coord{Customer: 1, Namespace: shape.Value(ns), Service: shape.Value(svc), Kind: shape.Value(kind)}
			if env != "" {
				paths = append(paths, fmt.Sprintf("/log/%s/%s/%s/%s.log", ns, env, svc, kind))
				tup.Env = shape.Value(env)
			} else {
				paths = append(paths, fmt.Sprintf("/log/%s/%s/%s.log", ns, svc, kind))
			}
			source = append(source, tup)
		}
		plan, err := planner.PlanIn("cat "+strings.Join(paths, " "), 1, scope, "", time.Time{}, time.Time{})
		if err != nil {
			continue // a source the scope leaves nothing of is refused before any file opens
		}

		// The key directories the coordinates admit. env is not a partition, so it
		// does not narrow them.
		want := 0
		for _, ns := range namespaces {
			for _, svc := range services {
				for _, kind := range kinds {
					for _, e := range envs {
						if source.Matches(1, ns, e, svc, kind) && scope.Matches(1, ns, e, svc, kind) {
							want++
							break
						}
					}
				}
			}
		}
		rows, err := db.Query("EXPLAIN ANALYZE "+strings.Replace(plan.ShardSQL, "FROM logs", "FROM "+read, 1), plan.Args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", paths, err, plan.ShardSQL)
		}
		got := -1
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			if m := filesRead.FindStringSubmatch(v); m != nil {
				got, _ = strconv.Atoi(m[1])
			}
		}
		rows.Close()
		if got != want {
			t.Fatalf("%v over scope %+v opened %d archive files, want %d (one per admitted key)\n%s", paths, scope, got, want, plan.ShardSQL)
		}
		checked++
	}
	if checked < 100 {
		t.Fatalf("generator is degenerate: %d checked", checked)
	}
}
