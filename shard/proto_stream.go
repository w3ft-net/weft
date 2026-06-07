package shard

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
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
// Returns io.EOF when the zero-length sentinel is encountered.
func ReadResultRow(r io.Reader) (*pb.ResultRow, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err // io.EOF or read error
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
