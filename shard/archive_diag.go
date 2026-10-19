package shard

import (
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrArchiveInFlight is returned by Archive when another attempt is still
// running on the same shard. The coordinator abandons an attempt at its
// timeout but cannot always stop the export, so the next tick (or the other
// coordinator) must not start a second export against the same DuckDB file.
var ErrArchiveInFlight = errors.New("archive already in flight")

// beginArchive claims the single archive slot, returning this attempt's id
// (1-based, per process) and the release func. A second claim while one is
// held fails with ErrArchiveInFlight, naming the running attempt and its age.
func (s *DuckDBShard) beginArchive() (attempt int64, end func(), err error) {
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	if s.archiveRunning {
		return 0, nil, fmt.Errorf("%w: attempt %d, %.0fs in", ErrArchiveInFlight,
			s.archiveAttempt, time.Since(s.archiveStarted).Seconds())
	}
	s.archiveRunning = true
	s.archiveAttempt++
	s.archiveStarted = time.Now()
	attempt = s.archiveAttempt
	return attempt, func() {
		s.archiveMu.Lock()
		s.archiveRunning = false
		s.archiveMu.Unlock()
	}, nil
}

// memDiag is a point-in-time reading of the memory state that decides whether
// an export thrashes. DuckDB allocates natively, so the Go heap says nothing
// here: RSS, swap and memory pressure (PSI) come from /proc, and DuckDB's own
// accounting from its system tables. Every field is best-effort — /proc is
// absent off Linux and a failed read leaves the zero value.
type memDiag struct {
	RSSKB        int64
	SwapKB       int64
	PSISomeAvg10 float64 // % of the last 10s at least one task stalled on memory
	PSIFullAvg10 float64 // % of the last 10s all tasks stalled on memory (thrashing)
	SpillBytes   int64   // bytes under <db>.tmp, DuckDB's spill directory

	// The fields below need a DuckDB connection (HasSQL). They are skipped
	// while an export runs: S3Config.ConfigureDuckDB pins the pool to a single
	// connection, which the COPY holds for its whole duration.
	HasSQL         bool
	DuckDBMemBytes int64  // sum of duckdb_memory().memory_usage_bytes
	TempFiles      int64  // count of duckdb_temporary_files()
	MemoryLimit    string // DuckDB memory_limit setting
	Threads        int64  // DuckDB threads setting
}

func (d memDiag) String() string {
	out := fmt.Sprintf("mem rss_kb=%d swap_kb=%d psi_some10=%.1f psi_full10=%.1f spill_bytes=%d",
		d.RSSKB, d.SwapKB, d.PSISomeAvg10, d.PSIFullAvg10, d.SpillBytes)
	if d.HasSQL {
		out += fmt.Sprintf(" duckdb_mem_bytes=%d temp_files=%d memory_limit=%q threads=%d",
			d.DuckDBMemBytes, d.TempFiles, d.MemoryLimit, d.Threads)
	}
	return out
}

// procMemDiag reads the readings that need no DuckDB connection: /proc and the
// spill directory. Safe to call while an export holds the connection.
func (s *DuckDBShard) procMemDiag() memDiag {
	var d memDiag
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		d.RSSKB, d.SwapKB = parseProcStatusKB(string(b))
	}
	if b, err := os.ReadFile("/proc/pressure/memory"); err == nil {
		d.PSISomeAvg10, d.PSIFullAvg10 = parsePSIAvg10(string(b))
	}
	if s.dbPath != "" && s.dbPath != ":memory:" {
		d.SpillBytes = dirSize(s.dbPath + ".tmp")
	}
	return d
}

// nativeMemoryDiag is procMemDiag plus DuckDB's own accounting. Only call it
// when no export is holding the connection.
func (s *DuckDBShard) nativeMemoryDiag(ctx context.Context) memDiag {
	d := s.procMemDiag()
	d.HasSQL = true
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(memory_usage_bytes), 0) FROM duckdb_memory()`).Scan(&d.DuckDBMemBytes)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM duckdb_temporary_files()`).Scan(&d.TempFiles)
	_ = s.db.QueryRowContext(ctx, `SELECT current_setting('memory_limit')`).Scan(&d.MemoryLimit)
	_ = s.db.QueryRowContext(ctx, `SELECT current_setting('threads')`).Scan(&d.Threads)
	return d
}

// archiveSampleInterval is how often an in-flight archive logs memory state.
const archiveSampleInterval = 15 * time.Second

// sampleArchiveMemory logs the connection-free memory readings (procMemDiag)
// every `every` until the returned stop func is called. stop blocks until the
// sampler has exited, so nothing is logged after it returns.
func (s *DuckDBShard) sampleArchiveMemory(attempt int64, phase string, every time.Duration) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		start := time.Now()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				weftsdk.Infof(context.Background(), "archive attempt=%d progress phase=%s elapsed=%s %s",
					attempt, phase, time.Since(start).Round(time.Second), s.procMemDiag())
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

// parseProcStatusKB extracts VmRSS and VmSwap (kB) from /proc/<pid>/status.
func parseProcStatusKB(status string) (rssKB, swapKB int64) {
	for _, line := range strings.Split(status, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "VmRSS":
			rssKB = leadingInt(val)
		case "VmSwap":
			swapKB = leadingInt(val)
		}
	}
	return rssKB, swapKB
}

// parsePSIAvg10 extracts the 10-second averages of the "some" and "full" lines
// of /proc/pressure/memory.
func parsePSIAvg10(psi string) (some, full float64) {
	for _, line := range strings.Split(psi, "\n") {
		kind, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		for _, field := range strings.Fields(rest) {
			v, ok := strings.CutPrefix(field, "avg10=")
			if !ok {
				continue
			}
			f, _ := strconv.ParseFloat(v, 64)
			switch kind {
			case "some":
				some = f
			case "full":
				full = f
			}
		}
	}
	return some, full
}

func leadingInt(s string) int64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[0], 10, 64)
	return n
}
