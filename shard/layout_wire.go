package shard

import (
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// LayoutToProto is a layout's wire form, as sent in ConfigureRequest.
func LayoutToProto(l shape.Layout) *pb.PartitionLayout {
	out := &pb.PartitionLayout{Fingerprint: l.Fingerprint}
	for _, c := range l.Columns {
		out.Columns = append(out.Columns, &pb.PartitionColumn{Name: c.Name, Type: string(c.Type), Role: string(c.Role)})
	}
	return out
}

// LayoutFromProto reads a wire layout. An absent one is the legacy layout, which
// is what a coordinator from before layouts sends by sending nothing. One with
// no columns but a fingerprint is a real layout with no partition columns.
func LayoutFromProto(p *pb.PartitionLayout) shape.Layout {
	if p.GetFingerprint() == "" {
		return shape.Legacy()
	}
	cols := make([]shape.LayoutColumn, len(p.Columns))
	for i, c := range p.Columns {
		cols[i] = shape.LayoutColumn{Name: c.Name, Type: shape.ColumnType(c.Type), Role: shape.Role(c.Role)}
	}
	return shape.Layout{Columns: cols, Fingerprint: p.Fingerprint}
}
