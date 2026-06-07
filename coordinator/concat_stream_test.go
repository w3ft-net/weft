package coordinator

import (
	"context"
	"errors"
	"testing"

	"github.com/graybeardcorp/weft/compute"
)

// TestConcatStream_PrimaryThenNext walks the basic shape: primary
// emits some lines, exhausts cleanly, concat advances to next, next
// emits its lines and EOFs.
func TestConcatStream_PrimaryThenNext(t *testing.T) {
	primary := compute.NewSliceLineStream([]compute.Line{
		{Raw: "p1"}, {Raw: "p2"},
	}, nil)
	nextCalled := 0
	cs := &concatStream{
		primary: primary,
		next: func() compute.LineStream {
			nextCalled++
			return compute.NewSliceLineStream([]compute.Line{
				{Raw: "n1"}, {Raw: "n2"},
			}, nil)
		},
	}

	got := drainConcat(t, cs)
	want := []string{"p1", "p2", "n1", "n2"}
	if !equal(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	if nextCalled != 1 {
		t.Errorf("next factory called %d times, want 1", nextCalled)
	}
}

// TestConcatStream_PrimaryError_NextSkipped pins the error
// short-circuit: a primary error returns immediately without invoking
// the next factory. Important for the follow path — if the
// historical batch fails we don't want to fire up a broker
// subscription that the caller never reads.
func TestConcatStream_PrimaryError_NextSkipped(t *testing.T) {
	wantErr := errors.New("historical bombed")
	primary := compute.NewSliceLineStream(nil, wantErr)
	nextCalled := false
	cs := &concatStream{
		primary: primary,
		next: func() compute.LineStream {
			nextCalled = true
			return compute.NewSliceLineStream(nil, nil)
		},
	}

	_, _, err := cs.Next(context.Background())
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if nextCalled {
		t.Error("next factory called after primary error")
	}
}

// TestConcatStream_NextLazy ensures next() is invoked exactly once,
// at the moment primary returns ok=false the first time. Multiple
// reads after exhaustion shouldn't keep recreating the next stream.
func TestConcatStream_NextLazy(t *testing.T) {
	primary := compute.NewSliceLineStream(nil, nil)
	calls := 0
	cs := &concatStream{
		primary: primary,
		next: func() compute.LineStream {
			calls++
			return compute.NewSliceLineStream(nil, nil)
		},
	}

	for i := 0; i < 5; i++ {
		_, _, _ = cs.Next(context.Background())
	}
	if calls != 1 {
		t.Errorf("next factory called %d times, want exactly 1", calls)
	}
}

// drainConcat reads cs to exhaustion, returning Raw values.
func drainConcat(t *testing.T, cs *concatStream) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	for {
		line, ok, err := cs.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, line.Raw)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
