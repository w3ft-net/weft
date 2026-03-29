package shard

import (
	"encoding/json"
	"time"
)

// LogRow represents a single log entry stored in a shard.
type LogRow struct {
	LineNum  int64
	Raw      string
	Fields   []string
	Ts       time.Time
	Level    string
	App      string
	Instance string
	Customer string
	Payload  json.RawMessage
}
