package alert

import (
	"strings"
	"testing"
)

// An alert pages one alias. Naming a second used to be accepted, the last one
// winning without a word, so a cron meant to page two places paged one.
func TestParseArgs_RejectsMoreThanOneNotifyAlias(t *testing.T) {
	for _, args := range [][]string{
		{"above", "5", "--notify=ops", "--notify=oncall"},
		{"above", "5", "--notify", "ops", "--notify", "oncall"},
		{"above", "5", "--notify=ops", "--notify", "oncall"},
		{"--notify=ops", "--notify=ops"},
	} {
		_, err := ParseArgs(args)
		if err == nil || !strings.Contains(err.Error(), "--notify") {
			t.Errorf("ParseArgs(%v) = %v, want an error naming --notify", args, err)
		}
	}
	for _, args := range [][]string{
		{"above", "5", "--notify=ops"},
		{"above", "5", "--notify", "ops"},
		{"above", "5"},
	} {
		if _, err := ParseArgs(args); err != nil {
			t.Errorf("ParseArgs(%v) = %v, want ok", args, err)
		}
	}
}
