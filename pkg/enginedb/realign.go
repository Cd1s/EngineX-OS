package enginedb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
)

// A row the deck changed keeps its grid, cues and loops (ApplyPerformance), but they also hold the
// offset of the import that wrote them: a stick imported before #178 kept the raw rekordbox
// positions, an M4A the positions without its priming (real decks, 2026-11-19). realignKept moves
// what is still the import's to this import's positions; what the deck changed stays. A row is the
// import's while it holds what the bridge wrote (Track.Written, WrittenDigest): Engine's import
// mark is not used, since Engine shifts a marked row again when it loads it (2292 samples, MP3).

func writtenDigestBytes(beatsRaw, cuesRaw, loops []byte) []byte {
	h := sha256.New()
	for _, raw := range [][]byte{beatsRaw, cuesRaw} {
		_ = binary.Write(h, big, uint64(len(raw)))
		h.Write(raw)
	}
	h.Write(loops)
	return h.Sum(nil)[:16]
}

// writtenDigests decompresses beats and cues once and returns the normalized WrittenDigest and
// the un-normalized RawWrittenDigest. When quickCues is already adjusted (or not an 8-slot cue
// blob), both digests are identical and hashed once.
func writtenDigests(beats, cues, loops []byte) (norm, raw []byte) {
	beatsRaw, err := QUncompress(beats, 8<<20)
	if err != nil {
		beatsRaw = beats
	}
	cuesRaw, err := QUncompress(cues, 8<<20)
	if err != nil {
		cuesRaw = cues
	}
	raw = writtenDigestBytes(beatsRaw, cuesRaw, loops)
	normCues := normalizeMainCueForWrittenDigest(cuesRaw)
	if len(normCues) == len(cuesRaw) && (len(normCues) == 0 || &normCues[0] == &cuesRaw[0]) {
		return raw, raw
	}
	return writtenDigestBytes(beatsRaw, normCues, loops), raw
}

// RawWrittenDigest names the content of a row's grid, cues and loops without main-cue normalization.
func RawWrittenDigest(beats, cues, loops []byte) []byte {
	beatsRaw, err := QUncompress(beats, 8<<20)
	if err != nil {
		beatsRaw = beats
	}
	cuesRaw, err := QUncompress(cues, 8<<20)
	if err != nil {
		cuesRaw = cues
	}
	return writtenDigestBytes(beatsRaw, cuesRaw, loops)
}

// WrittenDigest names the content of a row's grid, cues and loops, however it is compressed.
func WrittenDigest(beats, cues, loops []byte) []byte {
	beatsRaw, err := QUncompress(beats, 8<<20)
	if err != nil {
		beatsRaw = beats
	}
	cuesRaw, err := QUncompress(cues, 8<<20)
	if err != nil {
		cuesRaw = cues
	}
	return writtenDigestBytes(beatsRaw, normalizeMainCueForWrittenDigest(cuesRaw), loops)
}

func normalizeMainCueForWrittenDigest(raw []byte) []byte {
	at, ok := cuePositions(raw)
	if !ok || raw[at[8]+8] != 0 {
		return raw
	}
	out := append([]byte(nil), raw...)
	clear(out[at[8] : at[8]+8])
	clear(out[at[9]:])
	return out
}

// UnadjustedLegacyMatches reports whether an unadjusted (adj==0) row whose main cue Engine rewrote
// on load still matches a pre-normalization RawWrittenDigest stored by v1 or 9f2c0770 (either with
// main=-1 for a track without a memory cue, its default main cue, stagedCues' memory cue position,
// or any candidateMains supplied by write-back from the track's ANLZ memory cues).
func UnadjustedLegacyMatches(beats, cues, loops, stagedCues, prevWritten []byte, candidateMains ...float64) bool {
	if prevWritten == nil {
		return false
	}
	cuesRaw, err := QUncompress(cues, 8<<20)
	if err != nil {
		return false
	}
	at, ok := cuePositions(cuesRaw)
	if !ok || cuesRaw[at[8]+8] != 0 {
		return false
	}
	beatsRaw, err := QUncompress(beats, 8<<20)
	if err != nil {
		beatsRaw = beats
	}
	buf := append([]byte(nil), cuesRaw...)
	tryMain := func(pos float64) bool {
		big.PutUint64(buf[at[8]:], math.Float64bits(pos))
		buf[at[8]+8] = 0
		big.PutUint64(buf[at[9]:], math.Float64bits(pos))
		return bytes.Equal(writtenDigestBytes(beatsRaw, buf, loops), prevWritten)
	}
	if tryMain(-1) {
		return true
	}
	if def := math.Float64frombits(big.Uint64(cuesRaw[at[9]:])); def >= 0 && tryMain(def) {
		return true
	}
	if refRaw, err := QUncompress(stagedCues, 64<<10); err == nil {
		if rat, ok := cuePositions(refRaw); ok {
			if main := math.Float64frombits(big.Uint64(refRaw[rat[8]:])); main >= 0 && tryMain(main) {
				return true
			}
		}
	}
	for _, pos := range candidateMains {
		if pos >= 0 && tryMain(pos) {
			return true
		}
	}
	return false
}

func unadjustedLegacyMatches(beats, cues, loops, stagedCues, prevWritten []byte) bool {
	return UnadjustedLegacyMatches(beats, cues, loops, stagedCues, prevWritten)
}

// MetadataDigest hashes the editable metadata fields write-back syncs (genre, comment, label)
// so an unchanged on-demand track load can verify metadata parity without decrypting exports.
func MetadataDigest(genre, comment, label string) []byte {
	h := sha256.New()
	for _, s := range []string{genre, comment, label} {
		_ = binary.Write(h, big, uint32(len(s)))
		h.Write([]byte(s))
	}
	return h.Sum(nil)[:16]
}

// SyncedDigest names what the deck can change in a row and cue write-back writes back: cues,
// loops, activeOnLoadLoops (#321), genre, comment and label (#314). The grid and the analysis are
// left out, so that an analysis does not set off a write-back.
func SyncedDigest(_ []byte, cues, loops []byte, activeOnLoadLoops int64, genre, comment, label string) []byte {
	h := sha256.New()
	rawCues, err := QUncompress(cues, 8<<20)
	if err != nil {
		rawCues = cues
	}
	rawCues = normalizeMainCueForWrittenDigest(rawCues)
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

// realignKept returns the row's three blobs moved to perf's positions. The row's default grid
// says how far off it is: perf's grid with every marker the same distance away, or the row is
// left alone (rekordbox's grid has changed since, or it is not ours).
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
	// A position is the import's when it is this import's of the same slot, the same distance
	// away; slot by slot, since a cue the DJ named or added moves the bytes of the slots after it.
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
