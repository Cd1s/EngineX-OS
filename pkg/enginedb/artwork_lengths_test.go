package enginedb

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// Review of 0196b77: a FLAC picture block's 24-bit length was allocated before checking what the
// file still holds, so an 8-byte file allocated 16 MiB on each of the two passes that read it.
func TestTruncatedFLACPictureBlockAllocatesOnlyTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cut.flac")
	if err := os.WriteFile(p, []byte{'f', 'L', 'a', 'C', 0x86, 0xFF, 0xFF, 0xFF}, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := allocated(func() { ExtractEmbeddedArtwork(p) }); n > 1<<20 {
		t.Fatalf("an 8-byte FLAC allocated %d bytes", n)
	}
}

// Review of 0196b77: on the first-generation decks' 32-bit build (int is 32 bits), the 32-bit
// lengths of an ID3v2.3 extended header or frame and of a FLAC picture's MIME type wrapped
// negative, passed the bounds checks and panicked rb-bridge or stream-sync. Run with
// GOARCH=arm too.
func TestMalformedArtworkLengthsAreRefusedWithoutPanic(t *testing.T) {
	filler := func(n int) []byte { return bytes.Repeat([]byte{'A'}, n) }
	be := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	cases := map[string][]byte{
		// ID3v2.3 with an extended header claiming 0xFFFFFFF0 bytes (32 bytes in all).
		"ext.mp3": append(append([]byte{'I', 'D', '3', 3, 0, 0x40, 0, 0, 0, 22}, be(0xFFFFFFF0)...), filler(18)...),
		// ID3v2.3 with an APIC frame claiming 0x7FFFFFFF bytes (32 bytes in all).
		"frame.mp3": append(append(append([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 22, 'A', 'P', 'I', 'C'}, be(0x7FFFFFFF)...), 0, 0), filler(12)...),
		// FLAC picture block whose MIME type claims 0x7FFFFFFC bytes (40 bytes in all).
		"mime.flac": append(append(append([]byte{'f', 'L', 'a', 'C', 0x86, 0, 0, 32}, be(3)...), be(0x7FFFFFFC)...), make([]byte, 24)...),
		// MP4 atom with a 64-bit size near the int64 limit after a first atom.
		"size.m4a": append(append([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e', 0, 0, 0, 1, 'f', 'r', 'e', 'e'}, binary.BigEndian.AppendUint64(nil, 1<<63-16)...), filler(8)...),
	}
	dir := t.TempDir()
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%d-byte %s panicked: %v", len(b), name, r)
				}
			}()
			if art, err := ExtractEmbeddedArtwork(p); err == nil {
				t.Fatalf("%s gave %d bytes of artwork", name, len(art))
			}
		})
	}
}
