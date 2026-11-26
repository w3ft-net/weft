//go:build !(js && wasm)

package shell

import (
	"context"
	"errors"
	"net/http"
)

// remoteClient is the client every call to the weft API goes through. net/http
// reports a failure as `Post "https://host/path": context deadline exceeded
// (Client.Timeout exceeded while awaiting headers)`, which names the server
// and says what the client is written in, and tells the user nothing to do.
// remoteClient replaces that text with a message for the user and keeps the
// original reachable through errors.Is and errors.As.
type remoteClient struct{ *http.Client }

func (c remoteClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, &transportError{err}
	}
	return resp, nil
}

type transportError struct{ err error }

func (e *transportError) Unwrap() error { return e.err }

func (e *transportError) Error() string {
	switch {
	case errors.Is(e.err, context.Canceled):
		return "request canceled"
	case isTimeout(e.err):
		return "request timed out"
	default:
		return "could not reach weft — check your network connection"
	}
}
