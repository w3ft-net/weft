package weftlog

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"regexp"
	"testing"

	weftsdk "github.com/w3ft-net/weft-go"
)

// stamp is the stdlib logger's default timestamp: the shape weft's schema
// detection already keys the weft/* sources' timestamps off.
const stamp = `\d{4}/\d\d/\d\d \d\d:\d\d:\d\d `

func lineMatches(t *testing.T, got, want string) {
	t.Helper()
	if !regexp.MustCompile(`^` + stamp + want + `\n$`).MatchString(got) {
		t.Errorf("line %q does not match %q", got, want)
	}
}

func TestInfofKeepsTheLegacyLineAndAddsTheTraceID(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf)

	Infof(context.Background(), "query rpc: done lines=%d", 10)
	lineMatches(t, buf.String(), `query rpc: done lines=10`)

	buf.Reset()
	ctx := weftsdk.ContextWithTraceID(context.Background(), "7f778536850bbbbaed3c25469f1a23dd")
	Infof(ctx, "query rpc: done lines=%d", 10)
	lineMatches(t, buf.String(), `query rpc: done lines=10 trace_id=7f778536850bbbbaed3c25469f1a23dd`)
}

func TestWarnfAndErrorfAreTheSameShape(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf)
	ctx := weftsdk.ContextWithTraceID(context.Background(), "abc")
	Warnf(ctx, "slow %s", "query")
	Errorf(ctx, "failed: %v", "boom")
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), buf.String())
	}
	lineMatches(t, string(lines[0])+"\n", `slow query trace_id=abc`)
	lineMatches(t, string(lines[1])+"\n", `failed: boom trace_id=abc`)
}

func TestAttrsRenderAfterTheMessageAndBeforeTheTraceID(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf)
	ctx := weftsdk.ContextWithTraceID(context.Background(), "abc")
	slog.With("shard", "s1").InfoContext(ctx, "rotated", "rows", 3, "note", "two words")
	lineMatches(t, buf.String(), `rotated shard=s1 rows=3 note="two words" trace_id=abc`)
}

func TestInitRoutesTheStdlibLoggerThroughTheSameFormat(t *testing.T) {
	var buf bytes.Buffer
	Init(&buf)
	// The SDK's span lines are written with the stdlib logger.
	log.Printf("span start name=%s trace_id=%s", "x", "abc")
	lineMatches(t, buf.String(), `span start name=x trace_id=abc`)
}
