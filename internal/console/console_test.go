package console

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// failingWriter always errors, standing in for a console whose device
// has gone away (EIO on /dev/tty0, a hung-up serial line).
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("EIO") }

// syncBuffer is a bytes.Buffer safe to read from the test goroutine
// while drain writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Len()
}

// TestDrain_SurvivesDeadDestination is the regression test for the
// deadlock drain replaces: with one destination erroring on every
// write, the pipe must still be read continuously and the other
// destination must still receive everything. The amount written is
// several times the pipe's 64 KiB capacity, so a drain that stopped
// on the first error would leave the writer blocked in write(2) and
// this test would hang (and be cut off by the deadline below).
func TestDrain_SurvivesDeadDestination(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	var good syncBuffer

	drained := make(chan struct{})

	go func() {
		drain(r, failingWriter{}, &good)
		close(drained)
	}()

	const total = 4 * 64 * 1024

	chunk := bytes.Repeat([]byte("x"), 4096)

	written := make(chan error, 1)

	go func() {
		for n := 0; n < total; n += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				written <- err

				return
			}
		}

		written <- w.Close()
	}()

	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("writing to pipe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer blocked: pipe was not drained past a failing destination")
	}

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after the pipe's write end closed")
	}

	if got := good.Len(); got != total {
		t.Fatalf("good destination received %d bytes, want %d", got, total)
	}
}

// TestDrain_ReturnsOnEOF: once every write end is closed, Read fails
// and drain returns rather than spinning.
func TestDrain_ReturnsOnEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	_ = w.Close()

	done := make(chan struct{})

	go func() {
		drain(r, io.Discard)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return on EOF")
	}
}
