package bridge

import (
	"context"
	"testing"

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
	if !importedRecordChanged("0123456789abcdef", track) {
		t.Fatal("opaque legacy fingerprint was treated as the current export record")
	}
	if importedRecordChanged("fast:record-hash", track) {
		t.Fatal("fast fingerprint should not be treated as an imported record baseline")
	}
}
