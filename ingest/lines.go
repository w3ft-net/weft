package ingest

import (
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/proto"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// FillJSONPayload makes a JSON object line its own payload when the row
// arrived without one. jq reads the payload, and an SDK record (weft-go
// Send, heartbeats, runtime stats) reaches here as a bare JSON line: its
// socket frame carries no fmt=json header and its source has no schema,
// so nothing upstream set one. Rows are changed in place.
func FillJSONPayload(rows []*pb.LogRow) {
	for _, r := range rows {
		if len(r.PayloadJson) > 0 {
			continue
		}
		raw := strings.TrimSpace(r.Raw)
		if strings.HasPrefix(raw, "{") && gjson.Valid(raw) {
			r.PayloadJson = []byte(raw)
		}
	}
}

// SplitLines stores a multi-line text event as one row per line, so a row
// is a line to the shard and to grep alike (docs/plans/line-storage.md).
//
// Each line keeps every other column of the event and gets a shared
// event_id (a fresh UUIDv7) and its 0-based line_no. One-line events keep
// both empty. Rows with a JSON payload are never split — jq reads the
// payload once per event — and a row that already carries an event_id has
// been split before (a DLQ re-ingest), so it passes through.
//
// Line endings are normalized first: "\r\n" becomes "\n", a trailing "\r"
// on a line is dropped, and one trailing "\n" ends the last line rather
// than starting an empty one. Blank lines inside an event stay as empty
// rows.
//
// The input rows are not mutated except for a one-line row's Raw (the
// normalization) and an event's first line, which reuses the original
// row; the result may alias the input.
func SplitLines(rows []*pb.LogRow) []*pb.LogRow {
	var out []*pb.LogRow // allocated on the first row that splits
	for i, r := range rows {
		split := splitRow(r)
		if split == nil {
			if out != nil {
				out = append(out, r)
			}
			continue
		}
		if out == nil {
			out = make([]*pb.LogRow, i, len(rows)+len(split))
			copy(out, rows[:i])
		}
		out = append(out, split...)
	}
	if out == nil {
		return rows
	}
	return out
}

// splitRow returns the rows an event becomes, or nil when r stays as it
// is. A one-line event is normalized in place and returns nil.
func splitRow(r *pb.LogRow) []*pb.LogRow {
	if r.EventId != "" || len(r.PayloadJson) > 0 {
		return nil
	}
	if !strings.ContainsAny(r.Raw, "\r\n") {
		return nil
	}
	raw := strings.ReplaceAll(r.Raw, "\r\n", "\n")
	raw = strings.TrimSuffix(raw, "\n")
	lines := strings.Split(raw, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	if len(lines) == 1 {
		r.Raw = lines[0]
		return nil
	}
	id := uuid.Must(uuid.NewV7()).String()
	out := make([]*pb.LogRow, len(lines))
	for i, l := range lines {
		row := r
		if i > 0 {
			row = proto.Clone(r).(*pb.LogRow)
		}
		row.Raw = l
		row.EventId = id
		row.LineNo = int32(i)
		out[i] = row
	}
	return out
}
