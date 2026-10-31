package compute

import (
	"io"
	"sync"
)

// pipeLimit is how much a stage's stdin or stdout pipe holds before its writer
// waits: enough to batch a stage's rows, and a bound on what a slow reader
// makes the writer keep.
const pipeLimit = 1 << 20

var newline = []byte{'\n'}

// chunkPipe is an in-memory pipe that buffers, as an OS pipe does: a write
// returns once its bytes are in the buffer (it waits only when the buffer is
// full), and a read takes whatever is there. An io.Pipe is unbuffered, so every
// write is a handoff to the reader and a row at a time through a stage costs a
// pair of goroutine switches per row; here the reader takes a batch whenever
// the writer has got ahead of it, and a write is seen at once when it hasn't.
type chunkPipe struct {
	mu       sync.Mutex
	notEmpty sync.Cond // data arrived, or the writer closed
	notFull  sync.Cond // data was read, or the reader closed
	buf      []byte
	off      int // buf[off:] is what's unread
	limit    int
	werr     error // set when the writer closed: io.EOF, or its error
	rclosed  bool
}

type chunkReader struct{ p *chunkPipe }
type chunkWriter struct{ p *chunkPipe }

// newChunkPipe returns the two ends of a pipe that holds up to limit bytes.
func newChunkPipe(limit int) (*chunkReader, *chunkWriter) {
	p := &chunkPipe{limit: limit}
	p.notEmpty.L, p.notFull.L = &p.mu, &p.mu
	return &chunkReader{p}, &chunkWriter{p}
}

// Read waits for data (or the writer to close), then copies what is there.
func (r *chunkReader) Read(b []byte) (int, error) {
	p := r.p
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == p.off {
		switch {
		case p.werr != nil:
			return 0, p.werr
		case p.rclosed:
			return 0, io.ErrClosedPipe
		}
		p.notEmpty.Wait()
	}
	n := copy(b, p.buf[p.off:])
	p.off += n
	switch {
	case p.off == len(p.buf):
		p.buf, p.off = p.buf[:0], 0
	case p.off > len(p.buf)/2:
		p.buf = p.buf[:copy(p.buf, p.buf[p.off:])]
		p.off = 0
	}
	p.notFull.Signal()
	return n, nil
}

// Close stops the reader: pending and later writes fail with io.ErrClosedPipe.
func (r *chunkReader) Close() error {
	p := r.p
	p.mu.Lock()
	p.rclosed = true
	p.notFull.Broadcast()
	p.notEmpty.Broadcast()
	p.mu.Unlock()
	return nil
}

// Write buffers b, waiting for room if the pipe is full.
func (w *chunkWriter) Write(b []byte) (int, error) {
	p := w.p
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for len(b) > 0 {
		if p.rclosed || p.werr != nil {
			return n, io.ErrClosedPipe
		}
		room := p.limit - (len(p.buf) - p.off)
		if room <= 0 {
			p.notFull.Wait()
			continue
		}
		k := min(room, len(b))
		p.buf = append(p.buf, b[:k]...)
		b, n = b[k:], n+k
		p.notEmpty.Signal()
	}
	return n, nil
}

// Close ends the stream: the reader gets what was written, then io.EOF.
func (w *chunkWriter) Close() error { return w.CloseWithError(nil) }

// CloseWithError ends the stream with err (io.EOF if nil) for the reader.
func (w *chunkWriter) CloseWithError(err error) error {
	if err == nil {
		err = io.EOF
	}
	p := w.p
	p.mu.Lock()
	if p.werr == nil {
		p.werr = err
	}
	p.notEmpty.Broadcast()
	p.notFull.Broadcast()
	p.mu.Unlock()
	return nil
}
