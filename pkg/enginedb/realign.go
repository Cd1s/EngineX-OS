package enginedb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
)

// A row Engine has loaded keeps its grid, cues and loops at the next import (sync.go): they
// may hold the deck's edits. They also hold the offset of the import that wrote them, and
// Engine's own shift if it loaded the row while it carried the import mark: a stick imported
// before #178 kept the raw rekordbox positions for good, an M4A the positions without its
// priming (real decks, 2026-11-19). What is still the import's is moved to this import's
// positions here; what the deck changed stays where it is.

// A row is the import's as long as it holds what the import left in it; BridgeWritten has the
// digest of that. Engine's import mark (thirdPartySourceId) said the same until Engine loaded
// the row, but with it Engine shifts the row again (2292 samples for an MP3) whenever it loads
// it with a decoder attached, so no row carries it any more. A row of before this record
// still has the mark, or has lost it to Engine.

// WrittenDigest names the content of a row's grid, cues and loops, however it is compressed.
func WrittenDigest(beats, cues, loops []byte) []byte {
	return writtenDigest(beats, cues, loops)
}

// writtenDigest names the content of a row's grid, cues and loops, however it is compressed.
func writtenDigest(beats, cues, loops []byte) []byte {
	h := sha256.New()
	for _, blob := range [][]byte{beats, cues} {
		raw, err := QUncompress(blob, 8<<20)
		if err != nil {
			raw = blob
		}
		_ = binary.Write(h, big, uint64(len(raw)))
		h.Write(raw)
	}
	h.Write(loops)
	return h.Sum(nil)[:16]
}

// SyncedDigest names what the deck can change in a row and cue write-back writes
// back to the stick's rekordbox files: cues, loops, activeOnLoadLoops (#321), and
// genre, comment, and label (#314). Beat-grid / analysis-only updates (beatData,
// trackData, overviewWaveFormData) are intentionally excluded so analysis passes
// do not trigger needless OneLibrary decryption.
func SyncedDigest(_ []byte, cues, loops []byte, activeOnLoadLoops int64, genre, comment, label string) []byte {
	h := sha256.New()
	rawCues, err := QUncompress(cues, 8<<20)
	if err != nil {
		rawCues = cues
	}
	_ = binary.Write(h, big, uint64(len(rawCues)))
	h.Write(rawCues)
	_ = binary.Write(h, big, uint64(len(loops)))
	h.Write(loops)
	_ = binary.Write(h, big, activeOnLoadLoops)
	for _, s := range []string{genre, comment, label} {
		_ = binary.Write(h, big, uint32(len(s)))
		h.Write([]byte(s))
	}
	return h.Sum(nil)[:16]
}

// realignKept returns the row's three blobs moved to perf's positions. The row's default
// grid says how far off it is: it has to be perf's grid, every marker the same distance
// away, or the row is left alone (rekordbox's grid has changed since, or it is not ours).
func realignKept(beats, cues, loops []byte, perf Performance, locked bool) ([]byte, []byte, []byte, bool) {
	rowRaw, e1 := QUncompress(beats, 8<<20)
	newRaw, e2 := QUncompress(perf.Beats, 8<<20)
	rowDef, rowAdj, ok1 := beatGrids(rowRaw)
	newDef, _, ok2 := beatGrids(newRaw)
	if e1 != nil || e2 != nil || !ok1 || !ok2 || len(rowDef) != len(newDef) || len(rowDef) < 8+24 || !bytes.Equal(rowRaw[:8], newRaw[:8]) {
		return beats, cues, loops, false
	}
	at := func(grid []byte, i int) float64 { return math.Float64frombits(little.Uint64(grid[8+24*i:])) }
	shift := at(rowDef, 0) - at(newDef, 0)
	for i := 0; i < (len(rowDef)-8)/24; i++ {
		if d := at(rowDef, i) - at(newDef, i) - shift; !(math.Abs(d) < 0.01) {
			return beats, cues, loops, false
		}
	}
	if !(math.Abs(shift) >= 0.5 && math.Abs(shift) <= 16384) {
		return beats, cues, loops, false
	}
	changed := false
	// A grid the deck has not adjusted is the import's.
	if !locked && bytes.Equal(rowDef, rowAdj) {
		beats, changed = perf.Beats, true
	}
	// A position is still the import's when it is this import's of the same slot, the same
	// distance away. Slot by slot: a cue the DJ named or added on the deck moves the bytes
	// of every slot after it, and must not keep the import's other cues where they were.
	move := func(row, now []byte, rowAt, nowAt []int, order binary.ByteOrder) bool {
		moved := false
		for i, off := range rowAt {
			p, n := math.Float64frombits(order.Uint64(row[off:])), math.Float64frombits(order.Uint64(now[nowAt[i]:]))
			if p >= 0 && n >= 0 && p != n && math.Abs(p-(n+shift)) < 0.5 {
				order.PutUint64(row[off:], math.Float64bits(n))
				moved = true
			}
		}
		return moved
	}
	if rowCues, e := QUncompress(cues, 64<<10); e == nil {
		newCues, e := QUncompress(perf.QuickCues, 64<<10)
		rowAt, ok1 := cuePositions(rowCues)
		newAt, ok2 := cuePositions(newCues)
		if e == nil && ok1 && ok2 && move(rowCues, newCues, rowAt, newAt, big) {
			if enc, e := QCompress(rowCues); e == nil {
				cues, changed = enc, true
			}
		}
	}
	rowAt, ok1 := loopPositions(loops)
	newAt, ok2 := loopPositions(perf.Loops)
	if ok1 && ok2 && len(rowAt) == len(newAt) {
		out := append([]byte(nil), loops...)
		if move(out, perf.Loops, rowAt, newAt, little) {
			loops, changed = out, true
		}
	}
	return beats, cues, loops, changed
}

// beatGrids returns the default and the adjusted grid of uncompressed beatData, each as its
// marker count and markers.
func beatGrids(raw []byte) (def, adj []byte, ok bool) {
	next := func(from int) ([]byte, int) {
		if from < 0 || len(raw)-from < 8 {
			return nil, -1
		}
		n := big.Uint64(raw[from:])
		if n > uint64(len(raw)-from-8)/24 {
			return nil, -1
		}
		end := from + 8 + 24*int(n)
		return raw[from:end], end
	}
	def, end := next(17)
	adj, end = next(end)
	return def, adj, end > 0
}

// cuePositions returns where the positions are in uncompressed quickCues: the eight hot cues,
// then the main cue and the main cue as imported.
func cuePositions(raw []byte) ([]int, bool) {
	if len(raw) < 8 || big.Uint64(raw) != 8 {
		return nil, false
	}
	at, off := make([]int, 0, 10), 8
	for i := 0; i < 8; i++ {
		if off >= len(raw) || len(raw)-off-1 < int(raw[off])+12 {
			return nil, false
		}
		off += 1 + int(raw[off])
		at = append(at, off)
		off += 12
	}
	if off+17 != len(raw) {
		return nil, false
	}
	return append(at, off, off+9), true
}

// loopPositions returns where each slot's start and end are in a loops blob; one that is
// not set reads -1 there.
func loopPositions(blob []byte) ([]int, bool) {
	if len(blob) < 8 || blob[0] > 8 || !bytes.Equal(blob[1:8], make([]byte, 7)) {
		return nil, false
	}
	var at []int
	off := 8
	for i := 0; i < int(blob[0]); i++ {
		if off >= len(blob) || len(blob)-off-1 < int(blob[off])+22 {
			return nil, false
		}
		off += 1 + int(blob[off])
		at = append(at, off, off+8)
		off += 22
	}
	return at, off == len(blob)
}
