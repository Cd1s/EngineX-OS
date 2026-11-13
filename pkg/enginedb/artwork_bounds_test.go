package enginedb

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// allocated returns how many bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// BH-GC1-14: a 10-byte file whose ID3 header claims a 256 MiB tag made the reader allocate 256 MiB
// on a 32-bit deck; an MP4 cover atom was read whatever its size.
func TestArtworkReadIsBoundedByTheFileAndTheCap(t *testing.T) {
	dir := t.TempDir()
	tiny := filepath.Join(dir, "tiny.mp3")
	if err := os.WriteFile(tiny, []byte{'I', 'D', '3', 4, 0, 0, 0x7f, 0x7f, 0x7f, 0x7f}, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := allocated(func() { ExtractEmbeddedArtwork(tiny) }); n > 1<<20 {
		t.Fatalf("a 10-byte file allocated %d bytes", n)
	}

	// A "data" atom claiming 40 MiB (a sparse file, so the test writes nothing).
	big := filepath.Join(dir, "big.m4a")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hdr := make([]byte, 16)
	binary.BigEndian.PutUint32(hdr[:4], 40<<20)
	copy(hdr[4:8], "data")
	if _, err := f.Write(hdr); err != nil || f.Truncate(40<<20) != nil {
		t.Fatal(err)
	}
	if n := allocated(func() { parseMP4Covr(f, 0, 40<<20) }); n > 1<<20 {
		t.Fatalf("a 40 MiB cover atom allocated %d bytes", n)
	}
}

// BH-GC1-14: ID3v2.2 frames have 6-byte headers and the cover is a PIC frame; the reader parsed
// them as v2.3 frames and found no artwork in any ID3v2.2 MP3.
func TestArtworkFromID3v22PIC(t *testing.T) {
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x42}, 64)...)
	body := append([]byte{0, 'J', 'P', 'G', 3, 'c', 'o', 'v', 'e', 'r', 0}, jpeg...)
	frame := append([]byte{'T', 'T', '2', 0, 0, 4, 0, 'a', 'b', 'c'}, 'P', 'I', 'C', byte(len(body)>>16), byte(len(body)>>8), byte(len(body)))
	frame = append(frame, body...)
	size := len(frame)
	tag := append([]byte{'I', 'D', '3', 2, 0, 0, byte(size >> 21 & 0x7f), byte(size >> 14 & 0x7f), byte(size >> 7 & 0x7f), byte(size & 0x7f)}, frame...)
	p := filepath.Join(t.TempDir(), "v22.mp3")
	if err := os.WriteFile(p, append(tag, 0xFF, 0xFB, 0x90, 0x00), 0o644); err != nil {
		t.Fatal(err)
	}
	art, err := ExtractEmbeddedArtwork(p)
	if err != nil || !bytes.Equal(art, jpeg) {
		t.Fatalf("ExtractEmbeddedArtwork = %d bytes, %v; want the PIC JPEG", len(art), err)
	}
}
