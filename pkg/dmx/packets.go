// Package dmx provides the optional, standalone EngineX lighting engine.
package dmx

import "encoding/binary"

// ArtNet encodes ArtDMX (Art-Net 4, protocol revision 14). Public universes
// are one-based; the Art-Net Port-Address on the wire is zero-based.
func ArtNet(universe int, sequence byte, frame [512]byte) []byte {
	p := make([]byte, 530)
	copy(p, "Art-Net\x00")
	binary.LittleEndian.PutUint16(p[8:], 0x5000)
	binary.BigEndian.PutUint16(p[10:], 14)
	p[12] = sequence
	binary.LittleEndian.PutUint16(p[14:], uint16(universe-1))
	binary.BigEndian.PutUint16(p[16:], 512)
	copy(p[18:], frame[:])
	return p
}

// SACN encodes ANSI E1.31 data packets with a null DMX start code and 512
// slots. CID must remain stable for the lifetime of this source.
func SACN(universe int, sequence byte, frame [512]byte, cid [16]byte, terminated bool) []byte {
	p := make([]byte, 638)
	copy(p, "\x00\x10\x00\x00ASC-E1.17\x00\x00\x00")
	binary.BigEndian.PutUint16(p[16:], 0x726e)
	binary.BigEndian.PutUint32(p[18:], 4)
	copy(p[22:], cid[:])
	binary.BigEndian.PutUint16(p[38:], 0x7258)
	binary.BigEndian.PutUint32(p[40:], 2)
	copy(p[44:108], "EngineX DMX Studio")
	p[108], p[111] = 100, sequence
	if terminated {
		p[112] = 0x40
	}
	binary.BigEndian.PutUint16(p[113:], uint16(universe))
	binary.BigEndian.PutUint16(p[115:], 0x720b)
	p[117], p[118] = 2, 0xa1
	binary.BigEndian.PutUint16(p[121:], 1)
	binary.BigEndian.PutUint16(p[123:], 513)
	copy(p[126:], frame[:])
	return p
}
