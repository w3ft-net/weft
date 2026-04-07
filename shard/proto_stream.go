package shard

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// WriteLogRow writes a length-prefixed protobuf LogRow to the writer.
// Format: [4-byte big-endian length][serialized protobuf]
func WriteLogRow(w io.Writer, row *pb.LogRow) error {
	data, err := proto.Marshal(row)
	if err != nil {
		return fmt.Errorf("marshal LogRow: %w", err)
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

// ReadLogRow reads a length-prefixed protobuf LogRow from the reader.
// Returns io.EOF when the zero-length sentinel is encountered.
func ReadLogRow(r io.Reader) (*pb.LogRow, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err // io.EOF or read error
	}
	if length == 0 {
		return nil, io.EOF // sentinel
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}
	row := &pb.LogRow{}
	if err := proto.Unmarshal(data, row); err != nil {
		return nil, fmt.Errorf("unmarshal LogRow: %w", err)
	}
	return row, nil
}

// ReadAllLogRows reads all length-prefixed protobuf LogRows until the sentinel.
func ReadAllLogRows(r io.Reader) ([]*pb.LogRow, error) {
	var rows []*pb.LogRow
	for {
		row, err := ReadLogRow(r)
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return rows, err
		}
		rows = append(rows, row)
	}
}
