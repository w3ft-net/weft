package live

// FactState is where a locator is on its way to being typed.
type FactState int8

const (
	FactUnknown FactState = iota
	FactTyped
	FactDirty
)

// FactKey names a locator in a source. Table is the source's logical table,
// logtable.Name of its partition, namespace, service and kind, whether or not
// the shard stores a table per key: shards are shared across groups, and the
// name carries the partition, so a fact of one group is never another's. Env
// so that a stray row in staging doesn't downgrade production. Locator is "$N"
// for awk field N, "$N#int" for awk field N holding only integers (see
// finiteInteger), or ".key" for a top-level payload key.
type FactKey struct {
	Table   string
	Env     string
	Locator string
}

// FieldFact is one locator's reportable state. Epoch increases with every
// transition the coordinator needs to hear about (typed, or typed then
// dirty), across all of the shard's facts.
type FieldFact struct {
	FactKey
	// Namespace, Service and Kind are the source's, for people reading a
	// table name, which is a hash.
	Namespace, Service, Kind string
	// Customer is the legacy layout's one partition value, for a coordinator
	// that identifies a source by it. Zero under any other layout. It goes
	// with tag 26.
	Customer int64
	State    FactState
	Epoch    uint64
}
