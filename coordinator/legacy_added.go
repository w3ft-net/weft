package coordinator

import (
	"strconv"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

// legacyAdded is the added columns of a row that carries its tenant only in
// LogRow.customer_id (tag 26): a peer coordinator's tail fan-out, or a
// dead-letter line written before the envelope. Only the legacy layout, whose
// one partition column is that customer, has anything to read there; under any
// other layout the columns are the zero value. These are the two places the
// coordinator reads tag 26, and they go with the tag-26 cut-over.
func legacyAdded(r *pb.LogRow) shape.Added {
	if !shape.BuildLayout().IsLegacy() {
		return shape.Added{}
	}
	a, _ := shape.AddedFromPartition([]string{strconv.FormatInt(r.CustomerId, 10)})
	return a
}
