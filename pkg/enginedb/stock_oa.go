package enginedb

import (
	"bytes"
	"database/sql"
	"math"
)

// StockOverwrittenRow describes the existing PerformanceData + Track lock state of a
// rekordbox-converted row (Track.pdbImportKey > 0) before deciding whether stock Engine
// analysis (or an aborted empty-cue load stub) overwrote it without any DJ edits.
type StockOverwrittenRow struct {
	TrackData          []byte
	Overview           []byte
	Beats              []byte
	QuickCues          []byte
	Loops              []byte
	ActiveOnLoadLoops  int64
	ThirdPartySourceID sql.NullInt64
	BeatGridLocked     bool
	// Optional baseline from .rb-bridge-snapshot for OneLibrary fast-imported rows whose
	// export cues/loops were written at mount time before stock Engine OA ran.
	FastKnown  bool
	FastCues   []byte
	FastLoops  []byte
	FastActive uint32
}

// IsStockOverwrittenPerformance reports whether row matches a rekordbox-converted row that
// stock Engine analysis (or an aborted empty-cue load stub) wrote over, with zero DJ-made
// cues, loops, main-cue moves, beatgrid edits, or beatgrid lock.
//
// It fails closed (returns false) on any DJ edit, any EngineX rekordbox-converted beatgrid
// (whose markers have beats_until_next == 1), or any unknown/corrupt blob.
func IsStockOverwrittenPerformance(row StockOverwrittenRow) bool {
	if row.BeatGridLocked {
		return false
	}
	if row.ThirdPartySourceID.Valid && row.ThirdPartySourceID.Int64 != 0 {
		return false
	}
	hasFastBase := row.FastKnown && (len(row.FastCues) > 0 || len(row.FastLoops) > 0 || row.FastActive != 0)
	if !hasFastBase {
		if row.ActiveOnLoadLoops != 0 || !loopsAreCompletelyEmpty(row.Loops) {
			return false
		}
		// Subcase A: aborted/partial load left trackData/overview/beatData NULL and only an
		// all-empty unadjusted quickCues stub (no hot cues, main cue not adjusted).
		if len(row.TrackData) == 0 && len(row.Overview) == 0 && len(row.Beats) == 0 {
			if len(row.QuickCues) == 0 {
				return false // ordinary untouched insert, handled by the normal fill path
			}
			return cuesAreStockOAUnedited(row.QuickCues, 0, false)
		}
		// Subcase B: stock Engine OfflineAnalyzer wrote a 2-marker constant-BPM grid
		// (default == adjusted, marker[0].beat <= 0, marker[0].beats_until_next == b1-b0 > 1,
		// marker[1].beats_until_next == 0) with zero hot cues, zero loops, and either an
		// unadjusted main cue or stock Engine's automatic beat-0 cue (main == default == beat 0).
		beat0Sample, ok := stockOABeat0Sample(row.Beats)
		if !ok {
			return false
		}
		return cuesAreStockOAUnedited(row.QuickCues, beat0Sample, true)
	}

	// OneLibrary fast-import baseline: Engine cleared thirdPartySourceId on load and may have
	// shifted MP3 cue/loop positions by a uniform engineLoadShifts value (0 or 2292 samples),
	// while stock OA overwrote beatData. Require activeOnLoadLoops, every cue slot, and every
	// loop slot to still match the mount-time rekordbox baseline with zero DJ edits.
	if row.ActiveOnLoadLoops != int64(row.FastActive) {
		return false
	}
	if len(row.TrackData) == 0 && len(row.Overview) == 0 && len(row.Beats) == 0 {
		if len(row.QuickCues) == 0 && len(row.Loops) == 0 {
			return false
		}
		return cuesAndLoopsMatchFastBase(row.QuickCues, row.Loops, row.FastCues, row.FastLoops, 0, false)
	}
	beat0Sample, ok := stockOABeat0Sample(row.Beats)
	if !ok {
		return false
	}
	return cuesAndLoopsMatchFastBase(row.QuickCues, row.Loops, row.FastCues, row.FastLoops, beat0Sample, true)
}

// stockOABeat0Sample returns (beat0Sample, true) iff beats is an unedited stock Engine
// OfflineAnalyzer 2-marker constant-BPM beatData blob.
func stockOABeat0Sample(beats []byte) (float64, bool) {
	if len(beats) == 0 || len(beats) > 256 {
		return 0, false
	}
	raw, err := QUncompress(beats, 4096)
	if err != nil || len(raw) < 129 || raw[16] != 1 {
		return 0, false
	}
	sr := displayFloat(raw[0:8], big)
	samples := displayFloat(raw[8:16], big)
	if !displayFinite(sr) || sr < 8000 || sr > 384000 || !displayFinite(samples) || samples <= 0 {
		return 0, false
	}
	def, adj, ok := beatGrids(raw)
	if !ok || !bytes.Equal(def, adj) {
		return 0, false
	}
	// Stock Engine OfflineAnalyzer always emits exactly 2 constant-BPM markers:
	// marker 0 at beat <= 0 (typically -4, bounded to [-64, 0]) with beats_until_next == b1 - b0 > 1,
	// and marker 1 at track end (beat > 0) with beats_until_next == 0.
	// EngineX EncodePerformance always emits beat >= 0 and beats_until_next == 1 per marker.
	if len(def) != 8+2*24 || big.Uint64(def[:8]) != 2 {
		return 0, false
	}
	p0 := displayFloat(def[8:16], little)
	b0 := int64(little.Uint64(def[16:24]))
	next0 := little.Uint32(def[24:28])
	p1 := displayFloat(def[32:40], little)
	b1 := int64(little.Uint64(def[40:48]))
	next1 := little.Uint32(def[48:52])
	if !displayFinite(p0) || !displayFinite(p1) || p1 <= p0 {
		return 0, false
	}
	if b0 < -64 || b0 > 0 || b1 <= 0 || next0 <= 1 || next1 != 0 || int64(next0) != b1-b0 {
		return 0, false
	}
	bpm := 60.0 * sr * float64(b1-b0) / (p1 - p0)
	if !displayFinite(bpm) || bpm < 20.0 || bpm > 500.0 {
		return 0, false
	}
	beat0 := p0 + float64(-b0)*(p1-p0)/float64(b1-b0)
	if !displayFinite(beat0) {
		return 0, false
	}
	return beat0, true
}

func isAutoMainAtBeat0(trailer []byte, beat0Sample float64) bool {
	if len(trailer) != 17 || trailer[8] == 0 {
		return false
	}
	mainPos := displayFloat(trailer[0:8], big)
	defPos := displayFloat(trailer[9:17], big)
	if !displayFinite(mainPos) || !displayFinite(defPos) {
		return false
	}
	// Stock Engine's auto-cue sets adjustedMain == defaultMain == beat 0 of the OA grid.
	// Any DJ cue move separates adjustedMain from defaultMain and/or moves it off beat 0.
	return math.Abs(mainPos-defPos) <= 1.0 && math.Abs(mainPos-beat0Sample) <= 2.0
}

func cuesAreStockOAUnedited(cues []byte, beat0Sample float64, allowAutoMainAtBeat0 bool) bool {
	if len(cues) == 0 {
		return true
	}
	slots, trailer, ok := cueSlots(cues)
	if !ok || len(trailer) != 17 {
		return false
	}
	for _, s := range slots {
		// Any named slot or any slot with position >= 0 is a hot cue (never overwrite).
		if len(s) != 1+12 || s[0] != 0 || cuePos(s) >= 0 {
			return false
		}
	}
	if trailer[8] == 0 {
		return true
	}
	return allowAutoMainAtBeat0 && isAutoMainAtBeat0(trailer, beat0Sample)
}

func loopsAreCompletelyEmpty(loops []byte) bool {
	if len(loops) == 0 {
		return true
	}
	slots, ok := loopSlots(loops)
	if !ok {
		return false
	}
	for _, s := range slots {
		if len(s) != 1+22 || s[0] != 0 {
			return false
		}
		start, end, set := loopAt(s)
		if start >= 0 || end >= 0 || set[0] != 0 || set[1] != 0 {
			return false
		}
	}
	return true
}

func cuesAndLoopsMatchFastBase(rowCuesBlob, rowLoopsBlob, baseCuesBlob, baseLoopsBlob []byte, beat0Sample float64, allowAutoMainAtBeat0 bool) bool {
	rowCues, rowTrailer, ok1 := cueSlots(rowCuesBlob)
	baseCues, baseTrailer, ok2 := cueSlots(baseCuesBlob)
	rowLoops, ok3 := loopSlots(rowLoopsBlob)
	baseLoops, ok4 := loopSlots(baseLoopsBlob)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return false
	}
	// Every non-empty cue/loop slot must match the OneLibrary baseline under ONE uniform
	// Engine load shift (0 or 2292 samples), with identical labels, colors, and set flags.
	for _, shift := range engineLoadShifts {
		if fastSlotsMatchWithShift(rowCues, rowTrailer, baseCues, baseTrailer, rowLoops, baseLoops, shift, beat0Sample, allowAutoMainAtBeat0) {
			return true
		}
	}
	return false
}

func fastSlotsMatchWithShift(
	rowCues [8][]byte, rowTrailer []byte,
	baseCues [8][]byte, baseTrailer []byte,
	rowLoops, baseLoops [][]byte,
	shift, beat0Sample float64, allowAutoMainAtBeat0 bool,
) bool {
	for i := 0; i < 8; i++ {
		rs, bs := rowCues[i], baseCues[i]
		rEmpty := len(rs) == 0 || (len(rs) == 1+12 && rs[0] == 0 && cuePos(rs) < 0)
		bEmpty := len(bs) == 0 || (len(bs) == 1+12 && bs[0] == 0 && cuePos(bs) < 0)
		if rEmpty && bEmpty {
			continue
		}
		if rEmpty != bEmpty || len(rs) < 13 || len(bs) < 13 {
			return false
		}
		// Label bytes and color bytes must match identically.
		if !bytes.Equal(rs[:len(rs)-12], bs[:len(bs)-12]) || !bytes.Equal(rs[len(rs)-4:], bs[len(bs)-4:]) {
			return false
		}
		rp, bp := cuePos(rs), cuePos(bs)
		if !displayFinite(rp) || !displayFinite(bp) || math.Abs((bp-shift)-rp) >= 0.5 {
			return false
		}
	}
	// Main cue check.
	baseMainAdj := len(baseTrailer) == 17 && baseTrailer[8] != 0
	rowMainAdj := len(rowTrailer) == 17 && rowTrailer[8] != 0
	switch {
	case !baseMainAdj && !rowMainAdj:
		// Both unadjusted (Engine may rewrite main/def to 0 or -1105 on load).
	case !baseMainAdj && rowMainAdj:
		if !allowAutoMainAtBeat0 || !isAutoMainAtBeat0(rowTrailer, beat0Sample) {
			return false
		}
	case baseMainAdj && !rowMainAdj:
		return false
	default:
		rm := displayFloat(rowTrailer[0:8], big)
		bm := displayFloat(baseTrailer[0:8], big)
		if !displayFinite(rm) || !displayFinite(bm) || math.Abs((bm-shift)-rm) >= 0.5 {
			return false
		}
	}
	// Loops check.
	nLoops := max(len(rowLoops), len(baseLoops))
	for i := 0; i < nLoops; i++ {
		var rs, bs []byte
		if i < len(rowLoops) {
			rs = rowLoops[i]
		}
		if i < len(baseLoops) {
			bs = baseLoops[i]
		}
		rEmpty := loopSlotStrictlyEmpty(rs)
		bEmpty := loopSlotStrictlyEmpty(bs)
		if rEmpty && bEmpty {
			continue
		}
		if rEmpty != bEmpty || len(rs) < 23 || len(bs) < 23 {
			return false
		}
		// Label bytes, set flags, and color bytes must match identically.
		if !bytes.Equal(rs[:len(rs)-22], bs[:len(bs)-22]) || !bytes.Equal(rs[len(rs)-6:], bs[len(bs)-6:]) {
			return false
		}
		rStart, rEnd, _ := loopAt(rs)
		bStart, bEnd, _ := loopAt(bs)
		if !displayFinite(rStart) || !displayFinite(rEnd) || !displayFinite(bStart) || !displayFinite(bEnd) {
			return false
		}
		if math.Abs((bStart-shift)-rStart) >= 0.5 || math.Abs((bEnd-shift)-rEnd) >= 0.5 {
			return false
		}
	}
	return true
}

func loopSlotStrictlyEmpty(slot []byte) bool {
	if len(slot) == 0 {
		return true
	}
	if len(slot) != 1+22 || slot[0] != 0 {
		return false
	}
	start, end, set := loopAt(slot)
	return start < 0 && end < 0 && set[0] == 0 && set[1] == 0
}

// BeatDataFirstBPM returns the opening BPM of an Engine qCompress beatData blob, or 0 if invalid.
func BeatDataFirstBPM(beats []byte) float64 {
	raw, err := QUncompress(beats, 8<<20)
	if err != nil || len(raw) < 17+8+2*24 || raw[16] != 1 {
		return 0
	}
	sr := displayFloat(raw[0:8], big)
	if !displayFinite(sr) || sr < 8000 || sr > 384000 {
		return 0
	}
	def, adj, ok := beatGrids(raw)
	if !ok {
		return 0
	}
	g := adj
	if len(g) < 8+2*24 {
		g = def
	}
	if len(g) < 8+2*24 {
		return 0
	}
	p0 := displayFloat(g[8:16], little)
	b0 := int64(little.Uint64(g[16:24]))
	p1 := displayFloat(g[32:40], little)
	b1 := int64(little.Uint64(g[40:48]))
	if !displayFinite(p0) || !displayFinite(p1) || p1 <= p0 || b1 <= b0 {
		return 0
	}
	bpm := 60.0 * sr * float64(b1-b0) / (p1 - p0)
	if !displayFinite(bpm) || bpm < 20.0 || bpm > 500.0 {
		return 0
	}
	return bpm
}
