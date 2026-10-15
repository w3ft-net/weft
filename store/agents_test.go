package store

import (
	"testing"
	"time"
)

func agentSource(t *testing.T, name string) (db *Store, lgID uint) {
	t.Helper()
	db, nsID, custID, nsName, app, _ := setupSource(t, name)
	if err := db.UpsertPendingLogGroup(nsID, app, "h", "d", `["x"]`, "{}", 1, false); err != nil {
		t.Fatal(err)
	}
	lg, err := db.FindLogGroup(custID, nsName, app)
	if err != nil || lg == nil {
		t.Fatalf("FindLogGroup: %v %v", lg, err)
	}
	return db, lg.ID
}

// A source has many hosts, so versions are kept per agent: the question the
// API asks is about the oldest agent reading a schema, not the last to poll.
func TestRecordAgentSeenKeepsOneRowPerAgent(t *testing.T) {
	db, id := agentSource(t, "agents-per-host")
	for _, a := range []struct{ instance, version string }{
		{"web-1", "0.28.0"}, {"web-2", "0.23.0"}, {"web-1", "0.28.0"},
	} {
		if err := db.RecordAgentSeen(id, a.instance, a.version); err != nil {
			t.Fatal(err)
		}
	}
	agents, err := db.RecentAgents(id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %+v, want one row per host", agents)
	}
	got := map[string]string{}
	for _, a := range agents {
		got[a.Instance] = a.Version
	}
	if got["web-1"] != "0.28.0" || got["web-2"] != "0.23.0" {
		t.Errorf("versions = %v", got)
	}
}

// An upgraded agent replaces its old version at once.
func TestRecordAgentSeenUpdatesOnVersionChange(t *testing.T) {
	db, id := agentSource(t, "agents-upgrade")
	if err := db.RecordAgentSeen(id, "web-1", "0.23.0"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAgentSeen(id, "web-1", "0.28.0"); err != nil {
		t.Fatal(err)
	}
	agents, _ := db.RecentAgents(id, time.Hour)
	if len(agents) != 1 || agents[0].Version != "0.28.0" {
		t.Fatalf("agents = %+v, want the upgraded version", agents)
	}
}

// Agents poll every minute; recording each poll would be a write per host per
// source per minute. An unchanged agent is touched only when its row is stale.
func TestRecordAgentSeenDoesNotRewriteFreshRows(t *testing.T) {
	db, id := agentSource(t, "agents-throttle")
	if err := db.RecordAgentSeen(id, "web-1", "0.28.0"); err != nil {
		t.Fatal(err)
	}
	// Age the row by a minute: a fresh-enough row must be left alone.
	if err := db.DB().Exec(`UPDATE log_group_agents SET seen_at = now() - interval '1 minute' WHERE log_group_id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	before, _ := db.RecentAgents(id, time.Hour)
	if err := db.RecordAgentSeen(id, "web-1", "0.28.0"); err != nil {
		t.Fatal(err)
	}
	after, _ := db.RecentAgents(id, time.Hour)
	if !after[0].SeenAt.Equal(before[0].SeenAt) {
		t.Errorf("a one-minute-old, unchanged row was rewritten (%v -> %v)", before[0].SeenAt, after[0].SeenAt)
	}
	// An hour-old one is refreshed.
	if err := db.DB().Exec(`UPDATE log_group_agents SET seen_at = now() - interval '1 hour' WHERE log_group_id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAgentSeen(id, "web-1", "0.28.0"); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.RecentAgents(id, 30*time.Minute); len(again) != 1 {
		t.Errorf("a stale row was not refreshed: %+v", again)
	}
}

// Agents that have gone quiet do not count: a host retired last month must not
// block a schema forever.
func TestRecentAgentsIgnoresQuietOnes(t *testing.T) {
	db, id := agentSource(t, "agents-quiet")
	if err := db.RecordAgentSeen(id, "gone", "0.20.0"); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().Exec(`UPDATE log_group_agents SET seen_at = now() - interval '30 days' WHERE log_group_id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	if agents, _ := db.RecentAgents(id, 7*24*time.Hour); len(agents) != 0 {
		t.Errorf("a month-quiet agent counted: %+v", agents)
	}
}

func TestOldestAgentVersion(t *testing.T) {
	agents := []AgentSeen{{Instance: "a", Version: "0.28.0"}, {Instance: "b", Version: "0.23.0"}, {Instance: "c", Version: "0.27.1"}}
	if v, n := OldestAgentVersion(agents); v != "0.23.0" || n != 3 {
		t.Errorf("oldest = %q of %d, want 0.23.0 of 3", v, n)
	}
	if v, n := OldestAgentVersion(nil); v != "" || n != 0 {
		t.Errorf("no agents = %q of %d", v, n)
	}
}
