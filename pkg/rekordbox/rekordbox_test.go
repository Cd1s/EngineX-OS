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
	for _, b := range [][]byte{{0x90, 3, 0, 0}, {0x40, 255, 0, 0}, {0x80, 4, 0, 0}, {0x90, 5, 0, 0, 1}} {
		if _, e := readDeviceSQLString(b, 0); e == nil {
			t.Errorf("invalid string accepted %x", b)
		}
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
