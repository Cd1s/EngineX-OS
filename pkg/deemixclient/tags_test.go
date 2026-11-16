package deemixclient

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// flacFile is a FLAC stream with the given metadata blocks (kind, data) and audio.
func flacFile(audio string, blocks ...flacMeta) []byte {
	b := []byte("fLaC")
	for i, blk := range blocks {
		kind := blk.kind
		if i == len(blocks)-1 {
			kind |= 0x80
		}
		b = append(b, kind, byte(len(blk.data)>>16), byte(len(blk.data)>>8), byte(len(blk.data)))
		b = append(b, blk.data...)
	}
	return append(b, audio...)
}

func vorbis(vendor string, fields ...string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(vendor)))
	b = append(b, vendor...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(fields)))
	for _, f := range fields {
		b = binary.LittleEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

// A download has no tags: the copy Engine imports gets title, artist and
// album, the audio and the other metadata stay as they were, and the download
// itself is not touched.
func TestCopyTaggedFLAC(t *testing.T) {
	dir := t.TempDir()
	info := bytes.Repeat([]byte{7}, 34)
	src := filepath.Join(dir, "3123276661.flac")
	original := flacFile("AUDIO-FRAMES", flacMeta{0, info}, flacMeta{1, bytes.Repeat([]byte{0}, 8)}, flacMeta{4, vorbis("reference libFLAC")}, flacMeta{6, []byte("picture")})
	if err := os.WriteFile(src, original, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "named.flac")
	tagged, err := CopyTagged(src, dst, "冲动的惩罚", "刀郎", "2002年的第一场雪")
	if err != nil || !tagged {
		t.Fatalf("CopyTagged = %v, %v", tagged, err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	want := flacFile("AUDIO-FRAMES", flacMeta{0, info}, flacMeta{4, vorbis("EngineX", "TITLE=冲动的惩罚", "ARTIST=刀郎", "ALBUM=2002年的第一场雪")}, flacMeta{1, bytes.Repeat([]byte{0}, 8)}, flacMeta{6, []byte("picture")})
	if !bytes.Equal(got, want) {
		t.Fatalf("tagged copy:\n got %q\nwant %q", got, want)
	}
	if now, _ := os.ReadFile(src); !bytes.Equal(now, original) {
		t.Fatal("the download itself was rewritten")
	}
	// A file that says what it is needs no copy.
	if tagged, err = CopyTagged(dst, filepath.Join(dir, "again.flac"), "Other", "", ""); err != nil || tagged {
		t.Fatalf("a titled file was tagged again: %v, %v", tagged, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "again.flac")); err == nil {
		t.Fatal("a copy was written for a file that needed none")
	}
}

func id3Frames(t *testing.T, file []byte) (version byte, frames map[string][]byte, rest []byte) {
	t.Helper()
	tag, _, err := readID3(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	frames = map[string][]byte{}
	f := tag.frames
	for len(f) >= 10 && f[0] != 0 {
		size := int(binary.BigEndian.Uint32(f[4:8]))
		if tag.version == 4 {
			size = int(f[4])<<21 | int(f[5])<<14 | int(f[6])<<7 | int(f[7])
		}
		frames[string(f[:4])] = f[10 : 10+size]
		f = f[10+size:]
	}
	return tag.version, frames, file[10+len(tag.frames):]
}

func TestCopyTaggedMP3(t *testing.T) {
	dir := t.TempDir()
	audio := []byte{0xff, 0xfb, 0x90, 0x00, 'm', 'p', '3'}
	// No tag at all: a new ID3v2.3 tag, text in UTF-16 with a byte order mark.
	src := filepath.Join(dir, "1578206552.mp3")
	os.WriteFile(src, audio, 0o644)
	dst := filepath.Join(dir, "named.mp3")
	if tagged, err := CopyTagged(src, dst, "冲动的惩罚", "刀郎", ""); err != nil || !tagged {
		t.Fatalf("CopyTagged = %v, %v", tagged, err)
	}
	file, _ := os.ReadFile(dst)
	version, frames, rest := id3Frames(t, file)
	text := func(b []byte) string {
		if len(b) < 3 || b[0] != 1 || b[1] != 0xff || b[2] != 0xfe {
			t.Fatalf("text frame %x is not UTF-16 with a byte order mark", b)
		}
		u := make([]uint16, (len(b)-3)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(b[3+2*i:])
		}
		return string(utf16.Decode(u))
	}
	if version != 3 || text(frames["TIT2"]) != "冲动的惩罚" || text(frames["TPE1"]) != "刀郎" || frames["TALB"] != nil || !bytes.Equal(rest, audio) {
		t.Fatalf("version %d frames %q rest %q", version, frames, rest)
	}
	// An ID3v2.4 tag with a picture only: the frames are added in UTF-8, the picture stays.
	var withPicture bytes.Buffer
	if err := insertAPIC(bytes.NewReader(append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}, audio...)), &withPicture, []byte("jpeg"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(src, withPicture.Bytes(), 0o644)
	if tagged, err := CopyTagged(src, dst, "Love", "SDM", "OCHO"); err != nil || !tagged {
		t.Fatalf("CopyTagged = %v, %v", tagged, err)
	}
	file, _ = os.ReadFile(dst)
	version, frames, rest = id3Frames(t, file)
	if version != 4 || string(frames["TIT2"]) != "\x03Love" || string(frames["TPE1"]) != "\x03SDM" || string(frames["TALB"]) != "\x03OCHO" || !bytes.HasSuffix(frames["APIC"], []byte("jpeg")) || !bytes.Equal(rest, audio) {
		t.Fatalf("version %d frames %q rest %q", version, frames, rest)
	}
	// It has a title now, and other formats are not tagged: no copy.
	for name, data := range map[string][]byte{"titled.mp3": file, "spotify-x.ogg": []byte("OggS-vorbis"), "1.mp4": []byte("\x00\x00\x00\x20ftypM4A ")} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, data, 0o644)
		out := filepath.Join(dir, "out-"+name)
		if tagged, err := CopyTagged(p, out, "Title", "Artist", ""); err != nil || tagged {
			t.Fatalf("%s: tagged %v, %v", name, tagged, err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatalf("%s: a copy was written", name)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".part-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %s", strings.Join(left, ", "))
	}
}
