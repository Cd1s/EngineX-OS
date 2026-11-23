package bridge_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// Real stick, 2026-11-19: the update every converted library gets once ran while the names of
// 707 of the stick's 6013 files were not found (every name with a non-ASCII character; the
// export still listed them all). The update took them for deleted and removed their tracks,
// their playlist entries and what the deck had saved for them. It was stopped by hand before
// the mount that would have published it. The files were all there, under names the volume
// did not give as the export wrote them (a mount without UTF-8 names): a deleted file leaves
// nothing behind, such a name leaves a file no track of the export leads to.

// guardStick is a stick of n tracks, imported complete with their files.
func guardStick(t *testing.T, n int) string {
	t.Helper()
	_, root, skeleton := testlibrary.Create(t)
	skeleton.Close()
	testfixture.LargeExport(t, root, n)
	if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, true); err != nil {
		t.Fatal(err)
	}
	if tracks, _ := guardCounts(t, root); tracks != n {
		t.Fatalf("imported %d of %d tracks", tracks, n)
	}
	return root
}

// guardCounts are the library's tracks with a grid and its playlist entries.
func guardCounts(t *testing.T, root string) (tracks, entries int) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	return scalar[int](t, db, `SELECT count(*) FROM Track t JOIN PerformanceData p ON p.trackId=t.id WHERE length(p.beatData)>0`),
		scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity`)
}

// guardListed are the library's tracks, with or without a grid.
func guardListed(t *testing.T, root string) int {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	return scalar[int](t, db, `SELECT count(*) FROM Track`)
}

// guardNewExport makes the next mount see another export of the same tracks.
func guardNewExport(t *testing.T, root string) {
	t.Helper()
	p := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	at := st.ModTime().Add(time.Hour)
	if err = os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

// guardHide gives the files of count tracks, spread over the stick, a name the export does
// not have, as a volume does whose names cannot be read as the export wrote them; the
// returned function gives them their names back.
func guardHide(t *testing.T, root string, n, count int) (restore func()) {
	t.Helper()
	var hidden []string
	for i := 1; i <= count; i++ {
		p := filepath.Join(root, testfixture.LargeTrackPath(i*(n/count)))
		if err := os.Rename(p, filepath.Join(filepath.Dir(p), "\xe6"+filepath.Base(p))); err != nil {
			t.Fatal(err)
		}
		hidden = append(hidden, p)
	}
	return func() {
		t.Helper()
		for _, p := range hidden {
			if err := os.Rename(filepath.Join(filepath.Dir(p), "\xe6"+filepath.Base(p)), p); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A tenth of the stick: 71 of 600 tracks the export lists and the library has imported.
func TestUpdateKeepsTheTracksWhoseFilesCannotBeFound(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	const n, unreadable = 600, 71
	root := guardStick(t, n)
	_, entries := guardCounts(t, root)
	ctx := context.Background()
	same := func(when string) {
		t.Helper()
		if tracks, e := guardCounts(t, root); tracks != n || e != entries {
			t.Fatalf("%s: %d tracks and %d playlist entries, want the %d and %d of before", when, tracks, e, n, entries)
		}
	}

	// The stick's own case: the update is built after the mount and published by a later one.
	restoreSize := bridge.SetLargeExportForTest()
	t.Cleanup(restoreSize) // also when this test stops before it gets there
	guardNewExport(t, root)
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount that starts the update: %+v %v", r, err)
	}
	restoreNames := guardHide(t, root, n, unreadable)
	if _, err := bridge.ImportInBackground(ctx, root, nil); !errors.Is(err, bridge.ErrFilesUnreadable) {
		t.Fatalf("update with %d of %d files not found: %v, want it refused", unreadable, n, err)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount after the refused update: %+v %v, want nothing published and the update still to do", r, err)
	}
	same("after the refused update and the mount that follows")
	// The names are readable again: the same update goes through, with every track.
	restoreNames()
	if _, err := bridge.ImportInBackground(ctx, root, nil); err != nil {
		t.Fatalf("update with the files back: %v", err)
	}
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatalf("mount after the update: %v", err)
	}
	same("after the update with the files back")
	restoreSize()

	// A small export is updated while the mount waits: the same refusal, the same library.
	guardNewExport(t, root)
	restoreNames = guardHide(t, root, n, unreadable)
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); !errors.Is(err, bridge.ErrFilesUnreadable) {
		t.Fatalf("update at the mount with %d of %d files not found: %v, want it refused", unreadable, n, err)
	}
	same("after the update refused at the mount")
	restoreNames()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Bypassed {
		t.Fatalf("next mount, files back: %+v %v, want the update done", r, err)
	}
	same("after the update at the mount with the files back")

	// A folder whose name cannot be read (the stick's artist folders) hides every track in it.
	guardNewExport(t, root)
	var folders []string
	for _, i := range []int{1, 11} { // two artists, ten tracks each
		dir := filepath.Dir(filepath.Dir(filepath.Join(root, testfixture.LargeTrackPath(i))))
		if err := os.Rename(dir, filepath.Join(filepath.Dir(dir), "\xe6"+filepath.Base(dir))); err != nil {
			t.Fatal(err)
		}
		folders = append(folders, dir)
	}
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); !errors.Is(err, bridge.ErrFilesUnreadable) {
		t.Fatalf("update with two artist folders not found: %v, want it refused", err)
	}
	same("after the update refused for two folders")
	for _, dir := range folders {
		if err := os.Rename(filepath.Join(filepath.Dir(dir), "\xe6"+filepath.Base(dir)), dir); err != nil {
			t.Fatal(err)
		}
	}
}

// A stick published from its export alone, before its files were read (every large stick
// between its first and its second remount): its tracks have no grid yet and nothing of the
// deck's, and are kept all the same.
func TestUpdateKeepsTheTracksOfAStickListedFromItsExport(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	const n, unreadable = 100, 30
	_, root, skeleton := testlibrary.Create(t)
	skeleton.Close()
	testfixture.LargeExport(t, root, n)
	defer bridge.SetLargeExportForTest()()
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	if _, err := bridge.ImportInBackground(ctx, root, nil); err != nil {
		t.Fatalf("build from the export: %v", err)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount publishing the export's list: %+v %v", r, err)
	}
	if got := guardListed(t, root); got != n {
		t.Fatalf("published from the export: %d of %d tracks", got, n)
	}
	restoreNames := guardHide(t, root, n, unreadable)
	if _, err := bridge.ImportInBackground(ctx, root, nil); !errors.Is(err, bridge.ErrFilesUnreadable) {
		t.Fatalf("update with %d of %d files not found: %v, want it refused", unreadable, n, err)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount after the refused update: %+v %v, want nothing published", r, err)
	}
	if got := guardListed(t, root); got != n {
		t.Fatalf("after the refused update: %d of %d tracks listed", got, n)
	}
	restoreNames()
	if _, err := bridge.ImportInBackground(ctx, root, nil); err != nil {
		t.Fatalf("update with the files back: %v", err)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("mount publishing the update: %+v %v", r, err)
	}
	if tracks, _ := guardCounts(t, root); tracks != n {
		t.Fatalf("after the update with the files back: %d of %d tracks with a grid", tracks, n)
	}
}

// Neighbours that stay as they were: files deleted from the stick after the export, few or
// many, tracks deleted in rekordbox, and tracks whose files were never on the stick.
func TestUpdateStillRemovesWhatIsGone(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	const n = 100
	ctx := context.Background()
	updated := func(what, root string, want int) {
		t.Helper()
		guardNewExport(t, root)
		if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
			t.Fatalf("update after %s: %v", what, err)
		}
		if tracks, _ := guardCounts(t, root); tracks != want {
			t.Fatalf("%s: %d tracks, want %d", what, tracks, want)
		}
	}

	// Five files deleted from the stick, the export still lists them.
	root := guardStick(t, n)
	for i := 1; i <= 5; i++ {
		if err := os.Remove(filepath.Join(root, testfixture.LargeTrackPath(i*20))); err != nil {
			t.Fatal(err)
		}
	}
	updated("five files were deleted", root, n-5)

	// Forty files deleted by hand, far more than a few: nothing lies where they were but what
	// a computer leaves beside music (a cover, a hidden companion file), so they are gone.
	root = guardStick(t, n)
	for i := 1; i <= 40; i++ {
		p := filepath.Join(root, testfixture.LargeTrackPath(i*2))
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		testfixture.Write(t, root, filepath.Join(filepath.Dir(testfixture.LargeTrackPath(i*2)), "cover.jpg"), []byte("jpeg"))
		testfixture.Write(t, root, filepath.Join(filepath.Dir(testfixture.LargeTrackPath(i*2)), "._"+filepath.Base(p)), []byte("AppleDouble"))
	}
	updated("forty files were deleted by hand", root, n-40)

	// Whole albums deleted by hand: three artists' folders removed, three more emptied with
	// their folders (and a hidden file) left behind. An empty folder hides no track.
	root = guardStick(t, n)
	for i := 1; i <= 60; i++ {
		p := filepath.Join(root, testfixture.LargeTrackPath(i))
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if i <= 30 && i%10 == 0 { // the artist's last track: its folder goes
			if err := os.RemoveAll(filepath.Dir(filepath.Dir(p))); err != nil {
				t.Fatal(err)
			}
		} else if i > 30 {
			testfixture.Write(t, root, filepath.Join(filepath.Dir(testfixture.LargeTrackPath(i)), ".DS_Store"), []byte("finder"))
		}
	}
	updated("six artists' albums were deleted by hand", root, n-60)

	// Forty tracks deleted in rekordbox: the new export does not list them.
	root = guardStick(t, n)
	testfixture.LargeExport(t, root, n-40)
	updated("a re-export without forty tracks", root, n-40)

	// Thirty files that were never on the stick: the first import lists their tracks from the
	// export alone, and the update that reads the files takes them out again.
	_, root, skeleton := testlibrary.Create(t)
	skeleton.Close()
	testfixture.LargeExport(t, root, n)
	for i := 1; i <= 30; i++ {
		if err := os.Remove(filepath.Join(root, testfixture.LargeTrackPath(i*3))); err != nil {
			t.Fatal(err)
		}
	}
	defer bridge.SetLargeExportForTest()()
	for step := 0; step < 2; step++ { // the build from the export, then the update with the files
		if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
			t.Fatalf("mount %d of a stick without thirty of its files: %+v %v", step+1, r, err)
		}
		if _, err := bridge.ImportInBackground(ctx, root, nil); err != nil {
			t.Fatalf("background import %d of a stick without thirty of its files: %v", step+1, err)
		}
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("mount publishing the update: %+v %v", r, err)
	}
	if got := guardListed(t, root); got != n-30 {
		t.Fatalf("stick without thirty of its files: %d tracks, want %d", got, n-30)
	}
}
