// Package mem is every store contract implemented in memory, for one group, with
// nothing persisted. It is what core's own tests run on, and a fixture for any
// deployment's tests; a deployment that keeps data uses a backend of its own
// (ext/backends/*). Each implementation passes its contract's conformance suite.
//
// Nothing here is safe to rely on across a restart, and none of it is meant to
// scale: a map under a mutex.
package mem
