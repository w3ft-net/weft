// Package cron is the contract of the cron domain: scheduled pipelines
// (watches), what each run recorded (results), and the alert incidents that
// dedupe notifications. It declares the Store interface every build implements
// (Postgres for the hosted build, SQLite for the open one; see
// defaults and cron/cronstoretest).
//
// Whose cron it is, is said in the build's own coordinates, a shape.Added, as in
// package source: the open build's is the empty value and its store has no
// org. A result belongs to a watch, not to an owner: reading results is by watch
// id, and the owner is checked when the watch is resolved (GetWatch).
//
// The store keeps and returns raw facts; it does not analyze them. Percentiles,
// the seasonal baseline and the like are pure functions over the rows (see
// analytics.go), so both stores agree on them by construction. How a store
// answers a long window is its own business: the hosted store stitches its
// hourly, 4-hour and daily rollups behind WindowStats and Series because it
// prunes raw rows; the open build keeps every raw row and has no rollups.
//
// Not in this contract yet: a watch's version history, the /bin scripts it
// depends on, and the cron-user home, which are entangled with other domains.
//
// The package imports only the standard library and the shape package, so a
// process that links an implementation stays cgo-free.
package cron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// ErrNotFound is the answer for a watch the caller named that does not exist.
// Another owner's watch is indistinguishable from a missing one.
var ErrNotFound = errors.New("cron not found")

// ErrNotYours is the answer to an amendment restricted to the author when the
// current version was written by someone else.
var ErrNotYours = errors.New("the current version was written by someone else")

// ErrExists is the answer to a move onto a name the new home already holds.
var ErrExists = errors.New("a cron with that name already exists there")

// Value types of a watch: numbers by default, text for a `# type: text` cron.
const (
	ValueNumber = "number"
	ValueText   = "text"
)

// Run statuses a watch records.
const (
	StatusOK      = "ok"
	StatusError   = "error"
	StatusAlert   = "alert"
	StatusTimeout = "timeout"
	StatusInvalid = "invalid"
)

// Compiled is what the caller parsed out of a pipeline at save time, stamped on
// the watch so the executor does not parse it again every tick. ValueField and
// TagField are the output schema: a 1-indexed column ("3") or a dot-prefixed
// JSON path (".foo.bar"); nil means the default text rule.
type Compiled struct {
	ValueField *string
	TagField   *string
	HasAlert   bool
	// ValueType is ValueNumber or ValueText.
	ValueType string
	// AlertCondition and AlertThreshold are the dashboard projection of the alert
	// stage's numeric cutoff, set only for above, below, eq and neq.
	AlertCondition *string
	AlertThreshold *float64
}

// WatchSpec is a watch as a caller saves it.
type WatchSpec struct {
	// Schedule is "*/5 * * * *" or "every 1m"; the caller has validated it.
	Schedule string
	Pipeline string
	Enabled  bool
	Compiled Compiled
	// LoweredPlan is the JSON plan produced at save time, nil when the pipeline
	// did not lower; the run path then plans inline. A store may reformat the
	// JSON (jsonb does); it keeps the document.
	LoweredPlan []byte
}

// Watch is a saved cron.
type Watch struct {
	ID    uint
	Owner shape.Added
	// Namespace is the cron's home namespace; 0 is the owner level. A name is
	// unique within its home.
	Namespace uint
	Name      string
	WatchSpec
	// ConsecutiveStrikes counts runs in a row that ran out of time; at the bench
	// threshold the cron is disabled. A clean run, and any save that re-enables
	// the cron, resets it.
	ConsecutiveStrikes int
	LastRunAt          *time.Time
	// LastStatus is one of the Status constants, "" before the first run.
	LastStatus string
	// ContentHash is sha256 over schedule + "\n" + pipeline.
	ContentHash string
	Message     string
	// CreatedBy is the user who first saved the watch, "" for none; an edit keeps
	// it. AuthoredBy is the user who wrote the current version.
	CreatedBy  string
	AuthoredBy string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Result is what one run, or one line of a multi-row run, recorded.
type Result struct {
	ID      uint
	WatchID uint
	RanAt   time.Time
	Stdout  string
	Stderr  string
	// ExitCode and DurationMs describe the run.
	ExitCode   int
	DurationMs int64
	// Value is the numeric form of the output, nil when it is not a number; Tag
	// labels the row in multi-row mode and is "" otherwise. TextValue is a text
	// cron's value, with Value nil.
	Value     *float64
	Tag       string
	TextValue *string
}

// Point is a numeric result: when, and what.
type Point struct {
	At    time.Time
	Value float64
}

// Ends is the oldest and newest numeric result in a window and how many there
// are. First and Last are 0 when N is 0.
type Ends struct {
	First, Last float64
	N           int64
}

// WindowStats is the exact sum-level statistics of a watch's numeric results
// over a window, merged however the store kept them. Count is 0 when the window
// held none.
type WindowStats struct {
	Count int64
	Sum   float64
	SumSq float64
	Min   float64
	Max   float64
}

// Mean is Sum/Count.
func (c WindowStats) Mean() float64 { return c.Sum / float64(c.Count) }

// Stddev is the population standard deviation, derived from the sums so it
// stays exact however the buckets were merged.
func (c WindowStats) Stddev() float64 {
	mean := c.Mean()
	v := c.SumSq/float64(c.Count) - mean*mean
	if v < 0 {
		v = 0 // float cancellation on a near-constant series
	}
	return math.Sqrt(v)
}

// Bucket is one point of a series. For a raw row Count is 1 and Sum, Min and Max
// all equal its value; a rollup bucket keeps the sums so merging stays exact.
// Tag is "" for single-row watches.
type Bucket struct {
	Start time.Time
	Tag   string
	Count int64
	Sum   float64
	SumSq float64
	Min   float64
	Max   float64
}

// Series is a watch's stored points over a window, oldest first.
type Series struct {
	// Granularity names the tier the points came from: "raw" for the open build.
	Granularity string
	Start, End  time.Time
	Buckets     []Bucket
}

// Incident is one continuous stretch of ticks during which a watch's alert
// holds, kept so a notification goes out once per stretch rather than once per
// tick. Identity is (watch, tag): in multi-row mode two tags open and close
// independently.
type Incident struct {
	ID         string
	WatchID    uint
	Tag        string
	OpenedAt   time.Time
	ClosedAt   *time.Time
	LastSeenAt time.Time
}

// ContentHash is a watch's content hash: sha256 over schedule + "\n" + pipeline,
// hex. A save that leaves it unchanged, and the enabled flag too, is a no-op.
func ContentHash(schedule, pipeline string) string {
	sum := sha256.Sum256([]byte(schedule + "\n" + pipeline))
	return hex.EncodeToString(sum[:])
}

// How long raw results and daily rollups are kept: the furthest back a weft::
// function can read. The hosted store prunes to these (its result tables'
// retention mirrors them); the open one keeps every row.
const (
	RawRetention   = 7 * 24 * time.Hour
	DailyRetention = 400 * 24 * time.Hour
)

// Store is the persistence contract of the cron domain.
type Store interface {
	// SaveWatch creates the watch (ns, name), or overwrites it, reviving it if it
	// was deleted. An overwrite keeps the original creator, sets who wrote the
	// version and clears its message, and a save that
	// changes nothing is a no-op. A save that turns a disabled watch on resets its
	// strikes. author may be empty.
	SaveWatch(owner shape.Added, ns uint, author, name string, w WatchSpec) error
	// GetWatch is the owner's watch by home and name, ErrNotFound if none.
	GetWatch(owner shape.Added, ns uint, name string) (*Watch, error)
	// ListWatches is the owner's watches by name.
	ListWatches(owner shape.Added) ([]Watch, error)
	// AmendMessage sets the commit message of a watch's current version. A
	// non-empty authorID restricts it to a version that user wrote, ErrNotYours
	// otherwise. ErrNotFound if there is no such watch.
	AmendMessage(owner shape.Added, ns uint, authorID, name, message string) error
	// DeleteWatch removes a watch. Deleting one that is not there is not an
	// error.
	DeleteWatch(owner shape.Added, ns uint, name string) error
	// MoveWatch re-homes a watch: the same row, so its results come with it.
	// ErrNotFound if there is no such watch, ErrExists if the new home holds the
	// name.
	MoveWatch(owner shape.Added, fromNS uint, fromName string, toNS uint, toName string) error
	// CountEnabled is how many of the owner's watches are on; a disabled one is
	// free.
	CountEnabled(owner shape.Added) (int, error)

	// ListEnabled is every enabled watch of every owner: what the executor
	// schedules. The one read that crosses owners.
	ListEnabled(ctx context.Context) ([]Watch, error)
	// RecordRun stamps a watch with the time and status of its latest run; a clean
	// run resets the strikes. ErrNotFound if there is no such watch.
	RecordRun(ctx context.Context, id uint, at time.Time, status string, resetStrikes bool) error
	// SetLoweredPlan replaces a watch's saved plan, nil to clear it.
	SetLoweredPlan(ctx context.Context, id uint, plan []byte) error
	// RecordStrike adds a strike to a watch whose run ran out of time and
	// disables it once it has limit in a row, reporting whether this call benched
	// it. It is one atomic step, so concurrent coordinators cannot both miss the
	// threshold. ErrNotFound if there is no such watch.
	RecordStrike(ctx context.Context, id uint, limit int) (benched bool, err error)
	// Disable turns a watch off at once, reporting whether this call did (false if
	// it was already off), so concurrent coordinators report it once.
	Disable(ctx context.Context, id uint) (disabled bool, err error)

	// SaveResults records a batch of results; an empty batch is a no-op.
	SaveResults(rs []Result) error
	// ListResults is the newest results of a watch, newest first. A non-empty tag
	// narrows to it.
	ListResults(watchID uint, limit int, tag string) ([]Result, error)
	// LatestRun is the rows of a watch's newest run that stored a value or text,
	// numbers highest first and then by tag. Rows with neither (a failed run's
	// diagnostic) are skipped.
	LatestRun(watchID uint) ([]Result, error)
	// CountResults is how many results a watch has had since the time. A non-empty
	// tag narrows to it; nonEmpty counts only those with output.
	CountResults(watchID uint, since time.Time, tag string, nonEmpty bool) (int64, error)
	// NumericEnds is the oldest and newest numeric result since the time.
	NumericEnds(watchID uint, since time.Time) (Ends, error)
	// NumericPoints is every numeric result since the time, oldest first: the
	// input of the analytics. Callers bound the window.
	NumericPoints(watchID uint, since time.Time) ([]Point, error)
	// WindowStats is the statistics of the numeric results since the time. A
	// non-empty tag narrows to it; empty combines every tag.
	WindowStats(watchID uint, since time.Time, tag string) (WindowStats, error)
	// Series is the watch's points with start <= time < end, oldest first, every
	// tag included, from the finest tier that covers start.
	Series(watchID uint, start, end time.Time) (Series, error)

	// OpenOrTouchIncident returns the open incident for (watch, tag), creating it
	// if there is none; a tick that finds one open only advances its LastSeenAt.
	OpenOrTouchIncident(watchID uint, tag string) (*Incident, error)
	// CloseIncident closes the open incident for (watch, tag), if any.
	CloseIncident(watchID uint, tag string) error
	// IncidentDeliveries is the targets already told about an incident.
	IncidentDeliveries(incidentID string) (map[string]bool, error)
	// RecordDeliveries marks targets as told about an incident.
	RecordDeliveries(incidentID string, targets []string) error
}

// UserID is the subject of namespace ns's cron user: the identity a cron homed
// there runs as, whose groups decide what it reads.
func UserID(ns uint) string { return fmt.Sprintf("cron:%d", ns) }
