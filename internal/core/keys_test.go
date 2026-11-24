package core

import (
	"bytes"
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

func TestWaitAvailableKeysIgnoresUnsolicitedCursorReplies(t *testing.T) {
	withStubStdin(t,
		readStep{data: []byte("\x1b[12;8R")},
		readStep{data: []byte("x\x1b[12;8R\x03")},
	)
	keys := &Keys{}
	done := make(chan struct{})
	go func() {
		WaitAvailableKeys(keys, &inputrc.Config{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a cursor reply blocked the key reader")
	}
	for _, want := range []byte{'x', 3} {
		if got, empty := keys.Pop(); empty || got != want {
			t.Fatalf("got key %q (empty: %t), want %q", got, empty, want)
		}
	}
	if got, empty := keys.Pop(); !empty {
		t.Fatalf("unexpected key after Ctrl-C: %q", got)
	}
}

func TestWaitAvailableKeysDoesNotWaitForTheCursorQueryToDrain(t *testing.T) {
	withStubStdin(t,
		readStep{data: []byte("\x1b[12;8R")},
		readStep{data: []byte("\x1b[12;8R\x03")},
	)
	replies := make(chan []byte, 1)
	keys := &Keys{cursor: replies}
	done := make(chan struct{})
	go func() {
		WaitAvailableKeys(keys, &inputrc.Config{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a duplicate reply blocked the key reader")
	}
	select {
	case reply := <-replies:
		if got := string(reply); got != "\x1b[12;8R" {
			t.Fatalf("unexpected cursor reply %q", got)
		}
	default:
		t.Fatal("the cursor query did not receive its reply")
	}
	if got, empty := keys.Pop(); empty || got != 3 {
		t.Fatalf("Ctrl-C did not reach the key reader: got %q (empty: %t)", got, empty)
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

// readFiltered reads and filters every chunk the stub stdin offers, until it
// reports EOF, and returns the keys that survived the terminal-reply filter.
func readFiltered(t *testing.T, keys *Keys) []byte {
	t.Helper()

	var got []byte

	for {
		chunk, err := keys.readInputFiltered()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return got
			}

			t.Fatalf("reading input: %v", err)
		}

		got = append(got, chunk...)
	}
}

// assertPendingOSC reports whether the reader is holding an incomplete OSC reply.
func assertPendingOSC(t *testing.T, keys *Keys, want bool) {
	t.Helper()

	if got := len(keys.pendingOSC) > 0; got != want {
		t.Fatalf("pending OSC hold = %t (held %q), want %t", got, keys.pendingOSC, want)
	}
}

// TestFilterStripsBELTerminatedOSCReply covers the stray OSC 11 background-color
// reply a terminal sends with a BEL terminator: it must not reach the key
// buffer, and the keys around it must.
func TestFilterStripsBELTerminatedOSCReply(t *testing.T) {
	withStubStdin(t, readStep{data: []byte("\x1b]11;rgb:2424/2323/2323\x07ab")})

	keys := &Keys{}

	if got := string(readFiltered(t, keys)); got != "ab" {
		t.Fatalf("filtered keys = %q, want %q", got, "ab")
	}
}

// TestFilterStripsSTTerminatedOSCReply covers the same reply with a ST
// (ESC \) terminator, sandwiched between real keys.
func TestFilterStripsSTTerminatedOSCReply(t *testing.T) {
	withStubStdin(t, readStep{data: []byte("a\x1b]11;rgb:2424/2323/2323\x1b\\b")})

	keys := &Keys{}

	if got := string(readFiltered(t, keys)); got != "ab" {
		t.Fatalf("filtered keys = %q, want %q", got, "ab")
	}
}

// TestFilterStripsOSCAndForwardsCursorReply checks that an OSC reply and a
// cursor-position reply arriving in the same read are both stripped, while the
// cursor reply still reaches a pending cursor query.
func TestFilterStripsOSCAndForwardsCursorReply(t *testing.T) {
	withStubStdin(t, readStep{data: []byte("\x1b]11;rgb:2424/2323/2323\x1b\\\x1b[24;1R")})

	replies := make(chan []byte, 1)
	keys := &Keys{cursor: replies}

	if got := readFiltered(t, keys); len(got) != 0 {
		t.Fatalf("filtered keys = %q, want none", got)
	}

	select {
	case reply := <-replies:
		if got := string(reply); got != "\x1b[24;1R" {
			t.Fatalf("cursor reply = %q, want %q", got, "\x1b[24;1R")
		}
	default:
		t.Fatal("the cursor query did not receive its reply")
	}
}

// TestFilterStripsSplitOSCReply covers a reply split across reads: mid-payload,
// right after the ESC of a ST terminator, and with the trailing key arriving
// in a later chunk.
func TestFilterStripsSplitOSCReply(t *testing.T) {
	withStubStdin(t,
		readStep{data: []byte("a\x1b]11;rgb:24")},
		readStep{data: []byte("24/2323")},
		readStep{data: []byte("\x1b")},
		readStep{data: []byte("\\b")},
	)

	keys := &Keys{}

	if got := string(readFiltered(t, keys)); got != "ab" {
		t.Fatalf("filtered keys = %q, want %q", got, "ab")
	}

	assertPendingOSC(t, keys, false)
}

// TestFilterNeverHoldsBareEscape covers the common case of a lone Escape key at
// the end of a read: only the two-byte OSC introducer may start a hold.
func TestFilterNeverHoldsBareEscape(t *testing.T) {
	withStubStdin(t,
		readStep{data: []byte("a\x1b")},
		readStep{data: []byte("b")},
	)

	keys := &Keys{}

	if got := string(readFiltered(t, keys)); got != "a\x1bb" {
		t.Fatalf("filtered keys = %q, want %q", got, "a\x1bb")
	}

	assertPendingOSC(t, keys, false)
}

// TestFilterAbortsUnterminatedOSCOnControlByte checks that an unterminated OSC
// reply interrupted by Enter or Ctrl-C stops being held, so the user's input is
// passed through instead of being swallowed.
func TestFilterAbortsUnterminatedOSCOnControlByte(t *testing.T) {
	for _, control := range []byte{'\r', '\n', 0x03, 0x04} {
		input := append([]byte("\x1b]11;rgb:2424"), control)

		withStubStdin(t, readStep{data: input})

		keys := &Keys{}

		if got := readFiltered(t, keys); string(got) != string(input) {
			t.Fatalf("control byte %q: filtered keys = %q, want %q", control, got, input)
		}

		assertPendingOSC(t, keys, false)
	}
}

// TestFilterAbortsOSCExceedingItsCap checks that a sequence that never
// terminates is released instead of being held forever.
func TestFilterAbortsOSCExceedingItsCap(t *testing.T) {
	input := append([]byte("\x1b]11;rgb:"), bytes.Repeat([]byte("2"), maxPendingOSC+10)...)

	withStubStdin(t, readStep{data: input})

	keys := &Keys{}

	if got := readFiltered(t, keys); string(got) != string(input) {
		t.Fatalf("filtered keys = %q, want %q", got, input)
	}

	assertPendingOSC(t, keys, false)
}

// TestFilterReleasesHeldOSCOnceItGrowsTooLong checks the cap across reads: a
// hold that exceeds the bound on the next chunk is released, not dropped.
func TestFilterReleasesHeldOSCOnceItGrowsTooLong(t *testing.T) {
	first := append([]byte("\x1b]11;rgb:"), bytes.Repeat([]byte("2"), maxPendingOSC-16)...)
	second := bytes.Repeat([]byte("3"), 32)

	withStubStdin(t,
		readStep{data: first},
		readStep{data: second},
	)

	keys := &Keys{}
	got := readFiltered(t, keys)

	if want := string(first) + string(second); string(got) != want {
		t.Fatalf("filtered keys = %q, want %q", got, want)
	}

	assertPendingOSC(t, keys, false)
}

// TestFilterKeepsPendingOSCResetOnAbort checks that an aborted read leaves no
// held bytes behind for a later read.
func TestFilterKeepsPendingOSCResetOnAbort(t *testing.T) {
	withStubStdin(t, readStep{data: []byte("\x1b]11;rgb:24")})

	keys := &Keys{}
	keys.readInputFiltered()

	assertPendingOSC(t, keys, true)

	keys.Abort()

	assertPendingOSC(t, keys, false)
}

// TestFilterPreservesMultibyteUTF8 checks that the filter is byte-exact for
// text: OSC-like bytes inside UTF-8 sequences (0x9c/0x9d are continuation
// bytes) and normal escape-prefixed keys must pass through untouched.
func TestFilterPreservesMultibyteUTF8(t *testing.T) {
	for _, input := range []string{
		"💩❯Ü",
		"\x9d11;rgb:2424/2323/2323\x9c",
		"\x1b[A\x1bb",
	} {
		withStubStdin(t, readStep{data: []byte(input)})

		keys := &Keys{}

		if got := string(readFiltered(t, keys)); got != input {
			t.Fatalf("filtered keys = %q, want %q", got, input)
		}

		assertPendingOSC(t, keys, false)
	}
}
