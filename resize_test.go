//go:build unix

package readline

import (
	"errors"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/reeflective/readline/internal/core"
)

// pipeShell is a Readline() call driven from a pipe instead of a terminal.
type pipeShell struct {
	writer    *os.File
	refreshes *atomic.Int32
	line      string
	err       chan error
	finished  chan struct{}
}

// startPipeShell runs Readline() on a pipe-backed stdin, discards its rendered
// frames, and counts its refreshes through a refresh hook.
func startPipeShell(t *testing.T, rl *Shell) *pipeShell {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening the stdin pipe: %v", err)
	}

	origStdin, origCoreStdin := os.Stdin, core.Stdin
	os.Stdin, core.Stdin = reader, reader

	SetRenderOutput(io.Discard)

	// Keep the echoed control character out of the test output.
	if err := rl.Config.Set("echo-control-characters", false); err != nil {
		t.Fatalf("setting echo-control-characters: %v", err)
	}

	shell := &pipeShell{
		writer:   writer,
		err:      make(chan error, 1),
		finished: make(chan struct{}),
	}

	var refreshes atomic.Int32
	shell.refreshes = &refreshes
	rl.SetRefreshHook(func() { refreshes.Add(1) })

	go func() {
		shell.line, err = rl.Readline()
		shell.err <- err
		close(shell.finished)
	}()

	t.Cleanup(func() {
		_ = writer.Close()

		select {
		case <-shell.finished:
		case <-time.After(5 * time.Second):
			t.Error("Readline did not return after its stdin was closed")
		}

		os.Stdin, core.Stdin = origStdin, origCoreStdin
		SetRenderOutput(nil)
	})

	return shell
}

// waitRefreshes waits until the display has refreshed at least want times.
func waitRefreshes(t *testing.T, refreshes *atomic.Int32, want int32) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for refreshes.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("display refreshed %d time(s), want at least %d", refreshes.Load(), want)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// typeString writes keys to the running read as if the user typed them.
func (sh *pipeShell) typeString(t *testing.T, keys string) {
	t.Helper()

	if _, err := sh.writer.WriteString(keys); err != nil {
		t.Fatalf("writing keys %q: %v", keys, err)
	}
}

// accept types Enter and returns the line the read accepted.
func (sh *pipeShell) accept(t *testing.T) string {
	t.Helper()

	sh.typeString(t, "\r")

	select {
	case err := <-sh.err:
		if err != nil {
			t.Fatalf("Readline error = %v, want nil", err)
		}

		return sh.line
	case <-time.After(5 * time.Second):
		t.Fatal("Readline did not return after Enter")
	}

	return ""
}

// end interrupts the running read with Ctrl-C and checks it returns.
func (sh *pipeShell) end(t *testing.T) {
	t.Helper()

	if _, err := sh.writer.Write([]byte{0x03}); err != nil {
		t.Fatalf("writing Ctrl-C: %v", err)
	}

	select {
	case err := <-sh.err:
		if !errors.Is(err, ErrInterrupt) {
			t.Fatalf("Readline error = %v, want ErrInterrupt", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Readline did not return after Ctrl-C")
	}
}

// TestReadlineRefreshesOnResize covers the default: Readline() watches for
// terminal resizes and refreshes its own display when one arrives.
func TestReadlineRefreshesOnResize(t *testing.T) {
	rl := NewShell()

	if !rl.Config.GetBool("watch-resize") {
		t.Fatal("watch-resize should default to true")
	}

	sh := startPipeShell(t, rl)
	waitRefreshes(t, sh.refreshes, 1)

	before := sh.refreshes.Load()

	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("sending SIGWINCH: %v", err)
	}

	waitRefreshes(t, sh.refreshes, before+1)
	sh.end(t)
}

// TestReadlineResizeWatchCanBeDisabled covers the opt-out: with watch-resize
// off, a host that redraws on its own gets no editor refresh racing it.
func TestReadlineResizeWatchCanBeDisabled(t *testing.T) {
	rl := NewShell()

	if err := rl.Config.Set("watch-resize", false); err != nil {
		t.Fatalf("setting watch-resize: %v", err)
	}

	sh := startPipeShell(t, rl)
	waitRefreshes(t, sh.refreshes, 1)

	before := sh.refreshes.Load()

	// Repeating the signal over a bounded window gives a wrongly installed
	// watcher every chance to wake the loop: a wake repaints immediately.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
			t.Fatalf("sending SIGWINCH: %v", err)
		}

		time.Sleep(25 * time.Millisecond)
	}

	if got := sh.refreshes.Load(); got != before {
		t.Fatalf("display refreshed %d time(s) on a resize with watch-resize off, want %d", got, before)
	}

	sh.end(t)
}
