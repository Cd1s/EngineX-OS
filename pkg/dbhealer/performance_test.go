package dbhealer

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// quickCuesBlob is a stock quickCues blob: eight pads, the first at pos with
// color argb, the others unset (-1), then the 17-byte main cue tail.
func quickCuesBlob(pos float64, argb [4]byte) []byte {
	raw := binary.BigEndian.AppendUint64(nil, 8)
	for i := 0; i < 8; i++ {
		p, c := -1.0, [4]byte{}
		if i == 0 {
			p, c = pos, argb
		}
		raw = append(raw, 0) // empty label
		raw = binary.BigEndian.AppendUint64(raw, math.Float64bits(p))
		raw = append(raw, c[:]...)
	}
	return EncodeQCompress(append(raw, make([]byte, 17)...))
}

// A Hot Cue before sample zero (stock AAC/MP3 pre-roll) is an active pad: its
// transparent color is healed like any other; only -1 is an unset pad (BH2-A1-12).
func TestHealQuickCuesHealsPreRollHotCue(t *testing.T) {
	color := [4]byte{0, 0x12, 0x34, 0x56}
	for _, tc := range []struct {
		name string
		pos  float64
		argb [4]byte
		want bool
	}{
		{"cue in the track", 44100, color, true},
		{"pre-roll cue", -1024, color, true},
		{"pre-roll cue just before zero", -0.5, color, true},
		{"unset pad keeps its bytes", -1, color, false},
		{"opaque pre-roll cue", -1024, [4]byte{255, 0x12, 0x34, 0x56}, false},
		{"pre-roll cue without color", -1024, [4]byte{}, false},
	} {
		blob := quickCuesBlob(tc.pos, tc.argb)
		got, changed := healQuickCues(blob)
		if changed != tc.want {
			t.Fatalf("%s: changed=%v, want %v", tc.name, changed, tc.want)
		}
		if !changed {
			if !bytes.Equal(got, blob) {
				t.Fatalf("%s: untouched blob was rewritten", tc.name)
			}
			continue
		}
		raw, err := DecodeQCompress(got)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		// first pad: 8-byte count, 1-byte label length, 8-byte position, ARGB
		if !bytes.Equal(raw[17:21], []byte{255, 0x12, 0x34, 0x56}) {
			t.Fatalf("%s: color %x", tc.name, raw[17:21])
		}
		if math.Float64frombits(binary.BigEndian.Uint64(raw[9:17])) != tc.pos {
			t.Fatalf("%s: position moved", tc.name)
		}
	}
}
