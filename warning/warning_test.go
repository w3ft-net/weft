package warning

import (
	"errors"
	"testing"
)

func TestSplit(t *testing.T) {
	plainErr := errors.New("boom")

	cases := []struct {
		name         string
		err          error
		wantBlocking int
		wantWarnings []string
	}{
		{name: "nil", err: nil, wantBlocking: 0, wantWarnings: nil},
		{name: "plain error", err: plainErr, wantBlocking: 1, wantWarnings: nil},
		{name: "single warning", err: New("caps at %d rows", 500), wantBlocking: 0, wantWarnings: []string{"caps at 500 rows"}},
		{
			name:         "joined warnings only",
			err:          errors.Join(New("first"), New("second")),
			wantBlocking: 0,
			wantWarnings: []string{"first", "second"},
		},
		{
			name:         "joined mix of error and warning still blocks",
			err:          errors.Join(plainErr, New("heads up")),
			wantBlocking: 1,
			wantWarnings: []string{"heads up"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocking, warnings := Split(c.err)
			if len(blocking) != c.wantBlocking {
				t.Errorf("blocking: got %d (%v), want %d", len(blocking), blocking, c.wantBlocking)
			}
			if len(warnings) != len(c.wantWarnings) {
				t.Fatalf("warnings: got %v, want %v", warnings, c.wantWarnings)
			}
			for i, w := range warnings {
				if w != c.wantWarnings[i] {
					t.Errorf("warnings[%d]: got %q, want %q", i, w, c.wantWarnings[i])
				}
			}
		})
	}
}

// TestSplit_BlockingWins pins that a hard error alongside a Warning
// is still a rejection — a save that both fails AND has something to
// say still reports as failed, not as a successful-with-warnings.
func TestSplit_BlockingWins(t *testing.T) {
	err := errors.Join(errors.New("bash syntax error"), New("also this"))
	blocking, warnings := Split(err)
	if len(blocking) == 0 {
		t.Fatal("want at least one blocking error, got none")
	}
	if len(warnings) != 1 || warnings[0] != "also this" {
		t.Errorf("warnings: got %v, want [\"also this\"]", warnings)
	}
}
