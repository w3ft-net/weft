package ec2

import (
	"context"
	"fmt"
	"sync"

	"github.com/w3ft-net/weft/logdb"
)

func strPtr(s string) *string { return &s }

// swapVolumes is a VolumeManager that tracks which instance each shard's
// volume is attached to, the one fact the swap phases re-derive from.
type swapVolumes struct {
	fakeVolumeManager
	mu          sync.Mutex
	volume      string // the shard's volume id
	shardID     string
	attachedTo  string
	detachErr   error
	detaches    []bool // force flag of each detach
	afterDetach func() // runs once a detach has gone through (a test's lease loss)
}

func (v *swapVolumes) FindByShard(_ context.Context, shardID string) (*logdb.VolumeInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if shardID != v.shardID {
		return nil, nil
	}
	st := "available"
	if v.attachedTo != "" {
		st = "in-use"
	}
	return &logdb.VolumeInfo{VolumeID: v.volume, State: st, InstanceID: v.attachedTo, ShardID: shardID}, nil
}

func (v *swapVolumes) AttachToInstance(_ context.Context, volumeID, instanceID, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.attachErr != nil {
		return v.attachErr
	}
	if v.attachedTo != "" && v.attachedTo != instanceID {
		return fmt.Errorf("VolumeInUse: %s is attached to %s", volumeID, v.attachedTo)
	}
	v.attachedTo = instanceID
	return nil
}

func (v *swapVolumes) DetachFromInstance(_ context.Context, _ string, force bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.detaches = append(v.detaches, force)
	if v.detachErr != nil && !force {
		return v.detachErr
	}
	v.attachedTo = ""
	if v.afterDetach != nil {
		v.afterDetach()
	}
	return nil
}

func (v *swapVolumes) where() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.attachedTo
}
