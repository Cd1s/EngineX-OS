package enginedb

import (
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestOrderTracksForPreparePriorityThenPathSorted(t *testing.T) {
	tracks := []rekordbox.Track{
		{ID: 1, Path: "/Contents/Zed/TrackZ.mp3"},
		{ID: 2, Path: "/Contents/Alpha/TrackB.mp3"},
		{ID: 3, Path: "/Contents/Beta/TrackM.mp3"},
		{ID: 4, Path: "/Contents/Alpha/TrackA.mp3"},
	}
	got := orderTracksForPrepare(tracks, []string{"../Contents/Beta/TrackM.mp3"}, true)
	wantIDs := []uint32{3, 4, 2, 1}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d tracks, want %d", len(got), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Fatalf("index %d: got track ID %d (%s), want %d", i, got[i].ID, got[i].Path, id)
		}
	}
	// Original slice must remain unmutated.
	if tracks[0].ID != 1 || tracks[1].ID != 2 || tracks[2].ID != 3 || tracks[3].ID != 4 {
		t.Fatal("orderTracksForPrepare mutated input slice")
	}

	// SortByPath without priority paths still sorts by volume-relative path.
	gotNoPri := orderTracksForPrepare(tracks, nil, true)
	wantNoPri := []uint32{4, 2, 3, 1}
	for i, id := range wantNoPri {
		if gotNoPri[i].ID != id {
			t.Fatalf("no-priority index %d: got track ID %d, want %d", i, gotNoPri[i].ID, id)
		}
	}
}
