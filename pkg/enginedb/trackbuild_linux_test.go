//go:build linux

package enginedb

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #253 review 4: while a deck plays, the build pauses between its reads so that the deck gets
// the stick, and no readahead may read the next part of the file during that pause. The page
// cache shows it (mincore): after one paced read only the pages that read asked for are in.
// Replaces TestIssue253PacedReadsLeaveTheRestOfTheFileUnread (the same check on the build's
// reader instead of the old reference check).
func TestBuildTrackPacedReadsLeaveTheRestOfTheFileUnread(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	const chunk = 64 << 10
	wave := make([]byte, 12+3*(16*chunk/3))
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], uint32((len(wave)-12)/3))
	for i := 12; i < len(wave); i++ {
		wave[i] = byte(i * 7)
	}
	data := testfixture.PMAI(testfixture.Section("PWV7", 24, wave))
	rel := "PIONEER/USBANLZ/P001/ANLZ0000.2EX"
	full := filepath.Join(root, filepath.FromSlash(rel))
	testfixture.Write(t, root, rel, data)
	page := os.Getpagesize()
	resident := func() (first, rest int) {
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
		vec := make([]byte, (len(data)+page-1)/page)
		if _, _, e := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
			t.Fatal(e)
		}
		for i, v := range vec {
			if v&1 == 0 {
				continue
			}
			if i <= chunk/page { // the file and section headers and the first chunk of the section
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
	settled := func() (first, rest int) {
		for end := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if first, rest = resident(); rest > 0 || time.Now().After(end) {
				return first, rest
			}
		}
	}
	drop()
	f, err := os.Open(full)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Read(make([]byte, chunk))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, rest := settled(); rest == 0 {
		t.Skip("this file system reads no further ahead than asked: nothing to observe")
	}
	drop()
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after the first read of the waveform")
	reads := 0
	_, err = buildTrack(buildResolver(t, root), l.Tracks[0], ReadOptions{Chunk: chunk, Pause: func(n int, _ time.Duration) error {
		if reads++; n >= chunk {
			return stop
		}
		return nil
	}})
	if !errors.Is(err, stop) {
		t.Fatalf("paced build: %v after %d reads", err, reads)
	}
	first, rest := settled()
	if first == 0 {
		t.Fatal("the paced read left none of its own pages in the cache: the test observes nothing")
	}
	if rest > 0 {
		t.Fatalf("one paced read of %d bytes also read %d pages ahead, into the pause before the next read", chunk, rest)
	}
}
