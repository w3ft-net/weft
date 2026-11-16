package token

import (
	"regexp"
	"testing"
)

func TestRawAndPublicIDForms(t *testing.T) {
	raw, err := NewRaw()
	if err != nil || !regexp.MustCompile(`^wft_[0-9a-f]{32}$`).MatchString(raw) {
		t.Fatalf("NewRaw = %q err=%v", raw, err)
	}
	pub, err := NewPublicID()
	if err != nil || !regexp.MustCompile(`^tok_[0-9a-f]{16}$`).MatchString(pub) {
		t.Fatalf("NewPublicID = %q err=%v", pub, err)
	}
	if Prefix(raw) != raw[:8] || Prefix("wft") != "wft" {
		t.Errorf("Prefix(%q) = %q, Prefix(wft) = %q", raw, Prefix(raw), Prefix("wft"))
	}
	// Hash is pinned: if it changes, every existing token stops validating.
	if got := Hash("wft_dev_0000000000000000"); got != "9215d4a3950cd56d42324c9baa13c8420a6792f6835ec3c40d4d9707a477b8db" {
		t.Errorf("Hash = %q: the digest changed", got)
	}
}

// The forecast is the old store computation: ~300 bytes a row, a row per
// collector per tick, 30 days a month, 30s ticks and all five collectors when
// the config says nothing.
func TestForecastMetrics(t *testing.T) {
	ts := []Token{
		{MetricsEnabled: true, MetricsInterval: "60s", MetricsCollectors: []string{"cpu", "memory"}}, // 1440 ticks * 2 * 300 * 30
		{MetricsEnabled: true},                              // defaults: 30s, 5 collectors: 2880 * 5 * 300 * 30
		{MetricsEnabled: true, Revoked: true},               // revoked: not counted
		{MetricsEnabled: false},                             // off: not counted
		{MetricsEnabled: true, MetricsInterval: "nonsense"}, // unparseable: the default interval
	}
	got := ForecastMetrics(ts)
	want := MetricsForecast{
		AgentsWithMetrics:   3,
		ProjectedBytesMonth: int64(1440*2*300*30) + 2*int64(2880*5*300*30),
	}
	if got != want {
		t.Errorf("forecast = %+v, want %+v", got, want)
	}
	if got := ForecastMetrics(nil); got != (MetricsForecast{}) {
		t.Errorf("empty forecast = %+v", got)
	}
}
