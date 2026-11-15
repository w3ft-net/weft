package logdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w3ft-net/weft/shard"
)

// recordingRetention is a Retention that remembers what it was told.
type recordingRetention struct {
	shardID string
	stats   []shard.PartitionStat
	// refuse, when set, is what CheckDelete returns; checked counts its calls.
	refuse  error
	checked int
}

func (r *recordingRetention) CheckDelete(string, int64, time.Time) error {
	r.checked++
	return r.refuse
}

func (r *recordingRetention) ArchiveRecorded(id string, stats []shard.PartitionStat) error {
	r.shardID, r.stats = id, stats
	return nil
}

func (r *recordingRetention) Start(context.Context, *Provisioner) {}

// A deployment's retention decides whether an archive may be deleted without
// force; force skips the question.
func TestProvisionerDeleteArchived_AsksTheRetentionUnlessForced(t *testing.T) {
	reg := NewShardRegistry()
	p := NewProvisioner(reg, nil, ProvisionerConfig{})
	setTestStore(p, openSMTestStore(t))
	reg.Add(&Shard{
		ShardRow: ShardRow{
			ID:        "sh-del",
			State:     StateArchived,
			CreatedAt: time.Now(),
			WindowEnd: time.Now().Add(-time.Hour),
		},
	})
	ret := &recordingRetention{refuse: ErrShardWithinRetention}
	p.config.Retention = ret

	if err := p.DeleteArchived("sh-del", false); !errors.Is(err, ErrShardWithinRetention) {
		t.Fatalf("err = %v, want ErrShardWithinRetention", err)
	}
	if ret.checked != 1 {
		t.Fatalf("retention was asked %d times, want 1", ret.checked)
	}
	_ = p.DeleteArchived("sh-del", true)
	if ret.checked != 1 {
		t.Errorf("a forced delete asked retention (%d calls)", ret.checked)
	}
}
