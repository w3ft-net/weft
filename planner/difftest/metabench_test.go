package difftest

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// metaRows are request lines with a level, an instance and a JSON payload, so a
// stage can read a meta-variable and a jq stage has a payload to read.
func metaRows(n int) []*pb.LogRow {
	rng := rand.New(rand.NewSource(1))
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	levels := []string{"INFO", "INFO", "INFO", "WARN", "ERROR"}
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		ms := rng.Intn(1000)
		rows[i] = &pb.LogRow{
			Raw:         fmt.Sprintf("req%07d GET %d /v1/items/%d status=200", i, ms, rng.Intn(5000)),
			PayloadJson: []byte(fmt.Sprintf(`{"q":%d,"path":"/v1/items/%d"}`, ms, rng.Intn(5000))),
			TsUnixNs:    base.Add(time.Duration(i) * time.Millisecond).UnixNano(),
			Level:       levels[i%len(levels)],
			Instance:    fmt.Sprintf("i-%d", i%7),
			Namespace:   "typed", Service: "api", Kind: "main",
		}
	}
	return rows
}

// BenchmarkMetaVarStages is a first stage that stays in compute and reads a
// meta-variable: the shard projects the meta columns, the writer queues each
// row's values for the stage, and the stage binds them per record.
//
//	go test -tags test -run xxx -bench MetaVarStages -benchtime 5x ./planner/difftest
func BenchmarkMetaVarStages(b *testing.B) {
	h := newHarness(b, metaRows(benchRows), 1)
	for _, c := range []struct{ name, pipeline string }{
		{"awk-level", `cat /log/typed/api/main.log | awk '{ s += $3; if (LEVEL == "ERROR") e++ } END { print s, e }'`},
		{"awk-two-vars", `cat /log/typed/api/main.log | awk '{ s += $3; if (LEVEL == "ERROR" && HOST == "i-3") e++ } END { print s, e }'`},
		{"jq-level", `cat /log/typed/api/main.log | jq -c '. + {l: $level}'`},
		{"awk-no-meta", `cat /log/typed/api/main.log | awk '{ s += $3 } END { print s }'`},
		// The same output volume as jq-level, with no meta-variable read.
		{"jq-no-meta-big-output", `cat /log/typed/api/main.log | jq -c '. + {x: 1}'`},
		{"awk-no-meta-big-output", `cat /log/typed/api/main.log | awk '{ print toupper($1), $3 }'`},
	} {
		plan, err := planner.Plan(c.pipeline, 0, "", time.Time{}, time.Time{})
		if err != nil {
			b.Fatal(err)
		}
		if len(plan.ComputeStages) == 0 {
			b.Fatalf("%s: no compute stage left, so it measures nothing", c.pipeline)
		}
		if _, err := h.runPlan(plan); err != nil { // checked once
			b.Fatal(err)
		}
		h.unchecked = true
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := h.runPlan(plan); err != nil {
					b.Fatal(err)
				}
			}
		})
		h.unchecked = false
	}
}
