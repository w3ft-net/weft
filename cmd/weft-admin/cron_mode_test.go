package main

import (
	"flag"
	"reflect"
	"strings"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

func TestParseCronIDs(t *testing.T) {
	ok := []struct {
		args     []string
		allowAll bool
		ids      []uint64
		all      bool
	}{
		{[]string{"12", "7"}, false, []uint64{12, 7}, false},
		{[]string{"all"}, true, nil, true},
	}
	for _, c := range ok {
		ids, all, err := parseCronIDs(c.args, c.allowAll)
		if err != nil || !reflect.DeepEqual(ids, c.ids) || all != c.all {
			t.Errorf("%v: ids=%v all=%v err=%v", c.args, ids, all, err)
		}
	}
	bad := []struct {
		args     []string
		allowAll bool
	}{
		{nil, true},                       // nothing given
		{[]string{"all"}, false},          // set must name crons
		{[]string{"all", "3"}, true},      // mixed
		{[]string{"0"}, false},            // not an id
		{[]string{"coord-uptime"}, false}, // names are not ids
		{[]string{"-1"}, false},
	}
	for _, c := range bad {
		if _, _, err := parseCronIDs(c.args, c.allowAll); err == nil {
			t.Errorf("%v (allowAll=%v): expected an error", c.args, c.allowAll)
		}
	}
}

// Flags may follow the ids: `set 12 13 --yes`.
func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	coord := fs.String("coord", "", "")
	pos := parseInterspersed(fs, []string{"12", "--yes", "13", "-coord", "h:1", "14"})
	if !reflect.DeepEqual(pos, []string{"12", "13", "14"}) || !*yes || *coord != "h:1" {
		t.Fatalf("pos=%v yes=%v coord=%q", pos, *yes, *coord)
	}
}

func TestFormatAndSelectCronModes(t *testing.T) {
	all := []*pb.CronModeInfo{
		{WatchId: 1, Name: "coord-uptime", Schedule: "every 5m", Mode: "claims", LeaseHolder: "claims"},
		{WatchId: 2, Name: "disk-fill-rate", Schedule: "every 1h", Mode: "lease", LeaseHolder: "coord-a"},
	}
	out := formatCronModes(selectCrons(all, []uint64{2}))
	if !strings.Contains(out, "disk-fill-rate") || strings.Contains(out, "coord-uptime") {
		t.Fatalf("selection leaked or dropped rows:\n%s", out)
	}
	if got := filterMode(all, "claims"); len(got) != 1 || got[0].WatchId != 1 {
		t.Fatalf("filterMode = %v", got)
	}
}
