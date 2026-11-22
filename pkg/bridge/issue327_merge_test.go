package bridge

import (
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestIssue327MergeTracksPlaylistsAndStableIDs(t *testing.T) {
	primary := &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 101, Path: "/Music/A.wav", RecordHash: "preferred-record"}},
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
	if merged.Tracks[0].ID != 101 || merged.Tracks[0].RecordHash != "preferred-record" {
		t.Fatalf("preferred record identity changed: id=%d hash=%q", merged.Tracks[0].ID, merged.Tracks[0].RecordHash)
	}
	if len(merged.Playlists) != 5 {
		t.Fatalf("merged playlists=%d, want 5", len(merged.Playlists))
	}
	var conflict, other int
	parents := map[uint32]string{0: ""}
	for _, p := range merged.Playlists {
		parents[p.ID] = p.Name
	}
	for _, p := range merged.Playlists {
		if p.Name == "Mix (Device Library)" {
			conflict++
		}
		if p.Name == "Mix" && parents[p.ParentID] == "Other" {
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

func TestIssue327OverlappingIDsStayWithTheirPaths(t *testing.T) {
	first := &rekordbox.Library{Tracks: []rekordbox.Track{
		{ID: 1, Path: "/Music/one.wav"},
		{ID: 2, Path: "/Music/two.wav"},
	}}
	second := &rekordbox.Library{Tracks: []rekordbox.Track{
		{ID: 1, Path: "/Music/other.wav"},
		{ID: 2, Path: "/Music/one.wav"},
	}}
	merge := func(readable []readableExport) map[string]uint32 {
		lib := mergeReadableExports(readable)
		got := make(map[string]uint32, len(lib.Tracks))
		for _, track := range lib.Tracks {
			got[foldPath(track.Path)] = track.ID
		}
		return got
	}
	a := merge([]readableExport{
		{candidate: exportCandidate{rel: "PIONEER/rekordbox/exportLibrary.db"}, library: first},
		{candidate: exportCandidate{rel: "PIONEER/rekordbox/export.pdb", isPDB: true}, library: second},
	})
	b := merge([]readableExport{
		{candidate: exportCandidate{rel: "PIONEER/rekordbox/export.pdb", isPDB: true}, library: second},
		{candidate: exportCandidate{rel: "PIONEER/rekordbox/exportLibrary.db"}, library: first},
	})
	if len(a) != len(b) || len(a) != 3 {
		t.Fatalf("merged path count changed: %d/%d", len(a), len(b))
	}
	if a["/music/one.wav"] != 1 || a["/music/two.wav"] != 2 || a["/music/other.wav"] < 1<<31 {
		t.Fatalf("preferred first IDs=%v", a)
	}
	if b["/music/other.wav"] != 1 || b["/music/one.wav"] != 2 || b["/music/two.wav"] < 1<<31 {
		t.Fatalf("preferred second IDs=%v", b)
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

func TestIssue327MergeKeepsCaseDistinctRecordsFromOneLibrary(t *testing.T) {
	first := &rekordbox.Library{Tracks: []rekordbox.Track{
		{ID: 60, Path: "/Music/Case/Song.mp3"},
		{ID: 61, Path: "/Music/case/Song.mp3"},
	}}
	second := &rekordbox.Library{Tracks: []rekordbox.Track{
		{ID: 7, Path: "/Music/case/Song.mp3"},
	}}
	merged := mergeReadableExports([]readableExport{
		{candidate: exportCandidate{rel: "PIONEER/rekordbox/exportLibrary.db"}, library: first},
		{candidate: exportCandidate{rel: "PIONEER/rekordbox/export.pdb", isPDB: true}, library: second},
	})
	if len(merged.Tracks) != 2 {
		t.Fatalf("case-distinct first-library records collapsed: %d", len(merged.Tracks))
	}
	if merged.Tracks[0].ID != 60 || merged.Tracks[1].ID != 61 {
		t.Fatalf("first-library IDs changed: %d,%d", merged.Tracks[0].ID, merged.Tracks[1].ID)
	}
}
