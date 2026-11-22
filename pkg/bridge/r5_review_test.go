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
	if err := os.Chtimes(pdb, when.Add(30*time.Second), when.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Added == 0 {
		t.Fatalf("initial union import: %+v %v", res, err)
	}
	db := dbOpen(t, root)
	if _, err := db.Exec(`UPDATE Track SET rating=77`); err != nil {
		t.Fatal(err)
	}
	before := scalar[int](t, db, `SELECT count(*) FROM Track`)
	db.Close()
	oneStat, err := os.Stat(one)
	if err != nil {
		t.Fatal(err)
	}
	pdbStat, err := os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	oldStamp := fmt.Sprintf("v4 PIONEER/rekordbox/export.pdb %d %d | PIONEER/rekordbox/exportLibrary.db %d %d", pdbStat.Size(), pdbStat.ModTime().UnixNano(), oneStat.Size(), oneStat.ModTime().UnixNano())
	dir := filepath.Join(root, "Engine Library/Database2")
	if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-stamp"), []byte(oldStamp+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-read"), []byte(oldStamp+"\nPIONEER/rekordbox/export.pdb\nPIONEER/rekordbox/exportLibrary.db\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(one, when.Add(2*time.Minute), when.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	oneStat, err = os.Stat(one)
	if err != nil {
		t.Fatal(err)
	}
	pdbStat, err = os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	newStamp := fmt.Sprintf("v4 PIONEER/rekordbox/exportLibrary.db %d %d | PIONEER/rekordbox/export.pdb %d %d", oneStat.Size(), oneStat.ModTime().UnixNano(), pdbStat.Size(), pdbStat.ModTime().UnixNano())
	if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-stamp"), []byte(newStamp+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(one+"-wal", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	log.SetOutput(oldWriter)
	if err != nil || !res.Bypassed {
		t.Fatalf("stale mark did not freeze unreadable primary: %+v %v", res, err)
	}
	if !strings.Contains(logs.String(), "existing library kept") {
		t.Fatalf("stale unreadable primary was not journaled: %q", logs.String())
	}
	if got := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM Track`); got != before {
		t.Fatalf("stale mark changed rows: %d -> %d", before, got)
	}
	if got := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM Track WHERE rating=77`); got != before {
		t.Fatalf("stale mark changed ratings: %d -> %d", before, got)
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

func TestIssue327JournalSidecarUsesTypedFreezePath(t *testing.T) {
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
	if err := os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/export.pdb"), when, when); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(one, when.Add(2*time.Minute), when.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(one+"-journal", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	log.SetOutput(oldWriter)
	if err != nil || !res.Bypassed || !strings.Contains(logs.String(), "existing library kept") {
		t.Fatalf("journal sidecar did not use typed freeze path: %+v %v logs=%q", res, err, logs.String())
	}
}
