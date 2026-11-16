package prolink

import (
	"bytes"
	"testing"
)

// compactWire frames a dbserver message the way rekordbox 7 does: its tag list is exactly
// as long as the argument list, where players pad it to 12.
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
	for _, f := range args {
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
