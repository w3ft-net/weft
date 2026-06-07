package planner

import (
	"testing"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// BenchmarkPipelineParse isolates the cost of `bash.Parse(pipeline)`
// from the rest of Plan(). Plan does at least two bash.Parse calls
// (one in extractTrailingFSRedirect, one in parsePipeline); if parse
// dominates the per-op number from BenchmarkPlan, the implication
// for save-time lowering is direct — caching the parsed AST is what
// buys us most of the savings.
//
// Run with:
//
//	go test -bench=BenchmarkPipelineParse -benchmem -benchtime=10x ./planner/
func BenchmarkPipelineParse(b *testing.B) {
	for _, fx := range benchFixtures {
		b.Run(fx.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				script, err := bash.Parse(fx.pipeline)
				if err != nil {
					b.Fatalf("Parse(%q): %v", fx.pipeline, err)
				}
				_ = script
			}
		})
	}
}
