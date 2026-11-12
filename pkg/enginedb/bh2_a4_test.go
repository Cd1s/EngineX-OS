package enginedb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestEncodePerformanceClampsEndOfTrackCuesAndLoopsForDisplay(t *testing.T) {
	track := rekordbox.Track{
		ID:          1,
		SampleRate:  44100,
		SampleCount: 44100 * 10, // 10.000 s exact audio
		Duration:    10,
	}
	analysis := &rekordbox.Analysis{
		Beats: []rekordbox.Beat{
			{Number: 1, BPM: 120, TimeMS: 0},
			{Number: 2, BPM: 120, TimeMS: 500},
		},
		Waveforms: []rekordbox.Waveform{
			{Tag: "PWV7", EntryBytes: 3, Data: make([]byte, 3*1500)},
		},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 10050, Name: "Outro"},                            // 50 ms past EOF
			{HotCue: 0, TimeMS: 10020, Name: "EndMem"},                           // main memory cue past EOF
			{HotCue: 2, Loop: true, TimeMS: 9500, EndMS: 10050, Name: "EndLoop"}, // loop end past EOF
		},
	}

	perf, err := EncodePerformance(track, analysis)
	if err != nil {
		t.Fatalf("EncodePerformance: %v", err)
	}
	disp, err := ParseDisplayPerformance(perf.TrackData, perf.Overview, perf.Beats, perf.QuickCues, perf.Loops)
	if err != nil {
		t.Fatalf("ParseDisplayPerformance rejected clamped end-of-track cues/loops: %v", err)
	}
	if len(disp.Cues) < 2 || len(disp.Loops) != 1 {
		t.Fatalf("unexpected display cues/loops: cues=%+v loops=%+v", disp.Cues, disp.Loops)
	}
}

func TestPlaylistHashIgnoresFastAndDatabaseDir(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	lib, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatalf("ReadPDB: %v", err)
	}

	tmpDB := t.TempDir()
	r1, err := Sync(context.Background(), mount, lib, Options{Fast: true, DatabaseDir: tmpDB})
	if err != nil || !r1.PlaylistsChanged {
		t.Fatalf("initial staged Fast sync: %+v %v", r1, err)
	}
	// Copy staged Database2 into mount/Engine Library/Database2 just like bridge.stagedImport does.
	dstDir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"m.db", ".rb-bridge-state.db"} {
		data, err := os.ReadFile(filepath.Join(tmpDB, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dstDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Second sync in-place with Fast=false, DatabaseDir="" and identical playlists must NOT rebuild playlists.
	r2, err := Sync(context.Background(), mount, lib, Options{Fast: false, DatabaseDir: ""})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if r2.PlaylistsChanged {
		t.Fatalf("PlaylistsChanged=true on unchanged playlists when Fast/DatabaseDir differed: %+v", r2)
	}
}
