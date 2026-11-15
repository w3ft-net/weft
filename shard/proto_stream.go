package shard

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// MaxResultRowBytes caps a single length-prefixed frame. One frame is
// one ResultRow (a log line + ts), so this is enormously generous —
// it exists only so a byte-misaligned or truncated stream can't make
// ReadResultRow allocate gigabytes and OOM the (512 MB) compute worker.
// A frame over this is treated as stream corruption, not data.
const MaxResultRowBytes = 64 << 20 // 64 MiB

// ErrOversizeFrame is returned by ReadResultRow when the length prefix
// exceeds MaxResultRowBytes — i.e. the stream is corrupt/misaligned.
// Callers should fail that shard, not the whole worker.
var ErrOversizeFrame = errors.New("result frame exceeds max size (stream corrupt or misaligned)")

// WriteResultRow writes a length-prefixed protobuf ResultRow to the writer.
// Format: [4-byte big-endian length][serialized protobuf]
func WriteResultRow(w io.Writer, row *pb.ResultRow) error {
	data, err := proto.Marshal(row)
	if err != nil {
		return fmt.Errorf("marshal ResultRow: %w", err)
	}
	length := uint32(len(data))
	if err := binary.Write(w, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}

// WriteStreamEnd writes a zero-length sentinel to indicate end of stream.
func WriteStreamEnd(w io.Writer) error {
	return binary.Write(w, binary.BigEndian, uint32(0))
}

// ReadResultRow reads a length-prefixed protobuf ResultRow from the reader.
// Returns io.EOF only when the zero-length sentinel is encountered; a stream
// that ends without one returns io.ErrUnexpectedEOF.
func ReadResultRow(r io.Reader) (*pb.ResultRow, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		if err == io.EOF {
			// The stream stopped where a frame or the end sentinel belonged:
			// the writer failed after it had sent its headers.
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if length == 0 {
		return nil, io.EOF // sentinel
	}
	if length > MaxResultRowBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrOversizeFrame, length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}
	row := &pb.ResultRow{}
	if err := proto.Unmarshal(data, row); err != nil {
		return nil, fmt.Errorf("unmarshal ResultRow: %w", err)
	}
	return row, nil
}

// ReadAllResultRows reads all length-prefixed protobuf ResultRows until the sentinel.
func ReadAllResultRows(r io.Reader) ([]*pb.ResultRow, error) {
	var rows []*pb.ResultRow
	for {
		row, err := ReadResultRow(r)
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		rows = append(rows, row)
	}
}

// ScanStats is what a cold-shard query read, sent after the end sentinel.
// Readers that stop at the sentinel never see it, and a stream from a Lambda
// that predates it simply ends there.
type ScanStats struct {
	// RowsScanned is the rows DuckDB's scans read, which counts the files a
	// query opened, not just the rows it returned. Partition pruning narrows
	// it; a bare count(*) or a LIMIT reads none.
	RowsScanned int64 `json:"rows_scanned"`
	// BytesRead is DuckDB's own total_bytes_read for the query.
	BytesRead int64 `json:"bytes_read"`
}

// ScanReporter is a RowSink that wants a cold shard's scan stats (the
// trailer after its stream). A sink without it never sees them.
type ScanReporter interface {
	ReportScan(shardID string, s ScanStats)
}

// maxScanStatsBytes bounds the trailer so a misaligned stream can't make
// ReadScanStats allocate.
const maxScanStatsBytes = 64 << 10

// WriteScanStats writes the trailer: [4-byte big-endian length][JSON].
func WriteScanStats(w io.Writer, s ScanStats) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal ScanStats: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}

// ReadScanStats reads the trailer that follows the end sentinel. ok is false
// with a nil error when the stream ends there, as an older Lambda's does.
func ReadScanStats(r io.Reader) (s ScanStats, ok bool, err error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		if err == io.EOF {
			return s, false, nil
		}
		return s, false, err
	}
	if length == 0 || length > maxScanStatsBytes {
		return s, false, fmt.Errorf("scan stats frame of %d bytes", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return s, false, fmt.Errorf("read scan stats: %w", err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, false, fmt.Errorf("unmarshal ScanStats: %w", err)
	}
	return s, true, nil
}
