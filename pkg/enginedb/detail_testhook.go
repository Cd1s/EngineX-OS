package enginedb

import (
	"io"
	"os"
	"syscall"
)

// SetDetailReadFailForTest makes the display's reading of ANLZ files (detailFrom: BridgeDetail and
// DetailServes) fail with EIO wherever bad says so for a read of n bytes at off in the file at
// path, as a stick's bad page does (#253 review 4). Tests only; restore puts the reader back.
func SetDetailReadFailForTest(bad func(path string, off int64, n int) bool) (restore func()) {
	old := detailRead
	detailRead = func(f *os.File, p []byte) (int, error) {
		off, err := f.Seek(0, io.SeekCurrent)
		if err == nil && bad(f.Name(), off, len(p)) {
			return 0, &os.PathError{Op: "read", Path: f.Name(), Err: syscall.EIO}
		}
		return old(f, p)
	}
	return func() { detailRead = old }
}
