package coordinator

import (
	"context"

	weftsdk "github.com/w3ft-net/weft-go"
)

// LogTailFanoutEvent emits one structured line per Redis TailFanout health
// event — subscribe/publish errors, and connection establishment (which,
// after the first, means go-redis reconnected after a drop) — so the
// cross-coordinator tail -f path, previously only smoke-tested against a
// local docker-compose Valkey, is observable in prod.
// op is a short
// verb (connect, psubscribe, punsubscribe, publish, unmarshal). err is nil
// for a clean event such as "connect".
func LogTailFanoutEvent(ctx context.Context, op string, err error) {
	if err != nil {
		weftsdk.Infof(ctx, "tailfanout_event op=%s err=%q", op, err)
		return
	}
	weftsdk.Infof(ctx, "tailfanout_event op=%s", op)
}
