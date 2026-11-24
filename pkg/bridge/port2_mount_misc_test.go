package bridge

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

func p2OpenDB(t *testing.T, mount string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func p2TrackID(t *testing.T, db *sql.DB, path string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM Track WHERE path=?`, path).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPort2NativeTrackConflictRemainsNative(t *testing.T) {
	mount := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	testfixture.LargeExport(t, mount, 5)
	mountNow(t, mount, MountOptions{})

	testfixture.LargeExport(t, mount, 6)
	db := p2OpenDB(t, mount)
	nativePath := "../" + testfixture.LargeTrackPath(6)
	if _, err := db.Exec(`INSERT INTO Track(path,title) VALUES(?,?)`, nativePath, "Native title"); err != nil {
		t.Fatal(err)
	}
	nativeID := p2TrackID(t, db, nativePath)
	db.Close()

	res, err := ConvertAtMount(context.Background(), mount, MountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 {
		t.Fatalf("native row was counted as bridge-owned: %+v", res)
	}
	var title string
	db = p2OpenDB(t, mount)
	if err := db.QueryRow(`SELECT title FROM Track WHERE id=?`, nativeID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Native title" {
		t.Fatalf("native title changed to %q", title)
	}
	s := snapshot(t, mount)
	tr := s.Tracks[rbstate.FoldPath(testfixture.LargeTrackPath(6))]
	if tr == nil || !tr.Native || tr.EngineID != nativeID {
		t.Fatalf("native row was not recorded in the snapshot: %+v", tr)
	}
}

func TestPort2FailedMountRollsBackLiveLibrary(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "Changed")
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", pdb)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}
	db := p2OpenDB(t, mount)
	if _, err := db.Exec(`CREATE TRIGGER p2_fail_import BEFORE INSERT ON PlaylistEntity BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	beforeDB, err := os.ReadFile(filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	beforeSnapshot, err := os.ReadFile(filepath.Join(dbDir(mount), rbstate.SnapshotName))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err = ConvertAtMount(context.Background(), mount, MountOptions{}); err == nil {
		t.Fatal("failing playlist trigger was ignored")
	}
	afterDB, err := os.ReadFile(filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	afterSnapshot, err := os.ReadFile(filepath.Join(dbDir(mount), rbstate.SnapshotName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeDB, afterDB) {
		t.Fatal("m.db changed after the failed mount")
	}
	if !bytes.Equal(beforeSnapshot, afterSnapshot) {
		t.Fatal("snapshot changed after the failed mount")
	}
}

// A track whose file is not on the stick has no row, and stays without one: no later window
// and no re-export adds the row again for the next window to remove (replica, KDATA: 32 such
// tracks were added by every window's diff and removed by the next one's staged data, twelve
// windows over, and the import never came to rest). When the file is back the track gets its
// row, its data and its place in the lists.
func TestMountLeavesATrackWithoutItsFileWithoutARow(t *testing.T) {
	mount := stick(t)
	file := filepath.Join(mount, "MUSIC/four.wav")
	audio, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	fill := func() {
		t.Helper()
		if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mountNow(t, mount, MountOptions{})
	fill()
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Removed != 1 || !res.Pending.None() {
		t.Fatalf("window that takes the row away: %+v", res)
	}
	rows := func() string { return query(t, mount, `SELECT count(*) FROM Track`)[0][0] }
	for i := 0; i < 3; i++ {
		fill()
		res := mountNow(t, mount, MountOptions{Full: true, Publish: true})
		if res.Added != 0 || res.Removed != 0 || !res.Pending.None() || rows() != "4" {
			t.Fatalf("window %d after the row went: %+v, %s rows", i, res, rows())
		}
	}
	// A re-export that touches nothing of this track does not bring the row back either.
	later := time.Now().Add(time.Hour)
	if err = os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}
	if res := mountNow(t, mount, MountOptions{}); res.Added != 0 || rows() != "4" {
		t.Fatalf("re-export: %+v, %s rows", res, rows())
	}
	fill()
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Added != 0 || res.Removed != 0 || !res.Pending.None() || rows() != "4" {
		t.Fatalf("window after the re-export: %+v, %s rows", res, rows())
	}
	tree := strings.Join(libraryTree(t, mount), "\n")
	if strings.Contains(tree, "four.wav") {
		t.Fatalf("the lists name the missing track:\n%s", tree)
	}
	// The file is back. An unchanged export is one stat and no job, so it is found at the next
	// export: its window starts the job, the job finds the file, and the job's window adds the row.
	if err = os.WriteFile(file, audio, 0o644); err != nil {
		t.Fatal(err)
	}
	later = later.Add(time.Hour)
	if err = os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}
	if res := mountNow(t, mount, MountOptions{}); res.Added != 0 || rows() != "4" {
		t.Fatalf("window of the export after the file came back: %+v, %s rows", res, rows())
	}
	fill()
	res := mountNow(t, mount, MountOptions{Publish: true})
	if res.Added != 1 || res.Applied != 1 || !res.Pending.None() || rows() != "5" {
		t.Fatalf("window after the file came back: %+v, %s rows", res, rows())
	}
	if tree = strings.Join(libraryTree(t, mount), "\n"); !strings.Contains(tree, "four.wav") {
		t.Fatalf("the lists lack the track that is back:\n%s", tree)
	}
	if again := mountNow(t, mount, MountOptions{}); !again.Bypassed {
		t.Fatalf("not at rest: %+v", again)
	}
}

func TestPort2MissingTrackIsRemovedAndDuplicateSourceIsJoined(t *testing.T) {
	mount := stick(t)
	if err := os.Remove(filepath.Join(mount, "MUSIC/four.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{Publish: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("missing track removal: %+v", res)
	}
	if got := query(t, mount, `SELECT count(*) FROM Track`); got[0][0] != "4" {
		t.Fatalf("track rows %s, want 4", got[0][0])
	}
	if got := libraryTree(t, mount); len(got) != 5 || strings.Contains(strings.Join(got, "\n"), "four.wav") {
		t.Fatalf("lists after missing track: %v", got)
	}

	r, files := mustSources(t, mount)
	libs, _, err := ReadSources(context.Background(), r, mount, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := libs[0].Lib.Tracks[0]
	duplicate.ID = 99
	libs[0].Lib.Tracks = append(libs[0].Lib.Tracks, duplicate)
	libs[0].Lib.Playlists = append(libs[0].Lib.Playlists, rekordbox.Playlist{ID: 99, Name: "Duplicate", Tracks: []uint32{1, 99}})
	v, err := BuildView(libs, rbstate.LayoutFolders, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Tracks) != 5 {
		t.Fatalf("duplicate source record created %d view tracks", len(v.Tracks))
	}
	for _, list := range v.Lists {
		if list.Name == "Duplicate" {
			if len(list.Tracks) != 1 || list.Tracks[0] != rbstate.FoldPath("MUSIC/"+testfixture.UnicodeName) {
				t.Fatalf("duplicate source list entries: %v", list.Tracks)
			}
			return
		}
	}
	t.Fatal("duplicate source list missing")
}

func TestPort2EngineAnalysisIsReplacedWhileDeckCueIsKept(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	s := snapshot(t, mount)
	key := rbstate.FoldPath("MUSIC/" + testfixture.UnicodeName)
	s.Tracks[key].Built = rbstate.TrackSig{}
	s.Tracks[key].Perf = rbstate.PerfNone
	s.Tracks[key].Written = nil
	s.Pending.Perf = 1
	if err := s.Save(dbDir(mount)); err != nil {
		t.Fatal(err)
	}
	stageAll(t, mount)
	s = snapshot(t, mount)
	stage, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := stage.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	stagedBeats := append([]byte(nil), rec.Perf.Beats...)
	_ = stage.Close()
	db := p2OpenDB(t, mount)
	id := s.Tracks[key].EngineID
	position := 250.0
	if err := enginedb.SetQuickCue(context.Background(), db, id, 0, "deck edit", "#12AB34", &position); err != nil {
		t.Fatal(err)
	}
	var deckCues []byte
	if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, id).Scan(&deckCues); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Applied != 1 {
		t.Fatalf("publish: %+v", res)
	}
	db = p2OpenDB(t, mount)
	var gotCues, gotBeats []byte
	if err := db.QueryRow(`SELECT quickCues,beatData FROM PerformanceData WHERE trackId=?`, id).Scan(&gotCues, &gotBeats); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCues, deckCues) {
		t.Fatal("deck hot cue was overwritten")
	}
	if !bytes.Equal(gotBeats, stagedBeats) {
		t.Fatal("Engine's analyzed grid was not replaced by staged rekordbox data")
	}
}

func TestPort2RebuiltPlaylistsAreReusedWithoutDuplicates(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	db := p2OpenDB(t, mount)
	var beforeLists, beforeEntries int
	if err := db.QueryRow(`SELECT count(*) FROM Playlist`).Scan(&beforeLists); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM PlaylistEntity`).Scan(&beforeEntries); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM PlaylistEntity;
CREATE TEMP TABLE p2_saved AS SELECT title,parentListId FROM Playlist WHERE parentListId=0;
DELETE FROM Playlist WHERE parentListId<>0;
DELETE FROM Playlist;
INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported)
SELECT title,parentListId,1,0,1 FROM p2_saved;
DROP TABLE p2_saved`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.PlaylistsChanged {
		t.Fatal("rebuilt playlists were not restored")
	}
	db = p2OpenDB(t, mount)
	var gotLists, gotEntries int
	_ = db.QueryRow(`SELECT count(*) FROM Playlist`).Scan(&gotLists)
	_ = db.QueryRow(`SELECT count(*) FROM PlaylistEntity`).Scan(&gotEntries)
	if gotLists != beforeLists || gotEntries != beforeEntries {
		t.Fatalf("rebuilt library counts %d/%d, want %d/%d", gotLists, gotEntries, beforeLists, beforeEntries)
	}
	if got := query(t, mount, `SELECT count(*) FROM Playlist WHERE title LIKE '%(rekordbox%'`); got[0][0] != "0" {
		t.Fatalf("duplicate rekordbox playlists: %s", got[0][0])
	}
}

func TestPort2ProtectedAndReadOnlyLibrariesAreNotWritten(t *testing.T) {
	noSource := t.TempDir()
	if res, err := ConvertAtMount(context.Background(), noSource, MountOptions{}); err != nil || !res.Bypassed {
		t.Fatalf("empty mount: %+v %v", res, err)
	}

	mount := stick(t)
	engine := filepath.Join(mount, "Engine Library")
	outside := t.TempDir()
	if err := os.Symlink(outside, engine); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err == nil {
		t.Fatal("symlinked Engine Library was accepted")
	}

	mount = stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	mdb := filepath.Join(dbDir(mount), "m.db")
	original, err := os.ReadFile(mdb)
	if err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(t.TempDir(), "m.db")
	if err := os.Rename(mdb, linkTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(linkTarget, mdb); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{Full: true}); err == nil {
		t.Fatal("hard-linked m.db was accepted")
	}
	if got, err := os.ReadFile(mdb); err != nil || !bytes.Equal(got, original) {
		t.Fatal("hard-linked m.db changed")
	}

	mount = stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	mdb = filepath.Join(dbDir(mount), "m.db")
	before, err := os.ReadFile(mdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mdb, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbDir(mount), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dbDir(mount), 0o755)
		_ = os.Chmod(mdb, 0o644)
	})
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{})
	if err != nil || !res.Bypassed {
		t.Fatalf("read-only unchanged mount: %+v %v", res, err)
	}
	after, err := os.ReadFile(mdb)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only m.db changed")
	}
}

func TestPort2UppercaseAndGeneratedExports(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	old := filepath.Join(mount, "PIONEER/rekordbox")
	upper := filepath.Join(mount, "PIONEER/REKORDBOX")
	if err := os.Rename(old, upper); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(upper, "export.pdb"), filepath.Join(upper, "EXPORT.PDB")); err != nil {
		t.Fatal(err)
	}
	if res, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil || res.Added != 5 {
		t.Fatalf("uppercase export: %+v %v", res, err)
	}

	generated := t.TempDir()
	testfixture.Export(t, generated)
	pdb := filepath.Join(generated, "PIONEER/rekordbox/export.pdb")
	st, err := os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(filepath.Dir(pdb), ".enginex-generated")
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := ConvertAtMount(context.Background(), generated, MountOptions{}); err != nil || !res.Bypassed {
		t.Fatalf("generated export: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dbDir(generated), "m.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("generated export created a library")
	}
	if err := os.WriteFile(marker, []byte("0:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := ConvertAtMount(context.Background(), generated, MountOptions{}); err != nil || res.Added != 5 {
		t.Fatalf("rewritten export: %+v %v", res, err)
	}
}

func TestPort2StagedCoverIsDeduplicatedAndServed(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	s := snapshot(t, mount)
	stage, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stage.Remove() })
	art := []byte{0xff, 0xd8, 0xff, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11}
	for _, key := range []string{rbstate.FoldPath("MUSIC/three.wav"), rbstate.FoldPath("MUSIC/four.wav")} {
		rec, err := stage.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		rec.Art = art
		if err := stage.Put(rec); err != nil {
			t.Fatal(err)
		}
	}
	_ = stage.Close()
	res := mountNow(t, mount, MountOptions{Publish: true})
	if res.Applied != 5 {
		t.Fatalf("publish: %+v", res)
	}
	db := p2OpenDB(t, mount)
	var count, hashLen, refs int
	// The image two tracks share is one row, whatever covers the other tracks have.
	sum := sha1.Sum(art)
	if err := db.QueryRow(`SELECT count(*),COALESCE(max(length(hash)),0) FROM AlbumArt WHERE hash=?`, sum[:]).Scan(&count, &hashLen); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM Track WHERE albumArtId IN (SELECT id FROM AlbumArt WHERE hash=?)`, sum[:]).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if count != 1 || hashLen != sha1.Size || refs != 2 {
		t.Fatalf("artwork rows=%d hash bytes=%d refs=%d", count, hashLen, refs)
	}
	path := filepath.Join(mount, "Engine Library/Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, art) {
		t.Fatalf("artwork file: %v", err)
	}
}

func TestPort2QueuedHotCueAppliesAtMount(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	db := p2OpenDB(t, mount)
	var id int64
	if err := db.QueryRow(`SELECT trackId FROM PerformanceData WHERE quickCues IS NOT NULL ORDER BY trackId LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := enginedb.QueuePending(mount, func(p *enginedb.Pending) {
		p.HotCues = append(p.HotCues, enginedb.PendingCue{TrackID: id, Index: 7, Label: "queued on the deck", Color: "#FF0000"})
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	res := mountNow(t, mount, MountOptions{})
	if !res.Bypassed {
		t.Fatalf("queued edit mount: %+v", res)
	}
	pending := filepath.Join(dbDir(mount), enginedb.PendingFile)
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queued edits remain: %v", err)
	}
	var blob []byte
	db = p2OpenDB(t, mount)
	if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, id).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	raw, err := enginedb.QUncompress(blob, 64<<10)
	if err != nil || !bytes.Contains(raw, []byte("queued on the deck")) {
		t.Fatalf("queued cue missing: %v", err)
	}
}

func TestPort2SmartMatchRecomputesAndValidatesReference(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{SmartMatch: 1})
	if err != nil || res.Bypassed {
		t.Fatalf("smart match mount: %+v %v", res, err)
	}
	if got := query(t, mount, `SELECT count(*) FROM Playlist WHERE title='[Smart Match]'`); got[0][0] != "1" {
		t.Fatalf("smart match list count: %s", got[0][0])
	}
	if got := query(t, mount, `SELECT t.id FROM Playlist p JOIN PlaylistEntity e ON e.listId=p.id JOIN Track t ON t.id=e.trackId WHERE p.title='[Smart Match]' ORDER BY t.id`); len(got) != 2 || got[0][0] != "2" || got[1][0] != "3" {
		t.Fatalf("smart match tracks: %v", got)
	}

	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "Changed")
	for _, pair := range [][2]uint32{{12400, 12000}, {11800, 13000}, {12401, 12000}} {
		from, to := make([]byte, 4), make([]byte, 4)
		binary.LittleEndian.PutUint32(from, pair[0])
		binary.LittleEndian.PutUint32(to, pair[1])
		pdb = bytes.ReplaceAll(pdb, from, to)
	}
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", pdb)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{SmartMatch: 1}); err != nil {
		t.Fatal(err)
	}
	if got := query(t, mount, `SELECT t.id FROM Playlist p JOIN PlaylistEntity e ON e.listId=p.id JOIN Track t ON t.id=e.trackId WHERE p.title='[Smart Match]' ORDER BY t.id`); len(got) != 2 || got[0][0] != "2" || got[1][0] != "5" {
		t.Fatalf("recomputed smart match tracks: %v", got)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{SmartMatch: 99}); err == nil || !strings.Contains(err.Error(), "reference track") {
		t.Fatalf("unknown smart match reference error: %v", err)
	}
}

func TestPort2IntelligentListCarriesMaterializedTracks(t *testing.T) {
	mount := stick(t)
	r, files := mustSources(t, mount)
	libs, _, err := ReadSources(context.Background(), r, mount, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	libs[0].Lib.Playlists = append(libs[0].Lib.Playlists, rekordbox.Playlist{
		ID: 99, Name: "Intelligent", Intelligent: true, Tracks: []uint32{1, 3},
	})
	v, err := BuildView(libs, rbstate.LayoutFolders, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range v.Lists {
		if list.Name == "Intelligent" {
			if len(list.Tracks) != 2 || list.Tracks[0] != rbstate.FoldPath("MUSIC/"+testfixture.UnicodeName) || list.Tracks[1] != rbstate.FoldPath("MUSIC/three.wav") {
				t.Fatalf("intelligent list tracks: %v", list.Tracks)
			}
			return
		}
	}
	t.Fatal("intelligent list missing from the view")
}

func TestPort2OnDiskSpellingReachesThePublishedRow(t *testing.T) {
	mount := stick(t)
	if err := os.Rename(filepath.Join(mount, "MUSIC/three.wav"), filepath.Join(mount, "MUSIC/Three.WAV")); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{Publish: true}); err != nil {
		t.Fatal(err)
	}
	if got := query(t, mount, `SELECT count(*) FROM Track WHERE path='../MUSIC/Three.WAV'`); got[0][0] != "1" {
		t.Fatalf("stored path spelling: %s", got[0][0])
	}
}

func p2OldMP3BeatData(t *testing.T, beat0, beat1 float64) []byte {
	t.Helper()
	raw := make([]byte, 130)
	binary.BigEndian.PutUint64(raw[0:], math.Float64bits(44100))
	binary.BigEndian.PutUint64(raw[8:], math.Float64bits(441000))
	raw[16] = 1
	binary.BigEndian.PutUint64(raw[17:], 2)
	binary.BigEndian.PutUint64(raw[73:], 2)
	put := func(off int, pos float64, beat int64, num uint32) {
		binary.LittleEndian.PutUint64(raw[off:], math.Float64bits(pos))
		binary.LittleEndian.PutUint64(raw[off+8:], uint64(beat))
		binary.LittleEndian.PutUint32(raw[off+16:], num)
	}
	put(25, beat0, -4, 24)
	put(49, beat1, 20, 0)
	put(81, beat0, -4, 24)
	put(105, beat1, 20, 0)
	return raw
}

func TestPort2MountFixesAnEngineXMP3GridOnAnUnchangedExport(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	testfixture.Write(t, mount, "Music/Stream/101.mp3", testfixture.MP3WithLAMETag())
	db := p2OpenDB(t, mount)
	trackRaw := make([]byte, 44)
	binary.BigEndian.PutUint64(trackRaw[0:], math.Float64bits(44100))
	binary.BigEndian.PutUint64(trackRaw[8:], 441000)
	trackData, err := enginedb.QCompress(trackRaw)
	if err != nil {
		t.Fatal(err)
	}
	beatData, err := enginedb.QCompress(p2OldMP3BeatData(t, 5000, 460000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(path,filename,fileType,comment,isAnalyzed,isAvailable,isMetadataImported,isBeatGridLocked)
VALUES('../Music/Stream/101.mp3','101.mp3','mp3','[320k] MP3 44100 Hz; FLAC unavailable',1,1,1,0)`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM Track WHERE path='../Music/Stream/101.mp3'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,beatData=?,thirdPartySourceId=NULL WHERE trackId=?`, trackData, beatData, id); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_ = os.Remove(filepath.Join(dbDir(mount), enginedb.MP3GridStampFile))
	_ = os.Remove(filepath.Join(dbDir(mount), enginedb.MP3GridCursorFile))

	res := mountNow(t, mount, MountOptions{})
	if !res.Bypassed {
		t.Fatalf("unchanged export mount: %+v", res)
	}
	db = p2OpenDB(t, mount)
	var blob []byte
	if err := db.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=?`, id).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	raw, err := enginedb.QUncompress(blob, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := math.Float64frombits(binary.LittleEndian.Uint64(raw[25:33]))
	if math.Abs(got-2743) > 1e-6 {
		t.Fatalf("beat0=%v, want 2743", got)
	}
	db.Close()
	before, err := os.ReadFile(filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dbDir(mount), "m.db"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("second MP3 grid mount changed m.db")
	}
}
