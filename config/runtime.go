// Package config holds runtime-tunable settings that live in SSM and can
// be overridden out-of-band via `kevcons config set` or a raw
// `aws ssm put-parameter --overwrite`. Everything here is read at boot
// and refreshed on a 60-second tick; every consumer reads from an
// atomic.Pointer so overrides propagate within that window without a
// restart.
//
// The on-disk shape is the single aggregated kevcons blob at
// /{appName}/config/app — a JSON object of string keys → string values.
// Keys we don't recognize are ignored (other teams' settings).
// Unknown/empty values for keys we do recognize fall through to the
// compile-time Defaults().
package config

import (
	"context"
	"encoding/json"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
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
	// ingest throughput per AgentToken — see docs/ingestion-syslog.md,
	// "Rate limiting". Unlike IngestStreamBudgetBytes (a per-connection
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
// has no entry for a given key. Numbers here are chosen conservatively
// — see docs/security-audit-pre-launch.md for the rationale.
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
	keyMinWeftCliVersion      = "MIN_WEFT_CLI_VERSION"
	keyMinWeftdVersion        = "MIN_WEFTD_VERSION"
	keyWeftdHardFloor         = "WEFTD_HARD_FLOOR"
)

// paramPath returns the SSM path the kevcons config-set command writes
// to for a Go app (see kevcons/cmd/config.go resolveConfigStore).
func paramPath(appName string) string {
	return fmt.Sprintf("/%s/config/app", appName)
}

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

// ssmClient is a tiny interface so tests can substitute a fake.
type ssmClient interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// Load fetches /{appName}/config/app from SSM and returns the
// defaults-with-overlay. Any failure (param missing, AWS unreachable,
// JSON malformed) falls back to defaults with a log line — we never
// fail startup over config. Region is required because the AWS SDK
// v2's default chain doesn't always resolve a region from instance
// metadata the way v1 did; passing it explicitly is the reliable path.
func Load(ctx context.Context, appName, region string) *Runtime {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		weftsdk.Errorf(ctx, "config: AWS config load failed, using defaults: %v", err)
		return Defaults()
	}
	return loadWithClient(ctx, ssm.NewFromConfig(cfg), appName)
}

func loadWithClient(ctx context.Context, client ssmClient, appName string) *Runtime {
	r := Defaults()
	name := paramPath(appName)
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: &name})
	if err != nil || out == nil || out.Parameter == nil || out.Parameter.Value == nil {
		// Missing param is the common case pre-seed; don't noise the
		// logs for it. Only log if AWS itself complains.
		if err != nil && !isParamNotFound(err) {
			weftsdk.Errorf(ctx, "config: SSM GetParameter %s failed, using defaults: %v", name, err)
		}
		return r
	}
	var blob map[string]string
	if err := json.Unmarshal([]byte(*out.Parameter.Value), &blob); err != nil {
		weftsdk.Infof(ctx, "config: %s is not valid JSON, using defaults: %v", name, err)
		return r
	}
	r.Overlay(blob)
	return r
}

func isParamNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ParameterNotFound")
}

// Watch kicks off a background refresher that re-reads SSM every
// interval and atomically swaps ptr when the loaded Runtime differs
// from the current one. Returns the pointer immediately so callers can
// start serving with the initial load while the goroutine runs
// forever until ctx is cancelled.
func Watch(ctx context.Context, appName, region string, interval time.Duration) *atomic.Pointer[Runtime] {
	ptr := new(atomic.Pointer[Runtime])
	ptr.Store(Load(ctx, appName, region))
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				next := Load(ctx, appName, region)
				cur := ptr.Load()
				if !reflect.DeepEqual(cur, next) {
					weftsdk.Infof(ctx, "config: runtime config changed, applying new values")
					ptr.Store(next)
				}
			}
		}
	}()
	return ptr
}
