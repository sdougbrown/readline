//go:build darwin || dragonfly || freebsd || (linux && !appengine) || netbsd || openbsd

package term

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY opens a pty pair the way a terminal would: the master end is
// what a test writes and reads, the slave end is the fd a shell holds.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("opening the pty master: %v", err)
	}

	unlock := 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), 0x40045431, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		t.Fatalf("unlocking the pty: %v", errno)
	}

	var number uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), 0x80045430, uintptr(unsafe.Pointer(&number))); errno != 0 {
		master.Close()
		t.Fatalf("naming the pty: %v", errno)
	}

	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatalf("opening the pty slave: %v", err)
	}

	t.Cleanup(func() {
		master.Close()
		slave.Close()
	})

	return master, slave
}

// TestMakeRawRestoresTheStateBeforeTheFirstRaw covers stacked raw modes: a
// host that made the terminal raw itself leaves the raw state as the
// "previous" one a second MakeRaw captures, and a restore to it ends raw.
// Every MakeRaw of an ownership hands back the state the terminal was in
// before the first raw of that ownership, so the restores land cooked.
func TestMakeRawRestoresTheStateBeforeTheFirstRaw(t *testing.T) {
	_, slave := openPTY(t)
	fd := int(slave.Fd())

	before, err := GetState(fd)
	if err != nil {
		t.Fatalf("reading the terminal's state: %v", err)
	}

	first, err := MakeRaw(fd)
	if err != nil {
		t.Fatalf("the first raw: %v", err)
	}

	second, err := MakeRaw(fd)
	if err != nil {
		t.Fatalf("the second raw: %v", err)
	}

	if *second != *before {
		t.Fatal("the second raw's restore state is not the state before the first raw")
	}

	if err := Restore(fd, second); err != nil {
		t.Fatalf("restoring: %v", err)
	}

	after, err := GetState(fd)
	if err != nil {
		t.Fatalf("reading the terminal after the restore: %v", err)
	}

	if *after != *before {
		t.Fatal("the terminal did not land back where it started")
	}

	_ = first
}
