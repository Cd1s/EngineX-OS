package rekordbox

import (
	"fmt"
	"sort"
	"unicode/utf16"
)

// BuildCueSections builds ANLZ PCOB (extended=false) or PCO2 (extended=true)
// sections for memory cues/loops (kind=0) and hot cues/loops (kind=1).
func BuildCueSections(cues []Cue, extended bool) [][]byte {
	var sections [][]byte
	for kind := uint32(0); kind < 2; kind++ {
		var selected []Cue
		for _, c := range cues {
			if (c.HotCue > 0) == (kind == 1) {
				selected = append(selected, c)
			}
		}
		sort.SliceStable(selected, func(i, j int) bool {
			if kind == 1 {
				return selected[i].HotCue < selected[j].HotCue
			}
			return selected[i].TimeMS < selected[j].TimeMS
		})
		header := 24
		tag := "PCOB"
		if extended {
			header = 20
			tag = "PCO2"
		}
		payload := make([]byte, header-12)
		be.PutUint32(payload, kind)
		countAt := 6
		if extended {
			countAt = 4
		} else {
			be.PutUint16(payload[4:], 0xffff)
			be.PutUint32(payload[8:], uint32(len(selected)))
		}
		be.PutUint16(payload[countAt:], uint16(len(selected)))

		for i, c := range selected {
			typ := byte(1)
			end := uint32(0xffffffff)
			if c.Loop {
				typ = 2
				end = c.EndMS
			}
			if !extended {
				p := make([]byte, 56)
				copy(p, "PCPT")
				be.PutUint32(p[4:], 28)
				be.PutUint32(p[8:], 56)
				be.PutUint32(p[12:], c.HotCue)
				if c.Active {
					be.PutUint32(p[16:], 4)
				}
				be.PutUint32(p[20:], 0x100000)
				first, last := uint16(i-1), uint16(i+1)
				if i == 0 {
					first = 0xffff
				}
				if i == len(selected)-1 {
					last = 0xffff
				}
				be.PutUint16(p[24:], first)
				be.PutUint16(p[26:], last)
				p[28] = typ
				p[30] = 3
				p[31] = 0xe8
				be.PutUint32(p[32:], c.TimeMS)
				be.PutUint32(p[36:], end)
				payload = append(payload, p...)
				continue
			}

			var label []uint16
			if c.Name != "" {
				label = utf16.Encode([]rune(c.Name + "\x00"))
			}
			p := make([]byte, 48+2*len(label))
			copy(p, "PCP2")
			be.PutUint32(p[4:], 16)
			be.PutUint32(p[8:], uint32(len(p)))
			be.PutUint32(p[12:], c.HotCue)
			p[16] = typ
			p[18] = 3
			p[19] = 0xe8
			be.PutUint32(p[20:], c.TimeMS)
			be.PutUint32(p[24:], end)
			p[28] = c.ColorID
			p[29] = 1
			be.PutUint16(p[36:], c.LoopNumerator)
			be.PutUint16(p[38:], c.LoopDenominator)
			be.PutUint32(p[40:], uint32(2*len(label)))
			for j, v := range label {
				be.PutUint16(p[44+2*j:], v)
			}
			q := 44 + 2*len(label)
			colorCode := c.ColorCode
			if colorCode == 0 && c.Color != [4]byte{} {
				colorCode = NearestCueColorCode(c.Color)
			}
			p[q] = colorCode
			copy(p[q+1:], c.Color[1:])
			payload = append(payload, p...)
		}

		sec := make([]byte, 12+len(payload))
		copy(sec, tag)
		be.PutUint32(sec[4:], uint32(header))
		be.PutUint32(sec[8:], uint32(len(sec)))
		copy(sec[12:], payload)
		sections = append(sections, sec)
	}
	return sections
}

// RewriteANLZCues replaces PCOB/PCO2 cue sections in a PMAI ANLZ file while
// preserving all other sections (beatgrids, waveforms, phrases, unknown tags)
// byte-for-byte. When onlyIfCueSectionPresent is true and the file has no
// existing PCOB/PCO2 section, it returns (raw, false, nil).
func RewriteANLZCues(raw []byte, cues []Cue, extended bool, onlyIfCueSectionPresent bool) ([]byte, bool, error) {
	if len(raw) < 12 || len(raw) > MaxAnalysisBytes || string(raw[:4]) != "PMAI" {
		return nil, false, fmt.Errorf("anlz: invalid PMAI header")
	}
	h, n := int(be.Uint32(raw[4:])), int(be.Uint32(raw[8:]))
	if h < 12 || h > n || n > len(raw) {
		return nil, false, fmt.Errorf("anlz: invalid PMAI bounds")
	}

	var kept [][]byte
	insertAt := -1
	hasCueSection := false
	for off := h; off < n; {
		if n-off < 12 {
			return nil, false, fmt.Errorf("anlz: truncated section header")
		}
		sn := int(be.Uint32(raw[off+8:]))
		sh := int(be.Uint32(raw[off+4:]))
		if sn < 12 || sh < 12 || sh > sn || sn > n-off {
			return nil, false, fmt.Errorf("anlz: invalid section bounds")
		}
		tag := string(raw[off : off+4])
		if tag == "PCOB" || tag == "PCO2" {
			hasCueSection = true
			if insertAt < 0 {
				insertAt = len(kept)
			}
		} else {
			sec := make([]byte, sn)
			copy(sec, raw[off:off+sn])
			kept = append(kept, sec)
		}
		off += sn
	}

	if onlyIfCueSectionPresent && !hasCueSection {
		return raw, false, nil
	}

	newCueSecs := BuildCueSections(cues, extended)
	var ordered [][]byte
	if insertAt < 0 {
		ordered = append(ordered, kept...)
		ordered = append(ordered, newCueSecs...)
	} else {
		ordered = append(ordered, kept[:insertAt]...)
		ordered = append(ordered, newCueSecs...)
		ordered = append(ordered, kept[insertAt:]...)
	}

	totalLen := h
	for _, s := range ordered {
		totalLen += len(s)
	}
	out := make([]byte, totalLen)
	copy(out[:h], raw[:h])
	be.PutUint32(out[8:], uint32(totalLen))
	pos := h
	for _, s := range ordered {
		copy(out[pos:], s)
		pos += len(s)
	}
	return out, true, nil
}
