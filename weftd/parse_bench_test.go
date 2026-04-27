package weftd

import (
	"fmt"
	"testing"
	"time"

	"github.com/kevlarcon/weft/ingest"
)

// benchSchemaYAML matches the schema the multi-customer demo (dev/demo)
// onboards for every service: a leading "<date> <time>" timestamp pair,
// followed by an upper-cased level token, followed by a body whose
// shape varies per service. All six services (api/worker/edge/orders/
// billing/notifier) share this prefix; varying body content does not
// change the parser's work.
const benchSchemaYAML = `envelope: none
format: text
timestamp_field: "1"
timestamp_fields: 2
timestamp_format: "2006-01-02 15:04:05"
level_field: "3"
`

// benchLines builds n synthetic lines spanning the demo services, with
// realistic body shapes (key=value pairs, mixed levels). Generation
// happens once before the bench loop runs so we measure parse cost
// alone.
func benchLines(n int) []string {
	services := []string{"api", "worker", "edge", "orders", "billing", "notifier"}
	levels := []string{"INFO", "INFO", "INFO", "WARN", "ERROR"}
	bodies := []string{
		"GET /api/v1/orders status=200 dur=42",
		"job_failed job=billing_recalc reason=deadline_exceeded duration_ms=64000",
		"order_failed order_id=O-3318 reason=payment_declined",
		"invoice_paid invoice_id=I-9012 amount=212.45",
		"notify_sent endpoint=billing-svc latency_ms=120",
		"checkout_started cart_id=c-77 user=u-204 items=3",
	}
	out := make([]string, n)
	base := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		svc := services[i%len(services)]
		lvl := levels[i%len(levels)]
		body := bodies[i%len(bodies)]
		out[i] = fmt.Sprintf("%s %-5s %s service=%s",
			ts.Format("2006-01-02 15:04:05"), lvl, body, svc)
	}
	return out
}

func newBenchParser(tb testing.TB) *ingest.Parser {
	tb.Helper()
	schema, err := ingest.LoadSchemaFromBytes([]byte(benchSchemaYAML))
	if err != nil {
		tb.Fatalf("load schema: %v", err)
	}
	schema.App = "demo/api"
	schema.Customer = "acme"
	parser, err := ingest.NewParser(schema)
	if err != nil {
		tb.Fatalf("new parser: %v", err)
	}
	return parser
}

// BenchmarkWeftdParse measures one ParseLine call per iteration over a
// rotating set of demo-shaped lines. Run with:
//
//	go test -bench BenchmarkWeftdParse -benchmem ./weftd/
func BenchmarkWeftdParse(b *testing.B) {
	parser := newBenchParser(b)
	lines := benchLines(1024)
	now := time.Now()

	// Touch every line once so any first-use lazy init in the parser
	// (regex JIT, time-format cache) doesn't land on the timed loop.
	for _, ln := range lines {
		_, _ = parser.ParseLine(ln, now)
	}

	var totalBytes int
	for _, ln := range lines {
		totalBytes += len(ln)
	}
	avgBytes := totalBytes / len(lines)

	b.ReportAllocs()
	b.SetBytes(int64(avgBytes))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = parser.ParseLine(lines[i%len(lines)], now)
	}
}

// TestWeftdParseBudget gates the parser against the rollout footprint
// budget from docs/plans/weftd-client-side-parsing.md (≤ 4500 ns/op,
// ≤ 600 B/op). A regression here means schema-driven parsing is no
// longer cheap enough to live in the agent's hot path.
//
// RSS is intentionally not gated — that's covered by fleet-week
// telemetry post-rollout, not CI. See the plan's "Footprint controls"
// section.
func TestWeftdParseBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bench-budget test in -short mode")
	}

	parser := newBenchParser(t)
	lines := benchLines(1024)
	now := time.Now()

	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = parser.ParseLine(lines[i%len(lines)], now)
		}
	})

	const (
		nsBudget = 4500
		bBudget  = 600
	)

	nsPerOp := res.NsPerOp()
	bPerOp := res.AllocedBytesPerOp()
	allocsPerOp := res.AllocsPerOp()

	t.Logf("parse: %d ns/op, %d B/op, %d allocs/op (over %d iterations)",
		nsPerOp, bPerOp, allocsPerOp, res.N)

	if nsPerOp > nsBudget {
		t.Errorf("ns/op = %d exceeds budget %d", nsPerOp, nsBudget)
	}
	if bPerOp > bBudget {
		t.Errorf("B/op = %d exceeds budget %d", bPerOp, bBudget)
	}
}
