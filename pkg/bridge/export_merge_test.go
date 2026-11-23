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

func TestMergeExportLibrariesKeepsSecondaryTrackIDStable(t *testing.T) {
	primary := &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: "Contents/a.mp3"}}}
	secondary := &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: "Contents/b.mp3"}}}
	first := mergeExportLibraries(primary, secondary)
	second := mergeExportLibraries(primary, secondary)
	if len(first.Tracks) != 2 || len(second.Tracks) != 2 {
		t.Fatalf("merged tracks=%d/%d, want 2/2", len(first.Tracks), len(second.Tracks))
	}
	if first.Tracks[1].ID != second.Tracks[1].ID {
		t.Fatalf("secondary track ID moved from %d to %d", first.Tracks[1].ID, second.Tracks[1].ID)
	}
	if first.Tracks[1].ID < 1<<24 {
		t.Fatalf("secondary track ID %d overlaps the source ID namespace", first.Tracks[1].ID)
	}
}

func TestDualExportDropsGhostEmptyRootPlaylistsAbsentFromPDB(t *testing.T) {
	one := &rekordbox.Library{Playlists: []rekordbox.Playlist{
		{ID: 1, Name: "ghost"},
		{ID: 2, Name: "real", Tracks: []uint32{1}},
		{ID: 3, Name: "unique"},
		{ID: 4, Name: "folder", Folder: true},
		{ID: 5, ParentID: 4, Name: "ghost", Tracks: []uint32{1}},
	}}
	pdb := &rekordbox.Library{Playlists: []rekordbox.Playlist{{ID: 10, Name: "other"}}}
	filtered := filterGhostEmptyRootPlaylists(one, pdb)
	merged := mergeExportLibraries(filtered, pdb)
	for _, p := range merged.Playlists {
		if p.Name == "ghost" && p.ParentID == 0 && !p.Folder {
			t.Fatal("empty root ghost playlist survived")
		}
	}
	for _, name := range []string{"real", "unique", "folder", "other"} {
		found := false
		for _, p := range merged.Playlists {
			if p.Name == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("playlist %q was dropped", name)
		}
	}
}
