package compute

import (
	"fmt"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// BenchmarkStages feeds rows straight into a job (no shard, no planner), so it
// measures compute alone: the merge, the pipe, the stage, and the bind of any
// meta-variable the stage reads.
//
//	go test -tags test -run xxx -bench Stages -benchtime 3x ./compute
func BenchmarkStages(b *testing.B) {
	const n = 200_000
	rows := make([]*pb.ResultRow, n)
	for i := range rows {
		rows[i] = &pb.ResultRow{
			TsUnixNs: int64(i), Raw: fmt.Sprintf(`{"q":%d,"path":"/v1/items/%d"}`, i%1000, i%5000),
			MetaVars: map[string]string{"level": []string{"INFO", "WARN", "ERROR"}[i%3], "instance": fmt.Sprintf("i-%d", i%7)},
		}
	}
	awkRows := make([]*pb.ResultRow, n)
	for i := range awkRows {
		awkRows[i] = &pb.ResultRow{
			TsUnixNs: int64(i), Raw: fmt.Sprintf("req%07d GET %d /v1/items/%d", i, i%1000, i%5000),
			MetaVars: rows[i].MetaVars,
		}
	}
	for _, c := range []struct {
		name  string
		stage string
		rows  []*pb.ResultRow
	}{
		{"awk-no-meta", `awk '{ s += $3 } END { print s }'`, awkRows},
		{"awk-level", `awk '{ s += $3; if (LEVEL == "ERROR") e++ } END { print s, e }'`, awkRows},
		{"awk-no-meta-big-output", `awk '{ print toupper($1), $3 }'`, awkRows},
		{"jq-no-meta-big-output", `jq -c '. + {x: 1}'`, rows},
		{"jq-level", `jq -c '. + {l: $level}'`, rows},
		{"jq-no-meta-small-output", `jq -c '.q' | tail -n 1`, rows},
	} {
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				job := (&Engine{}).NewJob([]string{c.stage}, 1, "ts", 0, false, false)
				go func() {
					for _, r := range c.rows {
						job.SendRow(&pb.ResultRow{TsUnixNs: r.TsUnixNs, Raw: r.Raw, MetaVars: r.MetaVars}, "a")
					}
					job.SendSentinel("a")
				}()
				if _, err := job.Result(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
