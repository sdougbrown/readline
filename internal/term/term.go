package term

import (
	"fmt"
	"os"
	"sync"
)

// termFile is the file descriptor used for all low-level terminal queries (size,
// cursor position) and escape sequences. We deliberately use stderr rather than
// stdout: stdout is the stream most likely to be redirected (e.g. `app | other`),
// whereas stderr usually stays attached to the controlling terminal, giving a
// reliable terminal size even when the program's output is piped.
var termFile = os.Stderr

// queryMu guards termFile: a host that swaps the query target does so
// while the editor's goroutine still reads it, and the swap must not
// race a size or cursor query in flight.
var queryMu sync.Mutex

// queryFile returns the file the terminal queries read, under the
// query lock.
func queryFile() *os.File {
	queryMu.Lock()
	defer queryMu.Unlock()
	return termFile
}

// fallback terminal width when we can't get it through query.
var defaultTermWidth = 80

// SetQueryFile routes the terminal's size and cursor-position queries
// through f. A nil file puts the queries back on stderr, the default:
// stderr usually stays attached to the controlling terminal, giving a
// reliable terminal size even when the program's output is piped. A
// host that renders through a writer other than stderr and needs the
// queries to read the same terminal points it here.
func SetQueryFile(f *os.File) {
	queryMu.Lock()
	defer queryMu.Unlock()
	if f == nil {
		termFile = os.Stderr
		return
	}
	termFile = f
}

// GetWidth returns the width of the terminal or 80 if it cannot be established.
func GetWidth() (termWidth int) {
	var err error
	fd := int(queryFile().Fd())
	termWidth, _, err = GetSize(fd)

	if err != nil || termWidth == 0 {
		termWidth = defaultTermWidth
	}

	return
}

// GetLength returns the length of the terminal
// (Y length), or 80 if it cannot be established.
func GetLength() int {
	termFd := int(queryFile().Fd())

	_, length, err := GetSize(termFd)
	if err != nil || length == 0 {
		return defaultTermWidth
	}

	return length
}

func printf(format string, a ...interface{}) {
	WriteString(fmt.Sprintf(format, a...))
}

// EnableBracketedPaste enables bracketed paste mode.
func EnableBracketedPaste() {
	WriteString(BracketedPasteStart)
}

// DisableBracketedPaste disables bracketed paste mode.
func DisableBracketedPaste() {
	WriteString(BracketedPasteEnd)
}
