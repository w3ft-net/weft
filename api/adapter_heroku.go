package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/w3ft-net/weft/ingest/syslog"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/sources"
)

// HerokuAdapter parses Heroku's HTTPS log-drain protocol: octet-counted
// (RFC 6587) framing of RFC5424 syslog frames. Auth via the {token}
// URL path component (Heroku doesn't speak Bearer headers); namespace
// and env come from the token. Each (appname, procid) pair becomes a
// weft sub-app — `myapp web.1` → `app=myapp/web instance=web.1`,
// `myapp router` → `app=myapp/router`, and so on.
//
// Heroku's runtime-heroku-metrics labs feature emits l2met-style
// metric lines (`source=web.1 sample#memory_total=…`); when the
// adapter sees one it reroutes it to `<appname>/metrics` with the
// key/value pairs lifted into payload JSON, so customers get a
// natural `cat /log/<ns>/myapp/metrics.log | jq …` UX without any
// client-side awk.
type HerokuAdapter struct{ sources.Push }

// Route is the drain URL a customer adds with `heroku drains:add`; the token is
// its last path component, because Heroku doesn't speak Bearer headers.
func (HerokuAdapter) Route() string { return "POST /api/ingest/heroku/{token}" }

func (HerokuAdapter) ContentLimit() int64 { return 1 << 20 } // 1 MB; Heroku caps drains at ~1 MB

// Auth reads the {token} URL path component. Routed via path patterns
// like /api/ingest/heroku/{token}; we don't depend on a router framework,
// so the handler chain stuffs the token into the request via PathValue
// (Go 1.22+ http.ServeMux pattern syntax) before this is called.
func (HerokuAdapter) Auth(req *http.Request) (string, error) {
	tok := req.PathValue("token")
	if tok == "" {
		// Fallback for path-stripping reverse proxies: take the last
		// non-empty path segment. Heroku always presents the token at
		// the same depth, so this is robust against an ALB strip-path
		// rewrite.
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		if len(parts) > 0 {
			tok = parts[len(parts)-1]
		}
	}
	if tok == "" {
		return "", fmt.Errorf("missing token in URL path")
	}
	return tok, nil
}

// Parse walks the multi-frame body Heroku posts, delegating framing and
// RFC5424 parsing to the shared ingest/syslog package (Heroku uses
// octet-counting: `<length> <RFC5424 syslog message>`). After parsing we
// may emit one OR two LogRows per frame:
//   - the canonical row for the (app, procid) sub-app
//   - if the message is l2met-shaped, an additional metrics row
//     redirected to <app>/metrics (the original line still flows
//     through the sub-app pseudofile so customers can grep for it
//     literally if they want).
//
// The adapter ignores frames it can't parse — Heroku occasionally
// delivers malformed payloads during drain reconfiguration, and a
// hard-fail would make the customer's `heroku drains` view show
// permanent delivery failures. Operator visibility comes from a log
// line per skipped frame.
func (a HerokuAdapter) Parse(body io.Reader, _ http.Header, _ AdapterContext) ([]*pb.LogRow, any, error) {
	var rows []*pb.LogRow
	// Heroku's drain is always octet-counted (RFC 6587 §3.4.1).
	syslog.ParseStream(body, syslog.OctetCounting, syslog.DefaultMaxFrame, func(f syslog.Frame, err error) {
		if err != nil {
			// Skip this frame, keep going — see Parse comment.
			return
		}
		canonical, metrics := a.frameToRows(f)
		if canonical != nil {
			rows = append(rows, canonical)
		}
		if metrics != nil {
			rows = append(rows, metrics)
		}
	})
	return rows, nil, nil
}

// frameToRows converts one parsed syslog frame into the LogRow(s) we
// route into the drain. Returns (canonical, metrics): canonical is
// always set (the frame's primary row, app=<appname>/<procid-base>);
// metrics is non-nil only when the message body matches the l2met
// metric-line shape, in which case it carries the parsed key/value
// pairs to <appname>/metrics.
func (HerokuAdapter) frameToRows(f syslog.Frame) (*pb.LogRow, *pb.LogRow) {
	procBase, instance := syslog.SplitProcID(f.ProcID)
	appname := f.AppName

	// Heroku reuses appname=heroku for platform events emitted by the
	// platform itself (deploys, scale, restart). Map those to a
	// `<real-app>/platform` sub-app so they don't collide with the
	// customer's own log streams. f.host carries the customer's app
	// name in those frames; if it's missing or set to a placeholder,
	// fall back to the procid as a last resort.
	if appname == "heroku" {
		if f.Hostname != "" && f.Hostname != "host" {
			appname = f.Hostname
		}
		// Override the procid sub-app to "platform" — every "heroku"-
		// originated line is platform-y regardless of which dyno emits
		// it (api, web, router, etc.).
		procBase = "platform"
	}

	// appname is the service; the dyno proc becomes the kind (empty
	// procBase → "main" downstream). The coordinator stamps namespace
	// from the token.
	canonical := &pb.LogRow{
		Service:  appname,
		Kind:     procBase,
		Instance: instance,
		Raw:      f.Message,
		TsUnixNs: f.Timestamp.UnixNano(),
		Level:    "", // Heroku-emitted lines don't carry a level field; let the schema parser fill it later if applicable
	}

	// l2met detection lives in detectL2met which returns nil when the
	// line isn't metric-shaped — most Heroku frames take the early-out.
	if metricsPayload, ok := detectL2met(f.Message); ok {
		metricsRow := &pb.LogRow{
			Service:     appname,
			Kind:        "metrics",
			Instance:    instance,
			Raw:         f.Message,
			TsUnixNs:    f.Timestamp.UnixNano(),
			PayloadJson: metricsPayload,
		}
		return canonical, metricsRow
	}

	return canonical, nil
}

// l2metKVPattern matches Heroku's l2met metric grammar:
//
//	source=<src> [dyno=<dyno>] (sample|measure|count|bucket)#<key>=<value>[unit] ...
//
// The source/dyno prefix is what disambiguates a metric line from a
// regular log line that happens to contain `key=value` segments.
// Without the prefix, lots of structured logs would false-positive.
var l2metPrefix = regexp.MustCompile(`(?:^|\s)source=\S+`)
var l2metMetric = regexp.MustCompile(`\b(?:sample|measure|count|bucket)#\S+=\S+`)
var l2metPair = regexp.MustCompile(`(\w[\w.-]*)#([\w.-]+)=([^\s]+)`)

// detectL2met returns the parsed key/value payload (as JSON bytes)
// when the line is l2met-shaped, or nil when it isn't. The decision
// rule is "has source= AND has at least one (sample|measure|count|
// bucket)# segment" — a lone `source=` doesn't qualify because plenty
// of regular Heroku Router log lines include source-like fields.
func detectL2met(line string) ([]byte, bool) {
	if !l2metPrefix.MatchString(line) || !l2metMetric.MatchString(line) {
		return nil, false
	}
	out := map[string]any{}
	for _, m := range l2metPair.FindAllStringSubmatch(line, -1) {
		// m[1] = bucket name (sample, measure, count, bucket)
		// m[2] = key
		// m[3] = value (possibly with trailing unit, e.g. "512.00MB")
		key := m[2]
		val := m[3]
		// Try numeric parse first, falling back to the string with
		// any trailing unit preserved (so "512.00MB" stays parseable
		// downstream as "memory_total":"512.00MB").
		if n, err := strconv.ParseFloat(val, 64); err == nil {
			out[key] = n
		} else if num, unit := stripTrailingUnit(val); unit != "" {
			if n, err := strconv.ParseFloat(num, 64); err == nil {
				out[key] = n
				out[key+"_unit"] = unit
			} else {
				out[key] = val
			}
		} else {
			out[key] = val
		}
	}
	// Capture source/dyno even though they're not metric#-prefixed —
	// they're the metric's identity, so they belong on the row.
	for _, m := range regexp.MustCompile(`\b(source|dyno)=(\S+)`).FindAllStringSubmatch(line, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		return nil, false
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}

// stripTrailingUnit pulls a non-numeric unit suffix off a value like
// "512.00MB" or "0.42s" → ("512.00", "MB") / ("0.42", "s").
func stripTrailingUnit(v string) (num, unit string) {
	cut := -1
	for i := len(v) - 1; i >= 0; i-- {
		c := v[i]
		if c >= '0' && c <= '9' || c == '.' || c == '-' || c == '+' {
			cut = i + 1
			break
		}
	}
	if cut <= 0 || cut == len(v) {
		return v, ""
	}
	return v[:cut], v[cut:]
}
