package enginedb

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
)

// #253 review 3: the gate that lets a file replace a stored waveform row is the reader that will
// serve the detail. A malformed section of any kind the parser reads, in front of a valid PWV7,
// makes BridgeDetail find nothing; the gate must say the same. A healthy file still passes, and
// the gate reads the .2EX whole, as the display does (review 4).
func TestIssue253GateIsTheDetailReader(t *testing.T) {
	const base = "PIONEER/USBANLZ/P001/ANLZ0000"
	files := []string{base + ".2EX", base + ".EXT"}
	type stick struct{ ex2, ext []byte }
	cases := map[string]struct {
		s     stick
		serve bool
	}{
		"healthy":           {stick{testfixture.EX2(), testfixture.EXT()}, true},
		"healthy .EXT only": {stick{testfixture.EX2()[:40], testfixture.EXT()}, true},
		"truncated":         {stick{testfixture.EX2()[:len(testfixture.EX2())/2], testfixture.EXT()[:len(testfixture.EXT())/2]}, false},
	}
	for _, k := range testfixture.MalformedANLZKinds {
		bad := testfixture.MalformedANLZSection(k)
		cases["malformed "+k] = struct {
			s     stick
			serve bool
		}{stick{testfixture.WithANLZSectionFirst(testfixture.EX2(), bad), testfixture.WithANLZSectionFirst(testfixture.EXT(), bad)}, false}
		cases["malformed "+k+" in the .2EX only"] = struct {
			s     stick
			serve bool
		}{stick{testfixture.WithANLZSectionFirst(testfixture.EX2(), bad), testfixture.EXT()}, true} // the .EXT's PWV5 serves
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for f, b := range map[string][]byte{files[0]: c.s.ex2, files[1]: c.s.ext} {
				p := filepath.Join(root, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r, err := compat.NewResolver(root)
			if err != nil {
				t.Fatal(err)
			}
			ok, read, err := DetailServes(r, files, nil)
			served := BridgeDetail(packBridgeRef(1, files), root, 1<<20) != nil
			if err != nil || ok != served {
				t.Fatalf("the gate says %v (%v), the reader that serves the detail says %v", ok, err, served)
			}
			if ok != c.serve {
				t.Fatalf("serves=%v, want %v", ok, c.serve)
			}
			// #253 review 4: the gate is the display's reading, the whole .2EX and nothing else.
			if name == "healthy" && read != int64(len(c.s.ex2)) {
				t.Fatalf("the gate read %d bytes of a healthy pair, want the whole .2EX (%d)", read, len(c.s.ex2))
			}
		})
	}
}

// anlzWave is an ANLZ file of rekordbox's size: one waveform section of entries entries (PWV7, 3
// bytes each, in a .2EX; PWV5, 2 bytes, in a .EXT) and a PWV6 or PSSI behind it. body is where the
// waveform's entries are.
func anlzWave(ext string, entries int) (b []byte, bodyAt, bodyEnd int) {
	tag, width := "PWV7", 3
	if ext == ".EXT" {
		tag, width = "PWV5", 2
	}
	wave := make([]byte, 12+entries*width)
	be := binary.BigEndian
	be.PutUint32(wave, uint32(width))
	be.PutUint32(wave[4:], uint32(entries))
	for i := 12; i < len(wave); i++ {
		wave[i] = byte(i)
	}
	pwv6 := make([]byte, 8+3600)
	be.PutUint32(pwv6[0:], 3)
	be.PutUint32(pwv6[4:], 1200)
	tail := testfixture.Section("PWV6", 20, pwv6)
	b = testfixture.PMAI(testfixture.Section(tag, 24, wave), tail)
	return b, 28 + 24, 28 + 24 + entries*width
}

// failingRead reads a file as the product does, but a read that touches a span bad gives for its
// path fails with EIO, as a bad page of the stick does (#253 reviews 4 and 5: a reader made by the
// test, not a hook of the product).
func failingRead(bad func(path string, off int64, n int) bool) detailReader {
	return func(f *os.File, p []byte) (int, error) {
		off, err := f.Seek(0, io.SeekCurrent)
		if err == nil && bad(f.Name(), off, len(p)) {
			return 0, &os.PathError{Op: "read", Path: f.Name(), Err: syscall.EIO}
		}
		return f.Read(p)
	}
}

// #253 review 4: a file whose headers and other sections read but whose waveform body, or last
// page (padding past the declared length), does not (a bad page on the stick) gives the display
// nothing; the gate reads the same bytes the same way, so it says the same, and the row keeps its
// copy. A read error in the .2EX alone leaves the .EXT's PWV5 to serve, for both.
func TestIssue253GateReadsTheFileAsTheDisplayDoes(t *testing.T) {
	const base = "PIONEER/USBANLZ/P001/ANLZ0000"
	files := []string{base + ".2EX", base + ".EXT"}
	type span struct{ at, end int64 }
	for _, fault := range []string{"waveform body", "last page"} {
		for _, where := range []string{"both files", ".2EX only"} {
			t.Run(fault+"/"+where, func(t *testing.T) {
				root := t.TempDir()
				bad := map[string]span{}
				for _, ext := range []string{".2EX", ".EXT"} {
					b, at, end := anlzWave(ext, 60000) // 180 KB and 120 KB: two reads and one
					if fault == "last page" {
						b = append(b, make([]byte, 4096)...) // FAT padding past the declared length
					}
					p := filepath.Join(root, filepath.FromSlash(base+ext))
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, b, 0o644); err != nil {
						t.Fatal(err)
					}
					if ext == ".EXT" && where == ".2EX only" {
						continue
					}
					s := span{int64(at) + 1000, int64(at) + 1001}
					if fault == "last page" {
						last := int64(len(b)-1) &^ 4095
						s = span{last, int64(len(b))}
						if last < int64(end) {
							t.Fatalf("the last page holds the waveform")
						}
					}
					bad[p] = s
				}
				rd := failingRead(func(path string, off int64, n int) bool {
					s, ok := bad[path]
					return ok && off < s.end && off+int64(n) > s.at
				})
				r, err := compat.NewResolver(root)
				if err != nil {
					t.Fatal(err)
				}
				var reads []int
				ok, _, err := detailServes(r, files, func(n int, _ time.Duration) error {
					reads = append(reads, n)
					return nil
				}, rd)
				served := bridgeDetail(packBridgeRef(1, files), root, 1<<20, rd) != nil
				if err != nil || ok != served {
					t.Fatalf("the gate says %v (%v), the display's reader says %v", ok, err, served)
				}
				if want := where == ".2EX only"; ok != want {
					t.Fatalf("serves=%v, want %v", ok, want)
				}
				for _, n := range reads {
					if n > detailReadChunk {
						t.Fatalf("a read of %d bytes, more than %d", n, detailReadChunk)
					}
				}
			})
		}
	}
	// Paced: one pause after each read of at most 128 KiB; a stop of the pacer stops the reading
	// and confirms nothing.
	root := t.TempDir()
	b, _, _ := anlzWave(".2EX", 60000)
	p := filepath.Join(root, filepath.FromSlash(files[0]))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	var reads []int
	ok, read, err := DetailServes(r, files, func(n int, _ time.Duration) error { reads = append(reads, n); return nil })
	if !ok || err != nil || read != int64(len(b)) || len(reads) != 2 || reads[0] != detailReadChunk || reads[0]+reads[1] != len(b) {
		t.Fatalf("paced read: ok=%v err=%v read=%d of %d, reads %v", ok, err, read, len(b), reads)
	}
	stop := errors.New("job stopped")
	if ok, _, err = DetailServes(r, files, func(int, time.Duration) error { return stop }); ok || !errors.Is(err, stop) {
		t.Fatalf("a stopped read: ok=%v err=%v", ok, err)
	}
}
