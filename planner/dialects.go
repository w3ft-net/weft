package planner

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	coords "github.com/w3ft-net/weft/shape"
)

// Dialects are the dialects a deployment's two tiers of storage are written in:
// the live stores, and the archive the live stores are exported to.
type Dialects struct {
	Live, Archive dialect.Dialect
}

var configured atomic.Pointer[Dialects]

// SetDialects sets what Plan, Explain and Analyze plan for. A process calls it
// once at startup, before it plans, as it fixes the shape it is built for. A
// tier left nil is DuckDB, so the zero Dialects is the default, which is also
// what a process that never calls it plans for.
func SetDialects(d Dialects) {
	if d.Live == nil {
		d.Live = dialect.DuckDB
	}
	if d.Archive == nil {
		d.Archive = dialect.DuckDB
	}
	configured.Store(&d)
}

func configuredDialects() Dialects {
	if d := configured.Load(); d != nil {
		return *d
	}
	return Dialects{Live: dialect.DuckDB, Archive: dialect.DuckDB}
}

// split reports whether the tiers are written in different dialects, so the
// archive needs SQL of its own.
func (d Dialects) split() bool { return d.Live.Name() != d.Archive.Name() }

// liveSpelling is the dialect the live tier's plan is made in. A plan pushes the
// same work into both tiers, or what one leaves to compute the other would not,
// so it is planned with only what both have (dialect.Meet), spelled by the tier
// it is for.
func (d Dialects) liveSpelling() dialect.Dialect    { return dialect.Meet(d.Live, d.Archive) }
func (d Dialects) archiveSpelling() dialect.Dialect { return dialect.Meet(d.Archive, d.Live) }

// LiveDialect and ArchiveDialect are the names of the configured tiers' dialects.
func LiveDialect() string    { return configuredDialects().Live.Name() }
func ArchiveDialect() string { return configuredDialects().Archive.Name() }

// archivePlan plans the pipeline again, spelled for the archive tier, and checks
// it is the same plan: the same args and the same stages left to compute.
func archivePlan(live *QueryPlan, ds Dialects, pipeline string, scope coords.Set, cwd string, start, end time.Time) error {
	archive, _, err := compile(ds.archiveSpelling(), pipeline, scope, cwd, start, end)
	if err != nil {
		return fmt.Errorf("plan for the archive: %w", err)
	}
	if !slices.Equal(live.Args, archive.Args) || !slices.Equal(live.ComputeStages, archive.ComputeStages) {
		return errors.New("plan: the live and archive tiers plan differently")
	}
	if (live.TraceStats == nil) != (archive.TraceStats == nil) ||
		live.TraceStats != nil && !slices.Equal(live.TraceStats.Args, archive.TraceStats.Args) {
		return errors.New("plan: the live and archive tiers plan differently")
	}
	live.ArchiveDirectShardSQL = archive.DirectShardSQL
	if live.TraceStats != nil {
		live.TraceStats.ArchiveSQL = archive.TraceStats.SQL
	}
	return nil
}
