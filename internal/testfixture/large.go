package testfixture

import (
	"fmt"
	"testing"
)

// MP3WithLAMETag constructs a small MPEG-1 Layer III header fixture (44.1 kHz,
// 128 kbps). It has a Xing frame, a 576-sample LAME delay and two audio frames;
// it is for header inspection, not audio playback. Engine skips 2257 samples.
func MP3WithLAMETag() []byte {
	const frame = 417
	b := make([]byte, 3*frame)
	for i := 0; i < 3; i++ {
		copy(b[i*frame:], []byte{0xff, 0xfb, 0x90, 0})
	}
	copy(b[36:], "Xing")
	be.PutUint32(b[40:], 3) // frames and byte count
	be.PutUint32(b[44:], 2)
	be.PutUint32(b[48:], uint32(len(b)))
	copy(b[52:], "LAME3.100")
	b[73], b[74], b[75] = 0x24, 0, 0 // 576 << 12
	return b
}

// LargeTrackPath spreads tracks across hundreds of artist/album directories.
func LargeTrackPath(i int) string {
	return fmt.Sprintf("Contents/Artist%03d/Album%03d/Track%05d.mp3", (i-1)/10, (i-1)/5, i)
}

// LargePDB builds independent DeviceSQL wire pages, including multi-page Track
// and playlist-entry chains. Existing five-track fixtures are left unchanged.
func LargePDB(n int) []byte {
	if n < 1 {
		panic("LargePDB requires a positive track count")
	}
	types := []uint32{0, 7, 8}
	rows := make([][][]byte, len(types))
	for i := 1; i <= n; i++ {
		b := make([]byte, 136)
		le.PutUint32(b[8:], 44100)
		le.PutUint32(b[16:], uint32(len(MP3WithLAMETag())))
		le.PutUint32(b[48:], 128)
		le.PutUint32(b[56:], 12000)
		le.PutUint32(b[72:], uint32(i))
		le.PutUint16(b[84:], 8)
		for j := 0; j < 21; j++ {
			s := ""
			switch j {
			case 14:
				s = "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"
			case 17:
				s = fmt.Sprintf("Track %05d", i)
			case 20:
				s = "/" + LargeTrackPath(i)
			}
			le.PutUint16(b[94+2*j:], uint16(len(b)))
			b = append(b, str(s)...)
		}
		rows[0] = append(rows[0], b)
		rows[2] = append(rows[2], entry(uint32(i), uint32(i), 1))
	}
	rows[1] = [][]byte{playlist(1, 0, 1, "Large Set", false)}
	out := make([]byte, PageSize)
	le.PutUint32(out[4:], PageSize)
	le.PutUint32(out[8:], uint32(len(types)))
	for ti, typ := range types {
		first := len(out) / PageSize
		left := rows[ti]
		for len(left) > 0 {
			num := len(out) / PageSize
			p := make([]byte, PageSize)
			le.PutUint32(p[4:], uint32(num))
			le.PutUint32(p[8:], typ)
			p[27] = 0x24
			off, count := 40, 0
			for count < len(left) && off+len(left[count])+((count+16)/16)*36 <= PageSize {
				row := left[count]
				copy(p[off:], row)
				base := PageSize - (count/16)*36
				le.PutUint16(p[base-6-2*(count%16):], uint16(off-40))
				le.PutUint16(p[base-4:], le.Uint16(p[base-4:])|1<<uint(count%16))
				off += len(row)
				count++
			}
			if count == 0 {
				panic("LargePDB row exceeds page")
			}
			le.PutUint16(p[24:], uint16(count))
			le.PutUint16(p[30:], uint16(off-40))
			left = left[count:]
			if len(left) > 0 {
				le.PutUint32(p[12:], uint32(num+1))
			}
			out = append(out, p...)
		}
		h := out[28+16*ti:]
		le.PutUint32(h, typ)
		le.PutUint32(h[8:], uint32(first))
		le.PutUint32(h[12:], uint32(len(out)/PageSize-1))
	}
	le.PutUint32(out[12:], uint32(len(out)/PageSize))
	return out
}

// LargeExport creates a multi-folder MP3 export with shared ANLZ beat grids,
// hot/memory cues and loops. n may be changed on a later call to model re-export.
func LargeExport(t testing.TB, root string, n int) {
	t.Helper()
	Write(t, root, "PIONEER/rekordbox/export.pdb", LargePDB(n))
	base := "PIONEER/USBANLZ/P001/ANLZ0000"
	Write(t, root, base+".DAT", DAT())
	Write(t, root, base+".EXT", EXT())
	Write(t, root, base+".2EX", EX2())
	mp3 := MP3WithLAMETag()
	for i := 1; i <= n; i++ {
		Write(t, root, LargeTrackPath(i), mp3)
	}
}
