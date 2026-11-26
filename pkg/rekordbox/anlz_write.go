package rekordbox

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf16"
)

// ANLZ cue and beat-grid binary format references:
//   - Deep Symmetry crate-digger: rekordbox_anlz.ksy and doc/modules/ROOT/pages/anlz.adoc
//   - Holzhaus/rekordcrate: src/anlz.rs, data/complete_export/demo_tracks, data/anlz/sweep
//   - dylanljones/pyrekordbox: pyrekordbox/anlz/structs.py, tags.py, PR #231
//
// Verified rekordbox 7 section layout invariants (6,008 .DAT / 6,007 .EXT files):
//   - Section order: hot cue list (type 1) ALWAYS precedes memory cue list (type 0) in both
//     PCOB and PCO2.
//   - .DAT PCOB(1) holds hot cues A..C (1..3); .DAT PCOB(0) holds all memory cues with
//     memory_count = count-1 when non-empty (0xffffffff when empty).
//   - .EXT PCOB(1) holds hot cues D..H (4..8) (plus any active loop on A..C so single-file
//     .EXT readers keep the active-loop flag); empty when none exist.
//   - .EXT PCOB(0) is empty (unless a memory loop is marked active).
//   - .EXT PCO2(1) holds all hot cues A..H (1..8); .EXT PCO2(0) holds all memory cues.
//   - PCP2 entries come in two rekordbox-emitted profiles:
//       * Compact unnamed hot cues (byte 29 = 0x03, loop_time = 0 for point cues, len_comment = 0):
//         44 bytes when uncolored, or 48 bytes with 4-byte tail [0x00, R, G, B] when RGB-colored;
//         paired with PCPT status = 1 (or 4 active loop), u1 = 0, order = 0xffff/0xffff, loop_time = 0.
//       * Full entries (byte 29 = 0x01, loop_time = 0xffffffff for point cues, len_entry = 88 + len_comment):
//         used for named cues, palette-coded hot cues (ColorCode 1..62), and all memory cues;
//         44-byte tail = [color_code, R, G, B] + 40 zero bytes (all 44 zero for memory cues);
//         paired with PCPT status = 0 (or 4 active loop), u1 = 0x00010000, loop_time = 0xffffffff.

type anlzSection struct {
	tag  string
	kind uint32 // for PCOB/PCO2: 0=memory, 1=hot
	raw  []byte
}

func parseANLZSections(raw []byte) (int, []anlzSection, error) {
	if len(raw) < 12 || len(raw) > MaxAnalysisBytes || string(raw[:4]) != "PMAI" {
		return 0, nil, fmt.Errorf("anlz: invalid PMAI header")
	}
	h, n := int(be.Uint32(raw[4:])), int(be.Uint32(raw[8:]))
	if h < 12 || h > n || n > len(raw) {
		return 0, nil, fmt.Errorf("anlz: invalid PMAI bounds")
	}
	var secs []anlzSection
	for off := h; off < n; {
		if n-off < 12 {
			return 0, nil, fmt.Errorf("anlz: truncated section header")
		}
		sh := int(be.Uint32(raw[off+4:]))
		sn := int(be.Uint32(raw[off+8:]))
		if sn < 12 || sh < 12 || sh > sn || sn > n-off {
			return 0, nil, fmt.Errorf("anlz: invalid section bounds")
		}
		tag := string(raw[off : off+4])
		sec := make([]byte, sn)
		copy(sec, raw[off:off+sn])
		var kind uint32
		if (tag == "PCOB" || tag == "PCO2") && sn >= 16 {
			kind = be.Uint32(sec[12:])
		}
		secs = append(secs, anlzSection{tag: tag, kind: kind, raw: sec})
		off += sn
	}
	return h, secs, nil
}

// ParseANLZPath returns the PPTH track path embedded in a rekordbox ANLZ file, if present.
func ParseANLZPath(raw []byte) (string, bool) {
	_, secs, err := parseANLZSections(raw)
	if err != nil {
		return "", false
	}
	for _, s := range secs {
		if s.tag != "PPTH" || len(s.raw) < 16 {
			continue
		}
		plen := int(be.Uint32(s.raw[12:16]))
		if plen <= 0 || 16+plen > len(s.raw) || plen%2 != 0 {
			continue
		}
		u := make([]uint16, plen/2)
		for i := range u {
			u[i] = be.Uint16(s.raw[16+2*i:])
		}
		if p := strings.TrimRight(string(utf16.Decode(u)), "\x00"); p != "" {
			return p, true
		}
	}
	return "", false
}

func assembleANLZ(origHeader []byte, secs [][]byte) []byte {
	totalLen := len(origHeader)
	for _, s := range secs {
		totalLen += len(s)
	}
	out := make([]byte, totalLen)
	copy(out, origHeader)
	be.PutUint32(out[8:], uint32(totalLen))
	pos := len(origHeader)
	for _, s := range secs {
		copy(out[pos:], s)
		pos += len(s)
	}
	return out
}

func isCompactHotCue(c Cue) bool {
	return c.HotCue > 0 && c.Name == "" && c.ColorCode == 0
}

func buildPCOBSection(kind uint32, selected []Cue) []byte {
	return buildPCOBSectionPreserving(kind, selected, nil, false)
}

func buildPCO2Section(kind uint32, selected []Cue) []byte {
	return buildPCO2SectionPreserving(kind, selected, nil, false)
}

func sortCueList(kind uint32, selected []Cue) {
	sort.SliceStable(selected, func(i, j int) bool {
		if kind == 1 {
			return selected[i].HotCue < selected[j].HotCue
		}
		return selected[i].TimeMS < selected[j].TimeMS
	})
}

// BuildCueSections builds ANLZ PCOB (extended=false, for .DAT) or PCO2 (extended=true, for .EXT)
// sections in rekordbox order: hot cues/loops (kind=1) first, memory cues/loops (kind=0) second.
// For .DAT PCOB, only hot cues A..C (1..3) are emitted; hot cues D..H (4..8) belong in .EXT.
func BuildCueSections(cues []Cue, extended bool) [][]byte {
	var hot, mem []Cue
	for _, c := range cues {
		if c.HotCue > 0 {
			if extended || c.HotCue <= 3 {
				hot = append(hot, c)
			}
		} else {
			mem = append(mem, c)
		}
	}
	if extended {
		return [][]byte{buildPCO2Section(1, hot), buildPCO2Section(0, mem)}
	}
	return [][]byte{buildPCOBSection(1, hot), buildPCOBSection(0, mem)}
}

// buildExtPCOBSections builds the .EXT PCOB(1) and PCOB(0) sections in rekordbox layout:
// PCOB(1) carries hot cues D..H (4..8) plus any active hot loop on A..C (so the active-loop
// status flag is preserved even when .EXT is parsed standalone); PCOB(0) is empty unless a
// memory loop is marked active.
func buildExtPCOBSections(cues []Cue) [][]byte {
	var hot, mem []Cue
	for _, c := range cues {
		if c.HotCue > 0 {
			if c.HotCue >= 4 || (c.Loop && c.Active) {
				hot = append(hot, c)
			}
		} else if c.Loop && c.Active {
			mem = append(mem, c)
		}
	}
	return [][]byte{buildPCOBSection(1, hot), buildPCOBSection(0, mem)}
}

type rawANLZCueEntry struct {
	cue     Cue
	compact bool // PCOB PCPT compact profile (u1 == 0)
	valid   bool // passes rekordbox layout rules for raw byte reuse
	raw     []byte
}

func parseRawCueEntries(sec anlzSection) ([]rawANLZCueEntry, bool) {
	if sec.tag != "PCOB" && sec.tag != "PCO2" {
		return nil, false
	}
	minLen, off, countAt := 24, 24, 18
	if sec.tag == "PCO2" {
		minLen, off, countAt = 20, 20, 16
	}
	if len(sec.raw) < minLen {
		return nil, false
	}
	listType := be.Uint32(sec.raw[12:])
	n := int(be.Uint16(sec.raw[countAt:]))
	if n > (len(sec.raw)-off)/12 {
		return nil, false
	}
	entries := make([]rawANLZCueEntry, 0, n)
	for i := 0; i < n; i++ {
		if len(sec.raw)-off < 12 {
			return nil, false
		}
		p := sec.raw[off:]
		size := int(be.Uint32(p[8:]))
		if size < 12 || size > len(p) {
			return nil, false
		}
		p = p[:size]
		off += size
		c := Cue{}
		var cueType byte
		var compact, valid bool
		if sec.tag == "PCOB" {
			if size < 56 || string(p[:4]) != "PCPT" {
				return nil, false
			}
			cueType = p[28]
			c.HotCue = be.Uint32(p[12:])
			status := be.Uint32(p[16:])
			c.Active = status == 4
			c.Loop = cueType == 2
			c.TimeMS = be.Uint32(p[32:])
			c.EndMS = be.Uint32(p[36:])
			compact = be.Uint32(p[20:]) == 0
			valid = size == 56 && (c.HotCue == 0 || !compact == (status == 0 || status == 4))
		} else {
			if size < 40 || string(p[:4]) != "PCP2" {
				return nil, false
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
				cn := int(be.Uint32(p[40:]))
				if cn < 0 || cn%2 != 0 || cn > size-44 {
					return nil, false
				}
				c.Name = decodeUTF16BE(p[44 : 44+cn])
				q := 44 + cn
				if size > q {
					c.ColorCode = p[q]
				}
				if size >= q+4 && (c.HotCue > 0 || p[q] != 0 || p[q+1] != 0 || p[q+2] != 0 || p[q+3] != 0) {
					c.Color = [4]byte{255, p[q+1], p[q+2], p[q+3]}
				}
				b29 := p[29]
				valid = b29 == 0x03 || (b29 == 0x01 && size >= 88+cn)
			}
		}
		if cueType != 1 && cueType != 2 {
			return nil, false
		}
		if c.TimeMS == 0xffffffff || (c.Loop && c.EndMS == 0xffffffff) {
			return nil, false
		}
		switch listType {
		case 0:
			if c.HotCue != 0 {
				return nil, false
			}
		case 1:
			if c.HotCue < 1 || c.HotCue > 8 {
				return nil, false
			}
		default:
			return nil, false
		}
		if c.Loop && c.EndMS < c.TimeMS {
			return nil, false
		}
		entries = append(entries, rawANLZCueEntry{
			cue:     c,
			compact: compact,
			valid:   valid,
			raw:     append([]byte(nil), p...),
		})
	}
	if off != len(sec.raw) {
		return nil, false
	}
	return entries, true
}

func canonicalCueEnd(c Cue) uint32 {
	if c.Loop {
		return c.EndMS
	}
	return 0
}

func pco2ColorMatches(a, b Cue) bool {
	if a.HotCue == 0 {
		return a.ColorID == b.ColorID && a.Color == b.Color
	}
	if a.ColorCode != b.ColorCode {
		return false
	}
	if a.Color == b.Color {
		return true
	}
	if b.Color == ([4]byte{}) && b.ColorCode > 0 && int(b.ColorCode) <= len(rekordboxCuePalette) {
		pal := rekordboxCuePalette[b.ColorCode-1]
		return a.Color == [4]byte{255, pal[0], pal[1], pal[2]}
	}
	if a.Color == ([4]byte{}) && a.ColorCode > 0 && int(a.ColorCode) <= len(rekordboxCuePalette) {
		pal := rekordboxCuePalette[a.ColorCode-1]
		return b.Color == [4]byte{255, pal[0], pal[1], pal[2]}
	}
	return false
}

func rawCueMatchesTarget(tag string, entry rawANLZCueEntry, target Cue) bool {
	old := entry.cue
	if old.HotCue != target.HotCue || old.TimeMS != target.TimeMS || old.Loop != target.Loop || canonicalCueEnd(old) != canonicalCueEnd(target) {
		return false
	}
	if tag == "PCOB" {
		if old.Active != (target.Loop && target.Active) {
			return false
		}
		if target.HotCue > 0 && entry.compact != isCompactHotCue(target) {
			return false
		}
		return true
	}
	return old.Name == target.Name && old.LoopNumerator == target.LoopNumerator && old.LoopDenominator == target.LoopDenominator && pco2ColorMatches(old, target)
}

func cueEntriesMatchSet(tag string, entries []rawANLZCueEntry, targets []Cue) bool {
	if len(entries) != len(targets) {
		return false
	}
	used := make([]bool, len(entries))
	for _, target := range targets {
		matched := false
		for i, entry := range entries {
			if !used[i] && rawCueMatchesTarget(tag, entry, target) {
				used[i] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func hasExpectedCueSectionLayout(secs []anlzSection, extended bool) bool {
	var tags []string
	for _, s := range secs {
		if s.tag == "PCOB" || s.tag == "PCO2" {
			tags = append(tags, fmt.Sprintf("%s:%d", s.tag, s.kind))
		}
	}
	want := []string{"PCOB:1", "PCOB:0"}
	if extended {
		want = append(want, "PCO2:1", "PCO2:0")
	}
	if len(tags) != len(want) {
		return false
	}
	for i := range want {
		if tags[i] != want[i] {
			return false
		}
	}
	return true
}

func buildPCOBSectionPreserving(kind uint32, selected []Cue, pool []rawANLZCueEntry, allowReuse bool) []byte {
	sortCueList(kind, selected)
	payload := make([]byte, 12)
	be.PutUint32(payload, kind)
	be.PutUint16(payload[4:], 0)
	be.PutUint16(payload[6:], uint16(len(selected)))
	memCount := uint32(0xffffffff)
	if kind == 0 && len(selected) > 0 {
		// Rekordbox writes the last 0-based memory cue index (count - 1), or 0xffffffff when empty.
		memCount = uint32(len(selected) - 1)
	}
	be.PutUint32(payload[8:], memCount)

	used := make([]bool, len(pool))
	for i, c := range selected {
		first, last := uint16(0xffff), uint16(0xffff)
		if kind == 0 {
			if i > 0 {
				first = uint16(i - 1)
			}
			if i < len(selected)-1 {
				last = uint16(i + 1)
			}
		}
		if allowReuse {
			reused := false
			for j, entry := range pool {
				if !used[j] && entry.valid && len(entry.raw) == 56 && rawCueMatchesTarget("PCOB", entry, c) {
					used[j] = true
					p := append([]byte(nil), entry.raw...)
					be.PutUint16(p[24:], first)
					be.PutUint16(p[26:], last)
					payload = append(payload, p...)
					reused = true
					break
				}
			}
			if reused {
				continue
			}
		}
		typ := byte(1)
		if c.Loop {
			typ = 2
		}
		compact := isCompactHotCue(c)
		end := uint32(0xffffffff)
		if compact {
			end = 0
		}
		if c.Loop {
			end = c.EndMS
		}
		p := make([]byte, 56)
		copy(p, "PCPT")
		be.PutUint32(p[4:], 28)
		be.PutUint32(p[8:], 56)
		be.PutUint32(p[12:], c.HotCue)
		var status uint32
		if c.Loop && c.Active {
			status = 4
		} else if compact {
			status = 1
		}
		be.PutUint32(p[16:], status)
		if !compact {
			be.PutUint32(p[20:], 0x10000)
		}
		be.PutUint16(p[24:], first)
		be.PutUint16(p[26:], last)
		p[28] = typ
		p[30] = 3
		p[31] = 0xe8
		be.PutUint32(p[32:], c.TimeMS)
		be.PutUint32(p[36:], end)
		payload = append(payload, p...)
	}

	sec := make([]byte, 12+len(payload))
	copy(sec, "PCOB")
	be.PutUint32(sec[4:], 24)
	be.PutUint32(sec[8:], uint32(len(sec)))
	copy(sec[12:], payload)
	return sec
}

func buildPCO2SectionPreserving(kind uint32, selected []Cue, pool []rawANLZCueEntry, allowReuse bool) []byte {
	sortCueList(kind, selected)
	payload := make([]byte, 8)
	be.PutUint32(payload, kind)
	be.PutUint16(payload[4:], uint16(len(selected)))

	used := make([]bool, len(pool))
	for _, c := range selected {
		if allowReuse {
			reused := false
			for j, entry := range pool {
				if !used[j] && entry.valid && rawCueMatchesTarget("PCO2", entry, c) {
					used[j] = true
					payload = append(payload, entry.raw...)
					reused = true
					break
				}
			}
			if reused {
				continue
			}
		}
		typ := byte(1)
		if c.Loop {
			typ = 2
		}
		if isCompactHotCue(c) {
			end := uint32(0)
			if c.Loop {
				end = c.EndMS
			}
			entryLen := 44
			if c.Color != ([4]byte{}) {
				entryLen = 48
			}
			p := make([]byte, entryLen)
			copy(p, "PCP2")
			be.PutUint32(p[4:], 16)
			be.PutUint32(p[8:], uint32(entryLen))
			be.PutUint32(p[12:], c.HotCue)
			p[16] = typ
			p[18] = 3
			p[19] = 0xe8
			be.PutUint32(p[20:], c.TimeMS)
			be.PutUint32(p[24:], end)
			p[28] = 0
			p[29] = 0x03
			be.PutUint16(p[36:], c.LoopNumerator)
			be.PutUint16(p[38:], c.LoopDenominator)
			// bytes 40..43: len_comment = 0
			if entryLen == 48 {
				// Compact colored PCP2 tail: 0x00 followed by RGB.
				p[44] = 0
				copy(p[45:48], c.Color[1:4])
			}
			payload = append(payload, p...)
			continue
		}

		end := uint32(0xffffffff)
		if c.Loop {
			end = c.EndMS
		}
		var label []uint16
		if c.Name != "" {
			label = utf16.Encode([]rune(c.Name + "\x00"))
		}
		// Full rekordbox PCP2 layout: 44-byte header + UTF-16BE comment + 44-byte tail
		// ([color_code, R, G, B] + 40 zero bytes for hot cues; 44 zero bytes for memory cues).
		p := make([]byte, 88+2*len(label))
		copy(p, "PCP2")
		be.PutUint32(p[4:], 16)
		be.PutUint32(p[8:], uint32(len(p)))
		be.PutUint32(p[12:], c.HotCue)
		p[16] = typ
		p[18] = 3
		p[19] = 0xe8
		be.PutUint32(p[20:], c.TimeMS)
		be.PutUint32(p[24:], end)
		if c.HotCue == 0 {
			colorID := c.ColorID
			if colorID == 0 && c.Color != ([4]byte{}) {
				colorID = NearestMemoryColorID(c.Color)
			}
			if colorID > 8 {
				colorID = 0
			}
			p[28] = colorID
		}
		p[29] = 0x01
		be.PutUint16(p[36:], c.LoopNumerator)
		be.PutUint16(p[38:], c.LoopDenominator)
		be.PutUint32(p[40:], uint32(2*len(label)))
		for j, v := range label {
			be.PutUint16(p[44+2*j:], v)
		}
		q := 44 + 2*len(label)
		if c.HotCue > 0 {
			colorCode := c.ColorCode
			rgb := c.Color
			if colorCode == 0 && rgb != ([4]byte{}) {
				colorCode = NearestCueColorCode(rgb)
			}
			if rgb == ([4]byte{}) && colorCode > 0 && int(colorCode) <= len(rekordboxCuePalette) {
				pal := rekordboxCuePalette[colorCode-1]
				rgb = [4]byte{255, pal[0], pal[1], pal[2]}
			}
			p[q] = colorCode
			copy(p[q+1:q+4], rgb[1:4])
		}
		payload = append(payload, p...)
	}

	sec := make([]byte, 12+len(payload))
	copy(sec, "PCO2")
	be.PutUint32(sec[4:], 20)
	be.PutUint32(sec[8:], uint32(len(sec)))
	copy(sec[12:], payload)
	return sec
}

// RewriteANLZCues replaces PCOB/PCO2 cue sections in a PMAI ANLZ file in place in the
// original file's section order while preserving every non-cue section byte-for-byte.
// Hot cue lists (type 1) are always placed before memory cue lists (type 0).
// Unchanged valid sections and unchanged entries are kept verbatim when no cue repair is needed.
// When onlyIfCueSectionPresent is true and the file has no existing PCOB/PCO2 section,
// it returns (raw, false, nil).
func RewriteANLZCues(raw []byte, cues []Cue, extended bool, onlyIfCueSectionPresent bool) ([]byte, bool, error) {
	h, secs, err := parseANLZSections(raw)
	if err != nil {
		return nil, false, err
	}

	hasCueSection := false
	for _, s := range secs {
		if s.tag == "PCOB" || s.tag == "PCO2" {
			hasCueSection = true
			break
		}
	}
	if onlyIfCueSectionPresent && !hasCueSection {
		return raw, false, nil
	}

	var wantPCOBHot, wantPCOBMem, wantPCO2Hot, wantPCO2Mem []Cue
	for _, c := range cues {
		if extended {
			if c.HotCue > 0 {
				wantPCO2Hot = append(wantPCO2Hot, c)
				if c.HotCue >= 4 || (c.Loop && c.Active) {
					wantPCOBHot = append(wantPCOBHot, c)
				}
			} else {
				wantPCO2Mem = append(wantPCO2Mem, c)
				if c.Loop && c.Active {
					wantPCOBMem = append(wantPCOBMem, c)
				}
			}
		} else {
			if c.HotCue > 0 {
				if c.HotCue <= 3 {
					wantPCOBHot = append(wantPCOBHot, c)
				}
			} else {
				wantPCOBMem = append(wantPCOBMem, c)
			}
		}
	}

	needsRepair := ANLZNeedsCueRepair(raw, extended)
	keepWhole := !needsRepair && hasExpectedCueSectionLayout(secs, extended)
	type secKey struct {
		tag  string
		kind uint32
	}
	pools := map[secKey][]rawANLZCueEntry{}
	for _, s := range secs {
		if s.tag != "PCOB" && s.tag != "PCO2" {
			continue
		}
		if entries, ok := parseRawCueEntries(s); ok {
			k := secKey{s.tag, s.kind}
			pools[k] = append(pools[k], entries...)
		}
	}

	buildSection := func(tag string, kind uint32, targets []Cue) []byte {
		k := secKey{tag, kind}
		pool := pools[k]
		if keepWhole && len(pool) > 0 && cueEntriesMatchSet(tag, pool, targets) {
			for _, s := range secs {
				if s.tag == tag && s.kind == kind {
					return s.raw
				}
			}
		}
		allowReuse := !needsRepair
		if tag == "PCO2" {
			return buildPCO2SectionPreserving(kind, targets, pool, allowReuse)
		}
		return buildPCOBSectionPreserving(kind, targets, pool, allowReuse)
	}

	var pcobPair, pco2Pair [][]byte
	pcobPair = [][]byte{
		buildSection("PCOB", 1, wantPCOBHot),
		buildSection("PCOB", 0, wantPCOBMem),
	}
	if extended {
		pco2Pair = [][]byte{
			buildSection("PCO2", 1, wantPCO2Hot),
			buildSection("PCO2", 0, wantPCO2Mem),
		}
	}

	// Replace PCOB and PCO2 sections in place where they occur in the original file.
	// Swapped pairs (type 0 before type 1) are rewritten back into rekordbox order
	// (type 1 first, type 0 second) at the original cue-section slot.
	var ordered [][]byte
	pcobIdx, pco2Idx := 0, 0
	for _, s := range secs {
		switch s.tag {
		case "PCOB":
			if pcobIdx < len(pcobPair) {
				ordered = append(ordered, pcobPair[pcobIdx])
				pcobIdx++
			}
		case "PCO2":
			if !extended {
				continue
			}
			// If an .EXT file had PCO2 without preceding PCOB, insert PCOB first in rekordbox order.
			for pcobIdx < len(pcobPair) {
				ordered = append(ordered, pcobPair[pcobIdx])
				pcobIdx++
			}
			if pco2Idx < len(pco2Pair) {
				ordered = append(ordered, pco2Pair[pco2Idx])
				pco2Idx++
			}
		default:
			ordered = append(ordered, s.raw)
		}
	}
	// If a partial cue pair existed (e.g., only one PCOB or PCO2 section) or none existed,
	// emit the remaining sections in rekordbox order (hot before memory).
	for pcobIdx < len(pcobPair) {
		ordered = append(ordered, pcobPair[pcobIdx])
		pcobIdx++
	}
	if extended {
		for pco2Idx < len(pco2Pair) {
			ordered = append(ordered, pco2Pair[pco2Idx])
			pco2Idx++
		}
	}

	out := assembleANLZ(raw[:h], ordered)
	if bytes.Equal(out, raw) {
		return raw, false, nil
	}
	return out, true, nil
}

// ANLZNeedsCueRepair reports whether an ANLZ file carries legacy EngineX cue-writer
// artifacts that must be repaired back to rekordbox's layout:
//   - swapped cue list order (type 0 before type 1 in PCOB or PCO2),
//   - non-empty .EXT PCOB entries for A..C point cues or non-active memory cues,
//   - old PCP2 entries with byte 29 == 0x00 or truncated b29=0x01 length (< 88 + len_comment),
//   - PCOB(0) memory_count == count instead of count - 1.
func ANLZNeedsCueRepair(raw []byte, extended bool) bool {
	_, secs, err := parseANLZSections(raw)
	if err != nil {
		return false
	}
	sawPCOB0, sawPCO20 := false, false
	for _, s := range secs {
		switch s.tag {
		case "PCOB":
			if len(s.raw) < 24 {
				return false
			}
			kind := be.Uint32(s.raw[12:])
			count := int(be.Uint16(s.raw[18:]))
			memCount := be.Uint32(s.raw[20:])
			if kind == 0 {
				sawPCOB0 = true
				if count > 0 && memCount == uint32(count) {
					return true
				}
			} else if kind == 1 && sawPCOB0 {
				return true
			}
			if extended && count > 0 {
				off := 24
				for i := 0; i < count && off+12 <= len(s.raw); i++ {
					psn := int(be.Uint32(s.raw[off+8:]))
					if psn < 56 || off+psn > len(s.raw) {
						return false
					}
					hc := be.Uint32(s.raw[off+12:])
					status := be.Uint32(s.raw[off+16:])
					if kind == 0 && status != 4 {
						return true
					}
					if kind == 1 && hc <= 3 && status != 4 {
						return true
					}
					off += psn
				}
			}
		case "PCO2":
			if !extended || len(s.raw) < 20 {
				continue
			}
			kind := be.Uint32(s.raw[12:])
			count := int(be.Uint16(s.raw[16:]))
			if kind == 0 {
				sawPCO20 = true
			} else if kind == 1 && sawPCO20 {
				return true
			}
			off := 20
			for i := 0; i < count && off+12 <= len(s.raw); i++ {
				psn := int(be.Uint32(s.raw[off+8:]))
				if psn < 44 || off+psn > len(s.raw) {
					return false
				}
				b29 := s.raw[off+29]
				clen := int(be.Uint32(s.raw[off+40:]))
				if b29 == 0 {
					return true
				}
				if b29 == 1 && psn < 88+clen {
					return true
				}
				off += psn
			}
		}
	}
	return false
}

func beatEntryValues(b Beat) (num, tempo uint16, timeMS uint32) {
	num = b.Number
	if num < 1 || num > 4 {
		num = 1
	}
	bpm100 := math.Round(b.BPM * 100)
	if bpm100 < 1 {
		bpm100 = 1
	} else if bpm100 > math.MaxUint16 {
		bpm100 = math.MaxUint16
	}
	return num, uint16(bpm100), b.TimeMS
}

// BuildPQTZ builds a rekordbox PQTZ beat-grid section (.DAT).
func BuildPQTZ(beats []Beat) []byte {
	sec := make([]byte, 24+8*len(beats))
	copy(sec, "PQTZ")
	be.PutUint32(sec[4:], 24)
	be.PutUint32(sec[8:], uint32(len(sec)))
	be.PutUint32(sec[12:], 0)
	be.PutUint32(sec[16:], 0x80000)
	be.PutUint32(sec[20:], uint32(len(beats)))
	for i, b := range beats {
		num, tempo, timeMS := beatEntryValues(b)
		p := sec[24+8*i:]
		be.PutUint16(p[0:], num)
		be.PutUint16(p[2:], tempo)
		be.PutUint32(p[4:], timeMS)
	}
	return sec
}

// BuildPQT2 builds a rekordbox PQT2 extended beat-grid section (.EXT).
// Layout verified across 5,380/5,380 non-empty rekordbox 7 PQT2 sections:
//   - 56-byte header (len_header = 0x38), len_tag = 56 + 2*len(beats)
//   - u16 = 0x01000002 (constant BPM) or 0x02000002 (variable BPM)
//   - bytes 24..32: first PQTZ entry; bytes 32..40: last PQTZ entry
//   - bytes 40..44: len(beats)
//   - bytes 44..48: uint32 checksum = sum(beat_number + tempo_x100 + time_ms) over all beats
//   - body: len(beats) big-endian uint16 sub-millisecond offsets in microseconds (0..999).
func BuildPQT2(beats []Beat) []byte {
	sec := make([]byte, 56+2*len(beats))
	copy(sec, "PQT2")
	be.PutUint32(sec[4:], 56)
	be.PutUint32(sec[8:], uint32(len(sec)))
	be.PutUint32(sec[12:], 0)

	flags := uint32(0x01000002)
	var firstTempo uint16
	var checksum uint64
	for i, b := range beats {
		num, tempo, timeMS := beatEntryValues(b)
		if i == 0 {
			firstTempo = tempo
			be.PutUint16(sec[24:], num)
			be.PutUint16(sec[26:], tempo)
			be.PutUint32(sec[28:], timeMS)
		} else if tempo != firstTempo {
			flags = 0x02000002
		}
		if i == len(beats)-1 {
			be.PutUint16(sec[32:], num)
			be.PutUint16(sec[34:], tempo)
			be.PutUint32(sec[36:], timeMS)
		}
		checksum += uint64(num) + uint64(tempo) + uint64(timeMS)
		be.PutUint16(sec[56+2*i:], b.SubUS%1000)
	}
	be.PutUint32(sec[16:], flags)
	be.PutUint32(sec[40:], uint32(len(beats)))
	be.PutUint32(sec[44:], uint32(checksum))
	return sec
}

// RewriteANLZGrid rewrites PQTZ (extended=false, .DAT) or PQT2 (extended=true, .EXT)
// in place in the original file's section order while preserving every other section
// byte-for-byte. For .EXT files that do not carry a PQT2 section, it returns (raw, false, nil).
func RewriteANLZGrid(raw []byte, beats []Beat, extended bool) ([]byte, bool, error) {
	if len(beats) == 0 {
		return nil, false, fmt.Errorf("anlz: empty beat grid")
	}
	h, secs, err := parseANLZSections(raw)
	if err != nil {
		return nil, false, err
	}
	targetTag := "PQTZ"
	var replacement []byte
	if extended {
		targetTag = "PQT2"
		replacement = BuildPQT2(beats)
	} else {
		replacement = BuildPQTZ(beats)
	}

	found := false
	ordered := make([][]byte, 0, len(secs)+1)
	for _, s := range secs {
		if s.tag == targetTag {
			if !found {
				ordered = append(ordered, replacement)
				found = true
			}
			continue
		}
		ordered = append(ordered, s.raw)
	}
	if !found {
		if extended {
			return raw, false, nil
		}
		// Insert PQTZ in rekordbox's .DAT position (after PVBR, or after PPTH).
		insertAt := 0
		for i, s := range secs {
			if s.tag == "PPTH" || s.tag == "PVBR" {
				insertAt = i + 1
			}
		}
		ordered = append(ordered[:insertAt], append([][]byte{replacement}, ordered[insertAt:]...)...)
	}
	return assembleANLZ(raw[:h], ordered), true, nil
}
