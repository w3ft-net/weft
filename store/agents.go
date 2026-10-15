package store

import (
	"time"

	"github.com/graybeardcorp/weft/internal/release"
)

// AgentSeen is one agent (host) that has been polling a source, and the
// version it reported.
type AgentSeen struct {
	Instance string
	Version  string
	SeenAt   time.Time
}

// agentRefresh is how stale an unchanged agent's row may get before a poll
// rewrites it. Agents poll about once a minute; this keeps that from being a
// write per host per source per minute while keeping "recently seen" honest.
const agentRefresh = 10 * time.Minute

// RecordAgentSeen notes that the agent on instance, running version, polled
// the source. A new agent, a changed version, or a row older than agentRefresh
// is written; otherwise it is left alone.
func (s *Store) RecordAgentSeen(logGroupID uint, instance, version string) error {
	return s.db.Exec(`
		INSERT INTO log_group_agents (log_group_id, instance, version, seen_at)
		VALUES (?, ?, ?, now())
		ON CONFLICT (log_group_id, instance) DO UPDATE
		   SET version = EXCLUDED.version, seen_at = now()
		 WHERE log_group_agents.version <> EXCLUDED.version
		    OR log_group_agents.seen_at < now() - make_interval(secs => ?)`,
		logGroupID, instance, version, agentRefresh.Seconds()).Error
}

// RecentAgents lists the agents that polled the source within window.
func (s *Store) RecentAgents(logGroupID uint, window time.Duration) ([]AgentSeen, error) {
	var out []AgentSeen
	err := s.db.Raw(`
		SELECT instance, version, seen_at FROM log_group_agents
		 WHERE log_group_id = ? AND seen_at > now() - make_interval(secs => ?)
		 ORDER BY instance`, logGroupID, window.Seconds()).Scan(&out).Error
	return out, err
}

// OldestAgentVersion is the lowest version among agents, and how many there
// are; ("", 0) for none.
func OldestAgentVersion(agents []AgentSeen) (version string, count int) {
	for _, a := range agents {
		if version == "" || release.VersionNewer(version, a.Version) {
			version = a.Version
		}
	}
	return version, len(agents)
}

// RecentAgentsFor is RecentAgents for several sources in one query, keyed by
// log group id.
func (s *Store) RecentAgentsFor(logGroupIDs []uint, window time.Duration) (map[uint][]AgentSeen, error) {
	out := map[uint][]AgentSeen{}
	if len(logGroupIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		LogGroupID uint
		Instance   string
		Version    string
		SeenAt     time.Time
	}
	err := s.db.Raw(`
		SELECT log_group_id, instance, version, seen_at FROM log_group_agents
		 WHERE log_group_id IN ? AND seen_at > now() - make_interval(secs => ?)
		 ORDER BY log_group_id, instance`, logGroupIDs, window.Seconds()).Scan(&rows).Error
	for _, r := range rows {
		out[r.LogGroupID] = append(out[r.LogGroupID], AgentSeen{Instance: r.Instance, Version: r.Version, SeenAt: r.SeenAt})
	}
	return out, err
}

// NewestAgentVersion is the highest version among agents, "" for none.
func NewestAgentVersion(agents []AgentSeen) string {
	newest := ""
	for _, a := range agents {
		if newest == "" || release.VersionNewer(a.Version, newest) {
			newest = a.Version
		}
	}
	return newest
}
