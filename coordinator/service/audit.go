package service

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// AuditFleet serves the database half of `weft-admin audit`.
func (s *Server) AuditFleet(_ context.Context, _ *pb.AuditFleetRequest) (*pb.AuditFleetResponse, error) {
	if s.Store == nil {
		return nil, status.Errorf(codes.Unavailable, "store not configured")
	}
	store := logdb.ShardStore{Store: s.Store}
	shards, err := store.ListShards()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list shards: %v", err)
	}
	spares, err := store.ListSpares()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list spares: %v", err)
	}
	errs, err := store.IngestionErrorCount()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count ingestion errors: %v", err)
	}
	resp := &pb.AuditFleetResponse{IngestionErrors: errs}
	for _, sh := range shards {
		resp.Shards = append(resp.Shards, &pb.AuditShardState{
			Id:           sh.ID,
			State:        string(sh.State),
			InstanceId:   sh.InstanceID,
			VolumeId:     sh.EBSVolumeID,
			UpgradePhase: string(sh.UpgradePhase),
		})
	}
	for _, sp := range spares {
		resp.Spares = append(resp.Spares, &pb.AuditSpareState{InstanceId: sp.InstanceID, Claimed: sp.ClaimedBy != nil})
	}
	return resp, nil
}
