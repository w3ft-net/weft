package apitest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
)

// Crons runs the routes that read a cron's results.
func Crons(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"ChartRangeSpec", cronChartRangeSpec},
		{"SaveSyntaxErrorSurfacesTheRealMessage", cronSaveSyntaxError},
		{"SaveTruncationRiskWarnsAndSaves", cronSaveTruncationWarning},
		{"WriteFSCronPathWithAWarningStillSucceeds", cronWriteFSWithWarning},
	})
}

// saveCron saves an enabled cron in namespace ops and returns its id.
func (s Subject) saveCron(t *testing.T, name, pipeline, valueType string) uint {
	t.Helper()
	ns := s.namespace(t, "ops")
	spec := cron.WatchSpec{Schedule: "every 5m", Pipeline: pipeline, Enabled: true, Compiled: cron.Compiled{ValueType: valueType}}
	if err := s.Crons.SaveWatch(s.Owner, ns, "", name, spec); err != nil {
		t.Fatal(err)
	}
	w, err := s.Crons.GetWatch(s.Owner, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	return w.ID
}

// The shell's `chart <cron>` sends its range spec; the server resolves it and
// returns every tag's points inside the window, bounded at both ends. A text
// cron has nothing to chart, and its latest run comes back as text.
func cronChartRangeSpec(t *testing.T, s Subject) {
	id := s.saveCron(t, "hosts", "echo 1 a", cron.ValueNumber)
	now := time.Now().UTC().Truncate(time.Minute)
	val := func(v float64) *float64 { return &v }
	if err := s.Crons.SaveResults([]cron.Result{
		{WatchID: id, RanAt: now.Add(-3 * time.Hour), Value: val(9), Tag: "a"}, // before the window
		{WatchID: id, RanAt: now.Add(-90 * time.Minute), Value: val(1), Tag: "a"},
		{WatchID: id, RanAt: now.Add(-90 * time.Minute), Value: val(2), Tag: "b"},
		{WatchID: id, RanAt: now.Add(-10 * time.Minute), Value: val(3), Tag: "a"}, // after the window
	}); err != nil {
		t.Fatal(err)
	}

	rec := s.Do("GET", "/api/crons/ops/hosts/chart?spec="+url.QueryEscape("2h..30m"), nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Granularity string `json:"granularity"`
		Start, End  int64
		Points      []struct {
			Tag   string  `json:"tag"`
			Count int64   `json:"count"`
			Sum   float64 `json:"sum"`
		} `json:"points"`
	}
	decode(t, rec, &resp)
	if resp.Granularity != "raw" || len(resp.Points) != 2 {
		t.Fatalf("granularity=%q points=%+v, want the two raw points inside 2h..30m", resp.Granularity, resp.Points)
	}
	if resp.Points[0].Tag != "a" || resp.Points[0].Sum != 1 || resp.Points[1].Tag != "b" || resp.Points[1].Sum != 2 {
		t.Fatalf("points = %+v", resp.Points)
	}
	if got := time.UnixMilli(resp.End); now.Add(-30*time.Minute).Sub(got).Abs() > 2*time.Minute {
		t.Errorf("end = %s, want about 30m ago", got)
	}

	holders := s.saveCron(t, "holders", "# type: text\necho i-0abc lease", cron.ValueText)
	holder := "i-0abc"
	if err := s.Crons.SaveResults([]cron.Result{{WatchID: holders, RanAt: now, TextValue: &holder, Tag: "lease"}}); err != nil {
		t.Fatal(err)
	}
	if rec := s.Do("GET", "/api/crons/ops/holders/chart?spec=1h..", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "no numbers to chart") {
		t.Fatalf("chart of a text cron: %d %s", rec.Code, rec.Body)
	}
	if rec := s.Do("GET", "/api/crons/ops/holders/latest", nil); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `[{"tag":"lease","text":"i-0abc"}]` {
		t.Fatalf("latest of a text cron: %d %s", rec.Code, rec.Body)
	}
	if rec := s.Do("GET", "/api/crons/ops/hosts/chart?spec=nonsense", nil); rec.Code != 400 {
		t.Fatalf("bad spec: status %d, want 400", rec.Code)
	}
}

// A syntax error typed into a cron pipeline used to be discarded and answered
// "internal error" 500, so the web UI and MCP, which both save through this
// route, reported a mystery server failure instead of the actionable message.
func cronSaveSyntaxError(t *testing.T, s Subject) {
	s.namespace(t, "x")
	w := s.Do("PUT", "/api/crons/x/broken", []byte(`{"schedule":"*/5 * * * *","pipeline":"cat |"}`))
	if w.Code != 400 {
		t.Fatalf("bad pipeline: got %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no following command") {
		t.Errorf("response body should carry the real parse-error message, got: %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "internal error") {
		t.Errorf("response body should not be the old generic message, got: %q", w.Body.String())
	}
}

const riskyPipeline = `tail -n 1000 /log/x/y/main.log | grep -c \"boom\"`

// A pipeline shaped like a real truncation risk still saves (200), and the
// response carries the warning under a generic "warnings" field: the shape any
// save-time check uses.
func cronSaveTruncationWarning(t *testing.T, s Subject) {
	ns := s.namespace(t, "x")
	w := s.Do("PUT", "/api/crons/x/risky", []byte(`{"schedule":"*/5 * * * *","pipeline":"`+riskyPipeline+`"}`))
	if w.Code != 200 {
		t.Fatalf("truncation-risk pipeline: got %d, want 200 (warn, don't block): %s", w.Code, w.Body.String())
	}
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	decode(t, w, &resp)
	// The truncation lint, then the explanation of the grep -c stage.
	if len(resp.Warnings) != 2 {
		t.Fatalf("got %d warnings, want 2: %v (body: %s)", len(resp.Warnings), resp.Warnings, w.Body.String())
	}
	if !strings.Contains(resp.Warnings[0], "tail -n 1000") {
		t.Errorf("warning text should name the cap, got: %q", resp.Warnings[0])
	}
	if saved, err := s.Crons.GetWatch(s.Owner, ns, "risky"); err != nil || saved.Name != "risky" {
		t.Fatalf("GetWatch = %v, %v: want the cron to have actually saved", saved, err)
	}
}

// The generic file editor (PUT /api/fs/write) routes /cron.d/ writes through the
// same save. A non-blocking warning there was once reported as a 400: a
// truncation-risk cron written through it must still succeed (204), even though
// that surface has nowhere to show the warning text.
func cronWriteFSWithWarning(t *testing.T, s Subject) {
	ns := s.namespace(t, "x")
	body, _ := json.Marshal(map[string]string{
		"path":    "/cron.d/x/risky",
		"content": "# schedule: */5 * * * *\ntail -n 1000 /log/x/y/main.log | grep -c \"boom\"\n",
	})
	if w := s.Do("PUT", "/api/fs/write", body); w.Code != http.StatusNoContent {
		t.Fatalf("cron write with a truncation-risk pipeline: got %d, want 204: %s", w.Code, w.Body.String())
	}
	if saved, err := s.Crons.GetWatch(s.Owner, ns, "risky"); err != nil || saved.Name != "risky" {
		t.Fatalf("GetWatch = %v, %v: want the cron to have actually saved", saved, err)
	}
}
