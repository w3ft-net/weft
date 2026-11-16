// Package archivetest is the conformance suite for an archive: what a store
// writes, and what reads it back. Every pairing passes it: the open build's
// DuckDB writer and reader, and any store and reader a deployment builds
// another way.
package archivetest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// Case is one format a store writes.
type Case struct {
	// Format is the ID the archive's path must resolve to (Registry.FormatOf).
	Format string
	// Write archives rows, which carry increasing timestamps, and returns the
	// archive's path as the store reports it.
	Write func(t *testing.T, rows []*pb.LogRow) (path string)
}

// Spec is the writers and the reader under test.
type Spec struct {
	// Formats are the formats the build registers, which a path is resolved
	// against.
	Formats []archive.Format
	Cases   []Case
	// Reader reads what the cases write. It is asked for the archives by Layout,
	// which is the partition columns the writers wrote them in (zero: the legacy
	// layout), and Endpoint, where a reader that runs queries elsewhere reaches
	// its engine.
	Reader   archive.Reader
	Layout   shape.Layout
	Endpoint string
	// SelectAll is a query, in the reader's dialect, for every row's raw and ts in
	// ts order.
	SelectAll string
}

// sink collects what a reader streams.
type sink struct {
	mu        sync.Mutex
	raws      []string
	failed    []error
	sentinels int
}

func (s *sink) SendRow(row *pb.ResultRow, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raws = append(s.raws, row.Raw)
}

func (s *sink) Fail(_ string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, err)
}

func (s *sink) SendSentinel(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sentinels++
}

func rows(prefix string, n int) ([]*pb.LogRow, []string) {
	out := make([]*pb.LogRow, n)
	raws := make([]string, n)
	for i := range out {
		raws[i] = fmt.Sprintf("%s-%d", prefix, i)
		out[i] = &pb.LogRow{Raw: raws[i], TsUnixNs: int64(1_700_000_000+i*60) * 1e9, Namespace: "ns", Service: "svc", Kind: "main"}
	}
	return out, raws
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Run runs the suite for each case of spec.
func Run(t *testing.T, spec Spec) {
	reg := archive.NewRegistry()
	for _, f := range spec.Formats {
		if err := reg.AddFormat(f); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the reader names its dialect and the formats it reads", func(t *testing.T) {
		if spec.Reader.Dialect() == nil || spec.Reader.Dialect().Name() == "" {
			t.Error("the reader has no dialect, or its dialect has no name")
		}
		reads := map[string]bool{}
		for _, id := range spec.Reader.Reads() {
			reads[id] = true
		}
		for _, c := range spec.Cases {
			if !reads[c.Format] {
				t.Errorf("the reader does not read %s, which the store writes", c.Format)
			}
		}
	})

	// read is the raw text of every row of the archive at path, through the reader.
	read := func(t *testing.T, path string) (raws []string, s *sink) {
		t.Helper()
		h, err := spec.Reader.Open(archive.Archive{ID: "conformance", Path: path, Layout: spec.Layout, Endpoint: spec.Endpoint})
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		if h == nil {
			t.Fatal("open returned no handle and no error")
		}
		defer h.Close()
		s = &sink{}
		h.QueryToSink(context.Background(), spec.SelectAll, nil, s)
		return s.raws, s
	}

	for _, c := range spec.Cases {
		t.Run(c.Format, func(t *testing.T) {
			in, want := rows("a", 5)
			path := c.Write(t, in)

			got, err := reg.FormatOf(archive.Manifest{Path: path})
			if err != nil || got != c.Format {
				t.Errorf("the path %q is format %q (%v), want %q", path, got, err, c.Format)
			}
			raws, s := read(t, path)
			if !equal(raws, want) {
				t.Errorf("read back %v, want %v", raws, want)
			}
			if len(s.failed) != 0 {
				t.Errorf("the reader reported a failure: %v", s.failed)
			}
			if s.sentinels != 1 {
				t.Errorf("the stream ended %d times, want once", s.sentinels)
			}

			// A second archive is its own: reading one never returns the other's rows.
			in2, want2 := rows("b", 3)
			path2 := c.Write(t, in2)
			if path2 == path {
				t.Fatalf("two archives share the path %q", path)
			}
			if raws, _ := read(t, path2); !equal(raws, want2) {
				t.Errorf("second archive read back %v, want %v", raws, want2)
			}
			if raws, _ := read(t, path); !equal(raws, want) {
				t.Errorf("first archive after the second was written: %v, want %v", raws, want)
			}
		})
	}
}
