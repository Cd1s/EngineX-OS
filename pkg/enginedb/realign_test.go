package enginedb

import (
	"bytes"
	"math"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// What a row Engine has loaded kept from an import under an older offset is moved to this
// import's positions; what the deck changed stays (real decks, 2026-11-19: a stick's rows
// frozen at the raw rekordbox position, at Engine's own -2292, and an M4A without its priming).
func TestRealignKeptMovesTheImportsPositionsAndKeepsTheDecks(t *testing.T) {
	analysis := func(bpm float64) *rekordbox.Analysis {
		a := &rekordbox.Analysis{
			Cues: []rekordbox.Cue{
				{HotCue: 1, TimeMS: 550, Name: "A"},
				{HotCue: 2, TimeMS: 1550, Name: "B"},
				{HotCue: 3, TimeMS: 2000, EndMS: 4000, Loop: true, Name: "L"},
				{HotCue: 0, TimeMS: 175, Name: "Main"},
			},
			Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 80, 60}}},
		}
		for i := 0; i < 16; i++ {
			a.Beats = append(a.Beats, rekordbox.Beat{Number: uint16(i%4 + 1), BPM: bpm, TimeMS: uint32(175 + float64(i)*60000/bpm)})
		}
		return a
	}
	encode := func(offset int, bpm float64) Performance {
		t.Helper()
		p, err := EncodePerformance(rekordbox.Track{SampleRate: 44100, Duration: 30, StartOffsetSamples: offset}, analysis(bpm))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Engine's own correction of a marked row: every position of grid, cues and loops.
	engineShift := func(p Performance, by float64) Performance {
		t.Helper()
		raw, err := QUncompress(p.Beats, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		def, adj, _ := beatGrids(raw)
		for _, grid := range [][]byte{def, adj} {
			for i := 8; i < len(grid); i += 24 {
				little.PutUint64(grid[i:], math.Float64bits(math.Float64frombits(little.Uint64(grid[i:]))+by))
			}
		}
		p.Beats, _ = QCompress(raw)
		p.QuickCues, _, _ = shiftQuickCues(p.QuickCues, -by)
		p.Loops, _ = shiftLoops(p.Loops, -by, 30*44100)
		return p
	}
	cues := func(blob []byte) []float64 {
		t.Helper()
		raw, err := QUncompress(blob, 1<<16)
		at, ok := cuePositions(raw)
		if err != nil || !ok {
			t.Fatalf("quick cues: %v", err)
		}
		var out []float64
		for _, off := range at {
			out = append(out, math.Float64frombits(big.Uint64(raw[off:])))
		}
		return out
	}
	setCue := func(blob []byte, slot int, pos float64) []byte {
		t.Helper()
		raw, _ := QUncompress(blob, 1<<16)
		at, _ := cuePositions(raw)
		big.PutUint64(raw[at[slot]:], math.Float64bits(pos))
		out, _ := QCompress(raw)
		return out
	}
	now := encode(1105, 120)

	for _, tc := range []struct {
		name string
		row  Performance
	}{
		{"the USB number of #178", encode(2257, 120)},
		{"the raw rekordbox position", encode(0, 120)},
		{"raw, then Engine's own correction", engineShift(encode(0, 120), -2292)},
		{"#178, then Engine's own correction", engineShift(encode(2257, 120), -2292)},
	} {
		beats, quick, loops, changed := realignKept(tc.row.Beats, tc.row.QuickCues, tc.row.Loops, now, false)
		if !changed || !bytes.Equal(beats, now.Beats) || !bytes.Equal(loops, now.Loops) {
			t.Fatalf("%s: grid or loops not at this import's position (changed %v)", tc.name, changed)
		}
		for i, want := range cues(now.QuickCues) {
			if got := cues(quick)[i]; math.Abs(got-want) > 1e-6 {
				t.Fatalf("%s: cue %d at %v, want %v", tc.name, i, got, want)
			}
		}
	}

	// The deck moved hot cue B, added one in slot 5 and adjusted the grid: those stay.
	row := encode(2257, 120)
	row.QuickCues = setCue(setCue(row.QuickCues, 1, 70000), 4, 123456)
	raw, _ := QUncompress(row.Beats, 1<<20)
	_, adj, _ := beatGrids(raw)
	little.PutUint64(adj[8:], math.Float64bits(math.Float64frombits(little.Uint64(adj[8:]))+300))
	row.Beats, _ = QCompress(raw)
	beats, quick, _, changed := realignKept(row.Beats, row.QuickCues, row.Loops, now, false)
	got, want := cues(quick), cues(now.QuickCues)
	if !changed || !bytes.Equal(beats, row.Beats) || got[1] != 70000 || got[4] != 123456 || math.Abs(got[0]-want[0]) > 1e-6 || math.Abs(got[8]-want[8]) > 1e-6 {
		t.Fatalf("deck edits: changed %v, grid kept %v, cues %v, this import's %v", changed, bytes.Equal(beats, row.Beats), got, want)
	}
	// The DJ named hot cue A and loop L on the deck: the bytes of every later slot moved, and
	// the import's cues and loops are still found, slot by slot.
	rename := func(raw []byte, at int, name string) []byte {
		out := append(append(append([]byte(nil), raw[:at]...), byte(len(name))), name...)
		return append(out, raw[at+1+int(raw[at]):]...)
	}
	row = encode(2257, 120)
	rawCues, _ := QUncompress(row.QuickCues, 1<<16)
	row.QuickCues, _ = QCompress(rename(rawCues, 8, "Intro, first drop"))
	loopAt := 8
	for i := 0; i < 2; i++ { // the loop is in slot 3
		loopAt += 1 + int(row.Loops[loopAt]) + 22
	}
	row.Loops = rename(row.Loops, loopAt, "Breakdown loop")
	_, quick, loops, changed := realignKept(row.Beats, row.QuickCues, row.Loops, now, false)
	rowLoops, _ := loopPositions(loops)
	nowLoops, _ := loopPositions(now.Loops)
	if got := cues(quick); !changed || math.Abs(got[0]-want[0]) > 1e-6 || math.Abs(got[1]-want[1]) > 1e-6 ||
		little.Uint64(loops[rowLoops[4]:]) != little.Uint64(now.Loops[nowLoops[4]:]) || little.Uint64(loops[rowLoops[5]:]) != little.Uint64(now.Loops[nowLoops[5]:]) {
		t.Fatalf("a cue and a loop named on the deck: changed %v, cues %v, this import's %v", changed, got, want)
	}
	// A locked grid stays, its cues move.
	row = encode(2257, 120)
	if beats, quick, _, changed = realignKept(row.Beats, row.QuickCues, row.Loops, now, true); !changed || !bytes.Equal(beats, row.Beats) || math.Abs(cues(quick)[0]-want[0]) > 1e-6 {
		t.Fatal("locked grid: the grid was replaced or the cues were left")
	}
	// Neighbours: a row already at this import's position, another grid in rekordbox since
	// (not a row of this import to move), and a row that cannot be read are left as they are.
	for name, row := range map[string]Performance{"already there": now, "another grid": encode(2257, 128), "unreadable": {Beats: []byte("x"), QuickCues: now.QuickCues, Loops: now.Loops}} {
		if beats, quick, loops, changed := realignKept(row.Beats, row.QuickCues, row.Loops, now, false); changed ||
			!bytes.Equal(beats, row.Beats) || !bytes.Equal(quick, row.QuickCues) || !bytes.Equal(loops, row.Loops) {
			t.Fatalf("%s: the row was changed", name)
		}
	}
}
