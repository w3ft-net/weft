package coordinator

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Readiness backs the load balancer health check. On an ASG that check is the
// only readiness gate (there is no separate liveness probe), so /health
// answers 503 while the process boots and 200 once it can serve.
//
// It latches: there is no way back to 503. A health check that followed
// Postgres or a shard would have the ASG replace healthy coordinators during
// the very incident that blipped the dependency.
type Readiness struct{ ready atomic.Bool }

// MarkReady is called once, when the registry is loaded and every service is
// registered on the listener.
func (r *Readiness) MarkReady() { r.ready.Store(true) }

func (r *Readiness) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !r.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "starting")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
}
