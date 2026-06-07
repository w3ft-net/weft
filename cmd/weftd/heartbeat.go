package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/weftd/heartbeat"
)

// HeartbeatConfig is the YAML config for the local heartbeat
// listener. Defaults make weftd serve a Unix socket and skip the
// optional TCP listener; operators in trusted environments enable
// the TCP listener for friction-free shell/cron use. See
// docs/plans/app-heartbeats.md for the full design.
type HeartbeatConfig struct {
	Socket           string `yaml:"socket"`            // default /var/run/weftd-heartbeat.sock; empty disables
	Port             int    `yaml:"port"`              // default 0; 0 disables, 14001 conventional
	SnapshotInterval string `yaml:"snapshot_interval"` // default 5s
	MaxServices      int    `yaml:"max_services"`      // default 1024
	StaleAfter       string `yaml:"stale_after"`       // default 60s
}

const (
	defaultHeartbeatSocket   = "/var/run/weftd-heartbeat.sock"
	defaultSnapshotInterval  = 5 * time.Second
	defaultMaxHeartbeatLimit = 1024
	defaultStaleAfter        = 60 * time.Second
)

// startHeartbeat wires the heartbeat subsystem: at most one Unix
// socket listener, optionally a TCP listener, plus the periodic
// snapshot ticker that ships state to the coordinator alongside
// other agent records. Returns immediately; spawned goroutines
// run until ctx is cancelled.
//
// Returns the *heartbeat.Heartbeats so tests and integration
// callers can verify state without going through HTTP.
func startHeartbeat(ctx context.Context, cfg HeartbeatConfig, coordClient pb.WeftCoordinatorClient,
	customer, instance, namespace string) (*heartbeat.Heartbeats, error) {

	// Apply defaults. Empty socket + zero port → fall back to the
	// default socket path so the listener is on by default.
	socket := cfg.Socket
	if socket == "" && cfg.Port == 0 {
		socket = defaultHeartbeatSocket
	}

	snapshotInterval := defaultSnapshotInterval
	if cfg.SnapshotInterval != "" {
		d, err := time.ParseDuration(cfg.SnapshotInterval)
		if err != nil {
			return nil, fmt.Errorf("heartbeat.snapshot_interval: %w", err)
		}
		snapshotInterval = d
	}

	staleAfter := defaultStaleAfter
	if cfg.StaleAfter != "" {
		d, err := time.ParseDuration(cfg.StaleAfter)
		if err != nil {
			return nil, fmt.Errorf("heartbeat.stale_after: %w", err)
		}
		staleAfter = d
	}

	maxServices := cfg.MaxServices
	if maxServices == 0 {
		maxServices = defaultMaxHeartbeatLimit
	}

	h := heartbeat.New(maxServices, staleAfter)
	handler := h.Handler()

	if socket != "" {
		// Remove any stale socket file from a previous run; net.Listen
		// won't bind over an existing socket file.
		_ = os.Remove(socket)
		l, err := net.Listen("unix", socket)
		if err != nil {
			return nil, fmt.Errorf("heartbeat unix socket %s: %w", socket, err)
		}
		// Restrict to filesystem-permission auth: 0666 lets any local
		// uid post; tighten with directory permissions if multi-tenant
		// containers share the host. See plan's "open questions".
		_ = os.Chmod(socket, 0666)
		go serveHTTPUntil(ctx, l, handler, fmt.Sprintf("unix:%s", socket))
		go func() {
			<-ctx.Done()
			os.Remove(socket)
		}()
		log.Printf("heartbeat: listening on unix:%s", socket)
	}

	if cfg.Port != 0 {
		addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("heartbeat tcp %s: %w", addr, err)
		}
		go serveHTTPUntil(ctx, l, handler, addr)
		log.Printf("heartbeat: listening on %s", addr)
	}

	// Each service gets its own /log/<ns>/<service>/heartbeats.log
	// pseudofile — the snapshot ticker fans out one row per known
	// service to its own app stream.
	go runHeartbeatSnapshots(ctx, coordClient, h, customer, instance, namespace, snapshotInterval)

	return h, nil
}

// serveHTTPUntil runs an http.Server bound to l, shutting it down
// cleanly when ctx is cancelled. Logs unexpected listener errors
// but treats Shutdown's ErrServerClosed as the happy path.
func serveHTTPUntil(ctx context.Context, l net.Listener, handler http.Handler, label string) {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
		log.Printf("heartbeat %s: %v", label, err)
	}
}

// runHeartbeatSnapshots emits one record per known service every
// interval, each at app=<namespace>/<service>/heartbeats. Empty
// snapshots produce no rows (and don't open a stream), so a quiet
// host costs nothing on the wire.
func runHeartbeatSnapshots(ctx context.Context, client pb.WeftCoordinatorClient,
	h *heartbeat.Heartbeats, customer, instance, namespace string, interval time.Duration) {

	ss := &streamState{}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			for _, s := range h.Snapshot() {
				payload, err := json.Marshal(map[string]any{
					"type":      "heartbeat",
					"ts":        now.UTC().Format(time.RFC3339Nano),
					"service":   s.Service,
					"last_seen": s.LastSeen.UTC().Format(time.RFC3339Nano),
					"age_s":     round2(s.Age.Seconds()),
				})
				if err != nil {
					log.Printf("heartbeat: marshal: %v", err)
					continue
				}
				app := namespace + "/" + s.Service + "/heartbeats"
				ns, svc, kind := splitFullApp(app)
				row := &pb.LogRow{
					Namespace:   ns,
					Service:     svc,
					Kind:        kind,
					Customer:    customer,
					Instance:    instance,
					Raw:         string(payload),
					Level:       "INFO",
					TsUnixNs:    now.UnixNano(),
					PayloadJson: payload,
				}
				if err := sendRowWithReconnect(ctx, client, ss, row, app); err != nil {
					return // ctx cancelled
				}
			}
		}
	}
}
