package enginedb

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

var (
	testJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9}
	testPNG  = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R', 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89}
)

func createTestMP3(t *testing.T, path string, ver byte, imgData []byte) {
	t.Helper()
	var frameBody bytes.Buffer
	frameBody.WriteByte(0x00) // text encoding: ISO-8859-1
	frameBody.WriteString("image/jpeg\x00")
	frameBody.WriteByte(0x03) // picture type: front cover
	frameBody.WriteString("Cover\x00")
	frameBody.Write(imgData)

	bodyBytes := frameBody.Bytes()
	var frame bytes.Buffer
	frame.WriteString("APIC")
	if ver == 4 {
		sz := len(bodyBytes)
		frame.WriteByte(byte((sz >> 21) & 0x7F))
		frame.WriteByte(byte((sz >> 14) & 0x7F))
		frame.WriteByte(byte((sz >> 7) & 0x7F))
		frame.WriteByte(byte(sz & 0x7F))
	} else {
		var sz [4]byte
		binary.BigEndian.PutUint32(sz[:], uint32(len(bodyBytes)))
		frame.Write(sz[:])
	}
	frame.Write([]byte{0x00, 0x00}) // flags
	frame.Write(bodyBytes)

	frameBytes := frame.Bytes()
	var tag bytes.Buffer
	tag.WriteString("ID3")
	tag.WriteByte(ver)
	tag.WriteByte(0x00) // revision
	tag.WriteByte(0x00) // flags
	tagSz := len(frameBytes)
	tag.WriteByte(byte((tagSz >> 21) & 0x7F))
	tag.WriteByte(byte((tagSz >> 14) & 0x7F))
	tag.WriteByte(byte((tagSz >> 7) & 0x7F))
	tag.WriteByte(byte(tagSz & 0x7F))
	tag.Write(frameBytes)
	tag.Write([]byte("fake-mp3-audio-payload"))

	if err := os.WriteFile(path, tag.Bytes(), 0o644); err != nil {
		t.Fatalf("failed to write mp3: %v", err)
	}
}

func createTestFLAC(t *testing.T, path string, imgData []byte) {
	t.Helper()
	var picBlock bytes.Buffer
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], 3) // Picture type: Cover front
	picBlock.Write(u32[:])

	mime := "image/jpeg"
	binary.BigEndian.PutUint32(u32[:], uint32(len(mime)))
	picBlock.Write(u32[:])
	picBlock.WriteString(mime)

	binary.BigEndian.PutUint32(u32[:], 0) // Description len: 0
	picBlock.Write(u32[:])

	// Width, height, color depth, colors (4 x uint32)
	picBlock.Write(make([]byte, 16))

	binary.BigEndian.PutUint32(u32[:], uint32(len(imgData)))
	picBlock.Write(u32[:])
	picBlock.Write(imgData)

	picBytes := picBlock.Bytes()

	var file bytes.Buffer
	file.WriteString("fLaC")

	// Block header: byte 0 is (isLast << 7) | blockType; bytes 1..3 is length
	blockHdr := []byte{
		0x86, // isLast = true (0x80), type = 6 (0x06)
		byte((len(picBytes) >> 16) & 0xFF),
		byte((len(picBytes) >> 8) & 0xFF),
		byte(len(picBytes) & 0xFF),
	}
	file.Write(blockHdr)
	file.Write(picBytes)
	file.Write([]byte("fake-flac-audio-payload"))

	if err := os.WriteFile(path, file.Bytes(), 0o644); err != nil {
		t.Fatalf("failed to write flac: %v", err)
	}
}

func createTestMP4(t *testing.T, path string, imgData []byte) {
	t.Helper()
	// Build data atom
	var dataAtom bytes.Buffer
	dataPayloadLen := 8 + 8 + len(imgData) // 8 header + 4 type + 4 locale + data
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(dataPayloadLen))
	dataAtom.Write(u32[:])
	dataAtom.WriteString("data")
	binary.BigEndian.PutUint32(u32[:], 14) // Type 14 = PNG
	dataAtom.Write(u32[:])
	binary.BigEndian.PutUint32(u32[:], 0) // Locale
	dataAtom.Write(u32[:])
	dataAtom.Write(imgData)

	// Wrap in covr atom
	covrBytes := wrapAtom("covr", dataAtom.Bytes())
	// Wrap in ilst atom
	ilstBytes := wrapAtom("ilst", covrBytes)
	// Wrap in meta atom (meta has 4 bytes version/flags: 0x00000000)
	var metaContent bytes.Buffer
	metaContent.Write([]byte{0, 0, 0, 0})
	metaContent.Write(ilstBytes)
	metaBytes := wrapAtom("meta", metaContent.Bytes())
	// Wrap in udta atom
	udtaBytes := wrapAtom("udta", metaBytes)
	// Wrap in moov atom
	moovBytes := wrapAtom("moov", udtaBytes)

	var file bytes.Buffer
	// Add ftyp
	ftypBytes := wrapAtom("ftyp", []byte("M4A M4A mp42isom"))
	file.Write(ftypBytes)
	file.Write(moovBytes)
	file.Write([]byte("fake-aac-audio-payload"))

	if err := os.WriteFile(path, file.Bytes(), 0o644); err != nil {
		t.Fatalf("failed to write mp4: %v", err)
	}
}

func wrapAtom(fourcc string, payload []byte) []byte {
	var buf bytes.Buffer
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(8+len(payload)))
	buf.Write(u32[:])
	buf.WriteString(fourcc)
	buf.Write(payload)
	return buf.Bytes()
}

func TestExtractEmbeddedArtwork(t *testing.T) {
	tmp := t.TempDir()

	// 1. MP3 ID3v2.3 with JPEG
	mp3v3 := filepath.Join(tmp, "test_v3.mp3")
	createTestMP3(t, mp3v3, 3, testJPEG)
	art, err := ExtractEmbeddedArtwork(mp3v3)
	if err != nil {
		t.Fatalf("MP3 v2.3 failed: %v", err)
	}
	if !bytes.Equal(art, testJPEG) {
		t.Fatalf("MP3 v2.3 art mismatch")
	}

	// 2. MP3 ID3v2.4 with PNG
	mp3v4 := filepath.Join(tmp, "test_v4.mp3")
	createTestMP3(t, mp3v4, 4, testPNG)
	art, err = ExtractEmbeddedArtwork(mp3v4)
	if err != nil {
		t.Fatalf("MP3 v2.4 failed: %v", err)
	}
	if !bytes.Equal(art, testPNG) {
		t.Fatalf("MP3 v2.4 art mismatch")
	}

	// 3. FLAC with JPEG
	flacPath := filepath.Join(tmp, "test.flac")
	createTestFLAC(t, flacPath, testJPEG)
	art, err = ExtractEmbeddedArtwork(flacPath)
	if err != nil {
		t.Fatalf("FLAC failed: %v", err)
	}
	if !bytes.Equal(art, testJPEG) {
		t.Fatalf("FLAC art mismatch")
	}

	// 4. MP4/M4A with PNG
	m4aPath := filepath.Join(tmp, "test.m4a")
	createTestMP4(t, m4aPath, testPNG)
	art, err = ExtractEmbeddedArtwork(m4aPath)
	if err != nil {
		t.Fatalf("MP4/M4A failed: %v", err)
	}
	if !bytes.Equal(art, testPNG) {
		t.Fatalf("MP4/M4A art mismatch")
	}

	// 5. File without artwork
	noArtPath := filepath.Join(tmp, "no_art.mp3")
	if err := os.WriteFile(noArtPath, []byte("fake-audio-only"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractEmbeddedArtwork(noArtPath); err == nil {
		t.Errorf("expected error for file without artwork")
	}

	// 6. Nonexistent file
	if _, err := ExtractEmbeddedArtwork(filepath.Join(tmp, "does_not_exist.mp3")); err == nil {
		t.Errorf("expected error for missing file")
	}
}
