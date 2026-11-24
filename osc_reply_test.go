//go:build unix

package readline

import "testing"

// TestReadlineIgnoresStrayOSCReply covers the reported leak end to end: a
// terminal answering an OSC 11 query while the editor reads keys must not
// insert the reply's payload into the line.
func TestReadlineIgnoresStrayOSCReply(t *testing.T) {
	rl := NewShell()

	sh := startPipeShell(t, rl)
	waitRefreshes(t, sh.refreshes, 1)

	sh.typeString(t, "ab\x1b]11;rgb:2424/2323/2323\x07c")

	if line := sh.accept(t); line != "abc" {
		t.Fatalf("accepted line = %q, want %q", line, "abc")
	}
}
