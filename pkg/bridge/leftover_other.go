//go:build !linux

package bridge

// unsafeToFree: the FAT is checked on Linux only (#321).
func unsafeToFree(string) bool { return false }
