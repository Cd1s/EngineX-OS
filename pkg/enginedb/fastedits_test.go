package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func cueSlotPos(t *testing.T, blob []byte, slot int) float64 {
	t.Helper()
	slots, _, ok := cueSlots(blob)
	if !ok {
		t.Fatal("cue layout not recognized")
	}
	return cuePos(slots[slot])
}

func shiftCueBlob(t *testing.T, blob []byte, delta float64) []byte {
	t.Helper()
	raw, err := QUncompress(blob, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := cuePositions(raw)
	if !ok {
		t.Fatal("cue layout not recognized")
	}
	for i := 0; i < 8; i++ {
		p := math.Float64frombits(big.Uint64(raw[at[i]:]))
		if p >= 0 {
			big.PutUint64(raw[at[i]:], math.Float64bits(p+delta))
		}
	}
	out, err := QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMergeFastUsesTheStoredImportBaseline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old := rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", SampleRate: 44100, Duration: 10, Cues: []rekordbox.Cue{{HotCue: 1, TimeMS: 1000, Name: "old"}}}
	ids, _, _, err := BuildLibrary(ctx, dir, "", []TrackRow{{Rel: "Contents/a.mp3", Track: old}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate Engine loading the fast row without DJ edits: thirdPartySourceId is cleared.
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if _, err = lib.db.ExecContext(ctx, `UPDATE PerformanceData SET thirdPartySourceId=NULL WHERE trackId=?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	now := old
	now.Cues = []rekordbox.Cue{{HotCue: 1, TimeMS: 5000, Name: "new"}}
	p, err := EncodePerformance(now, &rekordbox.Analysis{Cues: now.Cues})
	if err != nil {
		t.Fatal(err)
	}
	base := FastBaseForTrack(old)
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, outcome, err := b.ApplyPerformance(ctx, ids[0], p, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{
		State: rbstate.PerfNone, FastKnown: true,
		FastCues: base.Cues, FastLoops: base.Loops, FastActive: base.Active,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfWritten {
		t.Fatalf("outcome=%d, want PerfWritten (stored baseline must prevent stale v1 cues becoming PerfMerged)", outcome)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var gotCues []byte
	if err = lib.db.QueryRowContext(ctx, `SELECT quickCues FROM PerformanceData WHERE trackId=?`, ids[0]).Scan(&gotCues); err != nil {
		t.Fatal(err)
	}
	if got, want := cueSlotPos(t, gotCues, 0), 5.0*44100; math.Abs(got-want) > 1 {
		t.Fatalf("hot cue 1 pos=%v, want %v (stale v1 cue was kept)", got, want)
	}
}

func TestApplyPerformanceUsesStoredFastBaselineEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old := rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", SampleRate: 44100, Duration: 10, Cues: []rekordbox.Cue{{HotCue: 1, TimeMS: 1000}}}
	ids, _, _, err := BuildLibrary(ctx, dir, "", []TrackRow{{Rel: "Contents/a.mp3", Track: old}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	// Simulate Engine 5.3.0 loading the marked MP3 fast row: clears thirdPartySourceId and shifts by -2292 samples.
	var curCues []byte
	if err = lib.db.QueryRowContext(ctx, `SELECT quickCues FROM PerformanceData WHERE trackId=?`, ids[0]).Scan(&curCues); err != nil {
		t.Fatal(err)
	}
	shifted := shiftCueBlob(t, curCues, -2292)
	if _, err = lib.db.ExecContext(ctx, `UPDATE PerformanceData SET quickCues=?,thirdPartySourceId=NULL WHERE trackId=?`, shifted, ids[0]); err != nil {
		t.Fatal(err)
	}
	now := old
	now.Cues = []rekordbox.Cue{{HotCue: 1, TimeMS: 5000}}
	p, err := EncodePerformance(now, &rekordbox.Analysis{Cues: now.Cues})
	if err != nil {
		t.Fatal(err)
	}
	base := FastBaseForTrack(old)
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, outcome, err := b.ApplyPerformance(ctx, ids[0], p, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{
		State: rbstate.PerfNone, FastKnown: true,
		FastCues: base.Cues, FastLoops: base.Loops, FastActive: base.Active,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfWritten {
		t.Fatalf("outcome=%d, want PerfWritten", outcome)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var gotCues []byte
	if err = lib.db.QueryRowContext(ctx, `SELECT quickCues FROM PerformanceData WHERE trackId=?`, ids[0]).Scan(&gotCues); err != nil {
		t.Fatal(err)
	}
	if got, want := cueSlotPos(t, gotCues, 0), 5.0*44100; math.Abs(got-want) > 1 {
		t.Fatalf("hot cue 1 pos=%v, want %v", got, want)
	}
}

func TestMergeFastPreservesDJFieldsAndHandlesLoadShift(t *testing.T) {
	tr := rekordbox.Track{
		SampleRate: 44100, Duration: 30, BPM: 120,
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 1000, Name: "A"},
			{HotCue: 2, TimeMS: 2000, Name: "B"},
			{HotCue: 3, TimeMS: 4000, EndMS: 6000, Loop: true, Name: "L1"},
			{HotCue: 4, TimeMS: 8000, EndMS: 10000, Loop: true, Name: "L2"},
		},
	}
	base := fastBaseOf(tr)
	nowTrack := tr
	nowTrack.Cues = []rekordbox.Cue{
		{HotCue: 1, TimeMS: 1500, Name: "A2"},
		{HotCue: 2, TimeMS: 2500, Name: "B2"},
		{HotCue: 3, TimeMS: 4500, EndMS: 6500, Loop: true, Name: "L1new"},
		{HotCue: 4, TimeMS: 8500, EndMS: 10500, Loop: true, Name: "L2new"},
	}
	nowPerf, err := EncodePerformance(nowTrack, &rekordbox.Analysis{
		Cues:  nowTrack.Cues,
		Beats: []rekordbox.Beat{{BPM: 120, TimeMS: 0}, {BPM: 120, TimeMS: 500}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Hot cue 1 shifted by Engine's 2292-sample MP3 load correction (untouched -> takes now),
	//    hot cue 2 moved by DJ (kept), main cue adjusted by DJ (adj=1 -> kept).
	rawCues, err := QUncompress(base.cues, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := cuePositions(rawCues)
	if !ok {
		t.Fatal("cue layout")
	}
	big.PutUint64(rawCues[at[0]:], math.Float64bits(1.0*44100-2292)) // Engine 2292-sample shift on slot 1
	big.PutUint64(rawCues[at[1]:], math.Float64bits(9.0*44100))      // DJ moved slot 2
	big.PutUint64(rawCues[at[8]:], math.Float64bits(3333))           // DJ set main cue
	rawCues[at[8]+8] = 1
	djCues, err := QCompress(rawCues)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Saved loop 1 untouched, saved loop 2 moved by DJ, and DJ toggled active-on-load bit for loop 1.
	djLoops := append([]byte(nil), base.loops...)
	lat, ok := loopPositions(djLoops)
	if !ok || len(lat) < 8 {
		t.Fatal("loop layout")
	}
	little.PutUint64(djLoops[lat[6]:], math.Float64bits(12.0*44100))
	little.PutUint64(djLoops[lat[7]:], math.Float64bits(14.0*44100))

	// 3. Engine's own beatgrid (def == adj) is replaced unless locked or adjusted.
	ownBeats := nowPerf.Beats
	row := deckRow{
		beats:  ownBeats,
		cues:   djCues,
		loops:  djLoops,
		active: sql.NullInt64{Valid: true, Int64: 1 << 2}, // bit 2 (slot 3, L1) toggled on by DJ
	}
	m := mergeFast(row, base, nowPerf)
	if !m.deck() || m.grid || m.beats != nil {
		t.Fatalf("own unadjusted grid should not be kept: grid=%v beatsNil=%v", m.grid, m.beats == nil)
	}
	if m.cues == nil || m.loops == nil || m.active == nil {
		t.Fatalf("expected DJ cue, loop, and active edits kept: cues=%v loops=%v active=%v", m.cues != nil, m.loops != nil, m.active)
	}
	if got := cueSlotPos(t, m.cues, 0); math.Abs(got-1.5*44100) > 1 {
		t.Fatalf("shifted untouched hot cue 1=%v, want rekordbox %v", got, 1.5*44100)
	}
	if got := cueSlotPos(t, m.cues, 1); math.Abs(got-9.0*44100) > 1 {
		t.Fatalf("DJ hot cue 2=%v, want %v", got, 9.0*44100)
	}
	_, trailer, ok := cueSlots(m.cues)
	if !ok || trailer[8] != 1 || math.Abs(math.Float64frombits(big.Uint64(trailer))-3333) > 1 {
		t.Fatalf("DJ adjusted main cue lost: trailer=%x", trailer)
	}
	mergedLoops, ok := loopSlots(m.loops)
	if !ok {
		t.Fatal("merged loops")
	}
	s1, _, _ := loopAt(mergedLoops[2])
	s2, _, _ := loopAt(mergedLoops[3])
	if math.Abs(s1-4.5*44100) > 1 {
		t.Fatalf("untouched loop 1 start=%v, want rekordbox %v", s1, 4.5*44100)
	}
	if math.Abs(s2-12.0*44100) > 1 {
		t.Fatalf("DJ loop 2 start=%v, want %v", s2, 12.0*44100)
	}
	if m.active.(int64)&(1<<2) == 0 {
		t.Fatalf("DJ active-on-load toggle lost: %b", m.active)
	}

	// 4. Locked grid is preserved even when def == adj.
	lockedRow := row
	lockedRow.locked = true
	ml := mergeFast(lockedRow, base, nowPerf)
	if !ml.grid || !bytes.Equal(ml.beats, ownBeats) {
		t.Fatal("locked beatgrid was not preserved")
	}

	// 5. Adjusted grid (def != adj) is preserved even when unlocked.
	rawBeats, err := QUncompress(ownBeats, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	def, adj, ok := beatGrids(rawBeats)
	if !ok || len(adj) < 16 {
		t.Fatal("beat grid layout")
	}
	adjOff := len(rawBeats) - len(adj)
	little.PutUint64(rawBeats[adjOff+8:], math.Float64bits(999))
	adjBeats, err := QCompress(rawBeats)
	if err != nil {
		t.Fatal(err)
	}
	_ = def
	adjRow := row
	adjRow.beats = adjBeats
	ma := mergeFast(adjRow, base, nowPerf)
	if !ma.grid || !bytes.Equal(ma.beats, adjBeats) {
		t.Fatal("adjusted beatgrid was not preserved")
	}
}
