package prolink

import (
	"bytes"
	"testing"
)

// compactWire frames a dbserver message the way rekordbox 7 does: its tag list is exactly
// as long as the argument list, where players pad it to 12, and an empty blob is not sent.
func compactWire(tx uint32, kind uint16, args ...dbField) []byte {
	b := dbNumber(dbMagic).appendTo(nil)
	b = dbNumber(tx).appendTo(b)
	b = (dbField{tag: 0x10, n: uint32(kind)}).appendTo(b)
	b = (dbField{tag: 0x0f, n: uint32(len(args))}).appendTo(b)
	tags := make([]byte, len(args))
	for i, f := range args {
		tags[i] = f.argumentTag()
	}
	b = (dbField{tag: 0x14, data: tags}).appendTo(b)
	for i, f := range args {
		if f.tag == 0x14 && len(f.data) == 0 && i > 0 && args[i-1].tag == 0x11 && args[i-1].n == 0 {
			continue // rekordbox leaves an empty blob off the wire, after its zero length
		}
		b = f.appendTo(b)
	}
	return b
}

func TestReadDBMessageCompactTagList(t *testing.T) {
	m, err := readDBMessage(bytes.NewReader(compactWire(7, 0x4000, dbNumber(8), dbNumber(17))))
	if err != nil || m.tx != 7 || m.kind != 0x4000 || len(m.args) != 2 || m.args[1].n != 17 {
		t.Fatalf("compact tag list: %+v %v", m, err)
	}
	// Longer than 32 or shorter than the argument count is not a dbserver message.
	long := compactWire(7, 0x4000, dbNumber(1))
	long[0x13] = 33
	short := compactWire(7, 0x4000, dbNumber(1), dbNumber(2))
	short[0x13] = 1
	for name, raw := range map[string][]byte{"over 32": append(long, make([]byte, 40)...), "under argc": short} {
		if _, err := readDBMessage(bytes.NewReader(raw)); err == nil {
			t.Fatalf("%s tag list accepted", name)
		}
	}
}

func TestReadDBMessageEmptyBlobIsLeftOffTheWire(t *testing.T) {
	// A cue list rekordbox answers for a track without cues: request, status, length 0 and
	// no blob, then an entry size and a second length 0 with no blob.
	raw := compactWire(3, 0x4702, dbNumber(0x2104), dbNumber(1), dbNumber(0), dbField{tag: 0x14}, dbNumber(36), dbNumber(0), dbNumber(0), dbNumber(0), dbNumber(0), dbField{tag: 0x14})
	r := bytes.NewReader(append(raw, compactWire(4, 0x4000, dbNumber(1))...))
	m, err := readDBMessage(r)
	if err != nil || len(m.args) != 10 || m.args[3].tag != 0x14 || len(m.args[3].data) != 0 || m.args[4].n != 36 || m.args[9].tag != 0x14 {
		t.Fatalf("%+v %v", m, err)
	}
	if next, err := readDBMessage(r); err != nil || next.tx != 4 {
		t.Fatalf("framing lost after the empty blobs: %+v %v", next, err)
	}
}
