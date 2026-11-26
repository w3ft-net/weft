package cli

import (
	"errors"
	"github.com/w3ft-net/weft/warning"
	"reflect"
	"testing"

	"github.com/w3ft-net/weft/cmd/weft/mcp/proposals"
)

// A save that comes back with only warnings was applied: the server stored it
// and is telling the user something. apply must unstage it and carry on, not
// stop the queue and leave an applied proposal staged.
func TestApplyStaged_WarningIsApplied(t *testing.T) {
	ps := []proposals.Proposal{{ID: "0001", Kind: "cron", Name: "a"}, {ID: "0002", Kind: "cron", Name: "b"}}
	var removed []string
	applied, failed := applyStaged(ps,
		func(p proposals.Proposal) error {
			if p.ID == "0001" {
				return warning.New("tail -n caps this cron")
			}
			return nil
		},
		func(id string) error { removed = append(removed, id); return nil })

	want := []string{"0001", "0002"}
	if !reflect.DeepEqual(applied, want) || len(failed) != 0 {
		t.Fatalf("applied=%v failed=%v, want both applied and none failed", applied, failed)
	}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed=%v, want both unstaged", removed)
	}
}

// A refusal still stops the queue and leaves that proposal and the rest staged.
func TestApplyStaged_RefusalStops(t *testing.T) {
	ps := []proposals.Proposal{{ID: "0001", Kind: "cron", Name: "a"}, {ID: "0002", Kind: "cron", Name: "b"}}
	var removed []string
	applied, failed := applyStaged(ps,
		func(proposals.Proposal) error { return errors.New("schedule required") },
		func(id string) error { removed = append(removed, id); return nil })

	if len(applied) != 0 || !reflect.DeepEqual(failed, []string{"0001"}) || len(removed) != 0 {
		t.Fatalf("applied=%v failed=%v removed=%v, want stop at 0001 with nothing unstaged", applied, failed, removed)
	}
}
