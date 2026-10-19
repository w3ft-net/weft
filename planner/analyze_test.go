package planner

import (
	"flag"
	"os"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/analyze.golden")

const src = "cat /log/acme/api/main.log"

func TestAnalyze_Placement(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		want     []Placement
		barrier  int
	}{
		{"plain filter", src + ` | grep ERROR`, []Placement{Optimized}, 0},
		{"awk pattern and metavar print, then grep",
			src + ` | awk '$3 == "ingest-stream:" { print HOST, $0 }' | grep 'first row'`,
			[]Placement{Optimized, Optimized}, 0},
		{"awk action that is more than a print",
			src + ` | awk '$3 == "x" { printf "%s\n", $0 }' | grep y`,
			[]Placement{Partial, PostRetrieval}, 1},
		{"awk with no pattern and a loop",
			src + ` | awk '{ for (i = 1; i <= NF; i++) n++; print n }'`,
			[]Placement{PostRetrieval}, 1},
		{"command with no source form, then a filter", src + ` | tr a b | grep y`,
			[]Placement{PostRetrieval, PostRetrieval}, 1},
		{"narrowing before the unlowerable stage", src + ` | grep y | tr a b`,
			[]Placement{Optimized, PostRetrieval}, 2},
		{"keyed sort blocks the grep after it", src + ` | sort -n | grep y`,
			[]Placement{PostRetrieval, PostRetrieval}, 1},
		{"plain sort lets the grep move below it", src + ` | sort | grep y`,
			[]Placement{PostRetrieval, Optimized}, 1},
		{"count aggregates at the source", src + ` | grep y | wc -l`,
			[]Placement{Optimized, Optimized}, 0},
		{"jq select", src + ` | jq 'select(.status == 500)'`, []Placement{Optimized}, 0},
		{"head is a limit", src + ` | head -n 5`, []Placement{Optimized}, 0},
		{"a filter after a source cap", `tail -n 100 /log/acme/api/main.log | grep y`, []Placement{PostRetrieval}, 1},
		{"a filter before a source cap", src + ` | grep y | head -n 5`, []Placement{Optimized, Optimized}, 0},
		{"an alert after the query is not a stage", src + ` | grep y | alert above 5`, []Placement{Optimized}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.pipeline, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []Placement
			for _, s := range a.Stages {
				got = append(got, s.Where)
				if s.Where == Optimized && s.Why != nil {
					t.Errorf("stage %d (%s) is optimized but Why = %v", s.Index, s.Src, s.Why)
				}
				if s.Index == a.Barrier && s.Why == nil {
					t.Errorf("barrier stage %d (%s) has no Why", s.Index, s.Src)
				}
			}
			if len(got) != len(tt.want) {
				t.Fatalf("placements = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("stage %d placement = %v, want %v", i+1, got[i], tt.want[i])
				}
			}
			if a.Barrier != tt.barrier {
				t.Errorf("barrier = %d, want %d", a.Barrier, tt.barrier)
			}
		})
	}
}

func TestAnalyze_AlertOnly(t *testing.T) {
	a, err := Analyze(`alert above 5`, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Stages) != 0 || a.Barrier != 0 {
		t.Errorf("an alert with no query has no stages: %+v", a)
	}
}

func TestAnalyze_NotPlannable(t *testing.T) {
	if _, err := Analyze(`echo hi`, "", nil); err == nil {
		t.Error("a pipeline that isn't a log query should return the planner's error")
	}
}

// A stage behind the barrier is covered by the barrier line, so it says
// nothing of its own; a stage that is unlowerable itself still says so.
func TestAnalyze_StageBehindBarrierSaysNothing(t *testing.T) {
	a, err := Analyze(src+` | tr a b | grep y | cut -c1-5`, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Barrier != 1 {
		t.Fatalf("barrier = %d, want 1", a.Barrier)
	}
	if a.Stages[1].Why != nil {
		t.Errorf("grep behind tr: Why = %+v, want none", a.Stages[1].Why)
	}
	if got := a.Stages[0].Why.Because; !strings.Contains(got, "tr") {
		t.Errorf("tr: Because = %q, want it to name the command", got)
	}
}

// Advice is specific to the command and flag, not one sentence for all.
func TestAnalyze_FlagAdviceNamesTheFix(t *testing.T) {
	tests := []struct{ stage, want string }{
		{"grep -c boom", "wc -l"},
		{"awk -F, '{ print $2 }'", "cut -d<sep>"},
		{`awk '{ n++ } END { print n }'`, "wc -l"},
		{"sed 's/a/b/'", "Substitution"},
	}
	for _, tt := range tests {
		a, err := Analyze(src+" | "+tt.stage, "", nil)
		if err != nil || a.Stages[0].Why == nil {
			t.Fatalf("%s: %v %+v", tt.stage, err, a)
		}
		if text := a.Stages[0].Why.Because + " " + a.Stages[0].Why.Try; !strings.Contains(text, tt.want) {
			t.Errorf("%s: advice %q lacks %q", tt.stage, text, tt.want)
		}
	}
}

func TestAnalyze_FilterAfterLineCountSaysSo(t *testing.T) {
	a, err := Analyze(`tail -n 100 /log/acme/api/main.log | grep y`, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Stages[0].Why.Because; !strings.Contains(got, "head or tail") {
		t.Errorf("Because = %q, want it to name the head or tail", got)
	}
}

// Every reason a frontend can give has user-facing advice, so adding a
// reasonCode without writing it fails here.
func TestAdviceCoversEveryReason(t *testing.T) {
	for code := reasonNone + 1; code < reasonCodeCount; code++ {
		adv := adviceFor(reason{code: code, detail: "x"}, "")
		if adv == nil || adv.Because == "" {
			t.Errorf("reason code %d has no advice", code)
		}
	}
}

// The rendered text for a spread of pipelines, pinned so a wording
// change shows up as a diff to review. Run `go test ./planner -update`
// to accept one.
func TestAnalyze_SummaryGolden(t *testing.T) {
	pipelines := []string{
		src + ` | grep ERROR`,
		src + ` | awk '$3 == "ingest-stream:" { print HOST, $0 }' | grep 'first row'`,
		src + ` | awk '$3 == "x" { printf "%s\n", $0 }' | grep y`,
		src + ` | awk '$NF > 10 { print $1 }'`,
		src + ` | awk 'BEGIN { FS = "," } { print $2 }'`,
		src + ` | awk -F, '{ print $2 }'`,
		src + ` | tr a b | grep y`,
		src + ` | grep y | tr a b`,
		src + ` | sort -n | grep y`,
		src + ` | jq -s 'length'`,
		src + ` | jq 'select(.a == 1) | .b | tostring | ascii_downcase'`,
		src + ` | sed 's/a/b/'`,
		src + ` | cut -d, -f1,3-`,
		src + ` | grep -e a -e b`,
		`tail -n 100 /log/acme/api/main.log | grep y`,
	}
	var b strings.Builder
	for _, p := range pipelines {
		a, err := Analyze(p, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		b.WriteString("$ " + strings.TrimPrefix(p, src+" | ") + "\n" + a.Render(false) + "\n")
	}
	const path = "testdata/analyze.golden"
	if *updateGolden {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("summary differs from %s (run with -update to accept):\n%s", path, b.String())
	}
}

// A leading /bin script is analyzed through its body, as a run inlines it,
// not reported as an unknown command that runs after transfer.
func TestAnalyze_InlinesBinScript(t *testing.T) {
	resolver := fakeBinResolver{binByName: map[string]string{
		"find-errors": `cat /log/loom/api/prod.log | grep $1`,
	}}
	a, err := Analyze(`find-errors ERROR | awk '{ printf "%s\n", $0 }'`, "", resolver)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(a.Stages) != 2 || a.Stages[0].Src != "grep ERROR" || a.Stages[0].Where != Optimized {
		t.Fatalf("stages = %+v, want grep ERROR at the source first", a.Stages)
	}
	if a.Barrier != 2 {
		t.Errorf("barrier = %d, want 2", a.Barrier)
	}
}

// Terminal output colors the placement: green optimized, yellow partly,
// red post-retrieval. Plain output, which travels to places that don't
// render colors, has none.
func TestAnalyze_RenderColors(t *testing.T) {
	a, err := Analyze(src+` | grep y | awk '$3 == "x" { printf "%s\n", $0 }' | tr a b`, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	colored := a.Render(true)
	for _, want := range []string{
		"\x1b[32moptimized retrieval\x1b[0m",
		"\x1b[33mpartly optimized\x1b[0m",
		"\x1b[31mpost-retrieval\x1b[0m",
	} {
		if !strings.Contains(colored, want) {
			t.Errorf("colored output lacks %q:\n%s", want, colored)
		}
	}
	if plain := a.Render(false); strings.Contains(plain, "\x1b") {
		t.Errorf("plain output has escape sequences:\n%q", plain)
	}
	ok, _ := Analyze(src+" | grep y", "", nil)
	if got := ok.Render(true); !strings.Contains(got, "\x1b[32mEvery stage: optimized retrieval.") {
		t.Errorf("all-optimized line isn't green: %q", got)
	}
}
