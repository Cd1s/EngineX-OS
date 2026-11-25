package dbhealer

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func seedPopulatedS4Mount(t *testing.T, trackCount, playlistCount int) (mount, dbPath string) {
	t.Helper()
	_, root, db := testlibrary.Create(t)
	var uuidVal string
	if err := db.QueryRow(`SELECT COALESCE(uuid,'') FROM Information LIMIT 1`).Scan(&uuidVal); err != nil {
		t.Fatal(err)
	}
	if uuidVal == "" {
		uuidVal = "s4-test-library-uuid"
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, uuidVal); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= trackCount; i++ {
		rel := fmt.Sprintf("../Music/track-%02d.mp3", i)
		full := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("audio-data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,fileBytes,isAnalyzed) VALUES(?,?,?,?,0)`,
			i, rel, filepath.Base(rel), 10,
		); err != nil {
			t.Fatal(err)
		}
	}
	entityID := 1
	for p := 1; p <= playlistCount; p++ {
		nextList := p + 1
		if p == playlistCount {
			nextList = 0
		}
		if _, err := db.Exec(
			`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(?,?,0,1,?,1700000000,1)`,
			p, fmt.Sprintf("Set %d", p), nextList,
		); err != nil {
			t.Fatal(err)
		}
		perList := trackCount / playlistCount
		start := (p-1)*perList + 1
		end := start + perList - 1
		if p == playlistCount {
			end = trackCount
		}
		for tid := start; tid <= end; tid++ {
			nextEnt := entityID + 1
			if tid == end {
				nextEnt = 0
			}
			if _, err := db.Exec(
				`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,?,?,?,?,0)`,
				entityID, p, tid, uuidVal, nextEnt,
			); err != nil {
				t.Fatal(err)
			}
			entityID++
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, "Engine Library", "Database2", "m.db")
}

func reportJSONMap(t *testing.T, r Report) map[string]any {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func queryCounts(t *testing.T, dbPath string) (tracks, playlists, entities int) {
	t.Helper()
	db, err := Open(dbPath, true)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM Track),(SELECT count(*) FROM Playlist),(SELECT count(*) FROM PlaylistEntity)`).Scan(&tracks, &playlists, &entities); err != nil {
		t.Fatalf("counts %s: %v", dbPath, err)
	}
	return tracks, playlists, entities
}

func TestS4SavesRollingSnapshotAndFastSkipsUnchanged(t *testing.T) {
	mount, dbPath := seedPopulatedS4Mount(t, 24, 4)

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("initial heal failed: %+v %v", r, err)
	}
	// Database2 must stay clean (no extra backup files inside Database2).
	db2Entries, err := os.ReadDir(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(db2Entries) != 1 || db2Entries[0].Name() != "m.db" {
		t.Fatalf("unexpected files in Database2: %v", db2Entries)
	}
	snapPath := filepath.Join(mount, ".enginex-backup", "db", "m.db")
	metaPath := filepath.Join(mount, ".enginex-backup", "db", "m.db.meta")
	meta1, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("expected rolling snapshot metadata at %s: %v", metaPath, err)
	}
	st1, err := os.Stat(snapPath)
	if err != nil || st1.Size() < 100 {
		t.Fatalf("expected rolling snapshot m.db at %s: %v", snapPath, err)
	}

	// Second pass on unchanged m.db must fast-skip without rewriting snapshot.
	time.Sleep(15 * time.Millisecond)
	r2, err := HealMountDataOnly(mount, 0)
	if err != nil || !r2.Healthy {
		t.Fatalf("second heal failed: %+v %v", r2, err)
	}
	meta2, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(meta2) != string(meta1) {
		t.Fatalf("unchanged m.db rewrote snapshot meta:\nbefore=%s\nafter=%s", meta1, meta2)
	}

	// Read-only mount must never create .enginex-backup.
	roMount, roDB := seedPopulatedS4Mount(t, 4, 1)
	if err := os.Chmod(roDB, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDB, 0o644) })
	roRep, err := HealMountDataOnly(roMount, 0)
	if err != nil || !roRep.ReadOnly || !roRep.Healthy {
		t.Fatalf("ro heal: %+v %v", roRep, err)
	}
	if _, err := os.Stat(filepath.Join(roMount, ".enginex-backup")); !os.IsNotExist(err) {
		t.Fatalf("read-only mount created .enginex-backup: %v", err)
	}
}

func TestS4RecoversStructurallyCorruptMainDBFromSnapshot(t *testing.T) {
	modes := []struct {
		name    string
		corrupt func(t *testing.T, dbPath string)
	}{
		{
			name: "corrupt_btree_pages",
			corrupt: func(t *testing.T, dbPath string) {
				raw, err := os.ReadFile(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				// Corrupt interior B-tree pages while keeping the SQLite header magic intact.
				for i := 4096; i < len(raw) && i < 16384; i++ {
					raw[i] = 0xAA
				}
				if err := os.WriteFile(dbPath, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "zero_byte_truncated",
			corrupt: func(t *testing.T, dbPath string) {
				if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "garbage_and_stale_journal",
			corrupt: func(t *testing.T, dbPath string) {
				if err := os.WriteFile(dbPath, []byte("not-a-sqlite-database-at-all"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dbPath+"-journal", []byte("stale-corrupt-journal"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			mount, dbPath := seedPopulatedS4Mount(t, 24, 4)
			if r, err := HealMountDataOnly(mount, 0); err != nil || !r.Healthy {
				t.Fatalf("seed heal: %+v %v", r, err)
			}
			tc.corrupt(t, dbPath)

			r, err := HealMountDataOnly(mount, 0)
			if err != nil || !r.Healthy {
				t.Fatalf("recovery failed: %+v %v", r, err)
			}
			m := reportJSONMap(t, r)
			if restored, _ := m["restored_from_backup"].(bool); !restored {
				t.Fatalf("expected restored_from_backup=true, got %+v", m)
			}
			tracks, playlists, entities := queryCounts(t, dbPath)
			if tracks != 24 || playlists != 4 || entities != 24 {
				t.Fatalf("restored counts=(%d,%d,%d), want (24,4,24)", tracks, playlists, entities)
			}
			if _, err := os.Stat(dbPath + "-journal"); !os.IsNotExist(err) {
				t.Fatalf("stale journal remained after restore: %v", err)
			}
			// Verify damaged copy was quarantined rather than deleted.
			qMatches, err := filepath.Glob(filepath.Join(mount, ".enginex-backup", "db", "corrupt-*", "m.db"))
			if err != nil || len(qMatches) != 1 {
				t.Fatalf("expected 1 quarantined m.db, got %v (%v)", qMatches, err)
			}
		})
	}

	t.Run("no_snapshot_still_errors", func(t *testing.T) {
		mount, dbPath := seedPopulatedS4Mount(t, 4, 1)
		if err := os.WriteFile(dbPath, []byte("corrupt-no-backup"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := HealMountDataOnly(mount, 0); err == nil {
			t.Fatal("expected error when corrupt m.db has no snapshot")
		}
	})

	t.Run("future_schema_never_overwritten", func(t *testing.T) {
		mount, dbPath := seedPopulatedS4Mount(t, 24, 4)
		if r, err := HealMountDataOnly(mount, 0); err != nil || !r.Healthy {
			t.Fatalf("seed heal: %+v %v", r, err)
		}
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Information SET schemaVersionMajor=99`); err != nil {
			t.Fatal(err)
		}
		db.Close()

		if _, err := HealMountDataOnly(mount, 0); err == nil || !strings.Contains(err.Error(), "unsupported library schema") {
			t.Fatalf("expected unsupported library schema error, got %v", err)
		}
		check, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var major int
		if err := check.QueryRow(`SELECT schemaVersionMajor FROM Information LIMIT 1`).Scan(&major); err != nil || major != 99 {
			t.Fatalf("future schema was overwritten: major=%d err=%v", major, err)
		}
	})
}

func TestS4RecoversLogicalPlaylistWipeWhenAudioFilesRemain(t *testing.T) {
	// Simulates forum t/71200: m.db passes integrity_check & keeps schema 3.0.2
	// and same Information.uuid, but playlists drop to 0 and most tracks vanish
	// while the audio files remain intact on the drive.
	mount, dbPath := seedPopulatedS4Mount(t, 30, 4)
	if r, err := HealMountDataOnly(mount, 0); err != nil || !r.Healthy {
		t.Fatalf("seed heal: %+v %v", r, err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM PlaylistEntity`,
		`DELETE FROM Playlist`,
		`DELETE FROM Track WHERE id > 5`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("logical collapse recovery failed: %+v %v", r, err)
	}
	m := reportJSONMap(t, r)
	if restored, _ := m["restored_from_backup"].(bool); !restored {
		t.Fatalf("expected restored_from_backup=true on logical collapse, got %+v", m)
	}
	tracks, playlists, entities := queryCounts(t, dbPath)
	if tracks != 30 || playlists != 4 || entities != 30 {
		t.Fatalf("restored counts=(%d,%d,%d), want (30,4,30)", tracks, playlists, entities)
	}
}

func TestS4DoesNotUndoIntentionalSyncManagerRemoval(t *testing.T) {
	mount, dbPath := seedPopulatedS4Mount(t, 30, 4)
	if r, err := HealMountDataOnly(mount, 0); err != nil || !r.Healthy {
		t.Fatalf("seed heal: %+v %v", r, err)
	}

	// Simulate intentional Sync Manager removal: delete playlists, tracks > 5,
	// AND remove those audio files from disk.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM PlaylistEntity`,
		`DELETE FROM Playlist`,
		`DELETE FROM Track WHERE id > 5`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for i := 6; i <= 30; i++ {
		if err := os.Remove(filepath.Join(mount, "Music", fmt.Sprintf("track-%02d.mp3", i))); err != nil {
			t.Fatal(err)
		}
	}

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("intentional removal heal failed: %+v %v", r, err)
	}
	m := reportJSONMap(t, r)
	if restored, _ := m["restored_from_backup"].(bool); restored {
		t.Fatalf("must not undo intentional Sync Manager removal: %+v", m)
	}
	tracks, playlists, entities := queryCounts(t, dbPath)
	if tracks != 5 || playlists != 0 || entities != 0 {
		t.Fatalf("intentional removal counts=(%d,%d,%d), want (5,0,0)", tracks, playlists, entities)
	}
	// Anti-clobber guard must preserve the prior populated snapshot.
	rawMeta, err := os.ReadFile(filepath.Join(mount, ".enginex-backup", "db", "m.db.meta"))
	if err != nil {
		t.Fatalf("prior snapshot meta missing: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		t.Fatal(err)
	}
	if int(meta["tracks"].(float64)) != 30 || int(meta["playlists"].(float64)) != 4 {
		t.Fatalf("anti-clobber guard failed, snapshot overwritten: %s", rawMeta)
	}
}

func TestS4QuarantinesCorruptSidecarsWithoutBlockingMainDB(t *testing.T) {
	mount, dbPath := seedPopulatedS4Mount(t, 12, 2)
	db2 := filepath.Dir(dbPath)
	if err := os.WriteFile(filepath.Join(db2, "hm.db"), []byte("corrupt-history-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db2, "sm.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db2, "stm.db"), []byte("corrupt-streaming-db"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("healthy m.db blocked by corrupt sidecars: %+v %v", r, err)
	}
	m := reportJSONMap(t, r)
	gotAny, _ := m["quarantined_dbs"].([]any)
	var got []string
	for _, x := range gotAny {
		got = append(got, fmt.Sprint(x))
	}
	want := []string{"hm.db", "sm.db", "stm.db"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("quarantined_dbs=%v, want %v", got, want)
	}
	for _, name := range want {
		if _, err := os.Stat(filepath.Join(db2, name)); !os.IsNotExist(err) {
			t.Fatalf("corrupt %s still present in Database2", name)
		}
	}
	tracks, playlists, _ := queryCounts(t, dbPath)
	if tracks != 12 || playlists != 2 {
		t.Fatalf("m.db damaged during sidecar quarantine: (%d,%d)", tracks, playlists)
	}

	// Future schema hm.db must still error and never be quarantined.
	hdb, hpath := newLibrary(t)
	mustExec(t, hdb, `UPDATE Information SET schemaVersionMajor=99`)
	hdb.Close()
	raw, err := os.ReadFile(hpath)
	if err != nil {
		t.Fatal(err)
	}
	hmDest := filepath.Join(db2, "hm.db")
	if err := os.WriteFile(hmDest, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := HealMountDataOnly(mount, 0); err == nil || !strings.Contains(err.Error(), "unsupported library schema") {
		t.Fatalf("expected unsupported schema error for future hm.db, got %v", err)
	}
	if _, err := os.Stat(hmDest); err != nil {
		t.Fatalf("future-schema hm.db was unexpectedly removed: %v", err)
	}
}

func TestS4TuneUsesFullSynchronous(t *testing.T) {
	db, _ := newLibrary(t)
	if err := Tune(db, false); err != nil {
		t.Fatal(err)
	}
	var syncMode int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&syncMode); err != nil {
		t.Fatal(err)
	}
	if syncMode != 2 {
		t.Fatalf("PRAGMA synchronous=%d, want 2 (FULL) for rollback-journal durability", syncMode)
	}
}
