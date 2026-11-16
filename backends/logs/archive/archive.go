// Package archive is the contract for archived log data: the format an archive
// is written in, the archiver that writes it, and the reader that queries it.
// Live shards are not part of it; they have their own query path.
//
// A deployment configures the format its archiver writes and a reader for each
// format ID still inside retention (Config). Every archive's manifest records
// the ID it was written with, and a query finds its reader through Router with
// one lookup. See docs/plans/pluggable-log-storage.md.
//
// The package is the contract and the routing, and knows no format. A format is
// an implementation in its own package (parquet is the open build's), which a
// build's main registers. The interfaces below hold only what routing and startup
// validation use; a reader's dialect and execution join them with phase 4.
package archive

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/backends/logs/live"
	"github.com/w3ft-net/weft/shape"
)

// Default is the key of Config.Readers that serves every format ID with no
// entry of its own. No format may use it as an ID.
const Default = "default"

// Manifest is what routing reads of an archive's manifest.
type Manifest struct {
	// FormatID is the format the archive was written in. Empty on an archive
	// written before manifests recorded one.
	FormatID string
	// Path is where the archive is, as the store that wrote it reported it. For an
	// archive with no recorded format ID it is also how the format is found
	// (Registry.FormatOf).
	Path string
}

// Format is how an archive is laid out in storage. It is data, with no SQL and no
// pruning: the reader renders the query over it, and which partitions it opens is
// its own business.
type Format interface {
	// ID is the stable name a manifest and the configuration know it by.
	ID() string
	// Recognizes reports whether path names an archive in this format. It is how
	// an archive written before manifests recorded an ID says what it is, so two
	// formats never recognize the same path.
	Recognizes(path string) bool
}

// Archiver writes archives. A live store supplies its own, since exporting rows
// is specific to the store.
type Archiver interface {
	// Writes lists the format IDs it can write.
	Writes() []string
}

// Archive is one archive, as a reader opens it.
type Archive struct {
	// ID is the shard whose rows the archive holds.
	ID       string
	Services []string
	// Path is where the archive is, as the store that wrote it reported it.
	Path string
	// Layout is the partition columns the archive was written in. The zero layout
	// is the legacy one.
	Layout shape.Layout
	// Endpoint is where a reader that runs its queries elsewhere reaches the
	// engine that does; empty for one that does not.
	Endpoint string
}

// Handle queries one archive. It is the reader's, and is closed when the engine
// is done with the archive.
type Handle interface {
	// QueryToSink runs sql, in the reader's dialect, over the archive and streams
	// its rows to sink: the first two columns are (raw, ts). It ends the stream,
	// and reports through sink if its rows are not all of them.
	QueryToSink(ctx context.Context, sql string, args []any, sink live.RowSink)
	Close() error
}

// Reader queries archives. Which partitions a query opens is its own business:
// it is not part of what it is asked, or of any format.
type Reader interface {
	// Reads lists the format IDs it can query.
	Reads() []string
	// Dialect is the SQL it runs. The coordinator plans the archive tier in one
	// dialect, so a reader that runs another is refused.
	Dialect() dialect.Dialect
	// Open opens a handle on an archive in one of the formats it reads.
	Open(a Archive) (Handle, error)
}

// Config is the deployment's archive settings.
type Config struct {
	// Format is the ID the archiver writes.
	Format string
	// Readers maps a format ID to the name of the reader that queries it. The
	// Default key names the reader for every ID without an entry; it is used for
	// an ID only if its Reads include it.
	Readers map[string]string
}

// Registry holds the formats and readers a build supplies, which Route checks a
// Config against.
type Registry struct {
	formats map[string]Format
	readers map[string]Reader
}

func NewRegistry() *Registry {
	return &Registry{formats: map[string]Format{}, readers: map[string]Reader{}}
}

// AddFormat registers a format under its ID.
func (r *Registry) AddFormat(f Format) error {
	id := f.ID()
	switch {
	case id == "":
		return errors.New("archive: a format has no ID")
	case id == Default:
		return fmt.Errorf("archive: %q is the key of the default reader, not a format ID", Default)
	case r.formats[id] != nil:
		return fmt.Errorf("archive: format %q is registered twice", id)
	}
	r.formats[id] = f
	return nil
}

// FormatOf is the ID of the format an archive was written in. A manifest that
// records one gives it; an older manifest's is told by its path: the one
// registered format that recognizes it.
func (r *Registry) FormatOf(m Manifest) (string, error) {
	if m.FormatID != "" {
		return m.FormatID, nil
	}
	var ids []string
	for id, f := range r.formats {
		if f.Recognizes(m.Path) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", fmt.Errorf("archive: %q has no format ID and no registered format recognizes its path", m.Path)
	}
	return "", fmt.Errorf("archive: %q has no format ID and %v all recognize its path", m.Path, ids)
}

// AddReader registers a reader under a name.
func (r *Registry) AddReader(name string, rd Reader) error {
	switch {
	case name == "":
		return errors.New("archive: a reader has no name")
	case r.readers[name] != nil:
		return fmt.Errorf("archive: reader %q is registered twice", name)
	}
	r.readers[name] = rd
	return nil
}

// Router finds the reader of an archive.
type Router struct {
	reg      *Registry
	readers  map[string]Reader // by format ID, explicit entries
	dflt     Reader            // nil if the Config names none
	dfltName string
	names    map[string]string // reader name by format ID, for errors
}

// Readers checks the reader entries against what is registered and returns the
// Router for them. It fails unless there is at least one, and every entry names
// a registered format (or Default) and a registered reader that reads it. It is
// what a process that only reads archives builds; Route adds what the one that
// writes them checks.
func (r *Registry) Readers(readers map[string]string) (*Router, error) {
	if len(readers) == 0 {
		return nil, errors.New("archive: no reader is configured")
	}
	rt := &Router{reg: r, readers: map[string]Reader{}, names: map[string]string{}}
	// Sorted, so the same bad config reports the same error every start.
	ids := make([]string, 0, len(readers))
	for id := range readers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		name := readers[id]
		rd := r.readers[name]
		if rd == nil {
			return nil, fmt.Errorf("archive: readers[%q] names reader %q, which is not registered", id, name)
		}
		if id == Default {
			rt.dflt, rt.dfltName = rd, name
			continue
		}
		if r.formats[id] == nil {
			return nil, fmt.Errorf("archive: readers[%q] names a format that is not registered", id)
		}
		if !slices.Contains(rd.Reads(), id) {
			return nil, fmt.Errorf("archive: reader %q does not read format %q", name, id)
		}
		rt.readers[id], rt.names[id] = rd, name
	}
	return rt, nil
}

// Route is Readers for the deployment's whole Config, checked against a, the
// archiver the live store supplies. It fails too unless the archiver writes
// cfg.Format and cfg.Format has a reader that reads it.
func (r *Registry) Route(cfg Config, a Archiver) (*Router, error) {
	if r.formats[cfg.Format] == nil {
		return nil, fmt.Errorf("archive: format %q is not registered", cfg.Format)
	}
	if !slices.Contains(a.Writes(), cfg.Format) {
		return nil, fmt.Errorf("archive: the archiver does not write format %q", cfg.Format)
	}
	rt, err := r.Readers(cfg.Readers)
	if err != nil {
		return nil, err
	}
	if _, err := rt.reader(cfg.Format); err != nil {
		return nil, fmt.Errorf("archive: the configured format has no reader: %w", err)
	}
	return rt, nil
}

// Reader is the reader of the archive m describes. It fails for an archive no
// reader is configured for, or whose default reader does not read its format:
// the query fails, it never guesses.
func (rt *Router) Reader(m Manifest) (Reader, error) {
	id, err := rt.reg.FormatOf(m)
	if err != nil {
		return nil, err
	}
	return rt.reader(id)
}

func (rt *Router) reader(id string) (Reader, error) {
	if rd := rt.readers[id]; rd != nil {
		return rd, nil
	}
	if rt.dflt == nil {
		return nil, fmt.Errorf("archive: no reader is configured for format %q", id)
	}
	if !slices.Contains(rt.dflt.Reads(), id) {
		return nil, fmt.Errorf("archive: the default reader %q does not read format %q", rt.dfltName, id)
	}
	return rt.dflt, nil
}

// Check fails unless every manifest in ms has a reader. A deployment runs it at
// startup over the archives it has, so a missing reader shows then and not on
// the first query to reach an old shard.
func (rt *Router) Check(ms []Manifest) error {
	var errs []error
	seen := map[string]bool{}
	for _, m := range ms {
		id, err := rt.reg.FormatOf(m)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := rt.reader(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
