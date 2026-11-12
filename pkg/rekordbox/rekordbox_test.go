package rekordbox

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestDeviceSQLStrings(t *testing.T) {
	for _, s := range []string{"", "short", string(bytes.Repeat([]byte{'a'}, 200)), "é ガ 한 Ａ 🎵"} {
		b := EncodeDeviceSQLString(s)
		got, e := readDeviceSQLString(b, 0)
		if e != nil || got != s {
			t.Fatalf("%q %v", got, e)
		}
		if len(b) > 1 {
			if _, e = readDeviceSQLString(b[:len(b)-1], 0); e == nil {
				t.Fatal("accepted truncated string")
			}
		}
	}
	for _, b := range [][]byte{{0x90, 3, 0, 0}, {0x40, 255, 0, 0}, {0x80, 4, 0, 0}, {0x90, 6, 0, 0, 1}} {
		if _, e := readDeviceSQLString(b, 0); e == nil {
			t.Errorf("invalid string accepted %x", b)
		}
	}
	// Rekordbox encodes mangled ISRC strings as kind 0x90 with 0x03 prefix + ASCII + NUL (often odd length).
	isrcBody := append([]byte{0x03}, []byte("USRC17607839\x00\x00")...) // 15 bytes (odd)
	isrcRaw := append([]byte{0x90, byte(4 + len(isrcBody)), 0, 0}, isrcBody...)
	if got, e := readISRCString(isrcRaw, 0); e != nil || got != "USRC17607839" {
		t.Fatalf("mangled ISRC 0x90 string = %q, %v", got, e)
	}
	// Odd-length non-0x03 0x90 strings trim the trailing odd byte instead of failing.
	oddUTF16 := append(EncodeDeviceSQLString("é🎵"), 0xff)
	binary.LittleEndian.PutUint16(oddUTF16[1:], uint16(len(oddUTF16)))
	if got, e := readDeviceSQLString(oddUTF16, 0); e != nil || got != "é🎵" {
		t.Fatalf("odd 0x90 UTF-16 string = %q, %v", got, e)
	}
	if got, e := readDeviceSQLString([]byte{0x90, 5, 0, 0, 1}, 0); e != nil || got != "" {
		t.Fatalf("1-byte 0x90 UTF-16 string = %q, %v", got, e)
	}
}

func TestPDBPagesAndInvalidData(t *testing.T) {
	b := testfixture.PDB()
	l, e := ParsePDB(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	if len(l.Tracks) != 5 || len(l.Playlists) != 5 || len(l.Columns) != 1 || l.Tracks[0].Artist != "Artist" || l.Tracks[0].Key != "8A" {
		t.Fatalf("library incomplete: %+v", l)
	}
	for _, mutate := range []func([]byte){func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 0) }, func(b []byte) { binary.LittleEndian.PutUint32(b[8:], 10000) }, func(b []byte) { binary.LittleEndian.PutUint32(b[testfixture.PageSize+12:], 1) }, func(b []byte) { binary.LittleEndian.PutUint16(b[3*testfixture.PageSize-6:], 65000) }, func(b []byte) { binary.LittleEndian.PutUint32(b[28+12:], 100000) }} {
		c := append([]byte(nil), b...)
		mutate(c)
		if _, e = ParsePDB(bytes.NewReader(c), int64(len(c))); e == nil {
			t.Fatal("invalid PDB accepted")
		}
	}
	// Inactive row bytes must never be decoded, even if malformed.
	c := append([]byte(nil), b...)
	p := c[2*testfixture.PageSize : 3*testfixture.PageSize]
	binary.LittleEndian.PutUint16(p[24:], 6)
	binary.LittleEndian.PutUint16(p[testfixture.PageSize-16:], 65000)
	if _, e = ParsePDB(bytes.NewReader(c), int64(len(c))); e != nil {
		t.Fatalf("inactive row was parsed: %v", e)
	}
}

func TestAnalysisMergeAndBounds(t *testing.T) {
	a, e := ParseAnalysis(testfixture.DAT())
	if e != nil {
		t.Fatal(e)
	}
	for _, b := range [][]byte{testfixture.EXT(), testfixture.EX2()} {
		ext, e := ParseAnalysis(b)
		if e != nil {
			t.Fatal(e)
		}
		a.Merge(ext)
	}
	if len(a.Beats) != 4 || len(a.Cues) != 4 || a.Structure == nil || a.Structure.Phrases[0].Kind != 6 {
		t.Fatalf("bad merged analysis: %+v", a)
	}
	for _, b := range [][]byte{nil, []byte("PMAI"), testfixture.DAT()[:60]} {
		if _, e = ParseAnalysis(b); e == nil {
			t.Fatal("accepted truncated analysis")
		}
	}
	b := testfixture.EXT()
	binary.BigEndian.PutUint32(b[28+8:], 0)
	if _, e = ParseAnalysis(b); e == nil {
		t.Fatal("zero length section accepted")
	}
	// An obfuscated PSSI body decodes to the same phrases as the plaintext tag.
	b = testfixture.EXT()
	off := bytes.Index(b, []byte("PSSI"))
	count := b[off+17]
	mask := []byte{0xcb, 0xe1, 0xee, 0xfa, 0xe5, 0xee, 0xad, 0xee, 0xe9, 0xd2, 0xe9, 0xeb, 0xe1, 0xe9, 0xf3, 0xe8, 0xe9, 0xf4, 0xe1}
	for i := off + 18; i < len(b); i++ {
		b[i] ^= mask[(i-off-18)%len(mask)] + count
	}
	masked, e := ParseAnalysis(b)
	if e != nil || masked.Structure.Mood != 2 || masked.Structure.Phrases[0].Kind != 6 {
		t.Fatal(masked, e)
	}
}

func FuzzAnalysis(f *testing.F) {
	f.Add(testfixture.DAT())
	f.Add(testfixture.EXT())
	f.Add(testfixture.EX2())
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseAnalysis(b) })
}
func FuzzPDB(f *testing.F) {
	f.Add(testfixture.PDB())
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParsePDB(bytes.NewReader(b), int64(len(b))) })
}

// r2 review: UTF-16LE text whose first byte is 0x03 (七 U+4E03, 〃 U+3003,
// ă U+0103, Ѓ U+0403) was decoded as a mangled ISRC and came out garbled.
func TestDeviceSQLStringsStartingWithByte03(t *testing.T) {
	for _, s := range []string{"七里香", "七夕", "ă test", "〃", "Ѓ", "七"} {
		b := EncodeDeviceSQLString(s)
		if b[0] != strKindLongUTF16 || b[4] != 0x03 {
			t.Fatalf("%q is not a UTF-16 string starting with byte 0x03: % x", s, b[:6])
		}
		if got, err := readDeviceSQLString(b, 0); err != nil || got != s {
			t.Errorf("%q decoded as %q, %v", s, got, err)
		}
	}
	pdb := testfixture.PDBNamed([5]string{"七里香.wav", "七夕.flac", "ă test.wav", "〃.wav", "five.wav"}, "七夕 set")
	l, err := ParsePDB(bytes.NewReader(pdb), int64(len(pdb)))
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, tr := range l.Tracks {
		titles[tr.Title] = true
		if tr.Title == "七里香.wav" && tr.Path != "/Music/"+testfixture.UnicodeName {
			t.Errorf("path of 七里香: %q", tr.Path)
		}
	}
	for _, want := range []string{"七里香.wav", "七夕.flac", "ă test.wav", "〃.wav"} {
		if !titles[want] {
			t.Errorf("title %q missing: %v", want, titles)
		}
	}
	found := false
	for _, p := range l.Playlists {
		found = found || p.Name == "七夕 set"
	}
	if !found {
		t.Errorf("playlist name garbled: %+v", l.Playlists)
	}
}
