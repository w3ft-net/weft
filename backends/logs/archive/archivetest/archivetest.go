// Package archivetest is the conformance suite for an archive: what a store
// writes, and what reads it back. Every pairing passes it: the open build's
// DuckDB writer and reader, and any store and reader a deployment builds
// another way.
package archivetest

import (
	"fmt"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// Case is one format a store writes.
type Case struct {
	// Format is the ID the archive's path must resolve to (archive.FormatOf).
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
	// Read is the raw text of every row of the archive at path, in ts order, as
	// the reader the deployment routes that format to returns it.
	Read func(t *testing.T, path string) []string
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
	for _, c := range spec.Cases {
		t.Run(c.Format, func(t *testing.T) {
			in, want := rows("a", 5)
			path := c.Write(t, in)

			got, err := reg.FormatOf(archive.Manifest{Path: path})
			if err != nil || got != c.Format {
				t.Errorf("the path %q is format %q (%v), want %q", path, got, err, c.Format)
			}
			if raws := spec.Read(t, path); !equal(raws, want) {
				t.Errorf("read back %v, want %v", raws, want)
			}

			// A second archive is its own: reading one never returns the other's rows.
			in2, want2 := rows("b", 3)
			path2 := c.Write(t, in2)
			if path2 == path {
				t.Fatalf("two archives share the path %q", path)
			}
			if raws := spec.Read(t, path2); !equal(raws, want2) {
				t.Errorf("second archive read back %v, want %v", raws, want2)
			}
			if raws := spec.Read(t, path); !equal(raws, want) {
				t.Errorf("first archive after the second was written: %v, want %v", raws, want)
			}
		})
	}
}
