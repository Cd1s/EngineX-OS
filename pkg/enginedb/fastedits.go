package enginedb

import (
	"bytes"
	"database/sql"
	"math"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// A track a fast import listed (fingerprint "fast:" + the export record's hash) has no analysis
// until its first full import, minutes later on a large stick (#301). A deck that loads it
// meanwhile has Engine analyse it, and the DJ may edit it then: set, move, rename, recolour or
// delete hot cues, save loops, adjust or lock the grid, rate it (#328). The full import gives
// the row rekordbox's analysis and keeps what the DJ did, field by field, against what the fast
// import left in the row (fastBase):
//
//   - hot cue and saved loop slots, one by one: a slot that still holds what the fast import
//     wrote, or the same moved by Engine's own correction of a marked row, or that is empty on
//     both, is rekordbox's; any other is the DJ's. A loop's active-on-load bit goes with its slot.
//   - the main cue: the DJ's when Engine says it was adjusted (the flag after the hot cues).
//   - the grid: the DJ's when it is locked or its adjusted grid is not Engine's own analysed one
//     (the rule of realignKept), with the Track's BPM; Engine's own analysis is replaced.
//   - the rating: the DJ's when it is not the one the fast import wrote.
//
// What the DJ kept is a deck edit from then on: the row is not what the import wrote, so it has
// no BridgeWritten digest, and the next import keeps it (importKeeps) as it keeps any deck edit.

// engineLoadShifts are what Engine 5.3.0 moves a marked row by when it loads it with a decoder
// attached (one_offset_rule_test: an MP3 by 2292 samples); it clears the mark either way.
var engineLoadShifts = []float64{0, 2292}

// fastBase is what a fast import left in a track's row for an export record: the record's rating,
// and a OneLibrary export's hot cues, loops and active-on-load loops without the file's offset,
// with Engine's mark.
type fastBase struct {
	rating      int64
	cues, loops []byte
	active      int64
}

// fastBaseOf is the fastBase of export track t, as the fast import computes it (sync.go).
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

// fastMerge is the row the full import leaves: perf's, with what the DJ did kept. A nil field
// is perf's.
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
	// The fast import's cues are in the row only if one of them still is where it put it: a
	// track listed fast without them (an in-place import out of time) has none, and Engine's
	// empty slots must not read as the DJ deleting what was never there. A DJ who deleted every
	// one of them gets rekordbox's back, as before #328.
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

// mergeCue is a hot cue slot after the full import: rekordbox's (now) where the row still holds
// what the fast import put in it (base), or that moved by Engine's own correction; the DJ's where
// the DJ set, moved or deleted it; the DJ's name and colour at rekordbox's position where the DJ
// renamed or recoloured it without moving it.
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
