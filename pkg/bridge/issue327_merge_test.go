package bridge

import (
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestIssue327MergeTracksPlaylistsAndStableIDs(t *testing.T) {
	primary := &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 101, Path: "/Music/A.wav"}},
		Playlists: []rekordbox.Playlist{
			{ID: 10, Name: "Folder", Folder: true},
			{ID: 11, ParentID: 10, Name: "Mix", Tracks: []uint32{101}},
		},
	}
	secondary := &rekordbox.Library{
		Tracks: []rekordbox.Track{
			{ID: 1, Path: "/music/a.WAV"},
			{ID: 2, Path: "/Music/B.wav"},
		},
		Playlists: []rekordbox.Playlist{
			{ID: 20, Name: "Folder", Folder: true},
			{ID: 21, ParentID: 20, Name: "Mix", Tracks: []uint32{1, 2}},
			{ID: 30, Name: "Other", Folder: true},
			{ID: 31, ParentID: 30, Name: "Mix", Tracks: []uint32{1}},
		},
	}
	readable := []readableExport{
		{candidate: exportCandidate{rel: "PIONEER/rekordbox/exportLibrary.db"}, library: primary},
		{candidate: exportCandidate{rel: "PIONEER/rekordbox/export.pdb", isPDB: true}, library: secondary},
	}
	merged := mergeReadableExports(readable)
	if err := merged.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(merged.Tracks) != 2 {
		t.Fatalf("merged tracks=%d, want 2", len(merged.Tracks))
	}
	if len(merged.Playlists) != 5 {
		t.Fatalf("merged playlists=%d, want 5", len(merged.Playlists))
	}
	var conflict, other int
	for _, p := range merged.Playlists {
		if p.Name == "Mix (Device Library)" {
			conflict++
		}
		if p.Name == "Mix" && p.ParentID != 10 {
			other++
		}
	}
	if conflict != 1 || other != 1 {
		t.Fatalf("playlist conflict/folder union missing: conflict=%d other=%d", conflict, other)
	}

	again := mergeReadableExports(readable)
	if err := again.Validate(); err != nil {
		t.Fatal(err)
	}
	for i := range merged.Tracks {
		if merged.Tracks[i].ID != again.Tracks[i].ID {
			t.Fatalf("track %d ID changed: %d -> %d", i, merged.Tracks[i].ID, again.Tracks[i].ID)
		}
	}
	for i := range merged.Playlists {
		if merged.Playlists[i].ID != again.Playlists[i].ID {
			t.Fatalf("playlist %d ID changed: %d -> %d", i, merged.Playlists[i].ID, again.Playlists[i].ID)
		}
	}
}

func TestIssue327OneLibraryMergeIsUnchanged(t *testing.T) {
	lib := &rekordbox.Library{
		Tracks:    []rekordbox.Track{{ID: 1, Path: "/Music/A.wav"}},
		Playlists: []rekordbox.Playlist{{ID: 2, Name: "Mix", Tracks: []uint32{1}}},
	}
	got := mergeReadableExports([]readableExport{{library: lib}})
	if got != lib || got.Tracks[0].ID != 1 || got.Playlists[0].ID != 2 {
		t.Fatal("single-library import was changed")
	}
}
