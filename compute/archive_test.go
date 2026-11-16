package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/backends/logs/archive/parquet"
	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/backends/logs/live"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// otherDialect is DuckDB under another name.
type otherDialect struct{ dialect.Dialect }

func (otherDialect) Name() string { return "other" }

// fakeReader reads every Parquet format, in a dialect of its choosing, and
// answers every query with rows. Its handle is not a shard.Shard.
type fakeReader struct {
	d      dialect.Dialect
	rows   []string
	opened []archive.Archive
}

func (r *fakeReader) Reads() []string {
	return []string{parquet.FormatFile, parquet.FormatTime, parquet.FormatKeyed, parquet.FormatSized}
}
func (r *fakeReader) Dialect() dialect.Dialect { return r.d }
func (r *fakeReader) Open(a archive.Archive) (archive.Handle, error) {
	r.opened = append(r.opened, a)
	return &fakeHandle{id: a.ID, rows: r.rows}, nil
}

type fakeHandle struct {
	id   string
	rows []string
}

func (h *fakeHandle) QueryToSink(_ context.Context, _ string, _ []any, sink live.RowSink) {
	defer sink.SendSentinel(h.id)
	for i, raw := range h.rows {
		sink.SendRow(&pb.ResultRow{Raw: raw, TsUnixNs: int64(i + 1)}, h.id)
	}
}
func (h *fakeHandle) Close() error { return nil }

func routerFor(t *testing.T, r archive.Reader, readers map[string]string) *archive.Router {
	t.Helper()
	reg := archive.NewRegistry()
	for _, f := range parquet.Formats() {
		if err := reg.AddFormat(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.AddReader("fake", r); err != nil {
		t.Fatal(err)
	}
	rt, err := reg.Readers(readers)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func archived(path, dialect string) *pb.ShardDescriptor {
	return &pb.ShardDescriptor{ShardId: "arch-1", ParquetPath: path, LambdaUrl: "https://fn.example/q", Services: []string{"api"}, Dialect: dialect}
}

// An archived shard is read through the reader its format is routed to, and what
// that reader is given is what the descriptor says.
func TestArchivedShard_IsOpenedThroughItsReader(t *testing.T) {
	custom := shape.NewLayout(shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition})
	fr := &fakeReader{d: dialect.DuckDB, rows: []string{"one", "two"}}
	d := archived("s3://b/shard.attempt-1/sized/**/*.parquet", "")
	d.Layout = &pb.PartitionLayout{Fingerprint: custom.Fingerprint, Columns: []*pb.PartitionColumn{{Name: "account_id", Type: "int64", Role: "partition"}}}
	e := &Engine{Archive: routerFor(t, fr, map[string]string{archive.Default: "fake"})}

	stream, err := e.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{Sql: "SELECT raw, ts FROM logs", Shards: []*pb.ShardDescriptor{d}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		line, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, line.Raw)
	}
	if strings.Join(got, ",") != "one,two" {
		t.Errorf("rows = %v", got)
	}
	if len(fr.opened) != 1 {
		t.Fatalf("opened %d times", len(fr.opened))
	}
	a := fr.opened[0]
	if a.ID != "arch-1" || a.Path != d.ParquetPath || a.Endpoint != "https://fn.example/q" || len(a.Services) != 1 {
		t.Errorf("the reader was given %+v", a)
	}
	if a.Layout.Fingerprint != custom.Fingerprint {
		t.Errorf("the reader was given layout %q, want %q", a.Layout.Fingerprint, custom.Fingerprint)
	}
}

// The coordinator plans the archive tier in one dialect. A reader that runs
// another would fail the query, so it is refused when the shard is opened; an
// older coordinator names none, which is DuckDB.
func TestArchivedShard_ARreaderOfAnotherDialectIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, planned string
		reader        dialect.Dialect
		want          string // "" for none
	}{
		{"named, the same", "other", otherDialect{dialect.DuckDB}, ""},
		{"named, another", "other", dialect.DuckDB, "written in other, but its reader runs duckdb"},
		{"an older coordinator, on DuckDB", "", dialect.DuckDB, ""},
		{"an older coordinator, on another", "", otherDialect{dialect.DuckDB}, "written in duckdb, but its reader runs other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeReader{d: tc.reader}
			r, err := ResolverFromDescriptor(archived("s3://b/a.parquet", tc.planned), "", routerFor(t, fr, map[string]string{archive.Default: "fake"}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Dial()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("dial: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// Every way an archive can have no reader is an error that says so.
func TestArchivedShard_WithNoReaderSaysSo(t *testing.T) {
	fr := &fakeReader{d: dialect.DuckDB}
	for _, tc := range []struct {
		name   string
		router *archive.Router
		path   string
		want   string
	}{
		{"no router", nil, "s3://b/a.parquet", "no archive reader configured"},
		{"a format with no reader", routerFor(t, fr, map[string]string{parquet.FormatSized: "fake"}), "s3://b/a.parquet", "no reader is configured for format"},
		{"a path of no format", routerFor(t, fr, map[string]string{archive.Default: "fake"}), "s3://b/a.csv", "no registered format recognizes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ResolverFromDescriptor(archived(tc.path, ""), "", tc.router)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Dial(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if _, err := ResolverFromDescriptor(&pb.ShardDescriptor{ShardId: "x"}, "", nil); err == nil {
		t.Error("a descriptor with neither an address nor a path resolved")
	}
}
