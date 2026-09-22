package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// One pool per known service. Ports dev/demo/generator.py's PATHS /
// METHODS / STATUSES / etc. verbatim — same words, same selection
// shape — so jq/awk grouping queries against the prod streams have
// the same cardinality as against the dev streams.

var (
	paths     = []string{"/health", "/api/v1/orders", "/api/v1/users", "/login", "/checkout", "/products"}
	methods   = []string{"GET", "POST", "PUT", "DELETE"}
	statuses  = []int{200, 200, 200, 200, 201, 204, 301, 400, 401, 404, 500, 502}
	errStatus = []int{400, 401, 404, 500, 502, 503}
	userIDs   = idPool("u_%03x", 64)
	jobNames  = []string{"index_rebuild", "thumb_resize", "email_digest", "billing_recalc", "purge"}
	endpoints = []string{"auth-svc", "search-svc", "geo-svc", "billing-svc", "stripe"}
	invoices  = idPool("inv_%05x", 128)
	orders    = idPool("ord_%05x", 256)
)

func idPool(format string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(format, i)
	}
	return out
}

// LineFor returns one log line for `service` at the current wall
// clock with the requested level and trace ID. Output shape matches
// the Python generator's content exactly (ISO8601-with-ms timestamp,
// padded level, service-specific key=value content) with one
// addition: a trace ID as field 3, right after level. Every line gets
// one, ambient or chained (see chain.go) — a solo, never-reused ID
// for ambient traffic, a shared one across a chain's hops — because
// ingest.Schema's trace_id_field only supports a positive, fixed
// 1-based field index (no negative/from-end form), so the ID has to
// sit at the same position regardless of which service/event
// produced the line, not at the end where field counts vary.
func LineFor(rng *rand.Rand, service, level, traceID string) string {
	var b strings.Builder

	switch service {
	case "api", "edge":
		method := pickStr(rng, methods)
		path := pickStr(rng, paths)
		var status, dur int
		if level == "INFO" {
			status = pickInt(rng, statuses)
			dur = 1 + rng.IntN(250)
		} else {
			status = pickInt(rng, errStatus)
			dur = 500 + rng.IntN(4501)
		}
		user := pickStr(rng, userIDs)
		fmt.Fprintf(&b, "request method=%s path=%s status=%d dur=%dms user_id=%s",
			method, path, status, dur, user)

	case "worker":
		job := pickStr(rng, jobNames)
		switch level {
		case "ERROR":
			fmt.Fprintf(&b, "job_failed job=%s reason=deadline_exceeded duration_ms=%d",
				job, 30000+rng.IntN(60001))
		case "WARN":
			fmt.Fprintf(&b, "job_retry job=%s attempt=%d", job, 2+rng.IntN(4))
		default:
			fmt.Fprintf(&b, "job_completed job=%s duration_ms=%d rows=%d",
				job, 50+rng.IntN(4951), 1+rng.IntN(1000))
		}

	case "orders":
		order := pickStr(rng, orders)
		amount := 5 + rng.Float64()*495
		if level == "ERROR" {
			fmt.Fprintf(&b, "order_failed order_id=%s reason=payment_declined", order)
		} else {
			fmt.Fprintf(&b, "order_placed order_id=%s amount=%.2f currency=USD", order, amount)
		}

	case "billing":
		inv := pickStr(rng, invoices)
		amount := 10 + rng.Float64()*1990
		switch level {
		case "ERROR":
			fmt.Fprintf(&b, "invoice_failed invoice_id=%s reason=stripe_timeout", inv)
		case "WARN":
			fmt.Fprintf(&b, "invoice_retry invoice_id=%s attempt=%d", inv, 2+rng.IntN(3))
		default:
			fmt.Fprintf(&b, "invoice_sent invoice_id=%s amount=%.2f", inv, amount)
		}

	case "notifier":
		endpoint := pickStr(rng, endpoints)
		if level == "ERROR" {
			fmt.Fprintf(&b, "notify_failed endpoint=%s reason=upstream_5xx", endpoint)
		} else {
			fmt.Fprintf(&b, "notify_sent endpoint=%s latency_ms=%d", endpoint, 20+rng.IntN(781))
		}

	default:
		fmt.Fprintf(&b, "event service=%s value=%d", service, rng.IntN(1001))
	}
	return formatLine(level, traceID, b.String())
}

// formatLine assembles the shared "<ts> <LEVEL> <traceID> <content>"
// prefix every loom-producer line carries — ambient (LineFor) and
// chain-triggered (chain.go's checkout/checkout_processed/fulfillment)
// lines alike — so a fixed field index resolves the trace ID
// identically no matter which service or event produced the line.
func formatLine(level, traceID, content string) string {
	var b strings.Builder
	b.WriteString(tsNow())
	b.WriteByte(' ')
	b.WriteString(padLevel(level))
	b.WriteByte(' ')
	b.WriteString(traceID)
	b.WriteByte(' ')
	b.WriteString(content)
	return b.String()
}

// pickLevel mirrors generator.py's _pick_level: ERROR with prob
// error_ratio, WARN with the next 15 percentage points, INFO otherwise.
func pickLevel(rng *rand.Rand, errorRatio float64) string {
	r := rng.Float64()
	if r < errorRatio {
		return "ERROR"
	}
	if r < errorRatio+0.15 {
		return "WARN"
	}
	return "INFO"
}

func tsNow() string {
	t := time.Now().UTC()
	return t.Format("2006-01-02T15:04:05.000Z")
}

// padLevel matches Python's f"{level:5s}" — left-justified, space-padded
// to 5 chars. INFO/WARN are 4 chars and become "INFO ", "WARN "; ERROR
// is exactly 5 already.
func padLevel(level string) string {
	if len(level) >= 5 {
		return level
	}
	return level + strings.Repeat(" ", 5-len(level))
}

func pickStr(rng *rand.Rand, pool []string) string { return pool[rng.IntN(len(pool))] }
func pickInt(rng *rand.Rand, pool []int) int       { return pool[rng.IntN(len(pool))] }
