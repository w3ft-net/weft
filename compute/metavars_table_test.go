package compute

import (
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/shard"
)

func TestMetaVarTable_MatchesDocsAndShardSchema(t *testing.T) {
	doc, err := os.ReadFile("../docs/manual/metavars.md")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := shard.NewDuckDBShard("metavar-table", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	for name, col := range planner.JQMetaVars {
		if !strings.Contains(string(doc), "`$"+name+"`") {
			t.Errorf("docs/manual/metavars.md has no row for $%s", name)
		}
		if _, err := ds.Query(t.Context(), "SELECT raw, ts, "+col+" FROM logs LIMIT 0"); err != nil {
			t.Errorf("meta-variable %s reads column %q, which the shard schema lacks: %v", name, col, err)
		}
	}
	for name := range planner.AwkMetaVars {
		if !strings.Contains(string(doc), "`"+name+"`") && !strings.Contains(string(doc), "`"+name+"` *") {
			t.Errorf("docs/manual/metavars.md has no row for %s", name)
		}
	}
}
