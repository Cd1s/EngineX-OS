package bridge

import (
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestMergeExportLibrariesUsesTheRicherPlaylistAndUnionsTracks(t *testing.T) {
	pdb := &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 1, Path: "Contents/a.mp3"}, {ID: 2, Path: "Contents/b.mp3"}},
		Playlists: []rekordbox.Playlist{
			{ID: 10, Name: "Folder", Folder: true},
			{ID: 11, ParentID: 10, Name: "Mix", Tracks: []uint32{1, 2}},
		},
	}
	oneLibrary := &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 1, Path: "Contents/a.mp3"}, {ID: 2, Path: "Contents/c.mp3"}},
		Playlists: []rekordbox.Playlist{
			{ID: 20, Name: "Folder", Folder: true},
			{ID: 21, ParentID: 20, Name: "Mix", Tracks: []uint32{1}},
			{ID: 22, ParentID: 20, Name: "Only", Tracks: []uint32{2}},
		},
	}

	merged := mergeExportLibraries(oneLibrary, pdb)
	if len(merged.Tracks) != 3 {
		t.Fatalf("tracks=%d, want 3", len(merged.Tracks))
	}
	if len(merged.Playlists) != 3 {
		t.Fatalf("playlists=%d, want one folder and two playlists", len(merged.Playlists))
	}

	byName := map[string]rekordbox.Playlist{}
	for _, playlist := range merged.Playlists {
		byName[playlist.Name] = playlist
	}
	mix := byName["Mix"]
	if len(mix.Tracks) != 2 {
		t.Fatalf("Mix entries=%d, want 2", len(mix.Tracks))
	}
	if mix.Tracks[0] != 1 {
		t.Fatalf("Mix order starts with %d, want the richer source's first track 1", mix.Tracks[0])
	}
	if mix.Tracks[1] == 2 {
		t.Fatalf("secondary track kept colliding source ID 2")
	}
	if len(byName["Only"].Tracks) != 1 {
		t.Fatalf("Only entries=%d, want 1", len(byName["Only"].Tracks))
	}
	if byName["Mix"].ParentID != byName["Only"].ParentID {
		t.Fatalf("same folder got different parent IDs: %d and %d", byName["Mix"].ParentID, byName["Only"].ParentID)
	}
}
