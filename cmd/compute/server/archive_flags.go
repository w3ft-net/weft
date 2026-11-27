package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/w3ft-net/weft/backends/logs/archive"
)

// readerFlag is the repeatable -archive-reader FORMAT=NAME: which reader reads
// the archives written in a format.
type readerFlag map[string]string

func (f readerFlag) String() string {
	pairs := make([]string, 0, len(f))
	for id, name := range f {
		pairs = append(pairs, id+"="+name)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (f readerFlag) Set(v string) error {
	id, name, ok := strings.Cut(v, "=")
	if !ok || id == "" || name == "" {
		return fmt.Errorf("want FORMAT=NAME, got %q", v)
	}
	if _, dup := f[id]; dup {
		return fmt.Errorf("a reader for %s is given twice", id)
	}
	f[id] = name
	return nil
}

// archiveReaders is the reader entries the flags give: the format-specific ones
// and, if one is named, the default for every format with none.
func archiveReaders(each readerFlag, def string) map[string]string {
	out := make(map[string]string, len(each)+1)
	for id, name := range each {
		out[id] = name
	}
	if def != "" {
		out[archive.Default] = def
	}
	return out
}
