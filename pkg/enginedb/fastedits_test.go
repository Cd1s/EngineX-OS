package enginedb

import (
	"database/sql"
	"testing"

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
