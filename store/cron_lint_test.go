package store

import (
	"context"
	"testing"

	"github.com/graybeardcorp/weft/planner"
)

type noBinResolver struct{}

func (noBinResolver) Source(name string) (string, string, bool) { return "", "", false }

// TestSaveCronWatch_TruncationWarning pins the generic warning
// mechanism end to end through the real save path: a pipeline whose
// lowered plan shows a head/tail cap feeding a counting stage saves
// successfully, but SaveCronWatch's returned error carries a
// store.Warning that Split surfaces as a warning, not a rejection.
func TestSaveCronWatch_TruncationWarning(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cron-lint-warn")
	user, _ := db.CreateUser("bob-lint@example.test")

	pipeline := `tail -n 1000 /log/x/y/main.log | grep -c "boom"`
	lowered := planner.LowerForStorage(context.Background(), pipeline, int64(cust.ID), "bolt", noBinResolver{})
	if lowered == nil {
		t.Fatal("LowerForStorage returned nil — fixture pipeline should lower cleanly")
	}

	err := db.SaveCronWatch(cust.ID, user.ID, "truncation-risk", "*/5 * * * *", pipeline, lowered)

	blocking, warnings := Split(err)
	if len(blocking) != 0 {
		t.Fatalf("SaveCronWatch: got blocking error %v, want the save to succeed", blocking)
	}
	if len(warnings) != 1 {
		t.Fatalf("SaveCronWatch: got %d warnings, want 1: %v", len(warnings), warnings)
	}

	// The save actually happened despite the non-nil error.
	w, getErr := db.GetCronWatch(cust.ID, "truncation-risk")
	if getErr != nil {
		t.Fatalf("GetCronWatch: %v (want the watch to have saved)", getErr)
	}
	if w.Pipeline != pipeline {
		t.Errorf("saved pipeline: got %q, want %q", w.Pipeline, pipeline)
	}
}

// TestSaveCronWatch_NoWarningForSafePipeline is the negative
// counterpart: a plain, uncapped pipeline saves clean, with no
// warnings at all.
func TestSaveCronWatch_NoWarningForSafePipeline(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cron-lint-safe")
	user, _ := db.CreateUser("alice-lint@example.test")

	pipeline := `cat /log/x/y/main.log | grep ERROR`
	lowered := planner.LowerForStorage(context.Background(), pipeline, int64(cust.ID), "bolt", noBinResolver{})

	err := db.SaveCronWatch(cust.ID, user.ID, "safe-cron", "*/5 * * * *", pipeline, lowered)
	blocking, warnings := Split(err)
	if len(blocking) != 0 {
		t.Fatalf("SaveCronWatch: got blocking error %v, want success", blocking)
	}
	if len(warnings) != 0 {
		t.Errorf("SaveCronWatch: got warnings %v, want none", warnings)
	}
}

// TestSaveCronWatch_SyntaxErrorStillBlocks pins that an actual hard
// validation error (unchanged behavior) still rejects the save — the
// generic mechanism must not accidentally turn real errors into
// warnings.
func TestSaveCronWatch_SyntaxErrorStillBlocks(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cron-lint-bad-syntax")
	user, _ := db.CreateUser("carol-lint@example.test")

	err := db.SaveCronWatch(cust.ID, user.ID, "bad-cron", "*/5 * * * *", `cat /log/x/y/main.log |`, nil)
	blocking, _ := Split(err)
	if len(blocking) == 0 {
		t.Fatal("want a blocking error for an unparseable pipeline, got none")
	}
}
