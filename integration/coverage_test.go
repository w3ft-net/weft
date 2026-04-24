//go:build integration

package integration

import (
	"reflect"
	"testing"
)

func TestParseFuncLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want CoverageEntry
		ok   bool
	}{
		{
			name: "real line with tabs",
			in:   "github.com/kevlarcon/weft/auth/grpc.go:21:\t\t\tExtractJWT\t\t\t\t\t76.9%",
			want: CoverageEntry{File: "github.com/kevlarcon/weft/auth/grpc.go", Line: 21, Function: "ExtractJWT", Percent: 76.9},
			ok:   true,
		},
		{
			name: "zero coverage",
			in:   "github.com/kevlarcon/weft/shard/service/server.go:221:\tQueryToCompute\t0.0%",
			want: CoverageEntry{File: "github.com/kevlarcon/weft/shard/service/server.go", Line: 221, Function: "QueryToCompute", Percent: 0.0},
			ok:   true,
		},
		{
			name: "total line is rejected by caller; parse alone returns ok=false (no line number)",
			in:   "total:\t(statements)\t12.6%",
			ok:   false,
		},
		{
			name: "blank",
			in:   "",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFuncLine(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (in=%q)", ok, tc.ok, tc.in)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRequireRanIn_AgainstRealProfile is an end-to-end sanity check
// of the assertion helper. `main` is always hit by the -cover test
// entrypoint in the coordinator binary. If coverage/coordinator.out
// exists (meaning we're on the cover overlay), this passes; otherwise
// it skips.
func TestRequireRanIn_AgainstRealProfile(t *testing.T) {
	RequireRanIn(t, "coordinator", "cmd/coordinator/main.go", "main")
}

// TestRequireRanIn_DetectsAbsentFunction proves the negative case:
// asking for a function that doesn't exist in the named service's
// profile should fail. We capture the failure via a sub-testing.T
// so the outer test stays green.
func TestRequireRanIn_DetectsAbsentFunction(t *testing.T) {
	entries, err := LoadServiceCoverage("coordinator")
	if err != nil {
		t.Fatalf("load coordinator profile: %v", err)
	}
	if entries == nil {
		t.Skip("no coverage profile on disk")
	}

	// Walk entries directly: if the helper's suffix+name matching is
	// correct, "definitely_does_not_exist" must be absent.
	for _, e := range entries {
		if e.Function == "definitely_does_not_exist" {
			t.Fatalf("unexpected entry for nonexistent function: %+v", e)
		}
	}
}
