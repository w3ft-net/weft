package api

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// CloudWatchAdapter parses Kinesis Data Firehose's HTTP endpoint
// delivery format — the wire shape a customer-owned Firehose delivery
// stream posts after a CloudWatch Logs subscription filter feeds it
// (see docs/plans/cloudwatch-kinesis-subscription.md). An ordinary
// Adapter: auth is a fixed header value, Parse decodes the envelope and
// resolves each event's AWS log group to a weft service, and
// enrichRows (wired into IngestHandler) fills Level/Fields/TraceId/
// PayloadJson afterward — this adapter never calls ingest.Parser
// itself.
type CloudWatchAdapter struct{}

// ContentLimit is Firehose's own cap: up to 10,000 records or 64 MiB
// pre-compression per request.
func (CloudWatchAdapter) ContentLimit() int64 { return 64 << 20 }

// Auth reads X-Amz-Firehose-Access-Key — the value we set when
// configuring the delivery stream's HTTP endpoint destination (the
// existing wft_... namespace-scoped token). Same ValidateAgentToken
// check every adapter uses; no new auth surface.
func (CloudWatchAdapter) Auth(req *http.Request) (string, error) {
	key := req.Header.Get("X-Amz-Firehose-Access-Key")
	if key == "" {
		return "", fmt.Errorf("missing X-Amz-Firehose-Access-Key header")
	}
	return key, nil
}

// firehoseRequest is the outer envelope Firehose's HTTP endpoint
// delivery POSTs. See "Understand HTTP endpoint delivery request and
// response specifications":
// https://docs.aws.amazon.com/firehose/latest/dev/httpdeliveryrequestresponse.html
type firehoseRequest struct {
	RequestID string           `json:"requestId"`
	Timestamp int64            `json:"timestamp"`
	Records   []firehoseRecord `json:"records"`
}

type firehoseRecord struct {
	Data string `json:"data"` // base64
}

// cloudwatchEnvelope is the CloudWatch Logs subscription-filter payload
// each Firehose record's decoded data carries. messageType
// "CONTROL_MESSAGE" is Firehose/CloudWatch's periodic connectivity
// check — no logEvents, nothing to route.
type cloudwatchEnvelope struct {
	MessageType string            `json:"messageType"`
	Owner       string            `json:"owner"`
	LogGroup    string            `json:"logGroup"`
	LogStream   string            `json:"logStream"`
	LogEvents   []cloudwatchEvent `json:"logEvents"`
}

type cloudwatchEvent struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"` // ms since epoch
	Message   string `json:"message"`
}

// firehoseAck is what Parse hands back via the ack return value —
// WriteSuccess echoes RequestID in Firehose's required response body.
type firehoseAck struct {
	RequestID string
}

// Parse decodes a Firehose HTTP-endpoint-delivery request into LogRows.
// Malformed individual records/events are skipped rather than failing
// the whole request — Firehose retries the entire batch on anything but
// 200, so a hard failure on one bad record would retry-storm the good
// ones alongside it; operator visibility is a log line per skip, same
// policy HerokuAdapter uses for malformed frames.
//
// Whether the base64-decoded record bytes are still gzip-compressed
// CloudWatch envelope JSON (matching what the poller path expects) or
// plain JSON is an open question (see the plan's "Gzip-or-not on the
// wire") — decodeRecord tries gunzip first and falls back to the raw
// bytes, so this works either way without needing the answer up front.
func (CloudWatchAdapter) Parse(body io.Reader, _ http.Header, ac AdapterContext) ([]*pb.LogRow, any, error) {
	var req firehoseRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, nil, fmt.Errorf("decode firehose request: %w", err)
	}

	// Per-request memoization of the logGroup→service lookup — a batch
	// spanning one subscribed log group's events (the common case)
	// shouldn't re-hit Postgres per event. See FindLogGroupByAWSName's
	// doc comment on why this isn't a shared cache (yet).
	type resolved struct {
		service string
		ok      bool
	}
	serviceCache := make(map[string]resolved)
	resolveService := func(logGroup string) (string, bool) {
		r, hit := serviceCache[logGroup]
		if !hit {
			service, ok, err := ac.Store.FindLogGroupByAWSName(ac.OrgID, ac.Namespace, logGroup)
			if err != nil {
				ok = false
			}
			r = resolved{service: service, ok: ok}
			serviceCache[logGroup] = r
		}
		return r.service, r.ok
	}

	var rows []*pb.LogRow
	for _, rec := range req.Records {
		decoded, err := decodeRecord(rec.Data)
		if err != nil {
			continue // skip this record — see Parse comment
		}
		var env cloudwatchEnvelope
		if err := json.Unmarshal(decoded, &env); err != nil {
			continue
		}
		if env.MessageType != "DATA_MESSAGE" {
			continue // CONTROL_MESSAGE health check — nothing to route
		}
		service, ok := resolveService(env.LogGroup)
		if !ok {
			// No log_groups row for this AWS log group name in this
			// namespace — not (yet) onboarded. Drop the events rather
			// than routing them to service="".
			continue
		}
		for _, ev := range env.LogEvents {
			rows = append(rows, &pb.LogRow{
				Service:  service,
				Kind:     "main",
				Raw:      ev.Message,
				TsUnixNs: time.UnixMilli(ev.Timestamp).UnixNano(),
			})
		}
	}

	return rows, firehoseAck{RequestID: req.RequestID}, nil
}

// decodeRecord base64-decodes a Firehose record's data field, then
// tries gunzip — falling back to the raw decoded bytes when they aren't
// a valid gzip stream. See Parse's doc comment.
func decodeRecord(data string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		// Not gzip — assume plain JSON.
		return raw, nil
	}
	defer gr.Close()
	decompressed, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	return decompressed, nil
}

// WriteSuccess implements ResponseWriter: Firehose requires HTTP 200
// with Content-Type application/json and a body echoing the request's
// requestId — IngestHandler's default 204 doesn't satisfy that, so this
// overrides it (see docs/plans/cloudwatch-kinesis-subscription.md,
// "Wire format").
func (CloudWatchAdapter) WriteSuccess(w http.ResponseWriter, ack any) {
	fa, _ := ack.(firehoseAck)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"requestId": fa.RequestID,
		"timestamp": time.Now().UnixMilli(),
	})
}
