package ec2

import (
	"sort"
	"sync"
	"time"

	"github.com/w3ft-net/weft/logdb"
)

// memStandbys is a StandbyStore over a slice, with the claim rules of the real
// one: a claim takes an unclaimed standby of the tier, AZ and image, preferring
// one that has heartbeat; a shard that already holds one gets it back.
type memStandbys struct {
	mu   sync.Mutex
	rows []logdb.Standby
}

func (m *memStandbys) ListStandbys() ([]logdb.Standby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]logdb.Standby(nil), m.rows...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].LaunchedAt.Before(out[j].LaunchedAt) })
	return out, nil
}

func (m *memStandbys) InsertStandby(sp *logdb.Standby) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.InstanceID == sp.InstanceID {
			return nil
		}
	}
	m.rows = append(m.rows, *sp)
	return nil
}

func (m *memStandbys) DeleteStandby(instanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rows {
		if r.InstanceID == instanceID {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			break
		}
	}
	return nil
}

func (m *memStandbys) StandbyClaimedBy(shardID string) (*logdb.Standby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claimedBy(shardID), nil
}

func (m *memStandbys) claimedBy(shardID string) *logdb.Standby {
	var best *logdb.Standby
	for i := range m.rows {
		r := &m.rows[i]
		if r.ClaimedBy != nil && *r.ClaimedBy == shardID && (best == nil || r.ClaimedAt.Before(*best.ClaimedAt)) {
			best = r
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

func (m *memStandbys) ClaimStandby(shardID, tier, az, imageID string, at time.Time) (*logdb.Standby, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held := m.claimedBy(shardID); held != nil {
		return held, nil
	}
	pick := -1
	for i, r := range m.rows {
		if r.ClaimedBy != nil || r.Tier != tier || r.AZ != az || r.ImageID != imageID {
			continue
		}
		switch {
		case pick < 0:
			pick = i
		case (r.LastHeartbeatAt != nil) != (m.rows[pick].LastHeartbeatAt != nil):
			if r.LastHeartbeatAt != nil {
				pick = i
			}
		case r.LaunchedAt.Before(m.rows[pick].LaunchedAt):
			pick = i
		}
	}
	if pick < 0 {
		return nil, nil
	}
	m.rows[pick].ClaimedBy, m.rows[pick].ClaimedAt = &shardID, &at
	cp := m.rows[pick]
	return &cp, nil
}

func (m *memStandbys) ObserveStandbyHeartbeat(instanceID, addr, binarySHA string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].InstanceID == instanceID {
			m.rows[i].Addr, m.rows[i].BinarySHA, m.rows[i].LastHeartbeatAt = addr, binarySHA, &at
		}
	}
	return nil
}
