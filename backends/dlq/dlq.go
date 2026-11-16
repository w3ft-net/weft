// Package dlq is the contract of the dead-letter domain: where a batch of
// rows goes when the coordinator cannot place it on a shard, and how it comes
// back. S3 implements it for the hosted build (dlq/dlqs3) and memory for
// the open one (defaults); dlq/dlqstoretest is the suite both pass.
//
// A batch belongs to one owner: the columns the shape adds to its rows travel
// with it, so a replay restores whose rows they are.
package dlq

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// Entry is one stored row with the columns the shape adds to it, which are not
// part of a LogRow.
type Entry struct {
	*pb.LogRow
	shape.Added
}

// Store is the persistence contract of the dead-letter domain.
type Store interface {
	// Write keeps a batch whose rows share the added columns, and returns where
	// it went (a key, a row id: only for the log and the failure record). An
	// empty batch is kept as nothing and returns "" with no error. A failed
	// write returns the error and the batch is not kept.
	Write(ctx context.Context, added shape.Added, rows []*pb.LogRow) (ref string, err error)

	// Replay hands every kept batch to sink, one batch at a time, and removes a
	// batch only after sink has returned: a crash in between delivers it twice
	// rather than never. It returns the rows delivered. A batch that cannot be
	// read or decoded stays, is reported in the error, and the others still
	// replay.
	Replay(ctx context.Context, sink func([]Entry)) (int, error)
}

// line is one stored row: the LogRow as protojson, and the columns the shape
// adds to it, so a replay restores every one.
type line struct {
	Added shape.Added     `json:"added"`
	Row   json.RawMessage `json:"row"`
}

// Encode is a batch as gzipped JSON lines, one per row. It is the format every
// store keeps, so a batch can be carried from one to another.
func Encode(added shape.Added, rows []*pb.LogRow) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, r := range rows {
		row, err := protojson.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("marshal row: %w", err)
		}
		b, err := json.Marshal(line{Added: added, Row: row})
		if err != nil {
			return nil, fmt.Errorf("marshal row: %w", err)
		}
		gz.Write(b)
		gz.Write([]byte{'\n'})
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode reads what Encode wrote.
func Decode(r io.Reader) ([]Entry, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	var out []Entry
	dec := json.NewDecoder(gz)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		e, err := decodeLine(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
}

// decodeLine reads one stored row from its envelope, which carries the group
// beside the row. A bare LogRow, written before the envelope, names its group
// nowhere a replay can read, so it is an error: its batch stays and is
// reported, and is never replayed as the zero group's.
func decodeLine(raw json.RawMessage) (Entry, error) {
	var env struct {
		Added *shape.Added    `json:"added"`
		Row   json.RawMessage `json:"row"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Entry{}, err
	}
	if env.Row == nil {
		return Entry{}, errors.New("line has no envelope: written before the dead-letter envelope, so its group is unknown")
	}
	lr := &pb.LogRow{}
	if err := protojson.Unmarshal(env.Row, lr); err != nil {
		return Entry{}, err
	}
	if env.Added == nil {
		return Entry{}, errors.New("row has no added columns")
	}
	return Entry{lr, *env.Added}, nil
}
