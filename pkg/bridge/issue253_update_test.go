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
	db := dbOpen(t, root)
	rnd := rand.New(rand.NewSource(253))
	// Four minutes of PWV7 a track, as earlier builds kept it, and free pages beside.
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
		rows.Scan(&r.id, &r.key)
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		if _, err = db.Exec(`UPDATE bridge.BridgeAnalysis SET sourceJSON=? WHERE trackId=?`, rba1(uint32(r.key), 36000, rnd), r.id); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
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
	if live < 12<<20 || staged > 256<<10 {
		t.Fatalf("the update's state is %d bytes, from a live one of %d", staged, live)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	db = dbOpen(t, root)
	issue253DeckEdit(t, db, cues)
	rows, err = db.Query(`SELECT a.sourceJSON,t.pdbImportKey FROM bridge.BridgeAnalysis a JOIN Track t ON t.id=a.trackId`)
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
	if n != 60 {
		t.Fatalf("%d of 60 tracks with a state row", n)
	}
}
