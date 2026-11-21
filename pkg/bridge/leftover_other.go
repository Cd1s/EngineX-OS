//go:build !linux

package bridge

import "errors"

// unsafeToFree: the FAT is checked on Linux only (#321).
func unsafeToFree(string) bool { return false }

// exchange: the FAT is checked on Linux only, so no file is replaced this way elsewhere.
func exchange(string, string) error { return errors.ErrUnsupported }
