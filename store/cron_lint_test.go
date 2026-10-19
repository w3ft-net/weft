package store

import (
	"context"
	"strings"
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
	// The truncation lint, then the explanation of the grep -c stage.
	if len(warnings) != 2 || !strings.Contains(warnings[0], "caps this cron") {
		t.Fatalf("SaveCronWatch: got warnings %q, want the truncation lint then the explanation", warnings)
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

// A cron pays for a stage that runs after transfer on every fire, so
// saving one explains it: a warning names the stage and what to change.
func TestSaveCronWatch_ExplainsStageAfterTransfer(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cron-lint-explain")
	user, _ := db.CreateUser("dave-lint@example.test")

	pipeline := `cat /log/x/y/main.log | awk '{ printf "%s\n", $0 }' | alert above 5`
	lowered := planner.LowerForStorage(context.Background(), pipeline, int64(cust.ID), "bolt", noBinResolver{})

	err := db.SaveCronWatch(cust.ID, user.ID, "explain-cron", "*/5 * * * *", pipeline, lowered)
	blocking, warnings := Split(err)
	if len(blocking) != 0 {
		t.Fatalf("SaveCronWatch: got blocking error %v, want success", blocking)
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	msg := warnings[0]
	for _, want := range []string{"on every fire", "awk", "after transfer", "Barrier: stage 1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning lacks %q:\n%s", want, msg)
		}
	}
}

// A /bin script's own stages aren't analyzed, so a pipeline that calls one
// is never reported as stuck after transfer.
func TestSaveCronWatch_NoExplanationForBinScript(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("cron-lint-bin")
	user, _ := db.CreateUser("erin-lint@example.test")

	pipeline := `cat /log/x/y/main.log | /bin/errs | alert above 5`
	err := db.SaveCronWatch(cust.ID, user.ID, "bin-cron", "*/5 * * * *", pipeline, nil)
	if blocking, warnings := Split(err); len(blocking) != 0 || len(warnings) != 0 {
		t.Fatalf("got blocking %v, warnings %q; want neither", blocking, warnings)
	}
}
