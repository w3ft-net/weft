package coordinator

import (
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

func TestApplyArchiveSQL(t *testing.T) {
	live := &pb.ShardDescriptor{ShardId: "live", GrpcAddr: "h:1"}
	typed := &pb.ShardDescriptor{ShardId: "typed", GrpcAddr: "h:2", Sql: "typed sql", Args: []string{"t"}, FieldSql: "typed fields"}
	archived := &pb.ShardDescriptor{ShardId: "archived", LambdaUrl: "https://l", ParquetPath: "s3://b/a/**/*.parquet", FieldSql: "stale"}
	descs := []*pb.ShardDescriptor{live, typed, archived}

	applyArchiveSQL("", []string{"x"}, descs)
	if live.Sql != "" || archived.Sql != "" {
		t.Fatalf("no archive SQL changed a descriptor: %+v %+v", live, archived)
	}

	applyArchiveSQL("archive sql", []string{"a", "b"}, descs)
	if live.Sql != "" || live.FieldSql != "" {
		t.Errorf("a live shard was given the archive's SQL: %+v", live)
	}
	if typed.Sql != "typed sql" || typed.FieldSql != "typed fields" || len(typed.Args) != 1 {
		t.Errorf("a live shard's typed variant was replaced: %+v", typed)
	}
	if archived.Sql != "archive sql" || len(archived.Args) != 2 || archived.FieldSql != "" {
		t.Errorf("the archived shard did not get the archive's SQL, args and no field form: %+v", archived)
	}
}
