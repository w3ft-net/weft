package hosted

import (
	"github.com/graybeardcorp/weft/coordination"
	"github.com/graybeardcorp/weft/store"
)

// Coordination is the hosted build's coordination: the store's own lease and
// cron-claim methods over its Postgres tables, unchanged. It is what
// app.Adapter.Coordination is when a deployment sets none, named here so the
// hosted adapter says so and a signature drift in store/ fails this build.
func Coordination(db *store.Store) coordination.Coordination { return db }
