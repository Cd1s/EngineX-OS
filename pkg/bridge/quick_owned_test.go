package bridge

import (
	"context"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestReconcileOwnedContextDoesNotWalkKnownTrackDirectories(t *testing.T) {
	root := t.TempDir()
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	l := &rekordbox.Library{
		Tracks:    []rekordbox.Track{{ID: 7, Path: "Contents/owned.mp3"}},
		Playlists: []rekordbox.Playlist{{ID: 1, Name: "list", Tracks: []uint32{7}}},
	}
	err = ReconcileOwnedContext(context.Background(), r, l,
		map[uint32]string{7: "Contents/owned.mp3"},
		map[string]string{"contents/owned.mp3": "Contents/owned.mp3"})
	if err != nil {
		t.Fatalf("known track was resolved from disk: %v", err)
	}
	if len(l.Tracks) != 1 || l.Tracks[0].ID != 7 {
		t.Fatalf("known track changed: %+v", l.Tracks)
	}
}
