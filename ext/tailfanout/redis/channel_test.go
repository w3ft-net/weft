package redisbus

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// A tail row's group is named by the channel it was published on, which is
// built from the publisher's added columns, and not by LogRow.customer_id
// (tag 26): the coordinator stops stamping that tag, and a receiver that read
// it would attribute every row to the zero group.
func TestRowFromMessage_TheGroupIsTheChannelsNotTag26(t *testing.T) {
	shapetest.NeedsGroup(t)
	payload, err := proto.Marshal(&pb.LogRow{Namespace: "ns", Service: "svc", Kind: "main", Raw: "x"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rowFromMessage(channelFor(coordinator.Row{LogRow: &pb.LogRow{Namespace: "ns", Service: "svc", Kind: "main"}, Added: shapetest.Added(7)}), payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Added != shapetest.Added(7) {
		t.Errorf("row read as group %+v, want 7's columns", got.Added)
	}
	if got.Raw != "x" {
		t.Errorf("row payload = %q, want x", got.Raw)
	}
}

// The channel is what a build of this version and the previous one both write
// the same way, so a row from a peer that still stamps tag 26 reads the same.
func TestRowFromMessage_ARowThatAlsoCarriesTag26ReadsTheSame(t *testing.T) {
	shapetest.NeedsGroup(t)
	payload, err := proto.Marshal(&pb.LogRow{Namespace: "ns", Service: "svc", Kind: "main", CustomerId: 7})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rowFromMessage("tail:7:ns:svc:main", payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Added != shapetest.Added(7) {
		t.Errorf("row read as group %+v, want 7's columns", got.Added)
	}
}

// A channel that does not name a group cannot be attributed to one, so the
// message is refused instead of being read as the zero group's.
func TestRowFromMessage_AChannelWithNoGroupIsRefused(t *testing.T) {
	shapetest.NeedsGroup(t)
	payload, err := proto.Marshal(&pb.LogRow{Namespace: "ns", Service: "svc", Kind: "main", CustomerId: 7})
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"tail:", "tail:ns", "tail:notanumber:ns:svc:main", "notail:7:ns:svc:main", ""} {
		if got, err := rowFromMessage(channel, payload); err == nil {
			t.Errorf("channel %q read as %+v, want an error", channel, got.Added)
		}
	}
}
