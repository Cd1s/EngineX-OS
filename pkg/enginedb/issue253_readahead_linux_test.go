//go:build linux

package enginedb

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
)

// #253 review 4: the background check pauses between its reads so that a deck playing from the
// same stick gets it, and no readahead may read the next part of the file during that pause. The
// page cache shows it (mincore): after one paced read only the pages that read asked for are in.
func TestIssue253PacedReadsLeaveTheRestOfTheFileUnread(t *testing.T) {
	root := t.TempDir()
	const rel = "PIONEER/USBANLZ/P001/ANLZ0000.2EX"
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 16*detailReadChunk)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
	page := os.Getpagesize()
	asked := detailReadChunk / page
	resident := func() (first, rest int) { // the file's pages in the page cache
		f, err := os.Open(full)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		m, err := unix.Mmap(int(f.Fd()), 0, len(data), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Munmap(m)
		vec := make([]byte, len(data)/page)
		if _, _, e := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
			t.Fatal(e)
		}
		for i, v := range vec {
			if v&1 == 0 {
				continue
			}
			if i < asked {
				first++
			} else {
				rest++
			}
		}
		return first, rest
	}
	drop := func() {
		f, err := os.OpenFile(full, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err = f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
			t.Fatal(err)
		}
		if a, b := resident(); a+b > 0 {
			t.Skipf("the page cache of %s cannot be emptied (tmpfs?): %d pages stay", root, a+b)
		}
	}
	settled := func() (first, rest int) { // readahead a read started has landed, or none came
		for end := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if first, rest = resident(); rest > 0 || time.Now().After(end) {
				return first, rest
			}
		}
	}

	// The control: a plain read of the same size on this file system reads further ahead.
	drop()
	f, err := os.Open(full)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Read(make([]byte, detailReadChunk))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, rest := settled(); rest == 0 {
		t.Skip("this file system reads no further ahead than asked: nothing to observe")
	}

	drop()
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after the first read")
	ok, read, err := DetailServes(r, []string{rel}, func(int, time.Duration) error { return stop })
	if ok || !errors.Is(err, stop) || read != detailReadChunk {
		t.Fatalf("one paced read: ok=%v read=%d err=%v, want false %d %v", ok, read, err, detailReadChunk, stop)
	}
	first, rest := settled()
	if first == 0 {
		t.Fatal("the paced read left none of its own pages in the cache: the test observes nothing")
	}
	if rest > 0 {
		t.Fatalf("one paced read of %d bytes also read %d pages ahead, into the pause before the next read", detailReadChunk, rest)
	}
}
