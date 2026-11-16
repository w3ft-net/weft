package compute

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shard"
)

// The layout in a cold shard's descriptor reaches the cold-scan Lambda in its
// request, so the Lambda types the archive's hive columns as the archive was
// written. A descriptor with none (an older coordinator's) is the legacy layout.
func TestComputeForwardsTheDescriptorsLayoutToTheLambda(t *testing.T) {
	custom := shape.NewLayout(shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition})
	for name, tc := range map[string]struct {
		layout *pb.PartitionLayout
		want   shape.Layout
	}{
		"stated": {shard.LayoutToProto(custom), custom},
		"absent": {nil, shape.Legacy()},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var got shard.LambdaQueryRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				_ = json.Unmarshal(b, &got)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/x-protobuf-stream")
				_ = shard.WriteStreamEnd(w)
			}))
			defer srv.Close()

			archives, err := NewArchiveRouter(map[string]string{archive.Default: ArchiveReaderLambda}, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			pool := &Engine{Archive: archives}
			stream, err := pool.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
				Sql: "SELECT raw, ts FROM logs",
				Shards: []*pb.ShardDescriptor{{
					ShardId:     "cold-test",
					LambdaUrl:   srv.URL,
					ParquetPath: "s3://weft-cold/shard-x/sized/**/*.parquet",
					Layout:      tc.layout,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			for {
				if _, ok, nerr := stream.Next(ctx); !ok {
					if nerr != nil {
						t.Fatalf("stream error: %v", nerr)
					}
					break
				}
			}
			_ = stream.Close()

			mu.Lock()
			defer mu.Unlock()
			if got.Layout.Fingerprint != tc.want.Fingerprint {
				t.Errorf("lambda got layout %+v, want %+v", got.Layout, tc.want)
			}
		})
	}
}
