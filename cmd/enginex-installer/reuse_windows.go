//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// setReuseControl configures SO_REUSEADDR on listening sockets on Windows,
// allowing discovery listeners to share ports with running instances of Engine DJ (#164).
func setReuseControl(_, _ string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	})
	if err != nil {
		return err
	}
	return sockErr
}
