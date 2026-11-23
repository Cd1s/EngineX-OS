package bridge

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestSQLite(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func TestVacuumStageMDBKeepsCommittedWALRows(t *testing.T) {
	restore := SetPriorityDirForTest(t.TempDir())
	defer restore()

	stage := t.TempDir()
	dbPath := filepath.Join(stage, "m.db")
	db := openTestSQLite(t, dbPath)
	mustExec(t, db, `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE t(v INTEGER); INSERT INTO t VALUES(1);`)
	mustExec(t, db, `INSERT INTO t VALUES(2);`)
	vacuumStageMDB(context.Background(), stage)
	closeAndReopen := openTestSQLite(t, dbPath)
	var got int
	if err := closeAndReopen.QueryRow(`SELECT count(*) FROM t`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("committed WAL row lost: got %d, want 2", got)
	}
}

func copyStageDirForCutTest(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestVacuumStageMDBSurvivesKillBetweenSteps(t *testing.T) {
	restore := SetPriorityDirForTest(t.TempDir())
	defer restore()

	cuts := []string{
		"compact vacuumed m.db",
		"compact ready m.db",
		"compact replaced m.db",
		"compact sidecars removed m.db",
	}
	for _, targetCut := range cuts {
		t.Run(targetCut, func(t *testing.T) {
			stage := t.TempDir()
			dbPath := filepath.Join(stage, "m.db")
			db := openTestSQLite(t, dbPath)
			mustExec(t, db, `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE t(v INTEGER); INSERT INTO t VALUES(1), (2), (3);`)

			var snapped string
			oldCut := cutPoint
			cutPoint = func(name string) {
				if name == targetCut && snapped == "" {
					if strings.HasPrefix(name, "compact vacuumed") || strings.HasPrefix(name, "compact ready") || strings.HasPrefix(name, "compact replaced") {
						if _, err := os.Stat(dbPath + "-wal"); err != nil {
							t.Errorf("WAL sidecar removed before replacement became durable at %q: %v", name, err)
						}
					}
					snapped = copyStageDirForCutTest(t, stage)
				}
			}
			defer func() { cutPoint = oldCut }()

			vacuumStageMDB(context.Background(), stage)
			if snapped == "" {
				t.Fatalf("cut point %q was not reached", targetCut)
			}
			if err := finishStageCompact(snapped); err != nil {
				t.Fatalf("finishStageCompact(%q): %v", targetCut, err)
			}
			for _, leftover := range []string{".compact", ".compact.ready"} {
				if _, err := os.Stat(filepath.Join(snapped, "m.db"+leftover)); !os.IsNotExist(err) {
					t.Fatalf("leftover %s after recovery from %q", leftover, targetCut)
				}
			}
			if targetCut != "compact vacuumed m.db" {
				for _, side := range []string{"-wal", "-shm", "-journal"} {
					if _, err := os.Stat(filepath.Join(snapped, "m.db"+side)); !os.IsNotExist(err) {
						t.Fatalf("leftover %s after durable replacement recovery from %q", side, targetCut)
					}
				}
			}
			reopened := openTestSQLite(t, filepath.Join(snapped, "m.db"))
			var got int
			if err := reopened.QueryRow(`SELECT count(*) FROM t`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != 3 {
				t.Fatalf("rows after recovery at %q: got %d, want 3", targetCut, got)
			}
		})
	}
}

func TestVacuumStageMDBDeckActiveAndDeadline(t *testing.T) {
	restore := SetPriorityDirForTest(t.TempDir())
	defer restore()
	pq := NewPriorityQueue()
	setActive := func(active bool) {
		now := time.Now()
		if active {
			pq.UpdateDeckActivity(true, now)
			return
		}
		pq.UpdateDeckActivity(false, now.Add(-2*idleSampleMinGap))
		pq.UpdateDeckActivity(false, now)
	}

	// 1. Skips immediately while a deck is active (keep WAL connection open so -wal stays on disk).
	stage := t.TempDir()
	dbPath := filepath.Join(stage, "m.db")
	db := openTestSQLite(t, dbPath)
	mustExec(t, db, `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE t(v BLOB); INSERT INTO t VALUES(randomblob(65536)); DELETE FROM t; INSERT INTO t VALUES(X'01');`)
	setActive(true)
	vacuumStageMDB(context.Background(), stage)
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Fatalf("active deck should skip compaction and leave WAL untouched: %v", err)
	}
	_ = db.Close()
	beforeSz, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Stops mid-compaction when a deck becomes active.
	setActive(false)
	oldCut := cutPoint
	cutPoint = func(name string) {
		if name == "compact vacuumed m.db" {
			setActive(true)
		}
	}
	vacuumStageMDB(context.Background(), stage)
	cutPoint = oldCut
	afterActiveSz, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterActiveSz.Size() != beforeSz.Size() {
		t.Fatalf("compaction replaced m.db after deck became active: before=%d after=%d", beforeSz.Size(), afterActiveSz.Size())
	}

	// 3. Enforces deadline without losing rows, then compacts once idle with normal deadline.
	setActive(false)
	oldTimeout := stageCompactTimeout
	stageCompactTimeout = time.Nanosecond
	vacuumStageMDB(context.Background(), stage)
	stageCompactTimeout = oldTimeout
	afterDeadlineSz, err := os.Stat(dbPath)
	if err != nil || afterDeadlineSz.Size() != beforeSz.Size() {
		t.Fatalf("expired deadline should not compact m.db: err=%v", err)
	}
	vacuumStageMDB(context.Background(), stage)
	compactedSz, err := os.Stat(dbPath)
	if err != nil || compactedSz.Size() >= beforeSz.Size()/2 {
		t.Fatalf("idle compaction did not shrink m.db: before=%d after=%d err=%v", beforeSz.Size(), compactedSz.Size(), err)
	}

	reopened := openTestSQLite(t, dbPath)
	var got int
	if err := reopened.QueryRow(`SELECT count(*) FROM t`).Scan(&got); err != nil || got != 1 {
		t.Fatalf("rows after compaction: got=%d err=%v, want 1", got, err)
	}
}
