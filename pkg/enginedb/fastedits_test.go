package enginedb

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestMergeFastUsesTheStoredImportBaseline(t *testing.T) {
	old := rekordbox.Track{SampleRate: 44100, Duration: 10, Cues: []rekordbox.Cue{{HotCue: 1, TimeMS: 1000, Name: "old"}}}
	now := rekordbox.Track{SampleRate: 44100, Duration: 10, Cues: []rekordbox.Cue{{HotCue: 1, TimeMS: 5000, Name: "new"}}}
	oldPerf, err := EncodePerformance(old, &rekordbox.Analysis{Cues: old.Cues})
	if err != nil {
		t.Fatal(err)
	}
	nowPerf, err := EncodePerformance(now, &rekordbox.Analysis{Cues: now.Cues})
	if err != nil {
		t.Fatal(err)
	}
	row := deckRow{
		cues: oldPerf.QuickCues, loops: oldPerf.Loops,
		mark: sql.NullInt64{Valid: true}, rating: sql.NullInt64{Valid: true, Int64: 77},
	}
	withStored := mergeFast(row, fastBaseOf(old), nowPerf)
	withCurrent := mergeFast(row, fastBaseOf(now), nowPerf)
	if withStored.cues != nil {
		t.Fatal("stored fast baseline treated the unchanged cue as a DJ edit")
	}
	if withCurrent.cues == nil {
		t.Fatal("current export baseline failed to expose the stale-cue regression")
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
	now := old
	now.Cues = []rekordbox.Cue{{HotCue: 1, TimeMS: 5000}}
	p, err := EncodePerformance(now, &rekordbox.Analysis{Cues: now.Cues})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, outcome, err := b.ApplyPerformance(ctx, ids[0], p, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{
		State: rbstate.PerfNone, FastKnown: true,
		FastCues: fastBaseOf(old).cues, FastLoops: fastBaseOf(old).loops, FastActive: uint32(fastBaseOf(old).active), FastRating: fastBaseOf(old).rating,
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
	lib.Close()
}
