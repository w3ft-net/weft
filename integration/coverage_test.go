//go:build integration && !localstack

package integration

import (
	"reflect"
	"testing"
)

// TestParseFuncLine has no infrastructure dependency — it tests the
// parseFuncLine helper directly. The two TestRequireRanIn_* tests
// that need a coverage profile on disk live in coverage_cover_test.go
// under the `cover` build tag.
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
