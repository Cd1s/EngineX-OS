package rekordbox

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

func TestPDBMultiplePagesAndRowGroups(t *testing.T) {
	const ps = 512
	b := make([]byte, 3*ps)
	le := binary.LittleEndian
	le.PutUint32(b[4:], ps)
	le.PutUint32(b[8:], 1)
	le.PutUint32(b[28:], 5)
	le.PutUint32(b[36:], 1)
	le.PutUint32(b[40:], 2)
	for n := 1; n <= 2; n++ {
		p := b[n*ps : (n+1)*ps]
		le.PutUint32(p[4:], uint32(n))
		le.PutUint32(p[8:], 5)
		le.PutUint32(p[12:], uint32(n+1))
		le.PutUint16(p[24:], 21)
		p[27] = 0x24
		off := 40
		for j := 0; j < 20; j++ {
			id := uint32((n-1)*20 + j + 1)
			le.PutUint32(p[off:], id)
			le.PutUint32(p[off+4:], id)
			name := EncodeDeviceSQLString(fmt.Sprintf("%dA", id))
			copy(p[off+8:], name)
			base := ps - (j/16)*36
			le.PutUint16(p[base-6-2*(j%16):], uint16(off-40))
			flags := le.Uint16(p[base-4:])
			le.PutUint16(p[base-4:], flags|1<<uint(j%16))
			off += 8 + len(name)
		}
		// Deleted offset aliases live row 0 and must not bound or invalidate it.
		le.PutUint16(p[ps-36-6-2*4:], 0)
		le.PutUint16(p[30:], uint16(off-40))
	}
	l, e := ParsePDB(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	if len(l.Keys) != 40 || l.Keys[1] != "1A" || l.Keys[40] != "40A" {
		t.Fatalf("lost rows across groups/pages: %v", l.Keys)
	}
	// Only the last row on page 1 survives. used_size counts its 12 live bytes;
	// it cannot bound that row, whose allocation is still near the heap's end.
	p := b[ps : 2*ps]
	le.PutUint16(p[ps-4:], 0)
	le.PutUint16(p[ps-36-4:], 8)
	le.PutUint16(p[30:], 12)
	l, e = ParsePDB(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal("deleted heap hole rejected", e)
	}
	if len(l.Keys) != 21 || l.Keys[20] != "20A" {
		t.Fatal(l.Keys)
	}
}

// A partial last row group only reserves its flags and used offsets, so a live row
// can sit where a full group would have been. Seen on a real export: 100 rows, 4 of
// them in the last group, with the final row in the 24 bytes a full group would take.
func TestPDBLiveRowInsideWhatAFullGroupWouldReserve(t *testing.T) {
	const ps = 512
	b := make([]byte, 2*ps)
	le := binary.LittleEndian
	le.PutUint32(b[4:], ps)
	le.PutUint32(b[8:], 1)
	le.PutUint32(b[28:], 5)
	le.PutUint32(b[36:], 1)
	le.PutUint32(b[40:], 1)
	p := b[ps:]
	le.PutUint32(p[4:], 1)
	le.PutUint32(p[8:], 5)
	le.PutUint16(p[24:], 20)
	p[27] = 0x24
	off := 40
	for j := 0; j < 20; j++ {
		id := uint32(j + 1)
		if j == 19 {
			off = ps - 70 // past 512-72, the end of the heap if the group were full
		}
		le.PutUint32(p[off:], id)
		le.PutUint32(p[off+4:], id)
		name := EncodeDeviceSQLString(fmt.Sprintf("%dA", id))
		copy(p[off+8:], name)
		base := ps - (j/16)*36
		le.PutUint16(p[base-6-2*(j%16):], uint16(off-40))
		le.PutUint16(p[base-4:], le.Uint16(p[base-4:])|1<<uint(j%16))
		off += 8 + len(name)
	}
	l, e := ParsePDB(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	if len(l.Keys) != 20 || l.Keys[20] != "20A" {
		t.Fatalf("%v", l.Keys)
	}
}
