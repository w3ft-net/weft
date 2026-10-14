package shard

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/awk"
)

// TestAwkFields_MatchesAwk pins the fields column to awk's own split of
// $0: the planner lowers awk's $N to fields[N], so they must agree on
// every line, whatever its whitespace.
func TestAwkFields_MatchesAwk(t *testing.T) {
	for _, line := range []string{
		"a b c",
		"  leading and trailing  ",
		"tabs\tand  doubled\t\tspaces",
		"carriage\rreturn",
		"non breaking space stays inside a field",
		"[2026-10-13 10:00:01] [i-0abc] ERROR checkout failed status=500",
		`{"json": "with spaces", "n": 1}`,
		"",
		"   ",
		"naïve café 🚀",
	} {
		var out bytes.Buffer
		if _, err := awk.Run(`{ for (i = 1; i <= NF; i++) print $i }`, strings.NewReader(line+"\n"), &out, ""); err != nil {
			t.Fatal(err)
		}
		want := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if out.Len() == 0 {
			want = nil
		}
		if got := awkFields(line); !slices.Equal(got, want) {
			t.Errorf("awkFields(%q) = %q, awk splits it into %q", line, got, want)
		}
	}
}
