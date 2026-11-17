// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || dragonfly || freebsd || (linux && !appengine) || netbsd || openbsd
// +build darwin dragonfly freebsd linux,!appengine netbsd openbsd

package term

import (
	"sync"

	"golang.org/x/sys/unix"
)

// State contains the state of a terminal.
type State struct {
	termios unix.Termios
}

// IsTerminal returns true if the given file descriptor is a terminal.
func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, ioctlReadTermios)

	return err == nil
}

// rawOwners remembers, per file descriptor, the state the terminal was in
// before its first MakeRaw: a host that made the terminal raw itself leaves
// the raw state as the "previous" one a second MakeRaw would capture, and
// a restore to it would end raw. Every MakeRaw of an ownership hands back
// the remembered state, and Restore consumes the memory, so the last
// restore of an ownership lands where the terminal started.
var rawOwners = struct {
	sync.Mutex
	states map[int]*State
}{states: make(map[int]*State)}

// rawen applies the raw modes the way cfmakeraw documents them in the
// termios(3) manpage.
func rawen(termios *unix.Termios) {
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON

	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	termios.Cflag &^= unix.CSIZE | unix.PARENB
	termios.Cflag |= unix.CS8
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0
}

// MakeRaw put the terminal connected to the given file descriptor into raw
// mode and returns the previous state of the terminal so that it can be
// restored. A MakeRaw of a terminal that is already raw of the same
// ownership re-asserts the raw modes and hands back the state the terminal
// was in before the first raw of it, so the restore lands where the
// terminal started and not on the raw modes themselves.
func MakeRaw(fd int) (*State, error) {
	rawOwners.Lock()
	remembered := rawOwners.states[fd]
	rawOwners.Unlock()

	if remembered != nil {
		termios, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
		if err != nil {
			return nil, err
		}

		rawen(termios)

		if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, termios); err != nil {
			return nil, err
		}

		return remembered, nil
	}

	termios, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}

	oldState := State{termios: *termios}
	rawen(termios)

	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, termios); err != nil {
		return nil, err
	}

	rawOwners.Lock()
	rawOwners.states[fd] = &oldState
	rawOwners.Unlock()

	return &oldState, nil
}

// GetState returns the current state of a terminal which may be useful to
// restore the terminal after a signal.
func GetState(fd int) (*State, error) {
	termios, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}

	return &State{termios: *termios}, nil
}

// Restore restores the terminal connected to the given file descriptor to a
// previous state. A restore of the state a MakeRaw handed out ends that
// MakeRaw's ownership of the fd, so the next one captures the state the
// terminal is actually in.
func Restore(fd int, state *State) error {
	rawOwners.Lock()
	if rawOwners.states[fd] == state {
		delete(rawOwners.states, fd)
	}
	rawOwners.Unlock()

	return unix.IoctlSetTermios(fd, ioctlWriteTermios, &state.termios)
}

// GetSize returns the dimensions of the given terminal.
func GetSize(fd int) (width, height int, err error) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return -1, -1, err
	}

	return int(ws.Col), int(ws.Row), nil
}
