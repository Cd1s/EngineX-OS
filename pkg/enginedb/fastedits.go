package enginedb

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"math"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// The row rule of #328. A row the export alone filled (InsertTracks: its cue rows, no analysis)
// may be loaded before its staged data is applied: Engine analyses it, and the DJ may set, move,
// rename, recolour or delete hot cues, save loops, adjust or lock the grid, rate it. Applying the
// staged data keeps what the DJ did, field by field, against what the insert left (fastBase):
//   - hot cue and loop slots one by one: a slot that still holds the insert's (also as moved by
//     Engine's correction of a marked row), or is empty on both, is rekordbox's; any other the
//     DJ's, a loop with its active-on-load bit;
//   - the main cue: the DJ's when Engine flags it adjusted;
//   - the grid: the DJ's when locked or adjusted (realignKept's rule), with the Track's BPM;
//   - the rating: the DJ's when it is not the insert's.
// What the DJ kept is a deck edit from then on: the row gets no Written digest.

// engineLoadShifts: Engine 5.3.0 moves a marked row when it loads it with a decoder (an MP3 by
// 2292 samples) and clears the mark.
var engineLoadShifts = []float64{0, 2292}

// fastBase is what the insert left in a row: the export's rating, and a OneLibrary export's hot
// cues, loops and active-on-load loops without the file's offset, with Engine's mark.
type fastBase struct {
	rating      int64
	cues, loops []byte
	active      int64
}

// FastBaseData is the baseline a fast row carries until its full performance is applied.
type FastBaseData struct {
	Rating int64
	Cues   []byte
	Loops  []byte
	Active uint32
}

// FastBaseForTrack returns the values InsertTracks writes before analysis is staged.
func FastBaseForTrack(t rekordbox.Track) FastBaseData {
	b := fastBaseOf(t)
	return FastBaseData{Rating: b.rating, Cues: b.cues, Loops: b.loops, Active: uint32(b.active)}
}

// FastBaseRecord is the legacy BridgeFastBase fingerprint check for a fast-row record.
func FastBaseRecord(fingerprint string) int64 {
	h := sha256.Sum256([]byte(fingerprint))
	return int64(binary.BigEndian.Uint64(h[:]))
}

// fastBaseOf is the fastBase of export track t, as InsertTracks writes it.
func fastBaseOf(t rekordbox.Track) fastBase {
	b := fastBase{rating: int64(t.Rating)}
	if len(t.Cues) > 0 {
		if t.BPM >= 499.5 {
			t.BPM = 0
		}
		if p, e := encodeCuesOnly(t, t.Cues); e == nil {
			b.cues, b.loops, b.active = p.QuickCues, p.Loops, int64(p.ActiveLoops)
		}
	}
	return b
}

// deckRow is a fast row as the deck left it.
type deckRow struct {
	beats, cues, loops []byte
	active, mark       sql.NullInt64
	locked             bool
	rating             sql.NullInt64
}

// fastMerge is what the DJ did, kept over perf; a nil field is perf's.
type fastMerge struct {
	beats, cues, loops []byte
	active             any
	grid, rating       bool // the DJ's grid (and the Track's BPM with it), the DJ's rating
}

func (m fastMerge) deck() bool {
	return m.beats != nil || m.cues != nil || m.loops != nil || m.active != nil
}

// mergeFast merges the deck's row of a track a fast import left as base into perf.
func mergeFast(row deckRow, base fastBase, perf Performance) fastMerge {
	var m fastMerge
	m.rating = row.rating.Valid && row.rating.Int64 != base.rating
	if row.mark.Valid && row.mark.Int64 != 0 {
		// Engine clears its import mark when it loads the row: never loaded, so nothing of the
		// deck's in it (a rating can be set in the browser, without a load).
		return m
	}
	if row.beats != nil && (row.locked || !engineOwnGrid(row.beats)) {
		m.beats, m.grid = row.beats, true
	}
	rowCues, rowTrailer, ok1 := cueSlots(row.cues)
	baseCues, baseTrailer, ok2 := cueSlots(base.cues)
	newCues, newTrailer, ok3 := cueSlots(perf.QuickCues)
	rowLoops, ok4 := loopSlots(row.loops)
	baseLoops, ok5 := loopSlots(base.loops)
	newLoops, ok6 := loopSlots(perf.Loops)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return m // a blob not in Engine's layout: rekordbox's, as before #328
	}
	// The insert's cues count only if one still is where it put it: Engine's empty slots must not
	// read as the DJ deleting what was never there (a DJ who deleted all gets rekordbox's back).
	seen := false
	for i := range rowCues {
		seen = seen || !emptyCue(baseCues[i]) && !emptyCue(rowCues[i]) && shiftedBy(cuePos(baseCues[i]), cuePos(rowCues[i]))
	}
	for i := range rowLoops {
		seen = seen || i < len(baseLoops) && !emptyLoop(baseLoops[i]) && !emptyLoop(rowLoops[i]) && sameLoopAt(rowLoops[i], baseLoops[i])
	}
	baseActive := base.active
	if !seen {
		baseCues, baseTrailer, baseLoops, baseActive = [8][]byte{}, nil, nil, 0
	}
	if row.cues != nil && newTrailer != nil {
		var cues bytes.Buffer
		changed := false
		cues.Write(cueCount)
		for i := range rowCues {
			slot := mergeCue(rowCues[i], baseCues[i], newCues[i])
			changed = changed || !bytes.Equal(slot, newCues[i])
			cues.Write(slot)
		}
		trailer := newTrailer
		if rowTrailer[8] != 0 && (baseTrailer == nil || !sameMainCue(rowTrailer, baseTrailer)) {
			// The DJ's main cue, with rekordbox's as the one imported.
			trailer = append(append([]byte(nil), rowTrailer[:9]...), newTrailer[9:]...)
			changed = changed || !bytes.Equal(trailer, newTrailer)
		}
		cues.Write(trailer)
		if changed {
			if enc, e := QCompress(cues.Bytes()); e == nil {
				m.cues = enc
			}
		}
	}
	if len(rowLoops) == 0 || len(rowLoops) != len(newLoops) {
		return m
	}
	loops := append([]byte(nil), perf.Loops[:8]...)
	active := int64(perf.ActiveLoops)
	changed := false
	for i := range rowLoops {
		slot, bit := newLoops[i], int64(1)<<uint(i)
		var b []byte
		if i < len(baseLoops) {
			b = baseLoops[i]
		}
		slot = mergeLoop(rowLoops[i], b, newLoops[i])
		switch {
		case !bytes.Equal(slot, newLoops[i]) && !(emptyLoop(slot) && emptyLoop(newLoops[i])):
			active = active&^bit | row.active.Int64&bit // the DJ's loop, and whether it is active on load
		case !emptyLoop(rowLoops[i]) && row.active.Int64&bit != baseActive&bit:
			active = active&^bit | row.active.Int64&bit // only switched on or off at the deck
		}
		changed = changed || !bytes.Equal(slot, newLoops[i])
		loops = append(loops, slot...)
	}
	if changed {
		m.loops = loops
	}
	if active != int64(perf.ActiveLoops) {
		m.active = active
	}
	return m
}

// engineOwnGrid reports whether beatData is a grid Engine analysed and nobody adjusted: its
// adjusted grid is its default one (realignKept). One it cannot read counts as Engine's.
func engineOwnGrid(beats []byte) bool {
	raw, e := QUncompress(beats, 8<<20)
	if e != nil {
		return true
	}
	def, adj, ok := beatGrids(raw)
	return !ok || bytes.Equal(def, adj)
}

// cueCount is quickCues' slot count.
var cueCount = []byte{0, 0, 0, 0, 0, 0, 0, 8}

// cueSlots splits uncompressed quickCues into its eight hot cue slots (label length, label,
// position, colour) and the main cue trailer (position, adjusted flag, position as imported).
// A nil blob is eight empty slots and no trailer.
func cueSlots(blob []byte) (slots [8][]byte, trailer []byte, ok bool) {
	if blob == nil {
		return slots, nil, true
	}
	raw, e := QUncompress(blob, 64<<10)
	if e != nil {
		return slots, nil, false
	}
	at, ok := cuePositions(raw)
	if !ok {
		return slots, nil, false
	}
	start := 8
	for i := 0; i < 8; i++ {
		slots[i] = raw[start : at[i]+12]
		start = at[i] + 12
	}
	return slots, raw[at[8]:], true
}

// loopSlots splits a loops blob into its slots (label length, label, start, end, the two set
// flags, colour). A nil blob has none.
func loopSlots(blob []byte) ([][]byte, bool) {
	if blob == nil {
		return nil, true
	}
	at, ok := loopPositions(blob)
	if !ok {
		return nil, false
	}
	var out [][]byte
	start := 8
	for i := 0; i < len(at); i += 2 {
		out = append(out, blob[start:at[i]+22])
		start = at[i] + 22
	}
	return out, true
}

func cuePos(slot []byte) float64 {
	return math.Float64frombits(big.Uint64(slot[len(slot)-12:]))
}

func emptyCue(slot []byte) bool { return slot == nil || cuePos(slot) < 0 }

// mergeCue is a hot cue slot: rekordbox's (now) where the row holds the insert's (base), also as
// moved by Engine's correction; the DJ's where the DJ set, moved or deleted it; the DJ's name and
// colour at rekordbox's position where the DJ only renamed or recoloured it.
func mergeCue(row, base, now []byte) []byte {
	switch {
	case emptyCue(row) && (emptyCue(base) || emptyCue(now)):
		return now
	case emptyCue(row) || emptyCue(base) || !shiftedBy(cuePos(base), cuePos(row)):
		return row // deleted, set, or moved at the deck
	case bytes.Equal(row[:len(row)-12], base[:len(base)-12]) && bytes.Equal(row[len(row)-4:], base[len(base)-4:]) || emptyCue(now):
		if emptyCue(now) {
			return row
		}
		return now
	}
	out := append([]byte(nil), row...)
	copy(out[len(out)-12:len(out)-4], now[len(now)-12:len(now)-4])
	return out
}

// sameMainCue: the main cue of a row's trailer still is base's.
func sameMainCue(row, base []byte) bool {
	return shiftedBy(math.Float64frombits(big.Uint64(base)), math.Float64frombits(big.Uint64(row)))
}

// shiftedBy reports whether position row is base, or base moved by Engine's own correction.
func shiftedBy(base, row float64) bool {
	for _, s := range engineLoadShifts {
		if math.Abs(base-s-row) < 0.5 {
			return true
		}
	}
	return false
}

func loopAt(slot []byte) (start, end float64, set []byte) {
	p := slot[len(slot)-22:]
	return math.Float64frombits(little.Uint64(p)), math.Float64frombits(little.Uint64(p[8:])), p[16:18]
}

func emptyLoop(slot []byte) bool {
	if slot == nil {
		return true
	}
	start, end, set := loopAt(slot)
	return start < 0 && end < 0 || set[0] == 0 && set[1] == 0
}

// sameLoopAt: a row's loop slot still starts and ends where base's does, or moved by Engine's
// own correction.
func sameLoopAt(row, base []byte) bool {
	rs, re, _ := loopAt(row)
	bs, be, _ := loopAt(base)
	for _, s := range engineLoadShifts {
		if math.Abs(bs-s-rs) < 0.5 && math.Abs(be-s-re) < 0.5 {
			return true
		}
	}
	return false
}

// mergeLoop is mergeCue for a saved loop slot: start and end, the set flags and the colour.
func mergeLoop(row, base, now []byte) []byte {
	switch {
	case emptyLoop(row) && (emptyLoop(base) || emptyLoop(now)):
		return now
	case emptyLoop(row) || emptyLoop(base) || !sameLoopAt(row, base):
		return row
	case bytes.Equal(row[:len(row)-22], base[:len(base)-22]) && bytes.Equal(row[len(row)-6:], base[len(base)-6:]) || emptyLoop(now):
		if emptyLoop(now) {
			return row
		}
		return now
	}
	out := append([]byte(nil), row...)
	copy(out[len(out)-22:len(out)-6], now[len(now)-22:len(now)-6])
	return out
}
