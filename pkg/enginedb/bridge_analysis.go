package enginedb

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// maxDetailFile bounds an ANLZ file read for the display: a PWV7 of 2<<20 entries and the rest.
const maxDetailFile = 16 << 20

// DetailOf is the scrolling detail of a track whose ANLZ files (volume-relative, the snapshot's
// Track.Detail) are files, as DisplayWaveFromRekordbox reads it. Nil when it cannot be read.
func DetailOf(root string, files []string) *rekordbox.Analysis {
	if len(files) == 0 {
		return nil
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		return nil
	}
	a, _, _ := detailFrom(r, files, nil, nil)
	return a
}

// DetailPause is called after each read of an ANLZ file (n bytes, which took took): the
// background job paces its reads with it. A non-nil error stops the reading.
type DetailPause func(n int, took time.Duration) error

// detailReadChunk is the most detailFrom reads at once: with a DetailPause, the longest a read of
// the job keeps the stick from a deck.
const detailReadChunk = 128 << 10

// detailReader reads the next part of an open ANLZ file in place of f.Read. Only this package's
// tests make one (to fail a read where they want); the product passes nil and reads the file.
type detailReader func(f *os.File, p []byte) (int, error)

// detailFrom is the display's detail of a reference: the files, in the order the display prefers
// them, each resolved by r (case, NFC, macOS's hidden .PIONEER; no traversal, no symlink), a
// regular file within maxDetailFile, read whole and accepted by ParseAnalysis; the first waveform
// the display draws (displayAnalysisWaveform) is the detail. Nil when none serves. err is only
// pause's.
func detailFrom(r *compat.Resolver, files []string, pause DetailPause, rd detailReader) (detail *rekordbox.Analysis, read int64, err error) {
	return detailFromChunk(r, files, detailReadChunk, pause, rd)
}

func detailFromChunk(r *compat.Resolver, files []string, chunk int, pause DetailPause, rd detailReader) (detail *rekordbox.Analysis, read int64, err error) {
	a := &rekordbox.Analysis{}
	for _, f := range files {
		data, n, err := readDetailFile(r, f, chunk, pause, rd)
		read += n
		if err != nil {
			var stop detailStopped
			if errors.As(err, &stop) {
				return nil, read, stop.err
			}
			continue
		}
		if one, err := rekordbox.ParseAnalysis(data); err == nil {
			a.Waveforms = append(a.Waveforms, one.Waveforms...)
		}
		// Only the section the display reads, as an RBA1 row held it; a PWV7 found first
		// leaves the .EXT unread.
		if w := displayAnalysisWaveform(a); w != nil {
			return &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{*w}}, read, nil
		}
	}
	return nil, read, nil
}

// detailStopped is the error a DetailPause stopped the reading with.
type detailStopped struct{ err error }

func (e detailStopped) Error() string { return "enginedb: detail read stopped: " + e.err.Error() }

func openReadNoAtime(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|openNoAtime, 0)
	if errors.Is(err, os.ErrPermission) {
		f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	}
	return f, err
}

// readDetailFile reads rel whole, as r resolves it, refusing traversal, symlinks, anything but a
// regular file and more than maxDetailFile bytes, chunk bytes at a time.
func readDetailFile(r *compat.Resolver, rel string, chunk int, pause DetailPause, rd detailReader) (b []byte, read int64, err error) {
	if chunk <= 0 {
		chunk = detailReadChunk
	}
	if rel, err = r.Resolve(rel); err != nil {
		return nil, 0, err
	}
	// No access time: on FAT a read otherwise writes the file's directory entry back (measured,
	// one write a track on the 6031-track stick copy). rb-bridge is root; others open it plainly.
	f, err := openReadNoAtime(r.FullPath(rel))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxDetailFile {
		return nil, 0, fmt.Errorf("not a regular file within %d bytes: %s", maxDetailFile, rel)
	}
	if pause != nil {
		noReadahead(f)
	}
	b = make([]byte, 0, st.Size())
	for {
		if len(b) == cap(b) {
			if len(b) > maxDetailFile {
				return nil, read, fmt.Errorf("grew past %d bytes: %s", maxDetailFile, rel)
			}
			b = append(b, 0)[:len(b)] // the file grew since its stat
		}
		start := time.Now()
		var n int
		if p := b[len(b):min(cap(b), len(b)+chunk)]; rd == nil {
			n, err = f.Read(p)
		} else {
			n, err = rd(f, p)
		}
		b, read = b[:len(b)+n], read+int64(n)
		if err == io.EOF {
			return b, read, nil
		}
		if err != nil {
			return nil, read, err
		}
		if pause != nil && n > 0 {
			if err = pause(n, time.Since(start)); err != nil {
				return nil, read, detailStopped{err}
			}
		}
	}
}
