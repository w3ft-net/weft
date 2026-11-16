// Package grpckeepalive is how weft's gRPC clients and servers tell a dead
// connection from a quiet one. A TCP connection whose path changes under it (a
// middlebox replaced, a load balancer's flow table lost) looks healthy to
// both ends until the kernel's retransmission timeout, some fifteen minutes
// later, so a client that only waits for an RPC to fail stalls for that long.
// Pinging is the only way to notice sooner, and it only works if the server
// accepts the pings: gRPC servers refuse a client that pings more often than
// their enforcement policy allows, with GOAWAY too_many_pings. Both halves live
// here so they cannot drift apart.
package grpckeepalive

import (
	"time"

	"google.golang.org/grpc/keepalive"
)

// Client is what a long-lived client (weftd) pings with: a probe after Time of
// quiet, and the connection closed if Timeout passes without a reply, so a
// silently dead one is noticed within Time+Timeout. PermitWithoutStream probes
// an idle connection too.
var Client = keepalive.ClientParameters{
	Time:                30 * time.Second,
	Timeout:             10 * time.Second,
	PermitWithoutStream: true,
}

// ServerEnforcement is the ping rate every coordinator gRPC server accepts. It
// is below Client.Time, with room for a client that retries after a timeout.
var ServerEnforcement = keepalive.EnforcementPolicy{
	MinTime:             20 * time.Second,
	PermitWithoutStream: true,
}
