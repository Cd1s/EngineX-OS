package bridge_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestIssue327StaleReadMarkFreezesUnreadablePrimary(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	one := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, one)
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(one, when, when); err != nil {
		t.Fatal(err)
	}
	pdb := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	if err := os.WriteFile(pdb, testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "r5"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdb, when, when); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Added == 0 {
		t.Fatalf("initial union import: %+v %v", res, err)
	}
	before := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM Track`)
	if err := os.Chtimes(one, when.Add(2*time.Minute), when.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	oneStat, err := os.Stat(one)
	if err != nil {
		t.Fatal(err)
	}
	pdbStat, err := os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	newStamp := fmt.Sprintf("v4 PIONEER/rekordbox/exportLibrary.db %d %d | PIONEER/rekordbox/export.pdb %d %d", oneStat.Size(), oneStat.ModTime().UnixNano(), pdbStat.Size(), pdbStat.ModTime().UnixNano())
	if err := os.WriteFile(filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp"), []byte(newStamp+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(one+"-wal", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !res.Bypassed {
		t.Fatalf("stale mark did not freeze unreadable primary: %+v %v", res, err)
	}
	if got := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM Track`); got != before {
		t.Fatalf("stale mark changed rows: %d -> %d", before, got)
	}
}

func TestIssue327ReadableLibraryReturningIsImportedWithoutExportChange(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	one := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, one)
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(one, when, when); err != nil {
		t.Fatal(err)
	}
	pdb := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	if err := os.Chtimes(pdb, when.Add(30*time.Second), when.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(one+"-wal", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Added == 0 {
		t.Fatalf("initial readable-library import: %+v %v", res, err)
	}
	if got := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM Track`); got != 5 {
		t.Fatalf("initial Device Library rows=%d, want 5", got)
	}
	if err := os.Remove(one + "-wal"); err != nil {
		t.Fatal(err)
	}
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || res.Bypassed {
		t.Fatalf("readable library was bypassed: %+v %v", res, err)
	}
	if got := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM Track`); got != 7 {
		t.Fatalf("union rows after library recovery=%d, want 7", got)
	}
}

func TestIssue327PersistentUnreadableLibraryKeepsFastBypass(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	for _, name := range []string{"one.wav", "two.wav", "three.wav"} {
		testfixture.Write(t, root, "MUSIC/"+name, testfixture.WAV(true))
	}
	one := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	copyEncryptedOneLibraryFixture(t, one)
	if err := os.WriteFile(one+"-wal", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdb := filepath.Join(root, "PIONEER/rekordbox/export.pdb")
	when := time.Unix(1_730_000_000, 0)
	if err := os.Chtimes(one, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdb, when.Add(30*time.Second), when.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Added == 0 {
		t.Fatalf("initial Device Library import: %+v %v", res, err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !res.Bypassed {
		t.Fatalf("persistent unreadable library lost the fast bypass: %+v %v", res, err)
	}
	if err := os.Remove(one + "-wal"); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Bypassed {
		t.Fatalf("library recovery was bypassed: %+v %v", res, err)
	}
}

func TestIssue327DuplicateFoldedCandidatesKeepBothEngineRows(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "THREE.wav", "five.wav"}, "r5"))
	testfixture.Write(t, root, "MUSIC/THREE.wav", testfixture.WAV(false))
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enginedb.Sync(context.Background(), root, l, enginedb.Options{MatchByPath: true}); err != nil {
		t.Fatal(err)
	}
	db := dbOpen(t, root)
	if _, err := db.Exec(`UPDATE Track SET rating=33 WHERE path='../MUSIC/three.wav'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET rating=44 WHERE path='../MUSIC/THREE.wav'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	shifted, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for i := range shifted.Tracks {
		shifted.Tracks[i].ID += 100
	}
	for i, j := 0, len(shifted.Tracks)-1; i < j; i, j = i+1, j-1 {
		shifted.Tracks[i], shifted.Tracks[j] = shifted.Tracks[j], shifted.Tracks[i]
	}
	if _, err := enginedb.Sync(context.Background(), root, shifted, enginedb.Options{MatchByPath: true, PathFirst: true}); err != nil {
		t.Fatalf("reversed duplicate candidates: %v", err)
	}
	db = dbOpen(t, root)
	defer db.Close()
	if got := scalar[int](t, db, `SELECT count(*) FROM Track WHERE lower(path)='../music/three.wav'`); got != 2 {
		t.Fatalf("folded duplicate rows=%d, want 2", got)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM Track WHERE rating IN (33,44)`); got != 2 {
		t.Fatalf("folded duplicate ratings kept=%d, want 2", got)
	}
}
