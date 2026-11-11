package shard

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/auth"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// Retry budget for transient shard-side failures. See
// docs/plans/shard-grpc-resilience.md for the failure mode
// (rebind-mid-flight after a coordinator restart or an ASG
// instance replacement). 3 attempts × 100ms ≈ 200ms worst-case
// added to user-visible tail latency, which matters less than
// the alternative of returning `Canceled` to the user mid-deploy.
const (
	maxRetryAttempts = 3
	retryBackoff     = 100 * time.Millisecond
)

// pauseHoldTimeout bounds how long a call waits out a shard's pause (the
// volume moving to a successor during an upgrade, ~10–30s; see
// docs/plans/shard-seamless-upgrade.md). It sits inside the query and cron
// run budgets and well inside what the coordinator's ingest ring buffer
// absorbs at peak. Past it the call fails: a query reports its rows
// missing rather than answering without them.
var pauseHoldTimeout = 60 * time.Second

// isRetriable narrows the retry class to gRPC connection-lifecycle
// errors. Canceled appears when the underlying *grpc.ClientConn is
// closed mid-RPC; Unavailable when the new conn after a rebind isn't
// ready yet. Deliberately leaves InvalidArgument / NotFound /
// PermissionDenied / etc. alone so the caller sees the real error
// without a 200ms delay.
func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.Canceled || s.Code() == codes.Unavailable
	}
	return false
}

// GRPCShard implements the Shard interface by connecting to a remote shard
// over gRPC. Drop-in replacement for DuckDBShard in the coordinator.
//
// The connection resolves through a per-handle manual resolver, so the
// address can change under a live handle: Repoint follows the shard to a
// new instance (its shard_records addr moved), and a paused shard's
// refusal redirects calls to the successor it names. In-flight calls on
// the old address finish there.
type GRPCShard struct {
	id     string
	conn   *grpc.ClientConn
	client pb.ShardQueryClient
	res    *manual.Resolver

	mu sync.Mutex
	// home is the address the shard is known to live at (dialed, or set by
	// Repoint); cur is where calls go now, which a pause redirect can move
	// ahead of home. A held call goes back home when the successor it was
	// sent to stops answering (an upgrade rolled back).
	home, cur string
}

// NewGRPCShard connects to a remote shard gRPC server. token is the
// shared SHARD_SECRET both ends agree on; it's attached as Bearer
// authorization on every outbound call so the shard's
// SharedTokenServerInterceptor accepts the connection. Empty token is
// only valid in tests where the shard also runs without interceptors.
func NewGRPCShard(id, addr, token string) (*GRPCShard, error) {
	res := manual.NewBuilderWithScheme("weft-shard")
	res.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: addr}}})
	conn, err := grpc.NewClient(res.Scheme()+":///"+id,
		grpc.WithResolvers(res),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(weftsdk.GRPC.UnaryClientInterceptor(), auth.ClientTokenUnaryInterceptor(token)),
		grpc.WithChainStreamInterceptor(weftsdk.GRPC.StreamClientInterceptor(), auth.ClientTokenStreamInterceptor(token)),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to shard %s at %s: %w", id, addr, err)
	}
	return &GRPCShard{
		id:     id,
		conn:   conn,
		client: pb.NewShardQueryClient(conn),
		res:    res,
		home:   addr,
		cur:    addr,
	}, nil
}

// Configure calls the Configure RPC to assign identity and time window.
func (s *GRPCShard) Configure(ctx context.Context, cfg *pb.ConfigureRequest) error {
	return s.call(ctx, "Configure", false, func() (bool, error) {
		_, err := s.client.Configure(ctx, cfg)
		return false, err
	})
}

func (s *GRPCShard) ID() string { return s.id }

func (s *GRPCShard) Archive(ctx context.Context, s3Prefix string) (string, []CustomerStat, error) {
	var resp *pb.ArchiveResponse
	err := s.call(ctx, "Archive", false, func() (bool, error) {
		var err error
		resp, err = s.client.Archive(ctx, &pb.ArchiveRequest{S3Prefix: s3Prefix})
		return false, err
	})
	if err != nil {
		return "", nil, fmt.Errorf("archive shard %s: %w", s.id, err)
	}
	return resp.ParquetPath, CustomerStatsFromProto(resp.CustomerStats), nil
}

func (s *GRPCShard) Close() error {
	return s.conn.Close()
}

// Addr returns the address calls currently go to.
func (s *GRPCShard) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Repoint moves the handle to the shard's new address: the coordinator
// calls it when the shard's recorded addr changes (an upgrade swapped its
// volume to another instance), so the handle follows without a redial.
func (s *GRPCShard) Repoint(addr string) {
	s.mu.Lock()
	s.home = addr
	s.mu.Unlock()
	s.redirect(addr)
}

// redirect sends subsequent calls to addr, leaving home where it is.
func (s *GRPCShard) redirect(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == addr {
		return
	}
	s.cur = addr
	s.res.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: addr}}})
}

// Home returns the address the shard is recorded to live at, which a pause
// redirect does not move.
func (s *GRPCShard) Home() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.home
}

// call runs attempt until it succeeds or fails for good. It is where a
// shard's pause becomes latency instead of an error:
//
//   - a paused refusal redirects the handle to the successor it names and
//     retries; a not-ready refusal (the successor still opening) retries in
//     place. A refused call was never applied, so every call kind may
//     retry it. The hold is bounded by pauseHoldTimeout.
//   - an Unavailable from an address we were redirected to (while holding,
//     or on a handle an earlier call left redirected) means the successor
//     went away (a rollback): go back home and retry.
//   - otherwise a connection-lifecycle error (isRetriable) is retried up to
//     maxRetryAttempts, but only when retryTransport is set: such a call
//     may have been applied before the connection dropped, which a read can
//     repeat and a write can't.
//
// attempt reports progressed once it has delivered anything (a row to a
// sink); after that nothing is retried, since a retry would deliver it
// twice.
//
// A held call logs once, when it ends, with how long it held: that line is
// what a pause cost a caller.
func (s *GRPCShard) call(ctx context.Context, op string, retryTransport bool, attempt func() (progressed bool, err error)) (err error) {
	var holdStart, holdUntil time.Time
	defer func() {
		if holdStart.IsZero() {
			return
		}
		if err != nil {
			weftsdk.Infof(ctx, "shard %s: %s held %s by a pause, then failed: %v", s.id, op, time.Since(holdStart), err)
			return
		}
		weftsdk.Infof(ctx, "shard %s: %s held %s by a pause, then went through at %s", s.id, op, time.Since(holdStart), s.Addr())
	}()
	transportFailures := 0
	for {
		var progressed bool
		progressed, err = attempt()
		if err == nil || progressed {
			return err
		}
		kind, successor := ParseRefusal(err)
		holding := !holdUntil.IsZero()
		switch {
		case kind != NotRefused:
			if !holding {
				holdStart = time.Now()
				holdUntil = holdStart.Add(pauseHoldTimeout)
			}
			if kind == RefusedPaused && successor != "" {
				s.redirect(successor)
			}
		case status.Code(err) == codes.Unavailable && (holding || s.Addr() != s.Home()):
			// The address we were sent to has gone away: a rollback tore the
			// successor down. Redirected earlier by another call that has since
			// ended, the handle can be there without this call holding, and a
			// handle that lives as long as its process (compute's) would fail
			// every call on the dead successor.
			s.redirect(s.Home())
		case retryTransport && isRetriable(err):
			transportFailures++
			if transportFailures >= maxRetryAttempts {
				return fmt.Errorf("shard %s: gave up after %d attempts: %w", s.id, maxRetryAttempts, err)
			}
			weftsdk.Errorf(ctx, "shard %s: %s attempt %d failed (%v); retrying", s.id, op, transportFailures, err)
		default:
			return err
		}
		if holding && time.Now().After(holdUntil) {
			return fmt.Errorf("shard %s: %s still refused after holding %s: %w", s.id, op, pauseHoldTimeout, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("shard %s: %s: %w (last error: %v)", s.id, op, ctx.Err(), err)
		case <-time.After(retryBackoff):
		}
	}
}

// Ingest forwards rows to the remote shard for insertion. On error,
// reports the failure to rec (if non-nil) and returns an error that
// carries the resulting consecutive-failure count, so callers only
// need a single log line.
func (s *GRPCShard) Ingest(ctx context.Context, rows []*pb.LogRow, rec IngestRecorder) error {
	return s.IngestBatch(ctx, Batch{Rows: rows}, rec)
}

// IngestBatch is Ingest for a batch that carries its partition values.
func (s *GRPCShard) IngestBatch(ctx context.Context, b Batch, rec IngestRecorder) error {
	err := s.call(ctx, "Ingest", false, func() (bool, error) {
		_, err := s.client.Ingest(ctx, &pb.IngestRequest{Rows: b.Rows, PartitionValues: b.PartitionValues})
		return false, err
	})
	if err != nil {
		// wrapIngestError already names the shard; pass the raw RPC
		// error so the formatted message reads as one phrase, not two.
		return wrapIngestError(s.id, err, rec)
	}
	if rec != nil {
		rec.ResetIngestFailures(s.id)
	}
	return nil
}

// Query executes SQL on the remote shard and collects all result
// rows. A failed attempt's buffered rows are discarded, so the whole
// query is safely re-driven from scratch.
func (s *GRPCShard) Query(ctx context.Context, sql string, args ...any) ([]*pb.ResultRow, error) {
	var rows []*pb.ResultRow
	err := s.call(ctx, "Query", true, func() (bool, error) {
		var err error
		rows, err = s.queryOnce(ctx, sql, args)
		return false, err
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *GRPCShard) queryOnce(ctx context.Context, sql string, args []any) ([]*pb.ResultRow, error) {
	stream, err := s.client.Query(ctx, &pb.QueryRequest{Sql: sql, Args: anyArgsToStrings(args)})
	if err != nil {
		return nil, fmt.Errorf("query shard %s: %w", s.id, err)
	}
	var rows []*pb.ResultRow
	for {
		row, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv from shard %s: %w", s.id, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// QueryToSink executes SQL on the remote shard and streams rows to
// the sink. Only the pre-first-row window is retried: once any row
// reaches the sink, a retry would double-send, so a mid-stream failure
// fails the shard's part of the query.
func (s *GRPCShard) QueryToSink(ctx context.Context, sql string, args []any, sink RowSink) {
	s.queryToSink(ctx, "", sql, "", args, sink)
}

// QueryFieldsToSink is QueryToSink with the query's field form for a shard that
// understands it; a shard from before it runs sql. See FieldQuerier.
func (s *GRPCShard) QueryFieldsToSink(ctx context.Context, sql, fieldSQL string, args []any, sink RowSink) {
	s.queryToSink(ctx, "", sql, fieldSQL, args, sink)
}

// QueryTableToSink is QueryToSink with the table the plan says holds the rows,
// and the field form if there is one. A shard from before the table field
// ignores it and runs sql. See TableQuerier.
func (s *GRPCShard) QueryTableToSink(ctx context.Context, table, sql, fieldSQL string, args []any, sink RowSink) {
	s.queryToSink(ctx, table, sql, fieldSQL, args, sink)
}

func (s *GRPCShard) queryToSink(ctx context.Context, table, sql, fieldSQL string, args []any, sink RowSink) {
	defer sink.SendSentinel(s.id)

	rowsSent := 0
	err := s.call(ctx, "QueryToSink", true, func() (bool, error) {
		n, err := s.queryToSinkOnce(ctx, table, sql, fieldSQL, args, sink)
		rowsSent += n
		return n > 0, err
	})
	switch {
	case err == nil:
	case rowsSent > 0:
		weftsdk.Errorf(ctx, "shard %s: QueryToSink mid-stream error after %d rows: %v", s.id, rowsSent, err)
		sink.Fail(s.id, fmt.Errorf("failed after %d rows: %w", rowsSent, err))
	default:
		weftsdk.Errorf(ctx, "shard %s: QueryToSink failed: %v (sql=%s)", s.id, err, sql)
		sink.Fail(s.id, err)
	}
}

// queryToSinkOnce attempts a single QueryToSink streaming pass.
// Returns (rowsSent, err); err==nil means EOF after streaming
// completed. A non-nil err with rowsSent==0 is safe to retry; with
// rowsSent>0 it isn't.
func (s *GRPCShard) queryToSinkOnce(ctx context.Context, table, sql, fieldSQL string, args []any, sink RowSink) (int, error) {
	stream, err := s.client.Query(ctx, &pb.QueryRequest{Sql: sql, FieldSql: fieldSQL, Table: table, Args: anyArgsToStrings(args)})
	if err != nil {
		return 0, err
	}
	rowsSent := 0
	for {
		row, err := stream.Recv()
		if err == io.EOF {
			return rowsSent, nil
		}
		if err != nil {
			return rowsSent, err
		}
		sink.SendRow(row, s.id)
		rowsSent++
	}
}
