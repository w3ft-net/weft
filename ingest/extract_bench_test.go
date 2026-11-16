package ingest

import (
	"testing"
	"time"
)

// The real parser, the same two values (a two-field timestamp and a level),
// read the legacy way and by an Extract spec, on a short line and a long one.
// Run: go test -tags test ./ingest -run xxx -bench ParseLineForm -benchmem
const (
	benchTSLayout = "2006-01-02 15:04:05.000"
	longMessage   = "2026-10-15 12:00:06.987 INFO handled GET /api/items 200 50ms the quick brown fox jumps over the lazy dog and keeps running through a very long message body that goes on and on with many words so the line has dozens of whitespace separated fields trace_id=6d07ef7b77e90d35 end of line extra words here and there to make it longer still ok"
)

func legacySchema() *Schema {
	return &Schema{Format: "text", TimestampField: "1", TimestampFields: 2, TimestampFormat: benchTSLayout, LevelField: "3"}
}

func extractSchema() *Schema {
	s := legacySchema()
	s.Extract = map[string]ExtractSpec{"timestamp": {}, "level": {}}
	return s
}

func BenchmarkParseLineForm(b *testing.B) {
	now := time.Now()
	for _, line := range []struct{ name, text string }{{"short", benchLine}, {"long", longMessage}} {
		for _, form := range []struct {
			name string
			s    *Schema
			full bool
		}{
			{"legacy", legacySchema(), false},
			{"extract-early-split", extractSchema(), false},
			{"extract-full-split", extractSchema(), true},
		} {
			p, err := NewParser(form.s)
			if err != nil {
				b.Fatal(err)
			}
			p.forceFullSplit = form.full
			b.Run(line.name+"/"+form.name, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					p.ParseLine(line.text, now)
				}
			})
		}
	}
}

func BenchmarkParseLineSpans(b *testing.B) {
	p, _ := NewParser(extractSchema())
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.ParseLineSpans(benchLine, now)
	}
}
