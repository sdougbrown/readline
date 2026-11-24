//go:build unix
// +build unix

package display

import (
	"os"
	"os/signal"
	"syscall"
)

// signalNotify and signalStop are the os/signal primitives the watcher uses.
// They are variables so tests can assert that a closed watcher unregisters its
// channel instead of leaving a subscription behind.
var (
	signalNotify = signal.Notify
	signalStop   = signal.Stop
)

// WatchResize redisplays the interface on terminal resize events.
func WatchResize(eng *Engine) chan<- bool {
	done := make(chan bool, 1)

	resizeChannel := make(chan os.Signal, 1)
	signalNotify(resizeChannel, syscall.SIGWINCH)

	go func() {
		// Unregister the channel when the watcher ends, so each Readline()
		// call leaves no signal subscription behind.
		defer signalStop(resizeChannel)

		for {
			select {
			case <-resizeChannel:
				// Route the regeneration + repaint through the input wake so
				// they run on the Readline goroutine, instead of mutating the
				// completion/display state and writing to stdout from here
				// (which races with the main loop).
				eng.completer.RequestRegen()
				eng.keys.RequestRefresh()
			case <-done:
				return
			}
		}
	}()

	return done
}
