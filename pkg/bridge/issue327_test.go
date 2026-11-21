package bridge_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestIssue327ReadLibraryPublishesBothReadableLibraries(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dbPath)
	pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(pdbPath, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dbPath, when, when); err != nil {
		t.Fatal(err)
	}

	lib, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(lib.Tracks); got != 7 {
		t.Fatalf("read %d tracks, want union of both libraries", got)
	}
}

func TestIssue327Pre2000TimestampDoesNotDemoteOneLibrary(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dbPath)
	pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	old := time.Unix(315532800, 0)
	current := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(dbPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdbPath, current, current); err != nil {
		t.Fatal(err)
	}

	lib, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Tracks) != 7 || lib.Tracks[0].ID != 101 {
		t.Fatalf("unknown file time changed source preference: tracks=%d first=%d", len(lib.Tracks), lib.Tracks[0].ID)
	}
}

func TestIssue327UnreadableOneLibraryFallsBackAndLogs(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dbPath)
	if err := os.WriteFile(dbPath+"-wal", []byte("uncheckpointed"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(dbPath, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdbPath, when.Add(30*time.Second), when.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)
	lib, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Tracks) != 5 {
		t.Fatalf("fallback read %d tracks, want Device Library only", len(lib.Tracks))
	}
	if !strings.Contains(logs.String(), "exportLibrary.db unreadable") {
		t.Fatalf("missing unreadable-library log: %q", logs.String())
	}
}

func TestIssue327UpgradeFromOneLibraryKeepsDeckEditsAndIDs(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	if err := os.Remove(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dbPath)
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(dbPath, when, when); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || res.Added != 3 {
		t.Fatalf("initial OneLibrary import: %+v %v", res, err)
	}
	db := dbOpen(t, root)
	trackID := scalar[int64](t, db, `SELECT id FROM Track WHERE pdbImportKey=101`)
	cues := append([]byte(nil), scalar[[]byte](t, db, `SELECT quickCues FROM PerformanceData WHERE trackId=?`, trackID)...)
	if _, err := db.Exec(`UPDATE Track SET rating=77,isPlayed=1,timeLastPlayed=1763370000 WHERE id=?`, trackID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	if err := os.WriteFile(pdbPath, testfixture.PDB(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdbPath, when, when); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Bypassed {
		t.Fatalf("union import: %+v %v", res, err)
	}
	db = dbOpen(t, root)
	if got := scalar[int64](t, db, `SELECT id FROM Track WHERE pdbImportKey=101`); got != trackID {
		t.Fatalf("OneLibrary track ID changed: %d -> %d", trackID, got)
	}
	if got := scalar[int](t, db, `SELECT rating FROM Track WHERE id=?`, trackID); got != 77 {
		t.Fatalf("deck rating changed: %d", got)
	}
	if got := scalar[int](t, db, `SELECT isPlayed FROM Track WHERE id=?`, trackID); got != 1 {
		t.Fatal("deck play mark changed")
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM Track`); got != 7 {
		t.Fatalf("merged track count %d", got)
	}
	if !bytes.Equal(cues, scalar[[]byte](t, db, `SELECT quickCues FROM PerformanceData WHERE trackId=?`, trackID)) {
		t.Fatal("deck cues changed")
	}
}

func TestIssue327ConvertedBothExportsCatchesUpUnionOnce(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	if err := os.Remove(pdbPath); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dbPath)
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(dbPath, when, when); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || res.Added != 3 {
		t.Fatalf("single-library preparation: %+v %v", res, err)
	}
	db := dbOpen(t, root)
	if _, err := db.Exec(`UPDATE Track SET rating=77`); err != nil {
		t.Fatal(err)
	}
	oldRows := map[string]int64{}
	rows, err := db.Query(`SELECT path,id FROM Track WHERE pdbImportKey>0`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var path string
		var id int64
		if err := rows.Scan(&path, &id); err != nil {
			t.Fatal(err)
		}
		oldRows[path] = id
	}
	rows.Close()
	db.Close()

	if err := os.WriteFile(pdbPath, testfixture.PDB(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdbPath, when, when); err != nil {
		t.Fatal(err)
	}
	pdbStat, err := os.Stat(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	dbStat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprintf("v4 PIONEER/rekordbox/export.pdb %d %d | PIONEER/rekordbox/exportLibrary.db %d %d\n", pdbStat.Size(), pdbStat.ModTime().UnixNano(), dbStat.Size(), dbStat.ModTime().UnixNano())
	if err := os.WriteFile(filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp"), []byte(stamp), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || res.Bypassed || res.Added == 0 {
		t.Fatalf("union catch-up: %+v %v", res, err)
	}
	db = dbOpen(t, root)
	if got := scalar[int](t, db, `SELECT count(*) FROM Track`); got != 7 {
		t.Fatalf("catch-up track count %d", got)
	}
	for path, wantID := range oldRows {
		var gotID, rating int64
		if err := db.QueryRow(`SELECT id,rating FROM Track WHERE path=?`, path).Scan(&gotID, &rating); err != nil {
			t.Fatal(err)
		}
		if gotID != wantID || rating != 77 {
			t.Fatalf("existing row %q changed: id %d/%d rating %d", path, wantID, gotID, rating)
		}
	}
	db.Close()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !res.Bypassed {
		t.Fatalf("second catch-up insert: %+v %v", res, err)
	}
}

func TestIssue327SourceFlipAndUnreadableKeepRowsByPath(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	dbPath := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, dbPath)
	pdbPath := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	base := time.Unix(1_730_000_000, 0)
	_ = os.Chtimes(dbPath, base, base)
	_ = os.Chtimes(pdbPath, base, base)
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || res.Added != 7 {
		t.Fatalf("initial union: %+v %v", res, err)
	}
	db := dbOpen(t, root)
	if _, err := db.Exec(`UPDATE Track SET rating=77`); err != nil {
		t.Fatal(err)
	}
	rows := map[string]int64{}
	r, err := db.Query(`SELECT path,id FROM Track WHERE pdbImportKey>0`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var path string
		var id int64
		if err := r.Scan(&path, &id); err != nil {
			t.Fatal(err)
		}
		rows[path] = id
	}
	r.Close()
	db.Close()

	if err := os.Chtimes(pdbPath, base.Add(100*time.Second), base.Add(100*time.Second)); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Bypassed {
		t.Fatalf("preferred-source flip: %+v %v", res, err)
	}
	db = dbOpen(t, root)
	for path, wantID := range rows {
		if got := scalar[int64](t, db, `SELECT id FROM Track WHERE path=?`, path); got != wantID {
			t.Fatalf("path %q changed row id %d -> %d", path, wantID, got)
		}
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM Track WHERE rating=77`); got != len(rows) {
		t.Fatalf("source flip kept %d/%d ratings", got, len(rows))
	}
	db.Close()

	if err := os.WriteFile(dbPath+"-wal", []byte("uncheckpointed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dbPath, base.Add(200*time.Second), base.Add(200*time.Second)); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !res.Bypassed {
		t.Fatalf("unreadable source: %+v %v", res, err)
	}
	db = dbOpen(t, root)
	if got := scalar[int](t, db, `SELECT count(*) FROM Track WHERE rating=77`); got != len(rows) {
		t.Fatalf("unreadable source kept %d/%d ratings", got, len(rows))
	}
	db.Close()
}

func TestIssue327DeletedFromBothLibrariesIsRemoved(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 3)
	ctx := context.Background()
	if res, err := bridge.Convert(ctx, root, enginedb.Options{}); err != nil || res.Added != 3 {
		t.Fatalf("initial import: %+v %v", res, err)
	}
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.LargePDB(2))
	if err := os.Remove(filepath.Join(root, testfixture.LargeTrackPath(3))); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.Convert(ctx, root, enginedb.Options{}); err != nil {
		t.Fatalf("deleted-track import: %+v %v", res, err)
	}
	if got := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM Track`); got != 2 {
		t.Fatalf("deleted track remained: %d", got)
	}
}
