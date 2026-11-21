package rekordbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"
)

var be = binary.BigEndian

type Beat struct {
	Number uint16
	BPM    float64
	TimeMS uint32
}
type Cue struct {
	HotCue                         uint32 // 0 = memory cue; 1..8 = A..H; never assigned by the bridge
	TimeMS, EndMS                  uint32
	Loop, Active                   bool
	Name                           string
	Color                          [4]byte // A,R,G,B
	ColorID, ColorCode             byte
	LoopNumerator, LoopDenominator uint16
	Extended                       bool
}
type Waveform struct {
	Tag        string
	EntryBytes uint32
	Data       []byte
}
type Phrase struct {
	Index, Beat, Kind uint16
	Raw               []byte
}
type SongStructure struct {
	Mood, EndBeat uint16
	Bank          byte
	Phrases       []Phrase
}
type Analysis struct {
	DurationMS uint32 // track duration from PDB, when available (overview has no timebase)
	Beats      []Beat
	Cues       []Cue
	Waveforms  []Waveform
	Structure  *SongStructure
	Unknown    map[string][][]byte
}

const MaxAnalysisBytes = 128 << 20

// ParseAnalysis validates every section boundary, including unrecognized tags.
func ParseAnalysis(b []byte) (*Analysis, error) {
	if len(b) < 12 || len(b) > MaxAnalysisBytes || string(b[:4]) != "PMAI" {
		return nil, fmt.Errorf("anlz: invalid PMAI header")
	}
	h, n := int(be.Uint32(b[4:])), int(be.Uint32(b[8:]))
	// FAT32/exFAT exports may pad the file past the declared length.
	if h < 12 || h > n || n > len(b) {
		return nil, fmt.Errorf("anlz: invalid file length")
	}
	b = b[:n]
	a := &Analysis{Unknown: map[string][][]byte{}}
	for off := h; off < n; {
		if n-off < 12 {
			return nil, fmt.Errorf("anlz: short section")
		}
		s := b[off:]
		tag := string(s[:4])
		sh, sn := int(be.Uint32(s[4:])), int(be.Uint32(s[8:]))
		if sh < 12 || sn < sh || sn > n-off {
			return nil, fmt.Errorf("anlz: invalid %s length", tag)
		}
		s = s[:sn]
		if err := a.section(tag, s, sh); err != nil {
			return nil, fmt.Errorf("anlz %s: %w", tag, err)
		}
		off += sn
	}
	return a, nil
}

func (a *Analysis) section(tag string, s []byte, h int) error {
	bad := func() error { return fmt.Errorf("truncated or invalid data") }
	switch tag {
	case "PQTZ":
		if len(s) < 24 {
			return bad()
		}
		n64 := uint64(be.Uint32(s[20:]))
		if n64 > uint64((len(s)-24)/8) {
			return bad()
		}
		n := int(n64)
		for i := 0; i < n; i++ {
			p := s[24+8*i:]
			a.Beats = append(a.Beats, Beat{be.Uint16(p), float64(be.Uint16(p[2:])) / 100, be.Uint32(p[4:])})
		}
	case "PCOB", "PCO2":
		min, off, countAt := 24, 24, 18
		if tag == "PCO2" {
			min, off, countAt = 20, 20, 16
		}
		if len(s) < min {
			return bad()
		}
		listType := be.Uint32(s[12:])
		n := int(be.Uint16(s[countAt:]))
		if n > (len(s)-off)/12 {
			return bad()
		}
		for i := 0; i < n; i++ {
			if len(s)-off < 12 {
				return bad()
			}
			p := s[off:]
			size := int(be.Uint32(p[8:]))
			if size < 12 || size > len(p) {
				return bad()
			}
			p = p[:size]
			off += size
			c := Cue{}
			var cueType byte
			if tag == "PCOB" {
				if size < 56 || string(p[:4]) != "PCPT" {
					return bad()
				}
				cueType = p[28]
				c.HotCue = be.Uint32(p[12:])
				c.Active = be.Uint32(p[16:]) == 4
				c.Loop = cueType == 2
				c.TimeMS = be.Uint32(p[32:])
				c.EndMS = be.Uint32(p[36:])
			} else {
				if size < 40 || string(p[:4]) != "PCP2" {
					return bad()
				}
				c.Extended = true
				cueType = p[16]
				c.HotCue = be.Uint32(p[12:])
				c.Loop = cueType == 2
				c.TimeMS = be.Uint32(p[20:])
				c.EndMS = be.Uint32(p[24:])
				c.ColorID = p[28]
				c.LoopNumerator = be.Uint16(p[36:])
				c.LoopDenominator = be.Uint16(p[38:])
				if size > 43 {
					n := int(be.Uint32(p[40:]))
					if n < 0 || n%2 != 0 || n > size-44 {
						return bad()
					}
					c.Name = decodeUTF16BE(p[44 : 44+n])
					q := 44 + n
					if size > q {
						c.ColorCode = p[q]
					}
					if size >= q+4 {
						c.Color = [4]byte{255, p[q+1], p[q+2], p[q+3]}
					}
				}
			}
			if cueType != 1 && cueType != 2 {
				continue
			}
			if c.TimeMS == 0xffffffff || (c.Loop && c.EndMS == 0xffffffff) {
				continue
			}
			switch listType {
			case 0:
				if c.HotCue != 0 {
					continue
				}
			case 1:
				if c.HotCue < 1 || c.HotCue > 8 {
					continue
				}
			default:
				continue
			}
			if c.Loop && c.EndMS < c.TimeMS {
				return fmt.Errorf("loop end precedes start")
			}
			a.mergeCue(c)
		}
	case "PWV3", "PWV5", "PWV7", "PWV4", "PWAV", "PWV2":
		width, count, ok := WaveformShape(tag, s, int64(h), int64(len(s)))
		if !ok {
			return bad()
		}
		a.Waveforms = append(a.Waveforms, Waveform{tag, width, append([]byte(nil), s[h:h+int(width*count)]...)})
	case "PSSI":
		if len(s) < 32 {
			return bad()
		}
		width, n := int(be.Uint32(s[12:])), int(be.Uint16(s[16:]))
		if width < 24 {
			return bad()
		}
		body := append([]byte(nil), s[18:]...)
		if be.Uint16(body) > 20 {
			mask := []byte{0xcb, 0xe1, 0xee, 0xfa, 0xe5, 0xee, 0xad, 0xee, 0xe9, 0xd2, 0xe9, 0xeb, 0xe1, 0xe9, 0xf3, 0xe8, 0xe9, 0xf4, 0xe1}
			for i := range body {
				body[i] ^= mask[i%len(mask)] + byte(n)
			}
		}
		if n > (len(body)-14)/width {
			return bad()
		}
		st := &SongStructure{Mood: be.Uint16(body), EndBeat: be.Uint16(body[8:]), Bank: body[12]}
		for i := 0; i < n; i++ {
			p := body[14+i*width : 14+(i+1)*width]
			st.Phrases = append(st.Phrases, Phrase{be.Uint16(p), be.Uint16(p[2:]), be.Uint16(p[4:]), append([]byte(nil), p...)})
		}
		a.Structure = st
	default:
		a.Unknown[tag] = append(a.Unknown[tag], append([]byte(nil), s...))
	}
	return nil
}

func decodeUTF16BE(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = be.Uint16(b[2*i:])
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00")
}

func (a *Analysis) mergeCue(c Cue) {
	for i, old := range a.Cues {
		if (c.HotCue != 0 && c.HotCue == old.HotCue) || (c.HotCue == 0 && old.HotCue == 0 && c.TimeMS == old.TimeMS && c.Loop == old.Loop && (!c.Loop || c.EndMS == old.EndMS)) {
			if c.Extended || !old.Extended {
				c.Active = c.Active || old.Active
				a.Cues[i] = c
			} else {
				a.Cues[i].Active = old.Active || c.Active
			}
			return
		}
	}
	a.Cues = append(a.Cues, c)
}

// Merge prefers extended cues while keeping DAT-only memory cues and all waveform variants.
func (a *Analysis) Merge(b *Analysis) {
	if len(b.Beats) > 0 {
		a.Beats = b.Beats
	}
	for _, c := range b.Cues {
		a.mergeCue(c)
	}
	a.Waveforms = append(a.Waveforms, b.Waveforms...)
	if b.Structure != nil {
		a.Structure = b.Structure
	}
	if a.Unknown == nil {
		a.Unknown = map[string][][]byte{}
	}
	for k, v := range b.Unknown {
		a.Unknown[k] = append(a.Unknown[k], v...)
	}
}

// ReadAnalysisSkipping is ReadAnalysis that leaves out the waveform sections skip names without
// reading their bodies (#253): a track's .EXT is mostly PWV3, PWV4 and PWV5, which an import does
// not use once the .2EX has a PWV7. Their headers are checked by WaveformShape, the rule
// ParseAnalysis applies to them (it never looks into a waveform's body), and everything else is
// read and given to ParseAnalysis, so a file is refused exactly when ReadAnalysis refuses it.
func ReadAnalysisSkipping(path string, skip func(tag string) bool) (*Analysis, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	// An import keeps every PWV7 it meets (only the .2EX normally has one).
	a, _, e := ReadAnalysisFile(f, func(tag string) bool { return tag != "PWV7" && skip(tag) }, nil)
	return a, e
}

// ReadAnalysisFile is ReadAnalysisSkipping on an open file; skipped is told the tag, entry width
// and count of each waveform section left out. read is the bytes read from the file.
func ReadAnalysisFile(f *os.File, skip func(tag string) bool, skipped func(tag string, width, count uint32)) (a *Analysis, read int64, e error) {
	st, e := f.Stat()
	if e != nil {
		return nil, 0, e
	}
	size := st.Size()
	whole := func() (*Analysis, int64, error) {
		if size > MaxAnalysisBytes {
			return nil, read, fmt.Errorf("anlz: file too large")
		}
		b := make([]byte, int(size))
		if _, e := f.ReadAt(b, 0); e != nil {
			return nil, read + size, e
		}
		a, e := ParseAnalysis(b)
		return a, read + size, e
	}
	if size > MaxAnalysisBytes || size < 12 {
		return whole()
	}
	head := make([]byte, 12)
	read = 12
	if _, e = f.ReadAt(head, 0); e != nil || string(head[:4]) != "PMAI" {
		return whole()
	}
	h, n := int64(be.Uint32(head[4:])), int64(be.Uint32(head[8:]))
	if h < 12 || h > n || n > size {
		return whole()
	}
	b := make([]byte, h, h+64<<10)
	read += h
	if _, e = f.ReadAt(b, 0); e != nil {
		return nil, read, e
	}
	var left []func()
	hdr := make([]byte, 24)
	for off := h; off < n; {
		if n-off < 12 {
			return whole()
		}
		k := min(int64(len(hdr)), n-off)
		read += k
		if _, e = f.ReadAt(hdr[:k], off); e != nil {
			return nil, read, e
		}
		tag := string(hdr[:4])
		sh, sn := int64(be.Uint32(hdr[4:])), int64(be.Uint32(hdr[8:]))
		if sh < 12 || sn < sh || sn > n-off {
			return whole()
		}
		if width, count, ok := WaveformShape(tag, hdr[:k], sh, sn); ok && isWaveform(tag) && skip(tag) {
			if skipped != nil {
				left = append(left, func() { skipped(tag, width, count) })
			}
			off += sn
			continue
		}
		start := int64(len(b))
		b = append(b, make([]byte, sn)...)
		read += sn
		if _, e = f.ReadAt(b[start:], off); e != nil {
			return nil, read, e
		}
		off += sn
	}
	be.PutUint32(b[8:], uint32(len(b)))
	if a, e = ParseAnalysis(b); e != nil {
		return nil, read, e
	}
	for _, l := range left { // only for a file ParseAnalysis accepts
		l()
	}
	return a, read, nil
}

func isWaveform(tag string) bool {
	switch tag {
	case "PWV3", "PWV5", "PWV7", "PWV4", "PWAV", "PWV2":
		return true
	}
	return false
}

// WaveformShape is how ParseAnalysis reads a waveform section's header: the width of an entry
// and their count, ok when it accepts them. hdr holds the section's first 24 bytes (all of a
// shorter one); h is its header length, n its whole length. The body is never looked at.
func WaveformShape(tag string, hdr []byte, h, n int64) (width, count uint32, ok bool) {
	if n < 20 || len(hdr) < 20 {
		return 0, 0, false
	}
	width, count = 1, be.Uint32(hdr[12:])
	if tag == "PWV3" || tag == "PWV4" || tag == "PWV5" || tag == "PWV7" {
		if n < 24 || len(hdr) < 24 {
			return 0, 0, false
		}
		width, count = be.Uint32(hdr[12:]), be.Uint32(hdr[16:])
	}
	if tag == "PWV4" {
		width = 6
	}
	return width, count, width != 0 && uint64(width)*uint64(count) <= uint64(n-h)
}

func ReadAnalysis(path string) (*Analysis, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if st.Size() > MaxAnalysisBytes {
		return nil, fmt.Errorf("anlz: file too large")
	}
	b := make([]byte, int(st.Size()))
	if _, e = f.ReadAt(b, 0); e != nil {
		return nil, e
	}
	return ParseAnalysis(b)
}
