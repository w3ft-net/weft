package main

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
)

// blackholeProxy forwards TCP to a backend until Blackhole, after which it
// keeps both sockets open and drops everything, which is what a middlebox
// swapped out from under an established connection looks like to the client:
// no reset, no close, only silence.
type blackholeProxy struct {
	ln   net.Listener
	hole atomic.Bool
}

func newBlackholeProxy(t *testing.T, backend string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &blackholeProxy{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", backend)
			if err != nil {
				c.Close()
				continue
			}
			t.Cleanup(func() { c.Close(); b.Close() })
			go p.pipe(c, b)
			go p.pipe(b, c)
		}
	}()
	return p
}

func (p *blackholeProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !p.hole.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				return
			}
			return
		}
	}
}

func (p *blackholeProxy) Addr() string { return p.ln.Addr().String() }
func (p *blackholeProxy) Blackhole()   { p.hole.Store(true) }

func startHealthServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             50 * time.Millisecond,
		PermitWithoutStream: true,
	}))
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// A coordinator connection that goes silent (a middlebox replaced under it)
// must be noticed by weftd, not left looking healthy until the kernel gives up
// some fifteen minutes later: weftd's telemetry stalled that long each time
// the route its connection took was replaced.
func TestCoordConnectionNoticesASilentPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a real keepalive interval")
	}
	// grpc-go will not ping a client connection more often than every 10s, so
	// this takes about 11s however small the settings are.
	prev := coordKeepalive
	coordKeepalive = keepalive.ClientParameters{Time: 10 * time.Second, Timeout: time.Second, PermitWithoutStream: true}
	t.Cleanup(func() { coordKeepalive = prev })

	proxy := newBlackholeProxy(t, startHealthServer(t))
	conn, err := grpc.NewClient(proxy.Addr(), coordDialOptions(false, bearerToken{token: "t"})...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn.Connect()
	for conn.GetState() != connectivity.Ready {
		if !conn.WaitForStateChange(ctx, conn.GetState()) {
			t.Fatalf("never became ready, state %v", conn.GetState())
		}
	}

	proxy.Blackhole()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer waitCancel()
	if !conn.WaitForStateChange(waitCtx, connectivity.Ready) {
		t.Fatal("the connection still looks ready 15s after the peer went silent: weftd would not notice a replaced middlebox")
	}
}

// weftd's own keepalive is on, and a coordinator accepts pings that often.
func TestCoordKeepaliveDefaultsAreOn(t *testing.T) {
	k := coordKeepalive
	if k.Time <= 0 || k.Time > time.Minute {
		t.Errorf("keepalive Time = %v, want a ping every few tens of seconds", k.Time)
	}
	if k.Timeout <= 0 {
		t.Errorf("keepalive Timeout = %v, want a bound on waiting for a reply", k.Timeout)
	}
	if !k.PermitWithoutStream {
		t.Error("PermitWithoutStream is false: an idle connection (no stream open) would never be probed")
	}
}
