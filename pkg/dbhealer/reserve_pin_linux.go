//go:build linux

package dbhealer

import (
	"os"

	"golang.org/x/sys/unix"
)

// openInodePin opens path with O_PATH|O_CLOEXEC so the caller keeps a stable
// reference to the exact kernel inode for child /proc/self/fd reads and copies
// without ever closing a normal file descriptor in the parent process (which
// would strip same-process POSIX fcntl locks).
func openInodePin(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// checkReadablePath verifies read permission for path without opening a parent
// file descriptor that could drop POSIX fcntl locks on close.
func checkReadablePath(path string) error {
	if err := unix.Access(path, unix.R_OK); err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	return nil
}
