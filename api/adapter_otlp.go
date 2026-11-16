package api

import (
	"encoding/json"
	"fmt"
	"github.com/w3ft-net/weft/sources"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	ingsyslog "github.com/w3ft-net/weft/ingest/syslog"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// OTLPAdapter ingests OpenTelemetry metrics over OTLP/HTTP with a JSON body,
// keeping only the system metrics weft models for weftd hosts (cpu, memory,
// network, disk) and dropping everything else without comment. Render's
// metrics stream is the first (and so far only) producer; its capture is
// api/testdata/render_otlp_metrics.jsonl.
//
// Rows are stored as the producer sent them: gauges as values, counters as
// cumulative totals. Nothing is derived across samples, so the adapter keeps
// no state and any coordinator can serve any request. Render splits one
// sample across requests (one resource-metrics entry holds one metric or a
// fixed pair), so each entry becomes its own row and a tick is never
// assembled. A pct is therefore only present where both inputs share an entry:
// memory's usage and limit do, a disk's usage and capacity do not.
//
// The OTLP instrument type is ignored. Render sends cumulative counters as
// gauges and the CPU allotment as a cumulative sum; the metric name decides
// what a point means.
type OTLPAdapter struct{ sources.Push }

// Route is the metrics path Render appends to the base URL its Custom metrics
// destination is configured with.
func (OTLPAdapter) Route() string { return "POST /api/ingest/otlp/v1/metrics" }

func (OTLPAdapter) ContentLimit() int64 { return 1 << 20 }

// Auth reads `Authorization: Bearer <weft token>`, which is what Render's
// Custom metrics destination sends.
func (OTLPAdapter) Auth(req *http.Request) (string, error) {
	tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("missing bearer token")
	}
	return strings.TrimSpace(tok), nil
}

// WriteSuccess answers 200 with an empty JSON object, which is what Render's
// exporter was verified against.
func (OTLPAdapter) WriteSuccess(w http.ResponseWriter, _ any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "{}")
}

func (OTLPAdapter) Parse(body io.Reader, headers http.Header, _ AdapterContext) ([]*pb.LogRow, any, error) {
	if ct := headers.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return nil, nil, fmt.Errorf("unsupported content type %q: only OTLP/JSON is accepted", ct)
	}
	var req otlpRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, nil, fmt.Errorf("decode OTLP/JSON: %w", err)
	}

	var rows []*pb.LogRow
	for _, rm := range req.ResourceMetrics {
		attrs := rm.Resource.attrs()
		service := attrs["service.name"]
		// Metric instances share the log rows' $host: the id splits on its
		// last "-" exactly as the Render syslog producer splits a log row's
		// "web-9nk6q". A resource with no instance (Render's connectivity
		// test) is not a service instance and is dropped.
		_, instance, ok := ingsyslog.SplitKindInstance(attrs["service.instance.id"])
		if service == "" || !ok {
			continue
		}

		// Gather this entry's kept points, grouped by timestamp and kind.
		type tickKey struct {
			ts   int64
			kind string
		}
		ticks := map[tickKey]map[string]float64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				spec, kept := renderMetrics[m.Name]
				if !kept {
					continue
				}
				for _, dp := range m.points() {
					v, ok := dp.value()
					if !ok || dp.TimeUnixNano <= 0 {
						continue
					}
					k := tickKey{int64(dp.TimeUnixNano), spec.kind}
					if ticks[k] == nil {
						ticks[k] = map[string]float64{}
					}
					ticks[k][spec.key] = v
				}
			}
		}

		keys := make([]tickKey, 0, len(ticks))
		for k := range ticks {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].ts != keys[j].ts {
				return keys[i].ts < keys[j].ts
			}
			return keys[i].kind < keys[j].kind
		})
		for _, k := range keys {
			payload := buildMetricPayload(k.kind, ticks[k])
			if payload == nil {
				continue
			}
			body, _ := json.Marshal(payload)
			rows = append(rows, &pb.LogRow{
				Service:     service,
				Kind:        k.kind,
				Instance:    instance,
				Cluster:     attrs["service.project"],
				Raw:         string(body),
				PayloadJson: body,
				Level:       "INFO",
				TsUnixNs:    k.ts,
			})
		}
	}
	return rows, nil, nil
}

// metricSpec says which row kind a kept metric lands in and the raw-value key
// buildMetricPayload reads it under.
type metricSpec struct{ kind, key string }

// renderMetrics is the kept set of Render's metrics. Anything not listed
// (HTTP, Postgres and Key Value internals, disk I/O) is dropped.
var renderMetrics = map[string]metricSpec{
	"render.service.cpu.limit":              {"host", "cpu.limit"},
	"render.service.cpu.time":               {"host", "cpu.time"},
	"render.service.memory.usage":           {"host", "memory.usage"},
	"render.service.memory.limit":           {"host", "memory.limit"},
	"render.service.memory.rss":             {"host", "memory.rss"},
	"render.service.memory.cache":           {"host", "memory.cache"},
	"render.service.network.receive.bytes":  {"network", "rx"},
	"render.service.network.transmit.bytes": {"network", "tx"},
	"render.service.disk.usage":             {"disk", "usage"},
	"render.service.disk.capacity":          {"disk", "capacity"},
}

const (
	bytesPerMB = 1 << 20
	bytesPerGB = 1 << 30
)

// buildMetricPayload turns one entry's raw values for a kind into the payload
// weftd's rows of that kind use, with keys a producer cannot supply left out
// (a field absent from a row reads as null). It returns nil when nothing in
// raw belongs to the kind.
func buildMetricPayload(kind string, raw map[string]float64) map[string]any {
	out := map[string]any{}
	switch kind {
	case "host":
		cpu := map[string]any{}
		if v, ok := raw["cpu.limit"]; ok {
			cpu["cores"] = v
		}
		if v, ok := raw["cpu.time"]; ok {
			cpu["time_s"] = v
		}
		if len(cpu) > 0 {
			out["cpu"] = cpu
		}
		mem := map[string]any{}
		used, hasUsed := raw["memory.usage"]
		total, hasTotal := raw["memory.limit"]
		if hasUsed {
			mem["used_mb"] = round2(used / bytesPerMB)
		}
		if hasTotal {
			mem["total_mb"] = round2(total / bytesPerMB)
		}
		if hasUsed && hasTotal && total > 0 {
			mem["pct"] = round2(used / total * 100)
		}
		if v, ok := raw["memory.rss"]; ok {
			mem["rss_mb"] = round2(v / bytesPerMB)
		}
		if v, ok := raw["memory.cache"]; ok {
			mem["cache_mb"] = round2(v / bytesPerMB)
		}
		if len(mem) > 0 {
			out["memory"] = mem
		}
	case "network":
		if v, ok := raw["rx"]; ok {
			out["rx_bytes_total"] = int64(v)
		}
		if v, ok := raw["tx"]; ok {
			out["tx_bytes_total"] = int64(v)
		}
	case "disk":
		used, hasUsed := raw["usage"]
		total, hasTotal := raw["capacity"]
		if hasUsed {
			out["used_gb"] = round2(used / bytesPerGB)
		}
		if hasTotal {
			out["total_gb"] = round2(total / bytesPerGB)
		}
		if hasUsed && hasTotal && total > 0 {
			out["pct"] = round2(used / total * 100)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// The parts of OTLP/JSON this adapter reads. int64 fields arrive as strings
// (proto3 JSON) and are accepted as numbers too.

type otlpRequest struct {
	ResourceMetrics []struct {
		Resource     otlpResource `json:"resource"`
		ScopeMetrics []struct {
			Metrics []otlpMetric `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

type otlpResource struct {
	Attributes []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
		} `json:"value"`
	} `json:"attributes"`
}

func (r otlpResource) attrs() map[string]string {
	m := make(map[string]string, len(r.Attributes))
	for _, a := range r.Attributes {
		m[a.Key] = a.Value.StringValue
	}
	return m
}

type otlpMetric struct {
	Name  string      `json:"name"`
	Gauge *otlpPoints `json:"gauge"`
	Sum   *otlpPoints `json:"sum"`
}

// points returns the data points whichever instrument the producer labelled
// the metric with.
func (m otlpMetric) points() []otlpPoint {
	switch {
	case m.Gauge != nil:
		return m.Gauge.DataPoints
	case m.Sum != nil:
		return m.Sum.DataPoints
	}
	return nil
}

type otlpPoints struct {
	DataPoints []otlpPoint `json:"dataPoints"`
}

type otlpPoint struct {
	TimeUnixNano otlpInt64  `json:"timeUnixNano"`
	AsDouble     *float64   `json:"asDouble"`
	AsInt        *otlpInt64 `json:"asInt"`
}

func (p otlpPoint) value() (float64, bool) {
	switch {
	case p.AsDouble != nil:
		return *p.AsDouble, true
	case p.AsInt != nil:
		return float64(*p.AsInt), true
	}
	return 0, false
}

// otlpInt64 decodes an int64 sent as a JSON string or number.
type otlpInt64 int64

func (i *otlpInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*i = otlpInt64(n)
	return nil
}
