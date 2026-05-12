package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Runaway saved scripts must be cancellable — otherwise an infinite
// loop in a /bin script holds a goroutine / a cron slot indefinitely.
// runLocal enforces this by running warp under a context.WithTimeout
// and calling interp.Cancel() when the deadline fires. Warp observes
// the cancel at the next AST boundary (between top-level lists or
// loop iterations) and stops.
func TestRunLocal_InfiniteLoopHitsTimeout(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	cust, err := db.GetCustomerByID(custID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}

	// A while loop that never terminates. Warp's `true` builtin keeps
	// returning 0, so the loop iterates forever — until we cancel it.
	// No internal I/O, just pure loop iterations going through
	// execWhile's per-iteration exiting check.
	body := `while true; do echo spin; done`
	if err := db.SaveShellFunction(custID, "", "runaway", body, "bash"); err != nil {
		t.Fatalf("save script: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{
		Store:                  db,
		ScriptExecutionTimeout: 200 * time.Millisecond,
	})

	start := time.Now()
	res, err := c.runLocal("runaway", TimeRange{}, cust.Name, custID, "", nil)
	if err != nil {
		t.Fatalf("runLocal returned synchronous error before stream drain: %v", err)
	}
	// Streaming runLocal surfaces the timeout via the terminal Next
	// after the watcher cancels warp and the goroutine closes the
	// pipes. Drain to provoke that terminal call.
	_, err = res.Drain(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "execution time limit") {
		t.Errorf("err = %q, want message about execution time limit", err)
	}
	// Timeout is 200ms; grace period is 500ms. Allow generous headroom
	// for test-machine variance without letting a regression that
	// ignores the timeout pass silently.
	if elapsed > 2*time.Second {
		t.Errorf("runLocal took %s; expected return within grace period of the 200ms timeout", elapsed)
	}
}

// A script that finishes inside the timeout returns normally, with
// no timeout error.
func TestRunLocal_QuickScriptCompletesNormally(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	cust, err := db.GetCustomerByID(custID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}

	if err := db.SaveShellFunction(custID, "", "quick", `echo ok`, "bash"); err != nil {
		t.Fatalf("save script: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{
		Store:                  db,
		ScriptExecutionTimeout: 5 * time.Second,
	})

	result, err := c.runLocal("quick", TimeRange{}, cust.Name, custID, "", nil)
	if err != nil {
		t.Fatalf("runLocal: %v", err)
	}
	resultD := drainExec(t, result)
	if strings.TrimSpace(resultD.Text) != "ok" {
		t.Errorf("stdout = %q, want `ok`", resultD.Text)
	}
}
