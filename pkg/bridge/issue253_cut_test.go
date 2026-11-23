package bridge_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #253 review: an update of a large stick is cut, by a power cut or the hook's kill, at each point
// between two of its writes: the quick update at the mount, the job's hand-off or stage, the
// state compaction, the in-place reread and the staged publish. What the volume holds there is
// what Engine and the next mount find. It must be the previous library with its own state or the
// new one with its own, keep what the deck did, and give the shadow display a waveform for every
// imported track; the next mounts must take it to the update's end without anyone's help, with
// the stamp, the pending and reread marks and the stage in agreement with the library.

// cutDeck is what a deck did to the stick before the update: track 3's cues, grid and loops, its
// rating and play, and a playlist of the deck's own.
type cutDeck struct {
	cues, grid, loops []byte
	list              int64
}

func cutDeckEdits(t *testing.T, root string) cutDeck {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	d := cutDeck{grid: []byte("deck grid 253"), loops: []byte("deck loops 253")}
	d.cues = append([]byte(nil), scalar[[]byte](t, db, `SELECT p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=1`)...)
	d.cues[len(d.cues)-1] ^= 1
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE PerformanceData SET quickCues=?,beatData=?,loops=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=3)`, []any{d.cues, d.grid, d.loops}},
		{`UPDATE Track SET rating=80,isPlayed=1,timeLastPlayed=1763370000 WHERE pdbImportKey=3`, nil},
		{`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Deck set 253',0,1,0)`, nil},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	d.list = scalar[int64](t, db, `SELECT id FROM Playlist WHERE title='Deck set 253'`)
	for _, key := range []int{4, 3} {
		if _, err := db.Exec(`INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId) SELECT ?,id,(SELECT uuid FROM Information),0 FROM Track WHERE pdbImportKey=?`, d.list, key); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d cutDeck) check(t *testing.T, root, when string) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	var cues, grid, loops []byte
	var rating, played, last int64
	if err := db.QueryRow(`SELECT p.quickCues,p.beatData,p.loops,t.rating,t.isPlayed,t.timeLastPlayed FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=3`).Scan(&cues, &grid, &loops, &rating, &played, &last); err != nil {
		t.Fatalf("%s: track 3: %v", when, err)
	}
	if string(cues) != string(d.cues) || string(grid) != string(d.grid) || string(loops) != string(d.loops) {
		t.Fatalf("%s: the cues, grid or loops a deck set on track 3 were lost", when)
	}
	if rating != 80 || played != 1 || last != 1763370000 {
		t.Fatalf("%s: track 3's rating %d, play %d at %d: the deck's were lost", when, rating, played, last)
	}
	if got := scalar[string](t, db, `SELECT group_concat(k) FROM (SELECT t.pdbImportKey k FROM PlaylistEntity e JOIN Track t ON t.id=e.trackId WHERE e.listId=? ORDER BY e.id)`, d.list); got != "4,3" {
		t.Fatalf("%s: the deck's playlist holds %q", when, got)
	}
}

// cutLibrary fails unless the library and its state are sound and every imported track's state row
// gives the shadow display its waveform.
func cutLibrary(t *testing.T, root, when string) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	for _, q := range []string{`PRAGMA main.integrity_check`, `PRAGMA bridge.integrity_check`} {
		if got := scalar[string](t, db, q); got != "ok" {
			t.Fatalf("%s: %s: %s", when, q, got)
		}
	}
	rows, err := db.Query(`SELECT t.pdbImportKey,a.sourceJSON FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId
		LEFT JOIN bridge.BridgeAnalysis a ON a.trackId=s.trackId WHERE s.fingerprint NOT LIKE 'fast:%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key int64
		var b []byte
		if err = rows.Scan(&key, &b); err != nil {
			t.Fatal(err)
		}
		if enginedb.BridgeDetail(b, root, 16<<20) == nil {
			t.Fatalf("%s: track %d's state row (%d bytes) gives the display no waveform", when, key, len(b))
		}
	}
}

// cutEnd is what an update leaves: the library, the stamp and every track's fingerprint.
type cutEnd struct {
	tracks, lists, entries, grids int
	stamp                         string
	prints                        map[int64]string
}

func cutResult(t *testing.T, root string) cutEnd {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	e := cutEnd{
		tracks:  scalar[int](t, db, `SELECT count(*) FROM Track WHERE pdbImportKey>0`),
		lists:   scalar[int](t, db, `SELECT count(*) FROM Playlist`),
		entries: scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity`),
		grids:   scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND p.beatData IS NOT NULL`),
		stamp:   readFileString(filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-stamp")),
		prints:  map[int64]string{},
	}
	rows, err := db.Query(`SELECT t.pdbImportKey,s.fingerprint FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key int64
		var fp string
		if err = rows.Scan(&key, &fp); err != nil {
			t.Fatal(err)
		}
		e.prints[key] = fp
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeState s LEFT JOIN Track t ON t.id=s.trackId WHERE t.id IS NULL`); n != 0 {
		t.Fatalf("%d state rows of tracks the library does not have", n)
	}
	return e
}

func readFileString(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// cutConverge mounts and runs the background job as the deck does until the update is over.
func cutConverge(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(root, "Engine Library", "Database2")
	for i := 0; i < 12; i++ {
		r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
		if err != nil {
			t.Logf("mount %d: %v", i, err)
		}
		left := false
		for _, name := range []string{".rb-bridge-import", ".rb-bridge-publish", ".rb-bridge-pending", ".rb-bridge-reread", ".rb-bridge-state.db.compact"} {
			if _, e := os.Lstat(filepath.Join(dir, name)); e == nil {
				left = true
			}
		}
		if err == nil && r.Bypassed && !r.Background && !left {
			return
		}
		if _, err = bridge.ImportInBackground(ctx, root, nil); err != nil {
			t.Logf("job %d: %v", i, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Fatalf("the update never ended: %v", names)
}

// copyTree copies src to dst with the files' times, as the volume holds them.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err = os.WriteFile(to, b, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(to, info.ModTime(), info.ModTime())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// cutFlow is an update of a converted 40-track stick whose state still holds the waveform copies
// of 5.3.0.5: the re-export adds 20 tracks and track 5's file changed. steps runs it as the deck
// does: the mount, the job, the remount and so on.
type cutFlow struct {
	settings func() func()
	steps    []string // "mount" or "job"
	// after, if set, checks the volume at each cut and after the next mounts.
	after func(t *testing.T, root, when string)
}

func cutStick(t *testing.T) (root string, deck cutDeck) {
	t.Helper()
	root, _ = issue253Converted(t, 40)
	issue253OldRows(t, root)
	deck = cutDeckEdits(t, root)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, testfixture.LargeTrackPath(5)), later, later); err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(60))
	for i := 41; i <= 60; i++ {
		testfixture.Write(t, root, testfixture.LargeTrackPath(i), testfixture.MP3WithLAMETag())
	}
	return root, deck
}

func (f cutFlow) run(root string) {
	ctx := context.Background()
	for _, s := range f.steps {
		if s == "mount" {
			bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
		} else {
			bridge.ImportInBackground(ctx, root, nil)
		}
	}
}

func (f cutFlow) test(t *testing.T) {
	defer f.settings()()
	base, deck := cutStick(t)
	// The update uncut: the points it passes and where it ends.
	var points []string
	ref := t.TempDir()
	copyTree(t, base, ref)
	restore := bridge.SetCutForTest(func(name string) { points = append(points, name) })
	f.run(ref)
	restore()
	cutConverge(t, ref)
	if f.after != nil {
		f.after(t, ref, "uncut")
	}
	want := cutResult(t, ref)
	if len(points) == 0 {
		t.Fatal("the update passed no cut point")
	}
	seen := map[string]int{}
	for _, name := range points {
		seen[name]++
		nth := seen[name]
		t.Run(fmt.Sprintf("%s #%d", name, nth), func(t *testing.T) {
			root := t.TempDir()
			copyTree(t, base, root)
			lib := filepath.Join(root, "Engine Library")
			cutAt := t.TempDir()
			count, taken := 0, false
			restore := bridge.SetCutForTest(func(p string) {
				if p == name {
					if count++; count == nth {
						copyTree(t, lib, cutAt)
						taken = true
					}
				}
			})
			f.run(root)
			restore()
			if !taken {
				t.Fatalf("the update did not pass %q #%d again", name, nth)
			}
			// The volume as the cut left it.
			if err := os.RemoveAll(lib); err != nil {
				t.Fatal(err)
			}
			copyTree(t, cutAt, lib)
			cutLibrary(t, root, "at the cut")
			deck.check(t, root, "at the cut")
			if f.after != nil {
				f.after(t, root, "at the cut")
			}
			cutConverge(t, root)
			cutLibrary(t, root, "after the next mounts")
			deck.check(t, root, "after the next mounts")
			if f.after != nil {
				f.after(t, root, "after the next mounts")
			}
			got := cutResult(t, root)
			if got.tracks != want.tracks || got.lists != want.lists || got.entries != want.entries || got.grids != want.grids || got.stamp != want.stamp {
				t.Fatalf("after the next mounts: %d tracks, %d playlists, %d entries, %d grids, stamp %q; uncut: %d, %d, %d, %d, %q",
					got.tracks, got.lists, got.entries, got.grids, got.stamp, want.tracks, want.lists, want.entries, want.grids, want.stamp)
			}
			for key, fp := range want.prints {
				if got.prints[key] != fp {
					t.Fatalf("after the next mounts: track %d's fingerprint %q, uncut %q: its files were not read as the update reads them", key, got.prints[key], fp)
				}
			}
		})
	}
}

// Few tracks to read: the quick update at the mount, the job's hand-off, the remount reading them
// in place and compacting the state.
func TestIssue253CutInPlaceUpdate(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	cutFlow{
		settings: func() func() {
			a, b := bridge.SetLargeExportForTest(), bridge.SetInPlaceUpdateForTest(1)
			return func() { b(); a() }
		},
		steps: []string{"mount", "job", "mount"},
	}.test(t)
}

// More to read than the mounts take: the quick update, the job's stage with its compact state
// copy, the remount publishing it.
func TestIssue253CutStagedUpdate(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	cutFlow{
		settings: func() func() {
			a, b := bridge.SetLargeExportForTest(), bridge.SetInPlaceUpdateForTest(1<<40)
			return func() { b(); a() }
		},
		steps: []string{"mount", "job", "mount"},
	}.test(t)
}

// A list too long for the mount: the job builds it first, a remount publishes it, then the update
// that reads the files.
func TestIssue253CutListFirstUpdate(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	cutFlow{
		settings: func() func() {
			a, b, c := bridge.SetLargeExportForTest(), bridge.SetInPlaceUpdateForTest(1), bridge.SetMountWindowForTest(time.Second)
			return func() { c(); b(); a() }
		},
		steps: []string{"mount", "job", "mount", "job", "mount"},
	}.test(t)
}

// A small export's update, read in place by the mount with no background job (#253 review 5):
// track 5's file changed, so the import reads it again; it keeps its waveform copy (nothing
// confirms the files here, and the import never decides it), at every cut and in the end.
func TestIssue253CutMountOnlyUpdate(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	cutFlow{
		settings: func() func() { return func() {} },
		steps:    []string{"mount"},
		after: func(t *testing.T, root, when string) {
			t.Helper()
			db := dbOpen(t, root)
			defer db.Close()
			b := scalar[[]byte](t, db, `SELECT a.sourceJSON FROM bridge.BridgeAnalysis a JOIN Track t ON t.id=a.trackId WHERE t.pdbImportKey=5`)
			if len(b) <= 4096 || enginedb.BridgeDetail(b, root, 16<<20) == nil {
				t.Fatalf("%s: track 5's state row (%d bytes) is not its waveform copy", when, len(b))
			}
		},
	}.test(t)
}
