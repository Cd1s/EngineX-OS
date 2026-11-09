//go:build !windows

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setReuseControl configures SO_REUSEADDR and SO_REUSEPORT on listening sockets,
// allowing discovery listeners to share ports with running instances of Engine DJ (#164).
func setReuseControl(_, _ string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if sockErr == nil {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
