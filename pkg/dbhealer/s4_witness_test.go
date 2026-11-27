package dbhealer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func seedVerifiedS4Library(t *testing.T, tracks, playlists int) (string, string) {
	t.Helper()
	mount, path := seedPopulatedS4Mount(t, tracks, playlists)
	if r, err := HealMountDataOnly(mount, 0); err != nil || !r.Healthy {
		t.Fatalf("initial heal: healthy=%v, err=%v", r.Healthy, err)
	}
	if _, _, err := verifySnapshot(mount); err != nil {
		t.Fatalf("initial snapshot: %v", err)
	}
	return mount, path
}

func editS4DB(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range statements {
		mustExec(t, db, q)
	}
}

func TestS4InterruptedSnapshotPreservesPriorGeneration(t *testing.T) {
	mount, path := seedVerifiedS4Library(t, 30, 4)
	editS4DB(t, path, "UPDATE Track SET title='new title' WHERE id=1")
	// Force metadata staging to fail after a new generation image has been staged.
	if err := os.Mkdir(filepath.Join(snapshotDir(mount), ".m.db.meta.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := HealMountDataOnly(mount, 0); err != nil {
		t.Fatal(err)
	}
	_, _, verifyErr := verifySnapshot(mount)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.RestoredFromBackup {
		t.Fatalf("no usable generation: verify=%v, restored=%v, err=%v", verifyErr, r.RestoredFromBackup, err)
	}
}

func TestS4AcceptedRemovalSurvivesLaterTruncation(t *testing.T) {
	mount, path := seedVerifiedS4Library(t, 30, 4)
	editS4DB(t, path, "DELETE FROM PlaylistEntity", "DELETE FROM Playlist", "DELETE FROM Track WHERE id>5")
	for i := 6; i <= 30; i++ {
		if err := os.Remove(filepath.Join(mount, "Music", fmt.Sprintf("track-%02d.mp3", i))); err != nil {
			t.Fatal(err)
		}
	}
	if r, err := HealMountDataOnly(mount, 0); err != nil || !r.Healthy || r.RestoredFromBackup {
		t.Fatalf("removal not accepted: %+v, %v", r, err)
	}
	if n, p, _ := queryCounts(t, path); n != 5 || p != 0 {
		t.Fatalf("removal counts were %d/%d", n, p)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := HealMountDataOnly(mount, 0); err != nil {
		t.Fatal(err)
	}
	if n, p, e := queryCounts(t, path); n != 5 || p != 0 || e != 0 {
		t.Fatalf("recovered %d tracks, %d playlists, %d entries after confirmed removal; want 5/0/0", n, p, e)
	}
}

func TestS4FutureLayoutPreservedWithoutDowngrade(t *testing.T) {
	mount, path := seedVerifiedS4Library(t, 30, 4)
	editS4DB(t, path,
		"UPDATE Information SET schemaVersionMajor=4, uuid='review-future'",
		"ALTER TABLE Information RENAME COLUMN schemaVersionPatch TO revision",
	)
	check := func(want int) {
		t.Helper()
		db, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var v int
		var integrity string
		if err := db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity=%s, err=%v", integrity, err)
		}
		if err := db.QueryRow("SELECT schemaVersionMajor FROM Information").Scan(&v); err != nil || v != want {
			t.Fatalf("schema major=%d, want %d, err=%v", v, want, err)
		}
	}
	check(4)
	r, err := HealMountDataOnly(mount, 0)
	if r.RestoredFromBackup {
		t.Fatalf("future layout was overwritten from backup: %+v %v", r, err)
	}
	if err == nil {
		t.Fatalf("expected unsupported schema error, got nil")
	}
	check(4)
}

func TestS4SmallLibraryWipeRetainsPopulatedSnapshot(t *testing.T) {
	mount, path := seedVerifiedS4Library(t, 12, 2)
	editS4DB(t, path, "DELETE FROM PlaylistEntity", "DELETE FROM Playlist", "DELETE FROM Track")
	if _, err := HealMountDataOnly(mount, 0); err != nil {
		t.Fatal(err)
	}
	_, meta, err := verifySnapshot(mount)
	if err != nil || meta.Tracks != 12 || meta.Playlists != 2 {
		t.Fatalf("last populated snapshot now has %d tracks, %d playlists, err=%v", meta.Tracks, meta.Playlists, err)
	}
}

func TestS4ExpiredMountLeavesLiveDBUnmodified(t *testing.T) {
	mount, path := seedVerifiedS4Library(t, 30, 4)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ := HealMountDataOnlyContext(ctx, mount, 0)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.RestoredFromBackup || st.Size() != 0 {
		t.Fatalf("expired mount replaced the live file: restored=%v, size=%d", r.RestoredFromBackup, st.Size())
	}
}

func TestS4DataOnlyEnforcesFullSynchronous(t *testing.T) {
	_, path := seedPopulatedS4Mount(t, 20, 3)
	db, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mode := func() int {
		var n int
		if err := db.QueryRow("PRAGMA synchronous").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	mustExec(t, db, "PRAGMA synchronous=NORMAL")
	if _, err := heal(db, true); err != nil {
		t.Fatal(err)
	}
	if n := mode(); n != 2 {
		t.Fatalf("data-only session retained synchronous=%d; want explicit FULL=2", n)
	}
	var statTables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='sqlite_stat1'").Scan(&statTables); err != nil || statTables != 0 {
		t.Fatalf("data-only heal must not run PRAGMA optimize / create sqlite_stat1: count=%d err=%v", statTables, err)
	}
}

func TestS4MissingLiveDBRecoversFromVerifiedSnapshot(t *testing.T) {
	mount, path := seedVerifiedS4Library(t, 30, 4)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.RestoredFromBackup {
		t.Fatalf("verified backup exists, live name absent: restored=%v, missing=%v, err=%v", r.RestoredFromBackup, os.IsNotExist(err), err)
	}
	if n, p, e := queryCounts(t, path); n != 30 || p != 4 || e != 30 {
		t.Fatalf("restored missing m.db counts=(%d,%d,%d), want (30,4,30)", n, p, e)
	}
}
