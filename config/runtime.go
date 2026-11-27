// Package config holds the runtime-tunable settings (rate limits, body caps,
// timeouts, version floors). A Source supplies a flat map of UPPER_SNAKE keys to
// values; the Runtime is Defaults() with the recognized keys applied. Everything
// is read at boot and refreshed on a tick (and on SIGHUP); every consumer reads
// from an atomic.Pointer so a change propagates without a restart.
//
// Keys we don't recognize are ignored. Unknown/empty values for keys we do
// recognize fall through to the compile-time Defaults().
package config

import (
	"context"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
)

// Runtime is the full set of tunable limits for the coordinator, api,
// and shard processes. Fields are plain values (not pointers) so a
// Runtime snapshot is immutable once atomically published.
type Runtime struct {
	// HTTP rate limits (api process). Requests per minute and burst
	// size for the token bucket.
	LoginRPM, LoginBurst                 int
	DevicePollRPM, DevicePollBurst       int
	AnonAuthRPM, AnonAuthBurst           int
	StripeWebhookRPM, StripeWebhookBurst int

	// CSV list of trusted proxy CIDRs. Requests from these source IPs
	// are assumed to be behind the ALB and we honor the last
	// X-Forwarded-For entry. Requests from anywhere else use
	// RemoteAddr as the rate-limit key.
	TrustedProxyCIDRs string

	// HTTP body caps (api process). Bytes.
	AuthBodyMax      int64
	ShellExecBodyMax int64
	FunctionBodyMax  int64
	WatchBodyMax     int64

	// gRPC caps. GRPCMaxRecvBytes is read once at server creation
	// (grpc.MaxRecvMsgSize is not runtime-mutable) — tuning this
	// requires a process restart. IngestStreamBudgetBytes is read on
	// each new stream, so changes take effect as agents reconnect.
	GRPCMaxRecvBytes        int
	IngestStreamBudgetBytes int64

	// IngestTokenBytesPerSec and IngestTokenBurstBytes bound sustained
	// ingest throughput per AgentToken. Unlike IngestStreamBudgetBytes (a per-connection
	// lifetime cap enforced for coordinator memory safety), this bucket
	// is keyed on the token and persists in memory across reconnects, so
	// a customer can't dodge it by cycling connections. A row that would
	// exceed the budget is delayed until the bucket refills rather than
	// dropped or disconnected. Read live on every row, so changes take
	// effect immediately for in-flight streams. Either <= 0 disables the
	// check.
	IngestTokenBytesPerSec int64
	IngestTokenBurstBytes  int64

	// IngestStreamIdleTimeoutSeconds reaps abandoned ingest streams: if
	// no row arrives within this window the handler closes the stream and
	// returns, releasing its goroutine (and the paired flush goroutine).
	// Without it, a client that opens a stream, ships a batch, and then
	// goes silent without half-closing pins the handler in stream.Recv()
	// forever — the dominant coordinator goroutine/memory leak. Read on
	// each new stream, so changes take effect as agents reconnect. 0
	// disables the reaper (pre-fix behavior).
	IngestStreamIdleTimeoutSeconds int

	// Shard query deadline. Read per-query, so changes take effect
	// on the next incoming Query RPC.
	ShardQueryTimeoutSeconds int

	// QueryDeadlineSeconds is the one time every one-shot query gets, from
	// the coordinator's Execute RPC taking it to its last line going out;
	// past it the query is cancelled (shards and compute with it) and the
	// caller gets a plain error with the explain of their pipeline. One
	// value for every caller. Follow is exempt: it lives as long as its
	// client. Read per query, so a change applies to the next one. 0
	// disables the deadline.
	QueryDeadlineSeconds int

	// MinWeftCliVersion is the lowest weft/<version> the HTTP API
	// accepts on X-Weft-Client; below it returns 426 Upgrade Required.
	// Empty disables the gate.
	MinWeftCliVersion string

	// MinWeftdVersion is the lowest weftd/<version> the coordinator
	// accepts on the weft-client gRPC metadata. Currently advisory —
	// versions in [MinWeftdVersion, WeftdHardFloor) are allowed but
	// their connections show up in the AgentsAtOrBelow query so the
	// operator can pressure the customer. Empty disables.
	MinWeftdVersion string

	// WeftdHardFloor is the lowest weftd/<version> the coordinator
	// will *serve* on IngestStream. Below it, the stream is rejected
	// with codes.FailedPrecondition. Used to retire an old wire shape
	// once AgentsAtOrBelow(old, lookback) is empty. Empty disables.
	WeftdHardFloor string
}

// Defaults returns the compile-time defaults. These are the values
// that apply when /weft/config/app does not exist, cannot be read, or
// has no entry for a given key. Numbers here are chosen conservatively.
func Defaults() *Runtime {
	return &Runtime{
		LoginRPM:                5,
		LoginBurst:              5,
		DevicePollRPM:           30,
		DevicePollBurst:         10,
		AnonAuthRPM:             20,
		AnonAuthBurst:           5,
		StripeWebhookRPM:        120,
		StripeWebhookBurst:      30,
		TrustedProxyCIDRs:       "10.0.0.0/8",
		AuthBodyMax:             4 * 1024,
		ShellExecBodyMax:        1024 * 1024,
		FunctionBodyMax:         64 * 1024,
		WatchBodyMax:            64 * 1024,
		GRPCMaxRecvBytes:        32 * 1024 * 1024,
		IngestStreamBudgetBytes: 64 * 1024 * 1024,
		// 4 MiB/s sustained, 16 MiB burst per token: comfortably above
		// any real customer's log volume (Render services included) while
		// still bounding a runaway or misconfigured firehose.
		IngestTokenBytesPerSec: 4 * 1024 * 1024,
		IngestTokenBurstBytes:  16 * 1024 * 1024,
		// 5 min: long enough not to disturb a live-but-sparse agent, short
		// enough to reap abandoned/half-open streams before they pile up.
		IngestStreamIdleTimeoutSeconds: 300,
		ShardQueryTimeoutSeconds:       30,
		// A starting value, not a measurement: the plan wants it chosen from
		// timings of the slow aggregations at 1 h, 6 h and 24 h. Tune it live
		// with `kevcons config set QUERY_DEADLINE_SECONDS=<n>`.
		QueryDeadlineSeconds: 60,
	}
}

// Keys matches the kevcons "app" blob convention: flat, UPPER_SNAKE.
const (
	keyLoginRPM               = "RATELIMIT_LOGIN_RPM"
	keyLoginBurst             = "RATELIMIT_LOGIN_BURST"
	keyDevicePollRPM          = "RATELIMIT_DEVICE_POLL_RPM"
	keyDevicePollBurst        = "RATELIMIT_DEVICE_POLL_BURST"
	keyAnonAuthRPM            = "RATELIMIT_ANON_AUTH_RPM"
	keyAnonAuthBurst          = "RATELIMIT_ANON_AUTH_BURST"
	keyStripeWebhookRPM       = "RATELIMIT_STRIPE_WEBHOOK_RPM"
	keyStripeWebhookBurst     = "RATELIMIT_STRIPE_WEBHOOK_BURST"
	keyTrustedProxyCIDRs      = "RATELIMIT_TRUSTED_PROXY_CIDRS"
	keyAuthBodyMax            = "BODYCAP_AUTH_BYTES"
	keyShellExecBodyMax       = "BODYCAP_SHELL_EXEC_BYTES"
	keyFunctionBodyMax        = "BODYCAP_FUNCTION_BYTES"
	keyWatchBodyMax           = "BODYCAP_WATCH_BYTES"
	keyGRPCMaxRecvBytes       = "GRPC_MAX_RECV_BYTES"
	keyIngestStreamBudget     = "GRPC_INGEST_STREAM_BUDGET_BYTES"
	keyIngestTokenBytesPerSec = "GRPC_INGEST_TOKEN_BYTES_PER_SEC"
	keyIngestTokenBurstBytes  = "GRPC_INGEST_TOKEN_BURST_BYTES"
	keyIngestStreamIdleSec    = "GRPC_INGEST_STREAM_IDLE_TIMEOUT_SECONDS"
	keyShardQueryTimeoutSec   = "SHARD_QUERY_TIMEOUT_SECONDS"
	keyQueryDeadlineSec       = "QUERY_DEADLINE_SECONDS"
	keyMinWeftCliVersion      = "MIN_WEFT_CLI_VERSION"
	keyMinWeftdVersion        = "MIN_WEFTD_VERSION"
	keyWeftdHardFloor         = "WEFTD_HARD_FLOOR"
)

// Overlay applies any keys from blob that we recognize onto r. Unknown
// keys and unparseable values are ignored (logged, but not fatal) so
// one bad entry can't break the whole config refresh.
func (r *Runtime) Overlay(blob map[string]string) {
	overlayInt(blob, keyLoginRPM, &r.LoginRPM)
	overlayInt(blob, keyLoginBurst, &r.LoginBurst)
	overlayInt(blob, keyDevicePollRPM, &r.DevicePollRPM)
	overlayInt(blob, keyDevicePollBurst, &r.DevicePollBurst)
	overlayInt(blob, keyAnonAuthRPM, &r.AnonAuthRPM)
	overlayInt(blob, keyAnonAuthBurst, &r.AnonAuthBurst)
	overlayInt(blob, keyStripeWebhookRPM, &r.StripeWebhookRPM)
	overlayInt(blob, keyStripeWebhookBurst, &r.StripeWebhookBurst)
	overlayString(blob, keyTrustedProxyCIDRs, &r.TrustedProxyCIDRs)
	overlayInt64(blob, keyAuthBodyMax, &r.AuthBodyMax)
	overlayInt64(blob, keyShellExecBodyMax, &r.ShellExecBodyMax)
	overlayInt64(blob, keyFunctionBodyMax, &r.FunctionBodyMax)
	overlayInt64(blob, keyWatchBodyMax, &r.WatchBodyMax)
	overlayInt(blob, keyGRPCMaxRecvBytes, &r.GRPCMaxRecvBytes)
	overlayInt64(blob, keyIngestStreamBudget, &r.IngestStreamBudgetBytes)
	overlayInt64(blob, keyIngestTokenBytesPerSec, &r.IngestTokenBytesPerSec)
	overlayInt64(blob, keyIngestTokenBurstBytes, &r.IngestTokenBurstBytes)
	overlayInt(blob, keyIngestStreamIdleSec, &r.IngestStreamIdleTimeoutSeconds)
	overlayInt(blob, keyShardQueryTimeoutSec, &r.ShardQueryTimeoutSeconds)
	overlayInt(blob, keyQueryDeadlineSec, &r.QueryDeadlineSeconds)
	overlayString(blob, keyMinWeftCliVersion, &r.MinWeftCliVersion)
	overlayString(blob, keyMinWeftdVersion, &r.MinWeftdVersion)
	overlayString(blob, keyWeftdHardFloor, &r.WeftdHardFloor)
}

func overlayInt(blob map[string]string, key string, dst *int) {
	v, ok := blob[key]
	if !ok || strings.TrimSpace(v) == "" {
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		weftsdk.Infof(context.Background(), "config: ignoring bad int for %s=%q: %v", key, v, err)
		return
	}
	*dst = n
}

func overlayInt64(blob map[string]string, key string, dst *int64) {
	v, ok := blob[key]
	if !ok || strings.TrimSpace(v) == "" {
		return
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		weftsdk.Infof(context.Background(), "config: ignoring bad int64 for %s=%q: %v", key, v, err)
		return
	}
	*dst = n
}

func overlayString(blob map[string]string, key string, dst *string) {
	if v, ok := blob[key]; ok {
		*dst = v
	}
}

// Load is Defaults() with src's values applied. A source that fails leaves the
// defaults: a deployment never fails to start over config.
func Load(ctx context.Context, src Source) *Runtime {
	r := Defaults()
	blob, err := src.Load(ctx)
	if err != nil {
		weftsdk.Errorf(ctx, "config: %v, using defaults", err)
		return r
	}
	r.Overlay(blob)
	return r
}

// Watch loads src now and reloads it every interval and on SIGHUP, atomically
// swapping the returned pointer when the result differs. A reload that fails
// keeps the values in force. It runs until ctx is cancelled.
func Watch(ctx context.Context, src Source, interval time.Duration) *atomic.Pointer[Runtime] {
	ptr := new(atomic.Pointer[Runtime])
	ptr.Store(Load(ctx, src))
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		defer signal.Stop(hup)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-hup:
			}
			blob, err := src.Load(ctx)
			if err != nil {
				weftsdk.Errorf(ctx, "config: reload failed, keeping the current values: %v", err)
				continue
			}
			next := Defaults()
			next.Overlay(blob)
			if !reflect.DeepEqual(ptr.Load(), next) {
				weftsdk.Infof(ctx, "config: runtime config changed, applying new values")
				ptr.Store(next)
			}
		}
	}()
	return ptr
}
