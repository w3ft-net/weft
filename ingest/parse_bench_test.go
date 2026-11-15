package ingest

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// Positional extraction (the parser today) against regexes for the same three
// values, on one representative line. Run: go test -tags test ./ingest -run x -bench ParseLine -benchmem
const benchLine = "2026-10-15 12:00:06.987 INFO handled GET /api/items 200 50ms trace_id=6d07ef7b77e90d35"

func benchSchema() *Schema {
	return &Schema{
		Format: "text", TimestampField: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05.000",
		LevelField: "3",
	}
}

func BenchmarkParseLine_PositionalTimestampAndLevel(b *testing.B) {
	p, _ := NewParser(benchSchema())
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.ParseLine(benchLine, now)
	}
}

func BenchmarkParseLine_PositionalPlusTraceRegex(b *testing.B) {
	s := benchSchema()
	s.TraceIDField, s.TraceIDPattern = "none", `trace_id=([0-9a-f]+)`
	p, _ := NewParser(s)
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.ParseLine(benchLine, now)
	}
}

// The same three values with a regex each (timestamp still needs time.Parse
// to turn the matched text into a time).
func BenchmarkRegexOnly_TimestampLevelTrace(b *testing.B) {
	tsRe := regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d[.,]\d{3})`)
	lvRe := regexp.MustCompile(`\b(DEBUG|INFO|WARN|WARNING|ERROR|FATAL)\b`)
	trRe := regexp.MustCompile(`trace_id=([0-9a-f]+)`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if m := tsRe.FindStringSubmatch(benchLine); m != nil {
			time.Parse("2006-01-02 15:04:05.000", m[1])
		}
		lvRe.FindStringSubmatch(benchLine)
		trRe.FindStringSubmatch(benchLine)
	}
}

// For scale: the split every positional extractor shares.
func BenchmarkStringsFields(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = strings.Fields(benchLine)
	}
}
