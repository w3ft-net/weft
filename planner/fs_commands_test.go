package planner

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Filesystem-navigation commands handed a /log/ path are not log reads:
// they browse the pseudofile tree, which plain warp+shellfs serves. Plan
// must classify them ErrNotPlannable so they fall through there, instead
// of rejecting them with the "pipeline must start with cat, head, or
// tail" error meant for content readers like grep.
func TestPlan_FilesystemCommandsWithLogPath_FallThrough(t *testing.T) {
	for _, pipeline := range []string{
		`ls /log/loom`,
		`ls -l /log/loom/prod`,
		`cd /log/loom/prod/api`,
		`tree /log/loom`,
		`pwd`,
		`pushd /log/loom`,
	} {
		_, err := planBare(pipeline, "", time.Time{}, time.Time{})
		if !errors.Is(err, ErrNotPlannable) {
			t.Errorf("Plan(%q) err = %v, want ErrNotPlannable", pipeline, err)
		}
	}
}

// The b5c68b6 guard stays: a content reader given a /log/ path directly
// still gets the explicit error rather than silently returning no rows.
func TestPlan_ContentReaderWithLogPath_StillRejected(t *testing.T) {
	_, err := planBare(`grep ERROR /log/weft/coordinator/main.log`, "", time.Time{}, time.Time{})
	if err == nil || errors.Is(err, ErrNotPlannable) || !strings.Contains(err.Error(), "must start with cat, head, or tail") {
		t.Fatalf("err = %v, want the 'must start with cat, head, or tail' rejection", err)
	}
}

// `tail -f > /tmp/x` has no sensible meaning: follow never finishes, so
// the redirect would receive a truncated finite read. It's refused rather
// than silently writing a partial file.
func TestPlan_FollowWithFSRedirectIsRefused(t *testing.T) {
	for _, p := range []string{
		`tail -f /log/acme/prod/api/main.log > /tmp/stream`,
		`tail -f /log/acme/prod/api/main.log | grep ERROR >> /tmp/stream`,
	} {
		_, err := planBare(p, "", time.Time{}, time.Time{})
		if err == nil || !strings.Contains(err.Error(), "tail -f") {
			t.Errorf("Plan(%q) = %v, want an error naming tail -f", p, err)
		}
	}
}
