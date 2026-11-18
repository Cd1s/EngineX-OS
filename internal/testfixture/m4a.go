package testfixture

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func AACExport(t testing.TB, root string) string {
	t.Helper()
	LargeExport(t, root, 1)
	old := LargeTrackPath(1)
	newPath := strings.TrimSuffix(old, ".mp3") + ".m4a"
	pdb := bytes.ReplaceAll(LargePDB(1), str("/"+old), str("/"+newPath))
	Write(t, root, "PIONEER/rekordbox/export.pdb", pdb)
	Write(t, root, newPath, AACWithPriming())
	return newPath
}

// AACWithPriming models an AAC file whose priming metadata comes after cover
// art. The container has 21,435,392 samples; Engine skips the first 2,048.
func AACWithPriming() []byte {
	box := func(name string, parts ...[]byte) []byte {
		b := make([]byte, 8)
		copy(b[4:], name)
		for _, part := range parts {
			b = append(b, part...)
		}
		binary.BigEndian.PutUint32(b, uint32(len(b)))
		return b
	}
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:], 44100)
	binary.BigEndian.PutUint32(mdhd[16:], 21435392)
	hdlr := make([]byte, 24)
	copy(hdlr[8:], "soun")
	stsd, mp4a := make([]byte, 8), make([]byte, 28)
	stsd[7], mp4a[17], mp4a[19] = 1, 2, 16
	binary.BigEndian.PutUint32(mp4a[24:], 44100<<16)
	trak := box("trak", box("mdia", box("mdhd", mdhd), box("hdlr", hdlr),
		box("minf", box("stbl", box("stsd", stsd, box("mp4a", mp4a))))))
	data := box("data", make([]byte, 8), []byte(" 00000000 00000800 000002EA 0000000001470916"))
	item := box("----", box("mean", make([]byte, 4), []byte("com.apple.iTunes")),
		box("name", make([]byte, 4), []byte("iTunSMPB")), data)
	meta := box("udta", box("meta", make([]byte, 4), box("ilst", box("covr", make([]byte, 70<<10)), item)))
	return append(append(box("ftyp", []byte("M4A \x00\x00\x00\x00")), box("moov", trak, meta)...), box("mdat", make([]byte, 100<<10))...)
}
