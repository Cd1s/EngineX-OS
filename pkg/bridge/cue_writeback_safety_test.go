package bridge

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestResolveRekordboxTrackUsesPathBeforeShiftedID(t *testing.T) {
	pathTrack := rekordbox.Track{ID: 7, Path: "/Contents/A.mp3"}
	idTrack := rekordbox.Track{ID: 42, Path: "/Contents/B.mp3"}
	got, ok := resolveRekordboxTrack(context.Background(), nil, false, 1, 42, "../Contents/A.mp3",
		map[uint32]rekordbox.Track{42: idTrack},
		map[string]rekordbox.Track{"Contents/A.mp3": pathTrack},
		map[string]rekordbox.Track{"contents/a.mp3": pathTrack})
	if !ok || got.ID != pathTrack.ID {
		t.Fatalf("resolved track=%+v ok=%v, want path track %+v", got, ok, pathTrack)
	}
}

func TestResolveRekordboxTrackRejectsIDWithDifferentPath(t *testing.T) {
	got, ok := resolveRekordboxTrack(context.Background(), nil, false, 1, 42, "../Contents/A.mp3",
		map[uint32]rekordbox.Track{42: {ID: 42, Path: "/Contents/B.mp3"}}, nil, nil)
	if ok || got.ID != 0 {
		t.Fatalf("resolved shifted ID track=%+v ok=%v, want no match", got, ok)
	}
}

func TestMergeEngineCuesPreservesUncolouredImportedCues(t *testing.T) {
	deck := enginedb.DeckCues{SampleRate: 1000, SampleCount: 10000}
	deck.HotCues[0] = &enginedb.DeckHotCue{Samples: 1000, Color: engineDefaultHotCueColor}
	deck.Loops[1] = &enginedb.DeckLoop{StartSamples: 2000, EndSamples: 3000, Color: engineDefaultLoopColor}
	got := mergeEngineCues([]rekordbox.Cue{
		{HotCue: 1, TimeMS: 1000},
		{HotCue: 0, TimeMS: 2000, EndMS: 3000, Loop: true},
	}, deck, 0)
	for _, cue := range got {
		if cue.HotCue == 1 && (cue.Color != [4]byte{} || cue.ColorCode != 0) {
			t.Fatalf("uncoloured hot cue gained color: %+v", cue)
		}
		if cue.HotCue == 0 && cue.Loop && (cue.Color != [4]byte{} || cue.ColorCode != 0 || cue.ColorID != 0) {
			t.Fatalf("uncoloured memory loop gained color: %+v", cue)
		}
	}
}

func TestWriteBackStampReason(t *testing.T) {
	current := "v4 PIONEER/rekordbox/exportLibrary.db 1 2"
	for _, tc := range []struct {
		name, imported, want string
	}{
		{"missing", "", "Engine library import stamp is missing"},
		{"old", "v3 export", "Engine library import stamp is older than v4"},
		{"mismatch", "v4 other", "Engine library import stamp does not match the current rekordbox export"},
		{"match", current, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := writeBackStampReason(tc.imported, current); got != tc.want {
				t.Fatalf("reason=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestImportedRecordChangedTreatsOpaqueFingerprintAsChanged(t *testing.T) {
	track := rekordbox.Track{RecordHash: "record-hash"}
	if importedRecordChanged("0123456789abcdef", track) {
		t.Fatal("opaque legacy fingerprint changed the first-record baseline")
	}
	if importedRecordChanged("fast:record-hash", track) {
		t.Fatal("fast fingerprint should not be treated as an imported record baseline")
	}
}

func TestMergeEngineCuesClampAtZeroPreserved(t *testing.T) {
	// MP3 at 44.1 kHz with 1105-sample Rekordbox offset (~25 ms): an original cue at 7 ms
	// clamps to Samples=0 on import and must stay 7 ms (cuesEquivalent) on write-back.
	orig := []rekordbox.Cue{
		{HotCue: 1, TimeMS: 7, Extended: true},
		{HotCue: 2, TimeMS: 10, EndMS: 2000, Loop: true, Extended: true},
	}
	deck := enginedb.DeckCues{SampleRate: 44100, SampleCount: 44100 * 60}
	deck.HotCues[0] = &enginedb.DeckHotCue{Samples: 0, Color: engineDefaultHotCueColor}
	deck.HotCues[1] = &enginedb.DeckHotCue{Samples: 0, Color: engineDefaultLoopColor}
	deck.Loops[1] = &enginedb.DeckLoop{StartSamples: 0, EndSamples: 2.0*44100 - 1105, Color: engineDefaultLoopColor}
	got := mergeEngineCues(orig, deck, 1105)
	if !cuesEquivalent(orig, got) {
		t.Fatalf("clamp-at-zero cues drifted on write-back: orig=%+v got=%+v", orig, got)
	}
}

func TestTrackRekordboxOffsetRejectsUnreadableMP3AndMP4(t *testing.T) {
	root := t.TempDir()
	mp3Path := filepath.Join(root, "Contents", "bad.mp3")
	m4aPath := filepath.Join(root, "Contents", "truncated.m4a")
	if err := os.MkdirAll(filepath.Dir(mp3Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp3Path, []byte("ID3\x04\x00\x00\x7f\x7f\x7f\x7fnot-valid-frames"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m4aPath, []byte("\x00\x00\x00\x14ftypM4A \x00\x00\x00\x00M4A "), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if off, ok := trackRekordboxOffset(r, "/Contents/bad.mp3"); ok || off != 0 {
		t.Fatalf("corrupt MP3 returned ok=%v off=%v, want ok=false", ok, off)
	}
	if off, ok := trackRekordboxOffset(r, "/Contents/truncated.m4a"); ok || off != 0 {
		t.Fatalf("moov-less M4A returned ok=%v off=%v, want ok=false", ok, off)
	}
}

func TestMergeEngineCuesPreservesLegacyMP3OffsetDrift(t *testing.T) {
	red := [4]byte{255, 255, 0, 23}
	orig := []rekordbox.Cue{{HotCue: 1, TimeMS: 116089, Color: red, ColorCode: 1, Extended: true}}
	// Engine fast-row load shifted unnamed MP3 cue by -2292 samples -> -27 ms after +1105 offset.
	deck := enginedb.DeckCues{SampleRate: 44100, SampleCount: 44100 * 300}
	deck.HotCues[0] = &enginedb.DeckHotCue{Samples: 5117232.9, Name: "", Color: red}
	got := mergeEngineCuesOpts(orig, deck, 1105, true)
	if !cuesEquivalent(orig, got) {
		t.Fatalf("legacy MP3 unnamed offset drift triggered false diff: orig=%+v got=%+v", orig, got)
	}
	// A deck-placed cue ("Cue 1") at the same position is NOT suppressed.
	deck.HotCues[0].Name = "Cue 1"
	gotNamed := mergeEngineCuesOpts(orig, deck, 1105, true)
	if cuesEquivalent(orig, gotNamed) {
		t.Fatalf("deck-named Cue 1 was unexpectedly suppressed: %+v", gotNamed)
	}
}

func TestStripShiftedForeignCuesPreservesOwnDeckCues(t *testing.T) {
	root := t.TempDir()
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	own := rekordbox.Track{ID: 5909, Path: "/Contents/Own.mp3", AnalysisPath: "/PIONEER/USBANLZ/P01/0001/ANLZ0000.DAT"}
	foreign := rekordbox.Track{
		ID:           5945,
		Path:         "/Contents/Foreign.mp3",
		AnalysisPath: "/PIONEER/USBANLZ/P02/0002/ANLZ0000.DAT",
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 86},
			{HotCue: 2, TimeMS: 270087},
		},
	}
	deck := enginedb.DeckCues{SampleRate: 44100, SampleCount: 44100 * 344}
	deck.HotCues[0] = &enginedb.DeckHotCue{Samples: 3792.6, Name: "", Color: engineDefaultHotCueColor}
	deck.HotCues[1] = &enginedb.DeckHotCue{Samples: 11910836.7, Name: "", Color: engineDefaultHotCueColor}
	deck.HotCues[6] = &enginedb.DeckHotCue{Samples: 582605.1, Name: "Cue 7", Color: [4]byte{255, 0, 168, 169}}

	cleaned := stripShiftedForeignCues(r, deck, false, 5945, "../Contents/Own.mp3", own, nil, 1105, map[uint32]rekordbox.Track{5909: own, 5945: foreign})
	if cleaned.HotCues[0] != nil || cleaned.HotCues[1] != nil {
		t.Fatalf("foreign shifted-ID cues were not stripped: %+v %+v", cleaned.HotCues[0], cleaned.HotCues[1])
	}
	if cleaned.HotCues[6] == nil || cleaned.HotCues[6].Name != "Cue 7" {
		t.Fatalf("own deck Cue 7 was lost: %+v", cleaned.HotCues[6])
	}
}

func TestInvalidateUnsyncedEditsKeepsWrittenAndDropsEdited(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, stateName)
	sdb, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`CREATE TABLE BridgeWritten(trackId INTEGER PRIMARY KEY, digest BLOB NOT NULL);
		CREATE TABLE BridgeSynced(trackId INTEGER PRIMARY KEY, lastEditTime INTEGER NOT NULL, digest BLOB NOT NULL);
		INSERT INTO BridgeWritten(trackId, digest) VALUES(1, X'AA');
		INSERT INTO BridgeSynced(trackId, lastEditTime, digest) VALUES(1, 100, X'01'), (2, 100, X'02');`); err != nil {
		sdb.Close()
		t.Fatal(err)
	}
	sdb.Close()
	if err := enginedb.InvalidateUnsyncedEdits(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	sdb, err = sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	var c1, c2 int
	_ = sdb.QueryRow(`SELECT count(*) FROM BridgeSynced WHERE trackId = 1`).Scan(&c1)
	_ = sdb.QueryRow(`SELECT count(*) FROM BridgeSynced WHERE trackId = 2`).Scan(&c2)
	if c1 != 1 || c2 != 0 {
		t.Fatalf("BridgeSynced after InvalidateUnsyncedEdits: written=%d (want 1) un-written=%d (want 0)", c1, c2)
	}
}
