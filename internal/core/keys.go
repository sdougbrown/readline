package core

import (
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"sync"

	"github.com/rivo/uniseg"

	"github.com/reeflective/readline/inputrc"
	"github.com/reeflective/readline/internal/strutil"
)

const (
	keyScanBufSize = 1024

	// maxPendingOSC bounds how many bytes of an unterminated OSC reply are held
	// across reads, so a terminal that never finishes one cannot hold up input.
	maxPendingOSC = 256
)

// Stdin is used by the Keys struct to read and write keys.
// It can be overwritten to use other file descriptors or
// custom io.Readers, such as the one used on Windows.
var Stdin io.ReadCloser = os.Stdin

var (
	rxRcvCursorPos = regexp.MustCompile(`\x1b\[([0-9]+);([0-9]+)R`)
	oscIntroducer  = []byte{0x1b, ']'}
)

// errInputWake is returned by the input read when it was interrupted by an
// async refresh request (RequestRefresh) rather than by actual key input.
var errInputWake = errors.New("readline: input wake")

// Keys is used to read, manage and use keys input by the shell user.
type Keys struct {
	buf       []byte      // Keys read and waiting to be used.
	matched   []rune      // Keys that have been successfully matched against a bind.
	macroKeys []rune      // Keys that have been fed by a macro.
	mustWait  bool        // Keys are in the stack, but we must still read stdin.
	waiting   bool        // Currently waiting for keys on stdin.
	reading   bool        // Currently reading keys out of the main loop.
	keysOnce  chan []byte // Passing keys from the main routine.
	cursor    chan []byte // Reply to a pending cursor query, or nil.
	resize    chan bool   //nolint:unused // Resize events on Windows are sent on stdin; consumed only by the windows build.

	// pendingOSC holds the bytes of an unterminated OSC reply from the
	// previous read, until the next chunk completes or rules it out.
	pendingOSC []byte

	wakeMu    sync.Mutex // Guards the wake fields against RequestRefresh (other goroutine).
	wakeR     int        // Read end of the async-refresh wake pipe.
	wakeW     int        // Write end of the async-refresh wake pipe.
	wakeReady bool       // Whether the wake pipe is initialized and usable.

	eof     bool            // EOF has been reached.
	readErr error           // First non-EOF, non-wake input failure (e.g. a revoked tty).
	cfg     *inputrc.Config // Configuration file used for meta key settings
	mutex   sync.RWMutex    // Concurrency safety
}

// WaitAvailableKeys waits until an input key is either read from standard input,
// or directly returns if the key stack still/already has available keys.
func WaitAvailableKeys(keys *Keys, cfg *inputrc.Config) {
	keys.cfg = cfg

	// An aborted stream reads as ended: the wait returns instead of
	// parking on input nobody will write.
	if keys.IsEOF() {
		return
	}

	// An aborted stream reads as ended: the wait returns instead of
	// parking on input nobody will write.
	if keys.IsEOF() {
		return
	}

	if len(keys.buf) > 0 && !keys.mustWait {
		return
	}

	// The macro engine might have fed some keys
	if len(keys.macroKeys) > 0 {
		return
	}

	keys.mutex.Lock()
	keys.waiting = true
	keys.mutex.Unlock()

	defer func() {
		keys.mutex.Lock()
		keys.waiting = false
		keys.mutex.Unlock()
	}()

	for {
		// Start reading from os.Stdin in the background.
		// We will either read keyBuf from user, or an EOF
		// send by ourselves, because we pause reading.
		keyBuf, err := keys.readInputFiltered()
		if err != nil {
			// Wake: an async refresh was requested (RequestRefresh) — return
			// with no keys so the main loop repaints the (possibly updated) UI
			// and waits again. EOF: input stream closed. Any other error (e.g.
			// a revoked tty) is a real read failure: record it so the main loop
			// can surface it and exit, instead of spinning on it forever.
			switch {
			case errors.Is(err, errInputWake):
			case errors.Is(err, io.EOF):
				keys.mutex.Lock()
				keys.eof = true
				keys.mutex.Unlock()
			default:
				keys.mutex.Lock()
				if keys.readErr == nil {
					keys.readErr = err
				}
				keys.mutex.Unlock()
			}

			return
		}

		if len(keyBuf) == 0 {
			continue
		}

		keys.mutex.RLock()
		reading, keysOnce := keys.reading, keys.keysOnce
		keys.mutex.RUnlock()

		switch {
		case reading:
			keysOnce <- keyBuf
			continue

		default:
			// When convert-meta is on, any meta-prefixed bind should
			// be stripped and replaced with an escape meta instead.
			if keys.cfg != nil && keys.cfg.GetBool("convert-meta") {
				keyBuf = []byte(strutil.ConvertMeta([]rune(string(keyBuf))))
			}

			keys.mutex.RLock()
			keys.buf = append(keys.buf, keyBuf...)
			keys.mutex.RUnlock()
		}

		return
	}
}

// Empty reports whether there are no keys available to dispatch: neither
// buffered input keys nor macro-fed keys. The main loop uses this to detect a
// bare async-refresh wake (which returns from WaitAvailableKeys with no keys).
func (k *Keys) Empty() bool {
	k.mutex.RLock()
	defer k.mutex.RUnlock()

	return len(k.buf) == 0 && len(k.macroKeys) == 0
}

// Abort ends a pending input read: a shell parked in its read loop returns
// as ended, and a wait that starts afterwards does not park on a stream
// nobody writes again. Keys already buffered still dispatch, and the caller
// deciding the stream is over is the only thing the recording rides on.
func (k *Keys) Abort() {
	k.mutex.Lock()
	k.eof = true
	k.pendingOSC = nil
	k.mutex.Unlock()
	k.RequestRefresh()
}

// IsEOF returns true if the input stream has reached the end.
func (k *Keys) IsEOF() bool {
	k.mutex.RLock()
	defer k.mutex.RUnlock()

	return k.eof
}

// ReadError returns the first non-EOF input failure observed while reading keys
// (for instance a revoked tty), or nil. Async-refresh wakes are not errors and
// are never reported here. The main loop uses this to exit cleanly instead of
// spinning on an unrecoverable input stream.
func (k *Keys) ReadError() error {
	k.mutex.RLock()
	defer k.mutex.RUnlock()

	return k.readErr
}

// PeekKey returns the first key in the stack, without removing it.
func PeekKey(keys *Keys) (key byte, empty bool) {
	switch {
	case len(keys.buf) > 0:
		key = keys.buf[0]
	case len(keys.macroKeys) > 0:
		key = byte(keys.macroKeys[0]) //nolint:gosec // G115: byte-keyed API; macro keys are control bytes, truncation is intentional.
	default:
		return byte(0), true
	}

	return key, false
}

// PopKey is used to pop a key off the key stack without
// yet marking this key as having matched a bind command.
func PopKey(keys *Keys) (key byte, empty bool) {
	switch {
	case len(keys.buf) > 0:
		key = keys.buf[0]
		keys.buf = keys.buf[1:]
	case len(keys.macroKeys) > 0:
		key = byte(keys.macroKeys[0]) //nolint:gosec // G115: byte-keyed API; macro keys are control bytes, truncation is intentional.
		keys.macroKeys = keys.macroKeys[1:]
	default:
		return byte(0), true
	}

	return key, false
}

// PeekChar returns the first character in the stack, without
// removing it (in order to provide Unicode support).
func PeekChar(keys *Keys) (char []byte, empty bool) {
	switch {
	case len(keys.buf) > 0:
		// Use the uniseg library to correctly determine where each characters stop.
		char, _, _, _ = uniseg.FirstGraphemeCluster(keys.buf, -1)
		return char, false

	case len(keys.macroKeys) > 0:
		// Macros already store keys as runes, just pick one.
		// Maybe long-term we should find a remedy to this: storing
		// macro keys as runes is inconsistent with the rest of our code.
		return []byte(string(keys.macroKeys[0])), false

	default:
		return nil, true
	}
}

// PopChar is used to pop a character off the key stack without
// yet marking this key as having matched a bind command.
func PopChar(keys *Keys) (char []byte, empty bool) {
	switch {
	case len(keys.buf) > 0:
		char, keys.buf, _, _ = uniseg.FirstGraphemeCluster(keys.buf, -1)
	case len(keys.macroKeys) > 0:
		char = []byte(string(keys.macroKeys[0]))
		keys.macroKeys = keys.macroKeys[1:]
	default:
		return nil, true
	}

	return char, false
}

// MatchedKeys is used to indicate how many keys have been evaluated against the shell
// commands in the dispatching process (regardless of if a command was matched or not).
// This function should normally not be used by external users of the library.
func MatchedKeys(keys *Keys, matched []byte, args ...byte) {
	if len(matched) > 0 {
		keys.matched = []rune(string(matched))
	}

	if len(args) > 0 {
		keys.buf = append(args, keys.buf...)
	}

	keys.mustWait = false
}

// MatchedPrefix is similar to MatchedKeys, except that the provided keys
// should not be flushed, since they only matched some binds by prefix and
// that we need more keys for an exact match (or failure).
func MatchedPrefix(keys *Keys, prefix ...byte) {
	if len(prefix) == 0 {
		return
	}

	keys.mutex.Lock()
	defer keys.mutex.Unlock()

	// Our keys are still considered unread, but they have been:
	// if there is no more keys in the stack, the next blocking
	// call to WaitAvailableKeys() should block for new keys.
	keys.mustWait = len(keys.buf) == 0
	keys.buf = append(prefix, keys.buf...)
	keys.matched = []rune(string(prefix))
}

// PopForce is used to force-remove a key from the buffer, without marking
// it as having matched a bind command. This is used, for example, when the
// escape has been handled specially as a Vim escape.
func PopForce(keys *Keys) (key byte, empty bool) {
	switch {
	case len(keys.buf) > 0:
		key = keys.buf[0]
		keys.buf = keys.buf[1:]
	case len(keys.macroKeys) > 0:
		key = byte(keys.macroKeys[0]) //nolint:gosec // G115: byte-keyed API; macro keys are control bytes, truncation is intentional.
		keys.macroKeys = keys.macroKeys[1:]
	default:
		return byte(0), true
	}

	// Force the macro recorder to use the matched keys.
	keys.mustWait = false

	return key, false
}

// MacroKeys returns the keys that have matched a given command, and thus can be recorded
// as a part of the current macro. This function is different from keys.Caller() in that it
// won't return keys that have only matched a prefix, to avoid recording them twice.
func MacroKeys(keys *Keys) []rune {
	if keys.mustWait {
		return nil
	}

	return keys.matched
}

// FlushUsed drops the keys that have matched a given command.
func FlushUsed(keys *Keys) {
	keys.mutex.Lock()
	keys.matched = nil
	defer keys.mutex.Unlock()
}

// ReadKey reads keys from stdin like Read(), but immediately
// returns them instead of storing them in the stack, along with
// an indication on whether this key is an escape/abort one.
func (k *Keys) ReadKey() (key rune, isAbort bool) {
	k.mutex.Lock()
	keysOnce := make(chan []byte)
	k.keysOnce = keysOnce
	k.reading = true
	waiting := k.waiting
	k.mutex.Unlock()

	defer func() {
		k.mutex.Lock()
		k.reading = false
		k.mutex.Unlock()
	}()

	switch {
	case len(k.macroKeys) > 0:
		key = k.macroKeys[0]
		k.macroKeys = k.macroKeys[1:]

	case waiting:
		buf := <-keysOnce
		if len(buf) == 0 {
			return rune(0), true
		}

		key = []rune(string(buf))[0]
	default:
		// Read until we get an actual key: ignore async-refresh wakes
		// (errInputWake) and empty reads, which carry no key to return.
		var buf []byte
		for len(buf) == 0 {
			b, err := k.readInputFiltered()
			if err != nil && !errors.Is(err, errInputWake) {
				break
			}

			buf = b
		}

		if len(buf) == 0 {
			return rune(0), true
		}

		key = []rune(string(buf))[0]
	}

	// Always mark those keys as matched, so that
	// if the macro engine is recording, it will
	// capture them
	k.matched = append(k.matched, key)

	return key, key == inputrc.Esc
}

// Pop removes the first byte in the key stack (first read) and returns it.
// It returns either a key and the empty boolean set to false, or if no keys
// are present, returns a zero rune and empty set to true.
// The key bytes returned by this function are not those that have been
// matched against the current command. The keys returned here are only
// keys that have not yet been dispatched. (ex: di" will match vim delete-to,
// then select-inside, but the quote won't match a command and will be passed
// to select-inside. This function Pop() will thus return the quote.)
func (k *Keys) Pop() (key byte, empty bool) {
	switch {
	case len(k.buf) > 0:
		key = k.buf[0]
		k.buf = k.buf[1:]
	case len(k.macroKeys) > 0:
		key = byte(k.macroKeys[0]) //nolint:gosec // G115: byte-keyed API; macro keys are control bytes, truncation is intentional.
		k.macroKeys = k.macroKeys[1:]
	default:
		return byte(0), true
	}

	k.matched = append(k.matched, rune(key))

	return key, false
}

// Caller returns the keys that have matched the command currently being ran.
func (k *Keys) Caller() (keys []rune) {
	return k.matched
}

// Feed can be used to directly add keys to the stack.
// If begin is true, the keys are added on the top of
// the stack, otherwise they are being appended to it.
func (k *Keys) Feed(begin bool, keys ...rune) {
	if len(keys) == 0 {
		return
	}

	keyBuf := []rune(string(keys))

	k.mutex.Lock()
	defer k.mutex.Unlock()

	if begin {
		k.macroKeys = append(keyBuf, k.macroKeys...)
	} else {
		k.macroKeys = append(k.macroKeys, keyBuf...)
	}
}

// filterCursorReplies strips terminal replies out of a chunk of input read from
// stdin (cursor-position responses and OSC responses), forwarding the last
// cursor-position reply to a pending query if there is one.
func (k *Keys) filterCursorReplies(keys []byte) []byte {
	keys = k.filterOSCReplies(keys)

	replies := rxRcvCursorPos.FindAll(keys, -1)
	if len(replies) == 0 {
		return keys
	}

	// A late reply or a duplicate must not hold up keyboard input.
	k.mutex.RLock()
	select {
	case k.cursor <- replies[len(replies)-1]:
	default:
	}
	k.mutex.RUnlock()

	return rxRcvCursorPos.ReplaceAll(keys, nil)
}

// filterOSCReplies strips 7-bit OSC responses (\x1b] ... \x07 or \x1b] ... \x1b\\)
// out of a chunk of input read from stdin. Terminals answer OSC queries (for
// instance the OSC 11 background-color one) on the same stream we read keys
// from, and their payload would otherwise be dispatched as user input.
//
// A reply split across reads is held on the Keys until the next chunk completes
// or rules it out. Only the two-byte introducer \x1b] starts a hold: a bare
// escape at the end of a chunk is a normal Escape key and passes through.
func (k *Keys) filterOSCReplies(keys []byte) []byte {
	k.mutex.Lock()
	pending := k.pendingOSC
	k.pendingOSC = nil
	k.mutex.Unlock()

	// Nothing to carry over and no introducer in this chunk: it is all keys.
	if len(pending) == 0 && !bytes.Contains(keys, oscIntroducer) {
		return keys
	}

	if len(pending) > 0 {
		keys = append(pending, keys...)
	}

	out := make([]byte, 0, len(keys))

	for i := 0; i < len(keys); {
		// Only a complete 7-bit introducer is a candidate. The 8-bit C1 forms
		// are not matched: their bytes are valid UTF-8 continuation bytes.
		if i+1 >= len(keys) || keys[i] != byte(inputrc.Esc) || keys[i+1] != ']' {
			out = append(out, keys[i])
			i++

			continue
		}

		size, result := scanOSCReply(keys[i+2:])

		switch result {
		case oscComplete:
			i += 2 + size

		case oscIncomplete:
			rest := keys[i:]

			// Too long to still be a reply: stop holding and let the bytes
			// through, so a stuck sequence never wedges real input.
			if len(rest) > maxPendingOSC {
				out = append(out, rest...)
				i = len(keys)

				continue
			}

			k.mutex.Lock()
			if !k.eof {
				k.pendingOSC = append([]byte(nil), rest...)
			}
			k.mutex.Unlock()

			return out

		case oscAbort:
			// Not a terminal reply: pass the introducer through and keep
			// scanning the rest, so the user's keys are never swallowed.
			out = append(out, keys[i])
			i++
		}
	}

	return out
}

// oscScan reports how a candidate OSC sequence is to be handled.
type oscScan int

const (
	// oscComplete: the payload ends with a valid terminator.
	oscComplete oscScan = iota
	// oscIncomplete: the payload may still be completed by the next read.
	oscIncomplete
	// oscAbort: the payload cannot be a terminal reply; it is user input.
	oscAbort
)

// scanOSCReply scans the bytes following an ESC ] introducer. On completion it
// returns how many of them belong to the sequence (terminator included), so the
// caller can drop the whole reply and keep the keys around it.
func scanOSCReply(payload []byte) (size int, result oscScan) {
	for i, char := range payload {
		switch {
		case char == 0x07: // BEL terminator
			return i + 1, oscComplete

		case char == byte(inputrc.Esc):
			// The chunk ended right after the ESC of a ST terminator: the
			// backslash arrives with the next read.
			if i+1 >= len(payload) {
				return 0, oscIncomplete
			}

			if payload[i+1] == '\\' {
				return i + 2, oscComplete
			}

			// An escape that does not terminate the payload starts another
			// sequence: this was never an OSC reply.
			return 0, oscAbort

		case char == '\r' || char == '\n' || char == 0x03 || char == 0x04:
			// Enter and interrupt keys must reach the editor immediately.
			return 0, oscAbort
		}

		if i >= maxPendingOSC {
			return 0, oscAbort
		}
	}

	return 0, oscIncomplete
}
