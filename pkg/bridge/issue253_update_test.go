package bridge_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #253 (5.3.0.6): on the maintainer's SC5000 a re-export of a converted 6000-track stick took
// 11.5 minutes and its new playlists were still not there afterwards: the mount only marked the
// update, the background job copied m.db (320 MB) and the bridge state (2.7 GB) byte for byte
// before its first pass, and a remount published it only once no deck held a track of the stick.

// issue253Converted is a stick whose export of n tracks a mount imported completely, with what a
// deck did to track 3 afterwards: a play and other hot cues.
func issue253Converted(t *testing.T, n int) (root string, cues []byte) {
	t.Helper()
	root = t.TempDir()
	testfixture.LargeExport(t, root, n)
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || r.Added != n {
		t.Fatalf("import: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	// Another track's cues, changed: a valid blob that is not what the import wrote for track 3.
	cues = append([]byte(nil), scalar[[]byte](t, db, `SELECT p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=1`)...)
	cues[len(cues)-1] ^= 1
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=3)`, cues); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET isPlayed=1,timeLastPlayed=1763370000 WHERE pdbImportKey=3`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return root, cues
}

// issue253DeckEdit fails unless track 3 still has what the deck did to it.
func issue253DeckEdit(t *testing.T, db *sql.DB, cues []byte) {
	t.Helper()
	if got := scalar[[]byte](t, db, `SELECT p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=3`); !bytes.Equal(got, cues) {
		t.Fatal("the hot cues a deck set on track 3 were lost")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE pdbImportKey=3 AND isPlayed=1 AND timeLastPlayed=1763370000`); n != 1 {
		t.Fatal("the play a deck recorded on track 3 was lost")
	}
}

func TestIssue253ReexportShowsItsPlaylistsAndNewTracksAtTheMount(t *testing.T) {
	root, cues := issue253Converted(t, 40)
	testfixture.LargeExport(t, root, 60) // re-exported with 20 new tracks, all in its playlist
	restore := bridge.SetLargeExportForTest()
	defer restore()
	ctx := context.Background()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.Background || r.Added != 20 || !r.PlaylistsChanged {
		t.Fatalf("mount of the re-export: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM Track`); n != 60 {
		t.Fatalf("%d tracks in the library at the mount, want 60", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity e JOIN Playlist p ON p.id=e.listId WHERE p.title='Large Set'`); n != 60 {
		t.Fatalf("the re-export's playlist has %d of its 60 entries at the mount", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeState WHERE fingerprint LIKE 'fast:%'`); n != 20 {
		t.Fatalf("%d tracks left for the background update, want the 20 new ones", n)
	}
	issue253DeckEdit(t, db, cues)
	db.Close()
	if _, err := os.Stat(stagePath(root, "KIND")); err != nil {
		t.Fatalf("the update did not go on in the background: %v", err)
	}
	// The background update reads the new tracks' files; a mount publishes it.
	if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("mount publishing the update: %+v %v", r, err)
	}
	db = dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND p.beatData IS NOT NULL`); n != 60 {
		t.Fatalf("%d of 60 tracks with their grid after the publish", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeState WHERE fingerprint LIKE 'fast:%'`); n != 0 {
		t.Fatalf("%d tracks still without their files read", n)
	}
	issue253DeckEdit(t, db, cues)
	if got := scalar[string](t, db, `PRAGMA integrity_check`); got != "ok" {
		t.Fatal(got)
	}
}

// A quick update that would not end before the hook's kill writes nothing: the update goes on in
// the background as before, and the library stays as it was until it is published.
func TestIssue253QuickUpdateThatDoesNotFitWritesNothing(t *testing.T) {
	root, cues := issue253Converted(t, 40)
	before := fileSum(t, stagePath(root, "..", "m.db"))
	testfixture.LargeExport(t, root, 60)
	restore := bridge.SetLargeExportForTest()
	defer restore()
	defer bridge.SetMountWindowForTest(time.Second)()
	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil || !r.Background || r.Added != 0 {
		t.Fatalf("mount: %+v %v", r, err)
	}
	if fileSum(t, stagePath(root, "..", "m.db")) != before {
		t.Fatal("a quick update that did not fit wrote the library")
	}
	db := dbOpen(t, root)
	issue253DeckEdit(t, db, cues)
	if _, err := os.Stat(stagePath(root, "KIND")); err != nil {
		t.Fatalf("the update did not go on in the background: %v", err)
	}
}

// A first import published at the mount from a large export starts the update that reads its
// files at once: it waited for the next insert.
func TestIssue253FastPublishStartsTheBackgroundUpdate(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 40)
	defer bridge.SetUpdateExportForTest()()
	ctx := context.Background()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || r.Added != 40 || !r.PlaylistsChanged || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	if b, err := os.ReadFile(stagePath(root, "KIND")); err != nil || string(bytes.TrimSpace(b)) != "update" {
		t.Fatalf("no update stage after the first publish: %q %v", b, err)
	}
	if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL`); n != 40 {
		t.Fatalf("%d of 40 tracks with their grid", n)
	}
}

// issue253OldRows gives every track of the converted stick at root the state row a build before
// #253 wrote, the source ID and four minutes of PWV7, and leaves 8 MB of free pages beside.
func issue253OldRows(t *testing.T, root string) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	rnd := rand.New(rand.NewSource(253))
	if _, err := db.Exec(`CREATE TABLE bridge.pad(b BLOB); INSERT INTO bridge.pad VALUES(zeroblob(8388608)); DROP TABLE bridge.pad`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT a.trackId,t.pdbImportKey FROM bridge.BridgeAnalysis a JOIN Track t ON t.id=a.trackId`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ id, key int64 }
	var all []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.id, &r.key); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		if _, err = db.Exec(`UPDATE bridge.BridgeAnalysis SET sourceJSON=? WHERE trackId=?`, rba1(uint32(r.key), 36000, rnd), r.id); err != nil {
			t.Fatal(err)
		}
	}
}

// issue253Refs fails unless every track of the library at root has a state row of this build: its
// source ID and the ANLZ files of its detail, in at most 256 bytes.
func issue253Refs(t *testing.T, root string, tracks int) {
	t.Helper()
	db := dbOpen(t, root)
	defer db.Close()
	rows, err := db.Query(`SELECT a.sourceJSON,t.pdbImportKey FROM bridge.BridgeAnalysis a JOIN Track t ON t.id=a.trackId`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var b []byte
		var key int64
		if err = rows.Scan(&b, &key); err != nil {
			t.Fatal(err)
		}
		var src struct {
			Track  struct{ ID uint32 }
			Detail []string
		}
		plain, err := enginedb.UnpackAnalysis(b, 1<<20)
		if len(b) > 256 || err != nil || json.Unmarshal(plain, &src) != nil || int64(src.Track.ID) != key || len(src.Detail) == 0 {
			t.Fatalf("track %d: row of %d bytes, %+v %v", key, len(b), src, err)
		}
		n++
	}
	if n != tracks {
		t.Fatalf("%d of %d tracks with a state row", n, tracks)
	}
}

// rba1 is a state row as builds before #253 wrote it: the source ID and a copy of a PWV7.
func rba1(id uint32, entries int, rnd *rand.Rand) []byte {
	raw := make([]byte, 20+3*entries)
	copy(raw, "RBA1")
	binary.LittleEndian.PutUint32(raw[4:], id)
	copy(raw[8:], "PWV7")
	binary.LittleEndian.PutUint32(raw[12:], 3)
	binary.LittleEndian.PutUint32(raw[16:], uint32(3*entries))
	rnd.Read(raw[20:])
	var b bytes.Buffer
	w, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	w.Write(raw)
	w.Close()
	return b.Bytes()
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// An update builds its state without the waveforms earlier builds copied into it and without
// their free pages, and does not read them: the publish has a reference to the track's ANLZ files
// for every track, with its source ID.
func TestIssue253UpdateLeavesTheWaveformCopiesBehind(t *testing.T) {
	root, cues := issue253Converted(t, 40)
	issue253OldRows(t, root)
	live := fileSize(t, stagePath(root, "..", ".rb-bridge-state.db"))
	testfixture.LargeExport(t, root, 60)
	restore := bridge.SetLargeExportForTest()
	defer restore()
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount: %+v %v", r, err)
	}
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	staged := fileSize(t, stagePath(root, ".rb-bridge-state.db"))
	t.Logf("state: live %d bytes, update's %d bytes", live, staged)
	if live < 10<<20 || staged > 256<<10 {
		t.Fatalf("the update's state is %d bytes, from a live one of %d", staged, live)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	issue253DeckEdit(t, db, cues)
	db.Close()
	issue253Refs(t, root, 60)
}

// An update of a large library with few tracks to read is read in place by the next mount, which
// the background job's remount is: the job copies nothing and writes neither database, it only
// checks every track's files. The quick update at the mount brought the playlists before.
func TestIssue253SmallUpdateOfALargeLibraryIsReadInPlace(t *testing.T) {
	root, cues := issue253Converted(t, 40)
	issue253OldRows(t, root)
	db := dbOpen(t, root)
	changed := scalar[string](t, db, `SELECT s.fingerprint FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId WHERE t.pdbImportKey=5`)
	db.Close()
	// Track 5's file changed on the stick since its import, and 20 tracks are new.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(stagePath(root, "..", "..", "..", testfixture.LargeTrackPath(5)), later, later); err != nil {
		t.Fatal(err)
	}
	testfixture.LargeExport(t, root, 60)
	defer bridge.SetLargeExportForTest()()
	defer bridge.SetInPlaceUpdateForTest(1)()
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background || r.Added != 20 {
		t.Fatalf("mount of the re-export: %+v %v", r, err)
	}
	mdb, state := fileSum(t, stagePath(root, "..", "m.db")), fileSum(t, stagePath(root, "..", ".rb-bridge-state.db"))
	r, err := bridge.ImportInBackground(ctx, root, nil)
	if err != nil || r.Bypassed || r.Deferred != 21 {
		t.Fatalf("background job: %+v %v", r, err)
	}
	if fileSum(t, stagePath(root, "..", "m.db")) != mdb || fileSum(t, stagePath(root, "..", ".rb-bridge-state.db")) != state {
		t.Fatal("the background job wrote the live library")
	}
	if _, err := os.Stat(stagePath(root)); !os.IsNotExist(err) {
		t.Fatalf("the job left its stage: %v", err)
	}
	// The job's remount: the mount reads the 21 tracks in place.
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background || r.Deferred != 0 {
		t.Fatalf("remount: %+v %v", r, err)
	}
	db = dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND p.beatData IS NOT NULL`); n != 60 {
		t.Fatalf("%d of 60 tracks with their grid", n)
	}
	if fp := scalar[string](t, db, `SELECT s.fingerprint FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId WHERE t.pdbImportKey=5`); fp == changed {
		t.Fatal("track 5's changed file was not read again")
	}
	issue253DeckEdit(t, db, cues)
	for _, name := range []string{".rb-bridge-pending", ".rb-bridge-reread"} {
		if _, err := os.Stat(stagePath(root, "..", name)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", name, err)
		}
	}
	db.Close()
	// That mount also left the waveform copies behind: references only.
	if n := fileSize(t, stagePath(root, "..", ".rb-bridge-state.db")); n > 256<<10 {
		t.Fatalf("live state %d bytes after the update", n)
	}
	issue253Refs(t, root, 60)
	// Nothing left: the next mount reads nothing.
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Bypassed {
		t.Fatalf("mount after: %+v %v", r, err)
	}
}

// A track the re-export moved keeps its row and what a deck did to it: the quick update leaves the
// row where it is, and the update moves it with its files read, as an import that owns it.
func TestIssue253QuickUpdateLeavesAMovedTrackToTheUpdate(t *testing.T) {
	root, cues := issue253Converted(t, 40)
	testfixture.LargeExport(t, root, 60)
	// rekordbox moved track 3's file: same length, so only the path string changes in the export.
	utf16le := func(s string) []byte {
		var b []byte
		for _, r := range s {
			b = append(b, byte(r), 0)
		}
		return b
	}
	from, to := testfixture.LargeTrackPath(3), strings.Replace(testfixture.LargeTrackPath(3), "Track00003", "Moved00003", 1)
	pdb := stagePath(root, "..", "..", "..", "PIONEER", "rekordbox", "export.pdb")
	b, err := os.ReadFile(pdb)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(b, utf16le(from)) != 1 {
		t.Fatal("track 3's path not found once in the export")
	}
	if err = os.WriteFile(pdb, bytes.Replace(b, utf16le(from), utf16le(to), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(stagePath(root, "..", "..", "..", from), stagePath(root, "..", "..", "..", to)); err != nil {
		t.Fatal(err)
	}
	defer bridge.SetLargeExportForTest()()
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background || r.Added != 20 {
		t.Fatalf("mount of the re-export: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	issue253DeckEdit(t, db, cues)
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId WHERE t.pdbImportKey=3 AND s.fingerprint NOT LIKE 'fast:%' AND t.path=?`, "../"+from); n != 1 {
		t.Fatal("the quick update moved track 3's row or made it an export-only row")
	}
	db.Close()
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	db = dbOpen(t, root)
	if got := scalar[string](t, db, `SELECT path FROM Track WHERE pdbImportKey=3`); got != "../"+to {
		t.Fatalf("track 3 at %q after the update, want the moved path", got)
	}
	issue253DeckEdit(t, db, cues)
}

// stateBudget is what the bridge state may hold per track: its row, fingerprint, written digest
// and the reference to its ANLZ files are a few hundred bytes. 5.3.0.5 kept about 100 KB a track
// (a copy of the PWV7); the maintainer's 6027-track stick had a 2.7 GB state file (#253).
const stateBudget = 1 << 10

// A large export with four minutes of detail per track stays within the budget, after its import
// and after an update of a re-export.
func TestIssue253StateStaysWithinAPerTrackBudget(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 600)
	wave := make([]byte, 12+3*36000)
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], 36000)
	rand.New(rand.NewSource(253)).Read(wave[12:])
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, wave)))
	ctx := context.Background()
	if r, err := bridge.Convert(ctx, root, enginedb.Options{}); err != nil || r.Added != 600 || r.Deferred != 0 {
		t.Fatalf("import: %+v %v", r, err)
	}
	state := stagePath(root, "..", ".rb-bridge-state.db")
	budget := func(when string, tracks int) {
		t.Helper()
		size := fileSize(t, state)
		t.Logf("%s: state %d bytes, %d a track", when, size, size/int64(tracks))
		if size > int64(tracks)*stateBudget {
			t.Fatalf("%s: state %d bytes for %d tracks, budget %d a track", when, size, tracks, stateBudget)
		}
	}
	budget("import", 600)
	testfixture.LargeExport(t, root, 650)
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, wave)))
	defer bridge.SetLargeExportForTest()()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("mount of the re-export: %+v %v", r, err)
	}
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	budget("update", 650)
	issue253Refs(t, root, 650)
}
