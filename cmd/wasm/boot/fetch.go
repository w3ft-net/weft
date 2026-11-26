//go:build js && wasm

package boot

import (
	"context"
	"errors"
	"io"
	"syscall/js"

	"github.com/w3ft-net/weft/shell"
)

// fetchTransport is shell.ExecTransport over the browser's fetch. It carries
// no net/http, which is several megabytes of wasm for one POST. Cancelling the
// request's context aborts the fetch.
type fetchTransport struct{}

var errUnreachable = errors.New("cannot reach API server")

func (fetchTransport) Post(ctx context.Context, url string, header map[string]string, body []byte) (*shell.ExecResponse, error) {
	ac := js.Global().Get("AbortController").New()
	stop := context.AfterFunc(ctx, func() { ac.Call("abort") })

	hdrs := js.Global().Get("Object").New()
	for k, v := range header {
		hdrs.Set(k, v)
	}
	opts := js.Global().Get("Object").New()
	opts.Set("method", "POST")
	opts.Set("headers", hdrs)
	opts.Set("body", string(body))
	opts.Set("signal", ac.Get("signal"))

	resp, err := await(ctx, js.Global().Call("fetch", url, opts))
	if err != nil {
		stop()
		return nil, err
	}
	return &shell.ExecResponse{
		Status: resp.Get("status").Int(),
		Body:   &fetchBody{reader: resp.Get("body").Call("getReader"), ctx: ctx, stop: stop},
	}, nil
}

// fetchBody reads a response body a chunk at a time off a ReadableStream.
type fetchBody struct {
	reader js.Value
	ctx    context.Context
	stop   func() bool
	buf    []byte
	done   bool
}

func (b *fetchBody) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		if b.done {
			return 0, io.EOF
		}
		chunk, err := await(b.ctx, b.reader.Call("read"))
		if err != nil {
			return 0, err
		}
		if chunk.Get("done").Bool() {
			b.done = true
			continue
		}
		v := chunk.Get("value")
		b.buf = make([]byte, v.Get("length").Int())
		js.CopyBytesToGo(b.buf, v)
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

func (b *fetchBody) Close() error {
	b.stop()
	b.reader.Call("cancel").Call("catch", js.FuncOf(func(js.Value, []js.Value) any { return nil }))
	return nil
}

// await blocks the calling goroutine on a Promise. A rejection that follows
// ctx's cancellation is reported as ctx's error; any other is "cannot reach",
// which is what a failed fetch means to a user.
func await(ctx context.Context, p js.Value) (js.Value, error) {
	type result struct {
		v   js.Value
		err error
	}
	ch := make(chan result, 1)
	ok := js.FuncOf(func(_ js.Value, a []js.Value) any { ch <- result{v: a[0]}; return nil })
	bad := js.FuncOf(func(_ js.Value, a []js.Value) any {
		if ctx.Err() != nil {
			ch <- result{err: ctx.Err()}
		} else {
			ch <- result{err: errUnreachable}
		}
		return nil
	})
	defer ok.Release()
	defer bad.Release()
	p.Call("then", ok, bad)
	r := <-ch
	return r.v, r.err
}
