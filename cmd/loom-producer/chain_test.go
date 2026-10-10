package main

import (
	"context"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

func tempLogFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "loom-chain-*.log")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func readLines(t *testing.T, f *os.File) []string {
	t.Helper()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestRunOneChain_SharesTraceIDAcrossHops is the core correlation
// property `trace <id>` depends on: all three hops of one chain carry
// the identical trace ID (field 3), and it's a real 32-lowercase-hex
// weft.NewTraceID()-shaped value.
func TestRunOneChain_SharesTraceIDAcrossHops(t *testing.T) {
	api, edge, worker := tempLogFile(t), tempLogFile(t), tempLogFile(t)
	rng := rand.New(rand.NewPCG(1, 2))

	if ok := runOneChain(context.Background(), rng, api, edge, worker); !ok {
		t.Fatal("runOneChain returned false with no cancellation")
	}

	apiFields := strings.Fields(readLines(t, api)[0])
	edgeFields := strings.Fields(readLines(t, edge)[0])
	workerFields := strings.Fields(readLines(t, worker)[0])

	traceID := apiFields[2]
	if len(traceID) != 32 {
		t.Errorf("trace ID %q is not 32 chars", traceID)
	}
	for _, c := range traceID {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("trace ID %q contains non-lowercase-hex char %q", traceID, c)
			break
		}
	}
	if edgeFields[2] != traceID || workerFields[2] != traceID {
		t.Errorf("hops don't share a trace ID: api=%s edge=%s worker=%s", traceID, edgeFields[2], workerFields[2])
	}
}

// TestRunOneChain_EventNames pins the event-name choice the tour's
// discovery lesson (grep ' checkout ') depends on — distinct from ambient
// traffic's request/job_completed/etc. names. (Ambient request lines can
// still mention /checkout as a path, which is why the lesson greps for the
// event token between spaces; see lesson_trace_test.go.)
func TestRunOneChain_EventNames(t *testing.T) {
	api, edge, worker := tempLogFile(t), tempLogFile(t), tempLogFile(t)
	rng := rand.New(rand.NewPCG(3, 4))

	if ok := runOneChain(context.Background(), rng, api, edge, worker); !ok {
		t.Fatal("runOneChain returned false with no cancellation")
	}

	apiEvent := strings.Fields(readLines(t, api)[0])[3]
	edgeEvent := strings.Fields(readLines(t, edge)[0])[3]
	workerEvent := strings.Fields(readLines(t, worker)[0])[3]

	if apiEvent != "checkout" {
		t.Errorf("api event = %q, want checkout", apiEvent)
	}
	if edgeEvent != "checkout_processed" {
		t.Errorf("edge event = %q, want checkout_processed", edgeEvent)
	}
	if workerEvent != "fulfillment" {
		t.Errorf("worker event = %q, want fulfillment", workerEvent)
	}
}

// TestRunOneChain_CancelledContextStops confirms a cancelled context
// halts mid-chain rather than writing the remaining hops.
func TestRunOneChain_CancelledContextStops(t *testing.T) {
	api, edge, worker := tempLogFile(t), tempLogFile(t), tempLogFile(t)
	rng := rand.New(rand.NewPCG(5, 6))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if ok := runOneChain(ctx, rng, api, edge, worker); ok {
		t.Error("runOneChain returned true with an already-cancelled context")
	}
}

func TestFulfillmentContent_ErrorReasonPresent(t *testing.T) {
	// A small deterministic sweep: at least one of many draws should
	// land the ~5% ERROR branch and carry a reason= field.
	sawError := false
	rng := rand.New(rand.NewPCG(7, 8))
	for i := 0; i < 200; i++ {
		level, content := fulfillmentContent(rng)
		if level == "ERROR" {
			sawError = true
			if !strings.Contains(content, "reason=") {
				t.Errorf("ERROR fulfillment content %q missing reason=", content)
			}
		}
	}
	if !sawError {
		t.Error("200 draws never hit the ~5% ERROR branch — check the probability or the seed")
	}
}
