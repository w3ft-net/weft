package shell

import (
	"context"
	"strings"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shellcore"
)

// GRPCBackend implements Backend over gRPC.
type GRPCBackend struct {
	client   pb.WeftCoordinatorClient
	customer string
}

// NewGRPCBackend creates a backend using the coordinator gRPC client.
func NewGRPCBackend(client pb.WeftCoordinatorClient, customer string) *GRPCBackend {
	return &GRPCBackend{client: client, customer: customer}
}

func (b *GRPCBackend) ListFunctions() ([]string, error) {
	resp, err := b.client.ListFunctions(context.Background(), &pb.ListFunctionsRequest{
		Customer: b.customer,
	})
	if err != nil {
		return nil, err
	}
	return resp.Names, nil
}

func (b *GRPCBackend) GetFunction(name string) (string, error) {
	resp, err := b.client.GetFunction(context.Background(), &pb.GetFunctionRequest{
		Customer: b.customer, Name: name,
	})
	if err != nil {
		return "", err
	}
	return resp.Source, nil
}

func (b *GRPCBackend) SaveFunction(name, source string) error {
	_, err := b.client.SaveFunction(context.Background(), &pb.SaveFunctionRequest{
		Customer: b.customer, Name: name, Source: source,
	})
	return err
}

func (b *GRPCBackend) DeleteFunction(name string) error {
	_, err := b.client.DeleteFunction(context.Background(), &pb.DeleteFunctionRequest{
		Customer: b.customer, Name: name,
	})
	return err
}

func (b *GRPCBackend) ListWatches() ([]string, error) {
	resp, err := b.client.ListWatches(context.Background(), &pb.ListWatchesRequest{
		Customer: b.customer,
	})
	if err != nil {
		return nil, err
	}
	return resp.Names, nil
}

func (b *GRPCBackend) GetWatch(name string) (*WatchData, error) {
	resp, err := b.client.GetWatch(context.Background(), &pb.GetWatchRequest{
		Customer: b.customer, Name: name,
	})
	if err != nil {
		return nil, err
	}
	return &WatchData{
		Schedule: resp.Schedule,
		Pipeline: resp.Pipeline,
		Enabled:  resp.Enabled,
	}, nil
}

func (b *GRPCBackend) SaveWatch(name string, w *WatchData) error {
	_, err := b.client.SaveWatch(context.Background(), &pb.SaveWatchRequest{
		Customer: b.customer,
		Name:     name,
		Schedule: w.Schedule,
		Pipeline: w.Pipeline,
	})
	return err
}

func (b *GRPCBackend) DeleteWatch(name string) error {
	_, err := b.client.DeleteWatch(context.Background(), &pb.DeleteWatchRequest{
		Customer: b.customer, Name: name,
	})
	return err
}

// GetWatchResults implements WatchResultsBackend.
func (b *GRPCBackend) GetWatchResults(name string, limit int) ([]shellcore.WatchResultValue, error) {
	resp, err := b.client.GetWatchResults(context.Background(), &pb.GetWatchResultsRequest{
		Customer: b.customer, Name: name, Limit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	var out []shellcore.WatchResultValue
	for _, r := range resp.Results {
		out = append(out, shellcore.WatchResultValue{
			Value: strings.TrimSpace(r.Stdout),
			RanAt: r.RanAt,
		})
	}
	return out, nil
}
