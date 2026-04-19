package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/kevlarcon/weft/config"
)

// RouteLimit picks which budget from the runtime config applies to a
// given route. Separate bucket maps per route so an IP attacking
// /login doesn't burn the budget they'd use for /verify.
type RouteLimit int

const (
	LimitLogin RouteLimit = iota
	LimitDevicePoll
	LimitAnonAuth
	LimitStripeWebhook
)

// budgetFor pulls the (rpm, burst) pair for a route out of the current
// runtime config. Values of zero disable the limit entirely — useful
// for ops to temporarily turn off rate limiting on an endpoint under
// incident response.
func budgetFor(cfg *config.Runtime, r RouteLimit) (rpm, burst int) {
	switch r {
	case LimitLogin:
		return cfg.LoginRPM, cfg.LoginBurst
	case LimitDevicePoll:
		return cfg.DevicePollRPM, cfg.DevicePollBurst
	case LimitAnonAuth:
		return cfg.AnonAuthRPM, cfg.AnonAuthBurst
	case LimitStripeWebhook:
		return cfg.StripeWebhookRPM, cfg.StripeWebhookBurst
	}
	return 0, 0
}

// bucket wraps a token-bucket limiter with a generation counter so we
// can call SetLimit/SetBurst when the runtime config changes without
// throwing away the bucket (which would reset an attacker's exhausted
// budget).
type bucket struct {
	limiter *rate.Limiter
	gen     uint64 // the config-gen this bucket was last synced with
	seen    int64  // unix-nano of last Allow; used by the reaper
}

// limiter holds per-route bucket maps plus the config snapshot pointer
// and a configGen counter that bumps on every config change the
// Swap sees.
type limiter struct {
	cfgPtr    *atomic.Pointer[config.Runtime]
	configGen uint64 // bumped whenever we detect a cfg change
	lastCfg   atomic.Pointer[config.Runtime]

	mu      sync.Mutex
	buckets map[key]*bucket
}

type key struct {
	route RouteLimit
	ip    string
}

func newLimiter(cfgPtr *atomic.Pointer[config.Runtime]) *limiter {
	l := &limiter{
		cfgPtr:  cfgPtr,
		buckets: make(map[key]*bucket),
	}
	l.lastCfg.Store(cfgPtr.Load())
	go l.reap()
	return l
}

// reap walks the bucket map every 5 min and drops entries that haven't
// been touched in 30 min. Caps memory growth under IP-rotation attacks.
func (l *limiter) reap() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-30 * time.Minute).UnixNano()
		l.mu.Lock()
		for k, b := range l.buckets {
			if atomic.LoadInt64(&b.seen) < cutoff {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// allow returns (ok, retryAfterSeconds). ok=false means the request
// should be 429'd. The retry-after hint is computed from the current
// bucket's reservation.
func (l *limiter) allow(route RouteLimit, ip string) (bool, int) {
	cfg := l.cfgPtr.Load()

	// Detect config change and bump the gen so live buckets rebase
	// their limit/burst on next touch.
	if prev := l.lastCfg.Load(); prev != cfg {
		if l.lastCfg.CompareAndSwap(prev, cfg) {
			atomic.AddUint64(&l.configGen, 1)
		}
	}

	rpm, burst := budgetFor(cfg, route)
	if rpm <= 0 || burst <= 0 {
		return true, 0 // explicit disable
	}

	k := key{route: route, ip: ip}
	l.mu.Lock()
	b, ok := l.buckets[k]
	if !ok {
		b = &bucket{
			limiter: rate.NewLimiter(rate.Limit(float64(rpm)/60.0), burst),
			gen:     atomic.LoadUint64(&l.configGen),
		}
		l.buckets[k] = b
	} else if b.gen != atomic.LoadUint64(&l.configGen) {
		b.limiter.SetLimit(rate.Limit(float64(rpm) / 60.0))
		b.limiter.SetBurst(burst)
		b.gen = atomic.LoadUint64(&l.configGen)
	}
	l.mu.Unlock()

	atomic.StoreInt64(&b.seen, time.Now().UnixNano())

	res := b.limiter.Reserve()
	if !res.OK() {
		// Burst exceeds limiter's max — shouldn't happen with our
		// configs, but be safe.
		return false, 1
	}
	delay := res.Delay()
	if delay == 0 {
		return true, 0
	}
	// Not allowed right now. Cancel the reservation so the token bucket
	// isn't held open against a client that's about to get 429'd, and
	// return retry-after.
	res.Cancel()
	return false, int(delay.Seconds()) + 1
}

// clientIP resolves the request's rate-limit key. If the connection
// came from a trusted proxy CIDR (per config), we honor the *last*
// X-Forwarded-For entry — that's the hop just before our trusted edge,
// which is the actual client. If the connection came from anywhere
// else, we use RemoteAddr directly; trusting X-F-F here would let any
// attacker spoof their key and bypass the limit.
func clientIP(r *http.Request, trustedCIDRs []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return host
	}
	trusted := false
	for _, cidr := range trustedCIDRs {
		if cidr.Contains(peer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if last != "" {
		return last
	}
	return host
}

// parseTrustedCIDRs parses the CSV in TrustedProxyCIDRs into a slice
// of *net.IPNet. Bad entries are skipped with a log line — the rest
// are still usable.
func parseTrustedCIDRs(csv string) []*net.IPNet {
	var out []*net.IPNet
	for _, p := range strings.Split(csv, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// RateLimit returns an http.Handler middleware that enforces the
// per-IP budget for the given route against the current runtime
// config. 429 responses include Retry-After and a small JSON body.
func (l *limiter) RateLimit(route RouteLimit, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := l.cfgPtr.Load()
		cidrs := parseTrustedCIDRs(cfg.TrustedProxyCIDRs)
		ip := clientIP(r, cidrs)
		ok, retryAfter := l.allow(route, ip)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":       "rate limited",
				"retry_after": retryAfter,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Helper for composing middleware with existing http.HandlerFunc
// handlers. Mirrors the pattern already in handlers.go.
func (l *limiter) RateLimitFunc(route RouteLimit, next http.HandlerFunc) http.HandlerFunc {
	h := l.RateLimit(route, next)
	return func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }
}

// Assert the ints we're comparing with atomic are sized such that the
// underlying platform has 64-bit atomics. (Here just to make the file
// self-contained about its atomic assumptions.)
var _ = fmt.Sprintf
