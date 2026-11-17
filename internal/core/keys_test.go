package core

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/reeflective/readline/inputrc"
)

// errReadFailed is a static stand-in for a non-EOF input failure in tests.
var errReadFailed = errors.New("read failed")

// readStep is one scripted result of a stub stdin Read.
type readStep struct {
	data []byte
	err  error
}

// stubReadCloser plays back a fixed sequence of Read results, then EOF. It is
// not an *os.File, so the Unix key reader's poll path is skipped and reads fall
// straight through to this stub.
type stubReadCloser struct {
	steps []readStep
	index int
}

func (s *stubReadCloser) Read(p []byte) (int, error) {
	if s.index >= len(s.steps) {
		return 0, io.EOF
	}

	step := s.steps[s.index]
	s.index++

	n := copy(p, step.data)

	return n, step.err
}

func (s *stubReadCloser) Close() error { return nil }

// withStubStdin swaps the package stdin for the duration of a test.
func withStubStdin(t *testing.T, steps ...readStep) {
	t.Helper()

	original := Stdin
	Stdin = &stubReadCloser{steps: steps}

	t.Cleanup(func() { Stdin = original })
}

// TestWaitAvailableKeysMarksEOF verifies a closed stream is recorded as EOF and
// is not mistaken for a read error.
func TestWaitAvailableKeysMarksEOF(t *testing.T) {
	withStubStdin(t, readStep{err: io.EOF})

	keys := &Keys{}
	WaitAvailableKeys(keys, &inputrc.Config{})

	if !keys.IsEOF() {
		t.Fatal("expected EOF to be recorded")
	}

	if err := keys.ReadError(); err != nil {
		t.Fatalf("expected no read error on EOF, got %v", err)
	}
}

// TestWaitAvailableKeysRecordsNonEOFReadError verifies a non-EOF failure (e.g. a
// revoked tty) is surfaced via ReadError instead of being swallowed, which is
// what previously left the input loop spinning.
func TestWaitAvailableKeysRecordsNonEOFReadError(t *testing.T) {
	want := errReadFailed

	withStubStdin(t, readStep{err: want})

	keys := &Keys{}
	WaitAvailableKeys(keys, &inputrc.Config{})

	if keys.IsEOF() {
		t.Fatal("non-EOF failure must not be marked as EOF")
	}

	if err := keys.ReadError(); !errors.Is(err, want) {
		t.Fatalf("expected read error %v, got %v", want, err)
	}
}

// TestReadKeyReturnsAbortOnEOF verifies ReadKey aborts cleanly on EOF rather
// than indexing into an empty buffer and panicking (Vim f/F/t/T motions).
func TestReadKeyReturnsAbortOnEOF(t *testing.T) {
	withStubStdin(t, readStep{err: io.EOF})

	keys := &Keys{}

	key, isAbort := keys.ReadKey()

	if !isAbort {
		t.Fatal("expected ReadKey to abort on EOF")
	}

	if key != 0 {
		t.Fatalf("expected zero key on EOF, got %q", key)
	}
}

// TestReadKeySkipsEmptyReads verifies ReadKey keeps waiting past an empty read
// and returns the next real key.
func TestReadKeySkipsEmptyReads(t *testing.T) {
	withStubStdin(t, readStep{}, readStep{data: []byte("x")})

	keys := &Keys{}

	key, isAbort := keys.ReadKey()

	if isAbort {
		t.Fatal("expected ReadKey to continue after an empty read")
	}

	if key != 'x' {
		t.Fatalf("expected key %q, got %q", 'x', key)
	}
}

// TestKeysAbortEndsAParkedWait covers the shutdown race: a Keys whose wait
// loop is parked in the poll must leave the park when Abort says the stream
// is over, and the wait that follows reads as ended instead of parking again.
// A reader that stays parked holds the terminal and reads the next shell's
// keystrokes.
func TestKeysAbortEndsAParkedWait(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening the pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	original := Stdin
	Stdin = reader
	t.Cleanup(func() { Stdin = original })

	keys := &Keys{}
	keys.InitWake()
	t.Cleanup(func() { keys.CloseWake() })

	waited := make(chan struct{})
	go func() {
		WaitAvailableKeys(keys, &inputrc.Config{})
		close(waited)
	}()

	// The wait is parked: no keys were written, so only the abort can
	// bring it back.
	keys.Abort()

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the parked wait did not return after the abort")
	}

	if !keys.IsEOF() {
		t.Error("the aborted keys do not read as ended")
	}

	// A wait that starts after the abort reads as ended too, instead of
	// parking on a stream nobody writes again.
	done := make(chan struct{})
	go func() {
		WaitAvailableKeys(keys, &inputrc.Config{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a wait after the abort parked again")
	}
}
