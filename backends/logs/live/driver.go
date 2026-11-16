package live

import (
	"fmt"
	"sort"
	"sync"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/shape"
)

// OpenParams is what a shard node gives a driver to open a store.
type OpenParams struct {
	// ID is the shard's ID.
	ID string
	// Path is where the store keeps its data on the node's volume. Empty means
	// the store keeps nothing on disk: a node under test.
	Path string
	// Layout is the partition columns every row of the store carries.
	Layout shape.Layout
}

// Driver is a kind of live store a shard node can run, by the name its
// configuration gives it.
type Driver interface {
	// Name is how configuration names the store.
	Name() string
	// Dialect is the SQL the store runs. The coordinator plans every query in
	// one dialect, so a node that reports another is refused.
	Dialect() dialect.Dialect
	// Open opens the store for a shard.
	Open(p OpenParams) (Store, error)
}

var (
	regMu sync.RWMutex
	reg   = map[string]Driver{}
)

// Register makes d available to a node's configuration under its name. A node's
// main registers the stores it links, before it reads its flags.
func Register(d Driver) error {
	regMu.Lock()
	defer regMu.Unlock()
	name := d.Name()
	if name == "" {
		return fmt.Errorf("live: a driver has no name")
	}
	if d.Dialect() == nil {
		return fmt.Errorf("live: driver %q has no dialect", name)
	}
	if _, dup := reg[name]; dup {
		return fmt.Errorf("live: driver %q is registered twice", name)
	}
	reg[name] = d
	return nil
}

// Lookup is the registered driver called name.
func Lookup(name string) (Driver, error) {
	regMu.RLock()
	defer regMu.RUnlock()
	if d, ok := reg[name]; ok {
		return d, nil
	}
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("live: no store called %q (have %v)", name, names)
}
