package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
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
type HerokuAdapter struct{}

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

// Parse walks the octet-counted multi-frame body Heroku posts.
// Each frame is `<length> <RFC5424 syslog message>`; the length is the
// byte count of the message exactly. After parsing we may emit one OR
// two LogRows per frame:
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
func (a HerokuAdapter) Parse(body io.Reader, _ http.Header) ([]*pb.LogRow, error) {
	var rows []*pb.LogRow
	br := bufio.NewReader(body)
	for {
		// Octet-counted framing: ASCII length, then a single space,
		// then `length` bytes of RFC5424.
		lengthBytes, err := br.ReadBytes(' ')
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return rows, fmt.Errorf("read frame length: %w", err)
		}
		length, err := strconv.Atoi(strings.TrimSpace(string(lengthBytes)))
		if err != nil || length <= 0 || length > 64<<10 {
			// Bogus length → bail. A malicious-or-buggy client could
			// slow us down with a stream of invalid lengths; cap at
			// 64 KB per frame (well beyond Heroku's actual ~10 KB).
			return rows, fmt.Errorf("invalid frame length %q", lengthBytes)
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(br, buf); err != nil {
			return rows, fmt.Errorf("read frame body: %w", err)
		}
		frame, err := parseRFC5424(string(buf))
		if err != nil {
			// Skip this frame, keep going — see Parse comment.
			continue
		}
		canonical, metrics := a.frameToRows(frame)
		if canonical != nil {
			rows = append(rows, canonical)
		}
		if metrics != nil {
			rows = append(rows, metrics)
		}
	}
}

// frameToRows converts one parsed syslog frame into the LogRow(s) we
// route into the drain. Returns (canonical, metrics): canonical is
// always set (the frame's primary row, app=<appname>/<procid-base>);
// metrics is non-nil only when the message body matches the l2met
// metric-line shape, in which case it carries the parsed key/value
// pairs to <appname>/metrics.
func (HerokuAdapter) frameToRows(f rfc5424Frame) (*pb.LogRow, *pb.LogRow) {
	procBase, instance := splitProcID(f.procid)
	appname := f.appname

	// Heroku reuses appname=heroku for platform events emitted by the
	// platform itself (deploys, scale, restart). Map those to a
	// `<real-app>/platform` sub-app so they don't collide with the
	// customer's own log streams. f.host carries the customer's app
	// name in those frames; if it's missing or set to a placeholder,
	// fall back to the procid as a last resort.
	if appname == "heroku" {
		if f.host != "" && f.host != "host" {
			appname = f.host
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
		Raw:      f.message,
		TsUnixNs: f.ts.UnixNano(),
		Level:    "", // Heroku-emitted lines don't carry a level field; let the schema parser fill it later if applicable
	}

	// l2met detection lives in detectL2met which returns nil when the
	// line isn't metric-shaped — most Heroku frames take the early-out.
	if metricsPayload, ok := detectL2met(f.message); ok {
		metricsRow := &pb.LogRow{
			Service:     appname,
			Kind:        "metrics",
			Instance:    instance,
			Raw:         f.message,
			TsUnixNs:    f.ts.UnixNano(),
			PayloadJson: metricsPayload,
		}
		return canonical, metricsRow
	}

	return canonical, nil
}

// splitProcID separates a Heroku procid into the sub-app name and the
// instance label. `web.1` → ("web", "web.1"); `router` → ("router",
// "router"); `heroku-postgres` → ("heroku-postgres", "heroku-postgres").
// The instance label keeps the dyno number intact so cross-dyno
// rollups still work, but the app name strips it so all dynos of one
// type land in the same pseudofile.
func splitProcID(procid string) (base, instance string) {
	if procid == "" || procid == "-" {
		return "", ""
	}
	if i := strings.LastIndexByte(procid, '.'); i > 0 {
		// Treat trailing-numeric suffix as a dyno number; otherwise
		// keep the procid intact (some Heroku add-ons carry a dot,
		// e.g. "scheduler.1234abcd" — that's still per-instance).
		if _, err := strconv.Atoi(procid[i+1:]); err == nil {
			return procid[:i], procid
		}
	}
	return procid, procid
}

// rfc5424Frame is the subset of RFC5424 fields the adapter extracts.
type rfc5424Frame struct {
	priority int
	ts       time.Time
	host     string
	appname  string
	procid   string
	msgid    string
	message  string
}

// parseRFC5424 parses one RFC5424 syslog message. Heroku emits a
// strict subset:
//
//	<priority>1 timestamp host appname procid msgid - message
//
// (no structured-data; Heroku always writes "-" in that slot). We
// accept the structured-data segment when present (`[...]`) but skip
// it — the relevant fields for weft are the message body and the
// header tuple.
func parseRFC5424(s string) (rfc5424Frame, error) {
	var f rfc5424Frame

	// <priority>VERSION
	if len(s) < 4 || s[0] != '<' {
		return f, fmt.Errorf("not RFC5424")
	}
	end := strings.IndexByte(s, '>')
	if end < 0 {
		return f, fmt.Errorf("missing > in priority")
	}
	pri, err := strconv.Atoi(s[1:end])
	if err != nil {
		return f, fmt.Errorf("priority: %w", err)
	}
	f.priority = pri
	rest := s[end+1:]
	// Skip VERSION (typically "1") and the following space.
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return f, fmt.Errorf("no space after VERSION")
	}
	rest = rest[sp+1:]

	// timestamp host appname procid msgid (5 space-separated fields)
	fields := strings.SplitN(rest, " ", 6)
	if len(fields) < 6 {
		return f, fmt.Errorf("expected at least 6 fields after VERSION, got %d", len(fields))
	}
	tsStr := fields[0]
	f.host = fields[1]
	f.appname = fields[2]
	f.procid = fields[3]
	f.msgid = fields[4]
	tail := fields[5]

	// Structured-data: either "-" or one or more "[id key=value ...]"
	// segments. Skip past it.
	if strings.HasPrefix(tail, "-") {
		// "- " or just "-" at end.
		if len(tail) >= 2 && tail[1] == ' ' {
			f.message = tail[2:]
		} else {
			f.message = ""
		}
	} else if strings.HasPrefix(tail, "[") {
		// Walk balanced brackets. Heroku doesn't emit structured-data
		// in practice, but accept it for forward compatibility.
		depth := 0
		idx := -1
		for i := 0; i < len(tail); i++ {
			switch tail[i] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					idx = i + 1
				}
			}
		}
		if idx < 0 {
			return f, fmt.Errorf("unterminated structured-data")
		}
		msg := tail[idx:]
		f.message = strings.TrimPrefix(msg, " ")
	} else {
		f.message = tail
	}

	if t, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
		f.ts = t
	} else {
		f.ts = time.Now().UTC()
	}

	return f, nil
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
