package shard

import (
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A shard refuses a call it isn't in a position to serve with one of two
// statuses, both raised before the handler runs, so a refused call was
// never applied and is always safe to retry (docs/plans/
// shard-seamless-upgrade.md):
//
//   - paused: the shard is handing its volume to a successor instance and
//     names that instance's address; the caller retries there.
//   - not ready: a warm-pool spare with no volume yet, or a successor still
//     mounting the volume and opening DuckDB; the caller holds and retries.
//
// The status code is Unavailable (the call may succeed later); the reason
// rides in an ErrorInfo detail so a caller can tell a refusal from a
// transport failure.
const (
	refusalDomain  = "weft.shard"
	reasonPaused   = "SHARD_PAUSED"
	reasonNotReady = "SHARD_NOT_READY"
	successorMDKey = "successor"
)

// Refusal classifies an error returned by a shard call.
type Refusal int

const (
	NotRefused Refusal = iota
	RefusedPaused
	RefusedNotReady
)

// PausedError is the status a paused shard refuses a call with.
func PausedError(successor string) error {
	return refusalError("shard paused: handing over to "+successor, reasonPaused, map[string]string{successorMDKey: successor})
}

// NotReadyError is the status a shard with no open database refuses a call
// with.
func NotReadyError() error {
	return refusalError("shard not ready: no volume mounted", reasonNotReady, nil)
}

func refusalError(msg, reason string, md map[string]string) error {
	st, err := status.New(codes.Unavailable, msg).WithDetails(&errdetails.ErrorInfo{
		Reason:   reason,
		Domain:   refusalDomain,
		Metadata: md,
	})
	if err != nil {
		// WithDetails fails only on a detail that can't be marshalled, which
		// ErrorInfo always can.
		panic(err)
	}
	return st.Err()
}

// ParseRefusal reports whether err is a shard refusal and, for a paused
// shard, the successor address it named. Wrapped errors are unwrapped.
func ParseRefusal(err error) (Refusal, string) {
	if err == nil {
		return NotRefused, ""
	}
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) {
		return NotRefused, ""
	}
	for _, d := range se.GRPCStatus().Details() {
		info, ok := d.(*errdetails.ErrorInfo)
		if !ok || info.Domain != refusalDomain {
			continue
		}
		switch info.Reason {
		case reasonPaused:
			return RefusedPaused, info.Metadata[successorMDKey]
		case reasonNotReady:
			return RefusedNotReady, ""
		}
	}
	return NotRefused, ""
}
