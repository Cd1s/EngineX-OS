package deemixclient

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Fuzz lane: the tag size in an ID3 header is the file's own claim (up to 256 MiB). A header
// that claims more than any real tag must be refused before anything is allocated for it.
func TestReadID3RefusesAHugeTagBeforeAllocating(t *testing.T) {
	header := []byte{'I', 'D', '3', 3, 0, 0, 0x7f, 0x7f, 0x7f, 0x7f}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err := readID3(bytes.NewReader(header))
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("a 256 MiB tag claim in a 10-byte file was accepted")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("readID3 allocated %d bytes for a 10-byte input", grew)
	}

	// The callers leave such a file alone.
	dir := t.TempDir()
	src := filepath.Join(dir, "huge.mp3")
	if err := os.WriteFile(src, append(append([]byte{}, header...), mp3Frames(14, 3)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if has, err := hasPicture(src); err != nil || !has {
		t.Fatalf("hasPicture on a file with an absurd tag: %v %v, want true (so that nothing is embedded)", has, err)
	}
	dst := filepath.Join(dir, "copy.mp3")
	if tagged, err := CopyTagged(src, dst, "Title", "Artist", "Album"); err != nil || tagged {
		t.Fatalf("CopyTagged on a file with an absurd tag: %v %v, want no copy and no error", tagged, err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("a copy was written: %v", err)
	}

	// A tag at the limit is still read; one byte more is refused.
	at := []byte{'I', 'D', '3', 3, 0, 0, 0x08, 0x00, 0x00, 0x00} // 16 MiB
	if _, _, err := readID3(bytes.NewReader(append(at, make([]byte, maxID3Tag)...))); err != nil {
		t.Fatalf("a tag of exactly the limit: %v", err)
	}
	over := []byte{'I', 'D', '3', 3, 0, 0, 0x08, 0x00, 0x00, 0x01}
	if _, _, err := readID3(bytes.NewReader(over)); err == nil {
		t.Fatal("a tag one byte over the limit was accepted")
	}
}
