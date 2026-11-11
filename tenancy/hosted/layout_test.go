package hosted

import (
	"reflect"
	"testing"

	"github.com/graybeardcorp/weft/shape"
)

// Existing hosted volumes record no layout and are adopted as the legacy
// customer_id one, so the hosted shape must generate exactly that layout, or a
// shape change would reinterpret data already on disk.
func TestHostedShapeGeneratesTheLegacyLayout(t *testing.T) {
	if got, want := shape.BuildLayout(), shape.Legacy(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the hosted shape's layout = %+v, want the legacy layout %+v", got, want)
	}
	if got := (shape.Coord{Customer: 7}).PartitionValues(); !reflect.DeepEqual(got, []string{"7"}) {
		t.Fatalf("PartitionValues = %v, want [7]", got)
	}
}
