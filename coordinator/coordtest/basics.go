//go:build test

package coordtest

import (
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/theme"
)

// Basics runs what the shell does that is not a log query: plain bash, a script
// read with the brand palette, a range ahead of a command, and a redirect into
// /tmp of a log query, of a file under /etc, and the read back.
func Basics(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		// Execute takes any pipeline, log or not: a plain bash one boots the
		// interpreter locally and returns its stdout.
		{"PlainBashPipelineRunsLocally", func(t *testing.T, f *queryFixture) {
			if got := strings.TrimRight(f.exec(t, "echo 42", f.full()).Text, "\n"); got != "42" {
				t.Errorf(`Execute("echo 42") text = %q, want "42"`, got)
			}
		}},
		// A `cat` of a /bin script run on the server highlights with weft's
		// palette, not the interpreter's default: when a delegated client ships
		// `cat /bin/x` there instead of reading it itself, the bytes that come
		// back must carry the same weave as a file it read locally.
		{"CatHighlightsWithWeftPalette", func(t *testing.T, f *queryFixture) {
			f.s.saveBin(t, "greet", "echo hello")
			out := f.exec(t, "cat /bin/greet", coordinator.TimeRange{}).Text
			// echo is a command token: brand blue (256-colour 33), where the
			// default is bold white (15).
			if want := theme.Palette().Command; !strings.Contains(out, want) {
				t.Errorf("cat /bin/greet output missing weft Command SGR %q\ngot: %q", want, out)
			}
			if strings.Contains(out, "1;38;5;15m") {
				t.Errorf("cat /bin/greet still uses the default Command SGR (bold white)\ngot: %q", out)
			}
		}},
		// The clipboard deep link and the web shell's range chip send
		// `range <spec>; <command>`: the range must apply and the command run.
		{"RangeThenCommand", func(t *testing.T, f *queryFixture) {
			d := f.exec(t, "range 2026-10-11T04:51:39Z..2026-10-12T04:51:39Z; echo ran", coordinator.TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)})
			if out := d.Text + d.Stderr; strings.Contains(out, "cannot parse") || !strings.Contains(out, "ran") {
				t.Errorf("out = %q", out)
			}
		}},
		// A log query whose output is redirected into /tmp lands the filtered rows
		// there and the redirect consumes the result, so the shell does not print
		// them twice. awk's LEVEL equality is a stable pushdown path, unlike a
		// grep, whose regex behaviour varies by DuckDB build.
		{"LogQueryRedirectToTmp", func(t *testing.T, f *queryFixture) {
			sentinel := f.exec(t, `cat /log/vimgym.log | awk 'LEVEL == "ERROR"' > /tmp/errs`, f.full())
			if sentinel.Text != "" || len(sentinel.Rows) != 0 {
				t.Errorf("a redirected query should leave the response empty (the file is the sink); got Text=%q rows=%d", sentinel.Text, len(sentinel.Rows))
			}
			body := resultText(f.exec(t, "cat /tmp/errs", f.full()))
			for _, want := range []string{
				"ERROR vimgym i-001 connection timeout",
				"ERROR vimgym i-001 null pointer exception",
				"ERROR vimgym i-003 timeout waiting for response",
				"ERROR vimgym i-001 500 internal server error",
				"ERROR vimgym i-001 connection timeout to upstream",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("/tmp/errs missing %q\nfile contents:\n%s", want, body)
				}
			}
			if strings.Contains(body, "INFO vimgym") {
				t.Errorf("/tmp/errs contains INFO lines, the filter was not applied:\n%s", body)
			}
			if strings.Contains(body, "i-999") {
				t.Errorf("/tmp/errs contains another group's rows:\n%s", body)
			}
		}},
		// `cat /etc/notify.yaml > /tmp/notify.yaml` once routed as a log query
		// against a source called "/etc/notify.yaml" and was refused as unknown,
		// writing nothing.
		{"CatRedirectFromEtcToTmp", func(t *testing.T, f *queryFixture) {
			const conf = "system:\n  email:\n    - \"oncall@example.test\"\nweft-info:\n  slack:\n    - \"#weft-auto\"\n"
			if err := f.s.Notify.Save(f.s.Owner, "", conf); err != nil {
				t.Fatal(err)
			}
			tr := coordinator.TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
			f.exec(t, "cat /etc/notify.yaml > /tmp/notify.yaml", tr)
			if got := f.exec(t, "cat /tmp/notify.yaml", tr).Text; got != conf {
				t.Errorf("/tmp/notify.yaml contents:\n got: %q\nwant: %q", got, conf)
			}
		}},
		// Once a /tmp file exists, `cat` streams it back rather than bouncing off
		// the source allowlist as an unknown source.
		{"CatTmpReturnsContent", func(t *testing.T, f *queryFixture) {
			tr := coordinator.TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
			f.exec(t, `echo "hello from tmp" > /tmp/note`, tr)
			if got := resultText(f.exec(t, "cat /tmp/note", tr)); !strings.Contains(got, "hello from tmp") {
				t.Errorf("cat /tmp/note: got %q, want it to contain %q", got, "hello from tmp")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newQueryFixture(t, newSubject(t))) })
	}
}
