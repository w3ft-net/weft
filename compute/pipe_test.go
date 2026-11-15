package compute

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestChunkPipe_DeliversWritesInOrderThenEOF(t *testing.T) {
	r, w := newChunkPipe(1 << 20)
	var want strings.Builder
	go func() {
		for i := 0; i < 5000; i++ {
			line := fmt.Sprintf("row %d\n", i)
			want.WriteString(line)
			if _, err := io.WriteString(w, line); err != nil {
				t.Error(err)
			}
		}
		w.Close()
	}()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want.String() {
		t.Errorf("read %d bytes that differ from the %d written", len(got), want.Len())
	}
}

// A write returns once its bytes are buffered; it does not wait for the reader.
func TestChunkPipe_WriteDoesNotWaitForTheReader(t *testing.T) {
	_, w := newChunkPipe(1 << 20)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			w.Write([]byte("0123456789\n"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writes blocked with no reader")
	}
}

// What was written is readable at once, not held until more arrives.
func TestChunkPipe_ReadSeesAWriteImmediately(t *testing.T) {
	r, w := newChunkPipe(1 << 20)
	go w.Write([]byte("first\n"))
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := r.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if s != "first\n" {
			t.Errorf("read %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read waited for more than one write")
	}
}

// A full pipe holds the writer until the reader takes some.
func TestChunkPipe_FullPipeBlocksTheWriterUntilRead(t *testing.T) {
	r, w := newChunkPipe(16)
	wrote := make(chan struct{})
	go func() {
		w.Write(bytes.Repeat([]byte("x"), 64))
		close(wrote)
	}()
	select {
	case <-wrote:
		t.Fatal("a write past the limit returned with nothing read")
	case <-time.After(100 * time.Millisecond):
	}
	got := make([]byte, 0, 64)
	buf := make([]byte, 8)
	for len(got) < 64 {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer was never released")
	}
}

// A reader that stops (head) fails the writes, blocked or not.
func TestChunkPipe_ClosingTheReaderFailsTheWriter(t *testing.T) {
	r, w := newChunkPipe(16)
	errc := make(chan error, 1)
	go func() {
		_, err := w.Write(bytes.Repeat([]byte("x"), 1024))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond) // let it block on the full pipe
	r.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("blocked write after the reader closed: %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked write was not released by the reader closing")
	}
	if _, err := w.Write([]byte("more")); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("write after the reader closed: %v", err)
	}
}

// The reader gets everything written before the writer closed, then the error.
func TestChunkPipe_CloseWithErrorAfterTheData(t *testing.T) {
	r, w := newChunkPipe(1 << 20)
	w.Write([]byte("abc"))
	boom := errors.New("boom")
	w.CloseWithError(boom)
	got, err := io.ReadAll(r)
	if string(got) != "abc" || !errors.Is(err, boom) {
		t.Errorf("read %q, %v; want abc then boom", got, err)
	}
}

func TestChunkPipe_ConcurrentProducerAndConsumer(t *testing.T) {
	r, w := newChunkPipe(4096)
	const n = 200000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "%d\n", i)
		}
		w.Close()
	}()
	var count int
	buf := make([]byte, 1000)
	var tail []byte
	for {
		k, err := r.Read(buf)
		tail = append(tail, buf[:k]...)
		for {
			i := bytes.IndexByte(tail, '\n')
			if i < 0 {
				break
			}
			if string(tail[:i]) != fmt.Sprint(count) {
				t.Fatalf("line %d = %q", count, tail[:i])
			}
			count++
			tail = tail[i+1:]
		}
		if err != nil {
			break
		}
	}
	wg.Wait()
	if count != n {
		t.Errorf("%d lines, want %d", count, n)
	}
}
