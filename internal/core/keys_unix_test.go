//go:build unix

package core

import "testing"

// TestGetCursorPosFallbackFiltersOSCReplies verifies that raw stdin reads in
// GetCursorPos strip stray OSC replies instead of queuing them onto k.buf as
// user input, while keeping real keystrokes intact.
func TestGetCursorPosFallbackFiltersOSCReplies(t *testing.T) {
	t.Run("pure OSC chunk before cursor reply", func(t *testing.T) {
		stdin := &stubReadCloser{steps: []readStep{
			{data: []byte("\x1b]11;rgb:2424/2323/2323\x1b\\")},
			{data: []byte("\x1b[12;34R")},
		}}
		keys := &Keys{}

		x, y, ok := keys.readCursorPosFrom(nil, stdin)
		if !ok || x != 34 || y != 12 {
			t.Fatalf("readCursorPosFrom = (%d, %d, %t), want (34, 12, true)", x, y, ok)
		}
		if len(keys.buf) != 0 {
			t.Fatalf("queued user input = %q, want empty", keys.buf)
		}
	})

	t.Run("split OSC with user input before cursor reply", func(t *testing.T) {
		stdin := &stubReadCloser{steps: []readStep{
			{data: []byte("x\x1b]11;rgb:24")},
			{data: []byte("24/2323\x1b\\y")},
			{data: []byte("\x1b[5;6R")},
		}}
		keys := &Keys{}

		x, y, ok := keys.readCursorPosFrom(nil, stdin)
		if !ok || x != 6 || y != 5 {
			t.Fatalf("readCursorPosFrom = (%d, %d, %t), want (6, 5, true)", x, y, ok)
		}
		if got := string(keys.buf); got != "xy" {
			t.Fatalf("queued user input = %q, want %q", got, "xy")
		}
	})
}
