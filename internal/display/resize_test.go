//go:build unix

package display

import (
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/reeflective/readline/inputrc"
	"github.com/reeflective/readline/internal/completion"
	"github.com/reeflective/readline/internal/core"
)

// parkWait starts an input wait and returns a channel closed when it returns.
// The wait parks in the poll on the stubbed stdin, so only a resize wake or
// real input can bring it back.
func parkWait(keys *core.Keys) <-chan struct{} {
	returned := make(chan struct{})

	go func() {
		core.WaitAvailableKeys(keys, &inputrc.Config{})
		close(returned)
	}()

	return returned
}

// sendResize delivers a terminal resize signal to this process.
func sendResize(t *testing.T) {
	t.Helper()

	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("sending SIGWINCH: %v", err)
	}
}

// waitGoroutines polls until the goroutine count is back to at most want.
func waitGoroutines(t *testing.T, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		if runtime.NumGoroutine() <= want {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("watcher goroutine leaked: %d goroutines, want at most %d", runtime.NumGoroutine(), want)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// settleGoroutines waits until the goroutine count stops moving, so a baseline
// can be taken after the os/signal machinery has spun up.
func settleGoroutines(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	last := -1

	for {
		current := runtime.NumGoroutine()
		if current == last || time.Now().After(deadline) {
			return
		}

		last = current
		time.Sleep(20 * time.Millisecond)
	}
}

// newResizeTestEngine returns a display engine whose resize watcher can run
// against a wake-enabled key reader.
func newResizeTestEngine(t *testing.T, keys *core.Keys) *Engine {
	t.Helper()

	cfg := &inputrc.Config{}

	return NewEngine(keys, nil, nil, nil, nil, completion.NewEngine(nil, nil, cfg), cfg)
}

// TestWatchResizeStopsOnClose covers both sides of the resize watcher: a resize
// signal wakes the parked input loop, and closing the returned channel stops
// the watcher goroutine, so later resizes no longer reach the editor.
func TestWatchResizeStopsOnClose(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("opening the stdin pipe: %v", err)
	}

	original := core.Stdin
	core.Stdin = reader

	t.Cleanup(func() {
		core.Stdin = original
		_ = reader.Close()
		_ = writer.Close()
	})

	keys := new(core.Keys)
	keys.InitWake()

	t.Cleanup(keys.CloseWake)

	eng := newResizeTestEngine(t, keys)

	// Warm the goroutine the signal package starts on the first Notify (it is
	// never stopped), and settle the baseline after it, so it cannot be
	// mistaken for a leak below.
	warm := WatchResize(eng)
	close(warm)
	settleGoroutines(t)

	baseline := runtime.NumGoroutine()

	resize := WatchResize(eng)

	// A resize event wakes the parked wait so the loop repaints.
	waited := parkWait(keys)
	sendResize(t)

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("a resize event did not wake the input loop")
	}

	// Closing the watcher's channel must end its goroutine.
	close(resize)
	waitGoroutines(t, baseline)

	waited = parkWait(keys)
	sendResize(t)

	select {
	case <-waited:
		t.Fatal("a resize event still woke the input loop after WatchResize was closed")
	case <-time.After(200 * time.Millisecond):
	}

	// The wait was only parked, not broken: real input still reaches it.
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatalf("writing input: %v", err)
	}

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("real input did not wake the input loop")
	}

	if key, empty := keys.Pop(); empty || key != 'x' {
		t.Fatalf("read key = %q (empty: %t), want %q", key, empty, 'x')
	}
}

// TestWatchResizeUnregistersItsSignalChannel checks the other half of the
// close: the watcher's signal subscription is removed, so a Readline() call
// does not leave a channel registered with os/signal behind.
func TestWatchResizeUnregistersItsSignalChannel(t *testing.T) {
	var registered, unregistered atomic.Int32

	notify, stop := signalNotify, signalStop

	signalNotify = func(c chan<- os.Signal, sig ...os.Signal) {
		registered.Add(1)
		notify(c, sig...)
	}

	signalStop = func(c chan<- os.Signal) {
		unregistered.Add(1)
		stop(c)
	}

	t.Cleanup(func() { signalNotify, signalStop = notify, stop })

	keys := new(core.Keys)
	keys.InitWake()

	t.Cleanup(keys.CloseWake)

	done := WatchResize(newResizeTestEngine(t, keys))
	close(done)

	deadline := time.Now().Add(5 * time.Second)

	for {
		if registered.Load() == 1 && unregistered.Load() == 1 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("watcher registered %d channel(s) and unregistered %d, want 1 and 1", registered.Load(), unregistered.Load())
		}

		time.Sleep(5 * time.Millisecond)
	}
}
