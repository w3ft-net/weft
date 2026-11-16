package dialect

import (
	"fmt"
	"sort"
	"sync"
)

var (
	regMu sync.RWMutex
	reg   = map[string]Dialect{"duckdb": DuckDB}
)

// Register makes d available to configuration under its name. A build's main
// registers the dialects it links, before it reads its flags.
func Register(d Dialect) error {
	regMu.Lock()
	defer regMu.Unlock()
	name := d.Name()
	if name == "" {
		return fmt.Errorf("dialect: a dialect has no name")
	}
	if _, dup := reg[name]; dup {
		return fmt.Errorf("dialect: %q is registered twice", name)
	}
	reg[name] = d
	return nil
}

// Lookup is the registered dialect called name.
func Lookup(name string) (Dialect, error) {
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
	return nil, fmt.Errorf("dialect: no dialect called %q (have %v)", name, names)
}

// Meet is spelled, restricted to what other has as well. A plan that pushes
// work into two stores must push the same work into both, or what one leaves
// to compute the other would not: it is planned with the Meet of the two
// dialects, once spelled by each.
func Meet(spelled, other Dialect) Dialect {
	if spelled.Name() == other.Name() {
		return spelled
	}
	return meet{spelled, other}
}

type meet struct {
	Dialect
	other Dialect
}

func (m meet) Supports(f Feature) bool { return m.Dialect.Supports(f) && m.other.Supports(f) }
