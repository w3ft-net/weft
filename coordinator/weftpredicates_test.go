//go:build !defaultshape

package coordinator

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPredicate_Above_Latest(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "cpu", []string{"70", "75", "80", "85"}, time.Minute)
	wf := testWF(t, db, custID)

	fn := predicateAbove(wf)
	if code := fn([]string{"ops/cpu", "80"}, nil, nil, &bytes.Buffer{}); code != 0 {
		t.Errorf("expected fire (>80), got code %d", code)
	}
	if code := fn([]string{"ops/cpu", "90"}, nil, nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire (>90), got code %d", code)
	}
}

func TestPredicate_Below_WindowAverage(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "cpu", []string{"70", "75", "80", "85"}, time.Minute)
	wf := testWF(t, db, custID)

	fn := predicateBelow(wf)
	// avg ~77.5; below 80 with --average → fires.
	if code := fn([]string{"ops/cpu", "80", "1h", "--average"}, nil, nil, &bytes.Buffer{}); code != 0 {
		t.Errorf("expected fire (avg<80), got code %d", code)
	}
	if code := fn([]string{"ops/cpu", "70", "1h", "--average"}, nil, nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire (avg<70), got code %d", code)
	}
}

func TestPredicate_Absent_NoWatch(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	wf := testWF(t, db, custID)

	fn := predicateAbsent(wf)
	// No watch named "ghost" → Latest errors → absent fires.
	if code := fn([]string{"ops/ghost"}, nil, nil, &bytes.Buffer{}); code != 0 {
		t.Errorf("expected fire on missing watch, got code %d", code)
	}
}

func TestPredicate_Changed(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "tag", []string{"v1", "v2"}, time.Minute)
	wf := testWF(t, db, custID)

	fn := predicateChanged(wf)
	if code := fn([]string{"ops/tag"}, nil, nil, &bytes.Buffer{}); code != 0 {
		t.Errorf("expected fire when latest differs from prev, got code %d", code)
	}

	seedWatch(t, db, custID, "stable", []string{"x", "x"}, time.Minute)
	if code := fn([]string{"ops/stable"}, nil, nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire when value unchanged, got code %d", code)
	}
}

func TestPredicate_Rising_PrevTick(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// latest=110, prev=100, +10%.
	seedWatch(t, db, custID, "errs", []string{"100", "110"}, time.Minute)
	wf := testWF(t, db, custID)

	fn := predicateRising(wf)
	if code := fn([]string{"ops/errs", "5"}, nil, nil, &bytes.Buffer{}); code != 0 {
		t.Errorf("expected fire (rising >=5%%), got code %d", code)
	}
	if code := fn([]string{"ops/errs", "20"}, nil, nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire (rising >=20%%), got code %d", code)
	}
}

func TestPredicate_Outlier(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// Stable history around 50 with one big spike at the end. The
	// current implementation's rolling avg/stddev includes the spike
	// itself (per the anomaly-conditions plan), so we need enough
	// stable points to dilute it. 19 stable values + 1 spike at 500
	// puts the spike well outside 2σ.
	stable := make([]string, 0, 20)
	for i := 0; i < 19; i++ {
		stable = append(stable, "50")
	}
	stable = append(stable, "500")
	seedWatch(t, db, custID, "spiky", stable, time.Minute)
	wf := testWF(t, db, custID)

	fn := predicateOutlier(wf)
	if code := fn([]string{"ops/spiky", "2", "30m"}, nil, nil, &bytes.Buffer{}); code != 0 {
		t.Errorf("expected fire (500 >> avg + 2σ), got code %d", code)
	}
	// --below restricts to below; we're high, so it shouldn't fire.
	if code := fn([]string{"ops/spiky", "2", "30m", "--below"}, nil, nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire on --below when current is high, got code %d", code)
	}
}

func TestPredicate_Outlier_FlatHistoryNoFire(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// Identical values — stddev=0; even a different latest shouldn't
	// trigger because we floor at 1%|avg|.
	seedWatch(t, db, custID, "flat", []string{"100", "100", "100", "100", "100"}, time.Minute)
	wf := testWF(t, db, custID)

	fn := predicateOutlier(wf)
	if code := fn([]string{"ops/flat", "2", "10m"}, nil, nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire on flat history, got code %d", code)
	}
}

func TestStdinComparator_Gt(t *testing.T) {
	fn := stdinComparator(func(x, t float64) bool { return x > t })
	if code := fn([]string{"50"}, strings.NewReader("75.0\n"), nil, &bytes.Buffer{}); code != 0 {
		t.Errorf("expected fire 75>50, got code %d", code)
	}
	if code := fn([]string{"50"}, strings.NewReader("25\n"), nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire 25>50, got code %d", code)
	}
	// Non-numeric input → no-fire.
	if code := fn([]string{"50"}, strings.NewReader("hello\n"), nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire on non-numeric, got code %d", code)
	}
	// Empty stdin → no-fire.
	if code := fn([]string{"50"}, strings.NewReader(""), nil, &bytes.Buffer{}); code != 1 {
		t.Errorf("expected no-fire on empty stdin, got code %d", code)
	}
}
