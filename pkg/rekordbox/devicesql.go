package rekordbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// DeviceSQL string encodings (see Deep Symmetry crate-digger rekordbox_pdb.ksy).
const (
	strKindLongASCII = 0x40
	strKindLongUTF16 = 0x90
)

var errShortString = errors.New("rekordbox: truncated DeviceSQL string")

// readDeviceSQLString decodes the DeviceSQL string starting at buf[off].
func readDeviceSQLString(buf []byte, off int) (string, error) {
	if off < 0 || off >= len(buf) {
		return "", errShortString
	}
	kind := buf[off]
	if kind&1 == 1 { // short ASCII: length (incl. header byte) in the upper 7 bits
		n := int(kind >> 1)
		if n == 0 {
			return "", nil
		}
		if off+n > len(buf) {
			return "", errShortString
		}
		return string(buf[off+1 : off+n]), nil
	}
	if off+4 > len(buf) {
		return "", errShortString
	}
	n := int(binary.LittleEndian.Uint16(buf[off+1:]))
	if n < 4 || off+n > len(buf) {
		return "", errShortString
	}
	body := buf[off+4 : off+n]
	switch kind {
	case strKindLongASCII:
		return string(body), nil
	case strKindLongUTF16:
		if len(body)%2 != 0 {
			body = body[:len(body)-1]
		}
		u := make([]uint16, len(body)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(body[2*i:])
		}
		return strings.TrimRight(string(utf16.Decode(u)), "\x00"), nil
	default:
		return "", fmt.Errorf("rekordbox: unknown DeviceSQL string kind 0x%02x", kind)
	}
}

// readISRCString decodes ofs_strings[0] of a track row. rekordbox marks an ISRC
// as UTF-16 (kind 0x90) but stores 0x03, the ASCII code and a NUL (Deep Symmetry,
// "ISRC Strings"). Only that slot may be read this way: ordinary UTF-16LE text
// can start with the byte 0x03 too (七 is 03 4E, 〃 03 30, ă 03 01).
func readISRCString(buf []byte, off int) (string, error) {
	if off >= 0 && off+5 <= len(buf) && buf[off] == strKindLongUTF16 && buf[off+4] == 0x03 {
		n := int(binary.LittleEndian.Uint16(buf[off+1:]))
		if n < 5 || off+n > len(buf) {
			return "", errShortString
		}
		return strings.TrimRight(string(buf[off+5:off+n]), "\x00"), nil
	}
	return readDeviceSQLString(buf, off)
}

// EncodeDeviceSQLString encodes s the way rekordbox does: short ASCII when it
// fits, long ASCII for longer ASCII and UTF-16LE for anything non-ASCII.
func EncodeDeviceSQLString(s string) []byte {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	switch {
	case ascii && len(s)+1 <= 0x7f:
		return append([]byte{byte((len(s)+1)<<1 | 1)}, s...)
	case ascii:
		out := make([]byte, 4, 4+len(s))
		out[0] = strKindLongASCII
		binary.LittleEndian.PutUint16(out[1:], uint16(4+len(s)))
		return append(out, s...)
	default:
		u := utf16.Encode([]rune(s))
		out := make([]byte, 4+2*len(u))
		out[0] = strKindLongUTF16
		binary.LittleEndian.PutUint16(out[1:], uint16(len(out)))
		for i, c := range u {
			binary.LittleEndian.PutUint16(out[4+2*i:], c)
		}
		return out
	}
}
