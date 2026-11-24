package bridge

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func pdbLib(l *rekordbox.Library) SourceLibrary {
	return SourceLibrary{Kind: rbstate.DeviceLibrary, Lib: l}
}
func oneLib(l *rekordbox.Library) SourceLibrary {
	return SourceLibrary{Kind: rbstate.OneLibrary, Lib: l}
}

func mustView(t *testing.T, layout string, libs ...SourceLibrary) *View {
	t.Helper()
	v, err := BuildView(libs, layout, "en")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// listsOf is the view's lists as "key-with-slashes[entries]" lines, for comparisons.
func listsOf(v *View) []string {
	var out []string
	for _, l := range v.Lists {
		s := strings.ReplaceAll(l.Key, "\x00", "/")
		if l.Folder {
			s += "/"
		}
		if len(l.Tracks) > 0 {
			s += " [" + strings.Join(l.Tracks, " ") + "]"
		}
		out = append(out, s)
	}
	return out
}

func twoLibraries() (pdb, one *rekordbox.Library) {
	pdb = &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 1, Path: "/Contents/a.mp3", Title: "pdb a"}, {ID: 2, Path: "/Contents/b.mp3"}},
		Playlists: []rekordbox.Playlist{
			{ID: 10, Name: "Folder", Folder: true},
			{ID: 11, ParentID: 10, Name: "Mix", Tracks: []uint32{1, 2}},
			{ID: 12, Name: "PDB only", Tracks: []uint32{2}},
		},
	}
	one = &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 1, Path: "/Contents/A.MP3", Title: "one a"}, {ID: 2, Path: "/Contents/c.mp3"}},
		Playlists: []rekordbox.Playlist{
			{ID: 20, Name: "Folder", Folder: true},
			{ID: 21, ParentID: 20, Name: "Mix", Tracks: []uint32{1}},
			{ID: 22, ParentID: 20, Name: "Only", Tracks: []uint32{2}},
		},
	}
	return pdb, one
}

// #327: a stick with both libraries shows every track once and three top folders, each with that
// view's whole tree: merged, Device Library, OneLibrary.
func TestViewOfTwoLibrariesHasThreeFoldersAndEveryTrackOnce(t *testing.T) {
	pdb, one := twoLibraries()
	v := mustView(t, rbstate.LayoutFolders, pdbLib(pdb), oneLib(one))
	if len(v.Tracks) != 3 {
		t.Fatalf("tracks %d, want 3 (a in both, b, c)", len(v.Tracks))
	}
	a, ok := v.Track("contents/a.mp3")
	if !ok || len(a.Refs) != 2 || a.Track.Title != "one a" {
		t.Fatalf("the track of both libraries: %+v", a)
	}
	for _, key := range []string{"contents/b.mp3", "contents/c.mp3"} {
		if tr, ok := v.Track(key); !ok || len(tr.Refs) != 1 {
			t.Fatalf("%s: %+v", key, tr)
		}
	}
	want := []string{
		"merged/",
		"merged/Folder/",
		"merged/Folder/Mix [contents/a.mp3 contents/b.mp3]", // the longer order first, then what only the other has
		"merged/Folder/Only [contents/c.mp3]",
		"merged/PDB only [contents/b.mp3]",
		"device/",
		"device/Folder/",
		"device/Folder/Mix [contents/a.mp3 contents/b.mp3]",
		"device/PDB only [contents/b.mp3]",
		"onelibrary/",
		"onelibrary/Folder/",
		"onelibrary/Folder/Mix [contents/a.mp3]",
		"onelibrary/Folder/Only [contents/c.mp3]",
	}
	if got := listsOf(v); !reflect.DeepEqual(got, want) {
		t.Fatalf("lists:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if v.Lists[0].Name != MergedFolderTitle("en") || v.Lists[5].Name != "Device Library" || v.Lists[9].Name != "OneLibrary" {
		t.Fatalf("folder titles: %q %q %q", v.Lists[0].Name, v.Lists[5].Name, v.Lists[9].Name)
	}
	if v.Lists[2].Parent != v.Lists[1].Key || v.Lists[1].Parent != "merged" || v.Lists[0].Parent != "" {
		t.Fatal("parents do not follow the tree")
	}
	if v.Lists[3].After != v.Lists[2].Key || v.Lists[2].After != "" || v.Lists[5].After != "merged" {
		t.Fatal("siblings are not chained in display order")
	}
}

// The setting's fixed choices show one view at the root; "device" and "onelibrary" leave the
// other library out, tracks included. A stick with one library ignores the setting.
func TestViewLayouts(t *testing.T) {
	pdb, one := twoLibraries()
	merged := mustView(t, rbstate.LayoutMerged, pdbLib(pdb), oneLib(one))
	if got, want := listsOf(merged), []string{"/Folder/", "/Folder/Mix [contents/a.mp3 contents/b.mp3]", "/Folder/Only [contents/c.mp3]", "/PDB only [contents/b.mp3]"}; !reflect.DeepEqual(got, want) || len(merged.Tracks) != 3 {
		t.Fatalf("merged: %v, %d tracks", got, len(merged.Tracks))
	}
	device := mustView(t, rbstate.LayoutDevice, pdbLib(pdb), oneLib(one))
	if got, want := listsOf(device), []string{"/Folder/", "/Folder/Mix [contents/a.mp3 contents/b.mp3]", "/PDB only [contents/b.mp3]"}; !reflect.DeepEqual(got, want) || len(device.Tracks) != 2 {
		t.Fatalf("device: %v, %d tracks", got, len(device.Tracks))
	}
	if a, _ := device.Track("contents/a.mp3"); a.Track.Title != "pdb a" || len(a.Refs) != 1 {
		t.Fatalf("device only still has OneLibrary's row: %+v", a)
	}
	oneOnly := mustView(t, rbstate.LayoutOneLibrary, pdbLib(pdb), oneLib(one))
	if got, want := listsOf(oneOnly), []string{"/Folder/", "/Folder/Mix [contents/a.mp3]", "/Folder/Only [contents/c.mp3]"}; !reflect.DeepEqual(got, want) || len(oneOnly.Tracks) != 2 {
		t.Fatalf("onelibrary: %v, %d tracks", got, len(oneOnly.Tracks))
	}
	for _, layout := range []string{rbstate.LayoutFolders, rbstate.LayoutMerged, rbstate.LayoutDevice, rbstate.LayoutOneLibrary} {
		single := mustView(t, layout, oneLib(one))
		if got, want := listsOf(single), listsOf(oneOnly); !reflect.DeepEqual(got, want) {
			t.Fatalf("one library with layout %s: %v", layout, got)
		}
	}
}

// rekordbox 7 writes empty root lists into OneLibrary that repeat a list with tracks and that the
// Device Library of the stick does not have: they are hidden in every folder (11-23 deck test).
func TestViewHidesOneLibraryGhostRootLists(t *testing.T) {
	one := &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 1, Path: "/Contents/a.mp3"}},
		Playlists: []rekordbox.Playlist{
			{ID: 1, Name: "ghost"},
			{ID: 2, Name: "real", Tracks: []uint32{1}},
			{ID: 3, Name: "unique"},
			{ID: 4, Name: "folder", Folder: true},
			{ID: 5, ParentID: 4, Name: "ghost", Tracks: []uint32{1}},
			{ID: 6, Name: "kept"},
			{ID: 7, ParentID: 4, Name: "kept", Tracks: []uint32{1}},
		},
	}
	pdb := &rekordbox.Library{Playlists: []rekordbox.Playlist{{ID: 10, Name: "other"}, {ID: 11, Name: "kept"}}}
	v := mustView(t, rbstate.LayoutFolders, pdbLib(pdb), oneLib(one))
	got := strings.Join(listsOf(v), "\n")
	for _, gone := range []string{"merged/ghost\n", "onelibrary/ghost\n"} {
		if strings.Contains(got+"\n", gone) {
			t.Fatalf("the ghost list %q is shown:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"merged/real", "merged/unique", "merged/folder/ghost", "merged/other", "merged/kept", "onelibrary/unique", "onelibrary/kept", "onelibrary/folder/ghost", "device/other"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%q was dropped:\n%s", kept, got)
		}
	}
	// Without a Device Library there is no evidence: nothing is hidden.
	alone := strings.Join(listsOf(mustView(t, rbstate.LayoutFolders, oneLib(one))), "\n")
	if !strings.Contains(alone, "/ghost\n") {
		t.Fatalf("a OneLibrary-only stick lost a list:\n%s", alone)
	}
}

// A re-export renumbers source IDs: no row hash and no list hash may change with them, or the
// mount would rewrite the whole library for nothing.
func TestViewHashesIgnoreSourceIDsAndFollowContent(t *testing.T) {
	_, one := twoLibraries()
	before := mustView(t, rbstate.LayoutFolders, oneLib(one))
	shifted := *one
	shifted.Tracks = append([]rekordbox.Track(nil), one.Tracks...)
	shifted.Playlists = append([]rekordbox.Playlist(nil), one.Playlists...)
	for i := range shifted.Tracks {
		shifted.Tracks[i].ID += 5000
	}
	for i := range shifted.Playlists {
		shifted.Playlists[i].ID += 700
		if shifted.Playlists[i].ParentID != 0 {
			shifted.Playlists[i].ParentID += 700
		}
		tracks := append([]uint32(nil), shifted.Playlists[i].Tracks...)
		for j := range tracks {
			tracks[j] += 5000
		}
		shifted.Playlists[i].Tracks = tracks
	}
	after := mustView(t, rbstate.LayoutFolders, oneLib(&shifted))
	for i := range before.Tracks {
		if before.Tracks[i].Hash != after.Tracks[i].Hash || before.Tracks[i].Key != after.Tracks[i].Key {
			t.Fatalf("track %d changed with its source ID", i)
		}
		if after.Tracks[i].Refs[0].ID != before.Tracks[i].Refs[0].ID+5000 {
			t.Fatal("the new source ID is not in the ref")
		}
	}
	if !reflect.DeepEqual(before.Lists, after.Lists) {
		t.Fatal("lists changed with their source IDs")
	}
	// One more list at the end: the lists before it keep their hashes.
	shifted.Playlists = append(shifted.Playlists, rekordbox.Playlist{ID: 999, Name: "New", Tracks: []uint32{5001}})
	more := mustView(t, rbstate.LayoutFolders, oneLib(&shifted))
	if len(more.Lists) != len(before.Lists)+1 || !reflect.DeepEqual(more.Lists[:len(before.Lists)], before.Lists) {
		t.Fatal("adding a list at the end changed another list")
	}
	// A changed title changes the row hash; a changed entry changes only the entries half.
	shifted.Tracks[0].Title = "renamed"
	shifted.Playlists[1].Tracks = []uint32{5002}
	edited := mustView(t, rbstate.LayoutFolders, oneLib(&shifted))
	if edited.Tracks[0].Hash == before.Tracks[0].Hash || edited.Tracks[1].Hash != before.Tracks[1].Hash {
		t.Fatal("row hashes do not follow the row's fields")
	}
	a, b := strings.Split(before.Lists[1].Hash, ":"), strings.Split(edited.Lists[1].Hash, ":")
	if a[0] != b[0] || a[1] == b[1] {
		t.Fatalf("list hash halves: %v %v", a, b)
	}
}

func TestViewTreeEdgeCases(t *testing.T) {
	l := &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 1, Path: "/Contents/a.mp3"}, {ID: 2, Path: `\Contents\A.mp3`}, {ID: 3, Path: "/Contents/b.mp3"}},
		Playlists: []rekordbox.Playlist{
			{ID: 1, Name: "Same", Tracks: []uint32{1, 2, 1, 3, 99}}, // the same file twice, a repeat, an unknown track
			{ID: 2, Name: "Same", Tracks: []uint32{3}},
			{ID: 3, ParentID: 77, Name: "Orphan"},
		},
	}
	v := mustView(t, rbstate.LayoutFolders, pdbLib(l))
	if len(v.Tracks) != 2 {
		t.Fatalf("two records of one file gave %d tracks", len(v.Tracks))
	}
	want := []string{"/Same [contents/a.mp3 contents/b.mp3]", "/Same#2 [contents/b.mp3]", "/Orphan"}
	if got := listsOf(v); !reflect.DeepEqual(got, want) {
		t.Fatalf("lists %v, want %v", got, want)
	}
	if v.Lists[1].Name != "Same" {
		t.Fatalf("the second list's title is %q", v.Lists[1].Name)
	}
	for _, bad := range []string{"/Contents/../../etc/passwd", "/Contents/a\x00.mp3"} {
		if _, err := BuildView([]SourceLibrary{pdbLib(&rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: bad}}})}, rbstate.LayoutFolders, "en"); err == nil {
			t.Errorf("a track at %q was accepted", bad)
		}
	}
	link := SourceLibrary{Kind: rbstate.LinkLibrary, Source: "CDJ 1 USB", Lib: &rekordbox.Library{
		Tracks:    []rekordbox.Track{{ID: 1, Path: "/.prolink-cache/CDJ 1 USB/Contents/x.mp3"}},
		Playlists: []rekordbox.Playlist{{ID: 1, Name: "Theirs", Tracks: []uint32{1}}},
	}}
	withLink := mustView(t, rbstate.LayoutFolders, pdbLib(l), link)
	got := listsOf(withLink)
	if len(got) != 5 || got[3] != "link:CDJ 1 USB/" || got[4] != "link:CDJ 1 USB/Theirs [.prolink-cache/cdj 1 usb/contents/x.mp3]" || withLink.Lists[3].Name != "[Pro DJ Link: CDJ 1 USB]" {
		t.Fatalf("staged library: %v", got)
	}
	if ref := withLink.Tracks[2].Refs[0]; ref.Kind != rbstate.LinkLibrary || ref.Source != "CDJ 1 USB" {
		t.Fatalf("staged ref %+v", ref)
	}
}
