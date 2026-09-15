package logdb

import (
	"context"
	"fmt"
	"sync"
)

// fakeVolumeManager is the in-memory VolumeManager fake the shard/launcher
// tests share. Re-homed from lifecycle_test.go when that file was deleted
// in the Slice C source cleanup (the slice-A note flagged this move).
type fakeVolumeManager struct {
	mu sync.Mutex

	availableByTier map[string][]VolumeInfo // keyed by tier+az ("small/us-east-2a")
	createdByShard  map[string]string       // shard_id → volume_id
	attached        []attachedPair
	tagged          map[string]string // volume → shard id
	createErr       error
	attachErr       error

	// findResult / findErr drive FindByShard for tests that exercise
	// volume-existence logic (e.g. the stranded-SUSPENDED reaper).
	// Unset → FindByShard returns (nil, nil) as before.
	findResult map[string]*VolumeInfo
	findErr    map[string]error

	nextVolID int
}

type attachedPair struct{ volumeID, instanceID, device string }

func (f *fakeVolumeManager) key(tier, az string) string { return tier + "/" + az }

func (f *fakeVolumeManager) CreateForShard(_ context.Context, opts CreateVolumeOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.nextVolID++
	id := fmt.Sprintf("vol-created-%d", f.nextVolID)
	if f.createdByShard == nil {
		f.createdByShard = make(map[string]string)
	}
	f.createdByShard[opts.ShardID] = id
	return id, nil
}

func (f *fakeVolumeManager) FindByShard(_ context.Context, shardID string) (*VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.findErr[shardID]; err != nil {
		return nil, err
	}
	return f.findResult[shardID], nil
}

func (f *fakeVolumeManager) ListAvailableByTier(_ context.Context, tier, az string) ([]VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.availableByTier[f.key(tier, az)], nil
}

func (f *fakeVolumeManager) TagWithShardID(_ context.Context, volumeID, shardID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tagged == nil {
		f.tagged = make(map[string]string)
	}
	f.tagged[volumeID] = shardID
	return nil
}

func (f *fakeVolumeManager) AttachToInstance(_ context.Context, volumeID, instanceID, device string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached = append(f.attached, attachedPair{volumeID, instanceID, device})
	return f.attachErr
}

func (f *fakeVolumeManager) DetachFromInstance(_ context.Context, _ string, _ bool) error {
	return nil
}

func (f *fakeVolumeManager) DeleteVolume(_ context.Context, _ string) error { return nil }
