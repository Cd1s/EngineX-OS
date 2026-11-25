package dbhealer

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func s19TestOverviewBlob(samplesPerBin float64, fill byte) []byte {
	raw := make([]byte, 3099)
	binary.BigEndian.PutUint64(raw[0:8], 1024)
	binary.BigEndian.PutUint64(raw[8:16], 1024)
	putBEFloat(raw[16:24], samplesPerBin)
	for i := 24; i < len(raw)-3; i++ {
		raw[i] = fill
	}
	raw[len(raw)-3] = fill
	raw[len(raw)-2] = fill
	raw[len(raw)-1] = fill
	return EncodeQCompress(raw)
}

func s19ReadMigratedJSON(t *testing.T, r Report) (int, bool) {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal Report: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal Report: %v", err)
	}
	raw, ok := m["rgb_overviews_migrated"]
	if !ok {
		return 0, false
	}
	n, ok := raw.(float64)
	if !ok {
		t.Fatalf("rgb_overviews_migrated unexpected type %T", raw)
	}
	return int(n), true
}

func s19SeedTrack(t *testing.T, db *sql.DB, id int64, spb float64, originUUID string) {
	t.Helper()
	var orig any
	if originUUID != "" {
		orig = originUUID
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,isAnalyzed,originDatabaseUuid,originTrackId) VALUES(?,?,?,?,1,?,?)`,
		id, fmt.Sprintf("../Music/track-%d.mp3", id), fmt.Sprintf("track-%d.mp3", id), 1024, orig, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=? WHERE trackId=?`,
		rawTrack(int64(spb*1024)), s19TestOverviewBlob(spb, 10), id); err != nil {
		t.Fatal(err)
	}
}

func s19WriteRGB(t *testing.T, overviewDir, libUUID string, id int64, data []byte) string {
	t.Helper()
	dir := filepath.Join(overviewDir, libUUID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, fmt.Sprintf("%d.rgb", id))
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func s19CreateHistoryDB(t *testing.T, db2Dir, histUUID string) {
	t.Helper()
	hpath := filepath.Join(db2Dir, "hm.db")
	hdb, err := sql.Open("sqlite", hpath)
	if err != nil {
		t.Fatal(err)
	}
	defer hdb.Close()
	schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", "")
	if _, err := hdb.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := hdb.Exec(`CREATE TABLE Historylist(id INTEGER PRIMARY KEY,startTime INTEGER);
		CREATE TABLE HistorylistEntity(id INTEGER PRIMARY KEY,listId INTEGER,trackId INTEGER);
		INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(?,3,0,2)`, histUUID); err != nil {
		t.Fatal(err)
	}
}

func TestS19WholeDirRenameHealMountAndDataOnly(t *testing.T) {
	const oldUUID = "11111111-1111-4111-8111-111111111111"
	const newUUID = "22222222-2222-4222-8222-222222222222"

	for _, dataOnly := range []bool{true, false} {
		name := "HealMount"
		if dataOnly {
			name = "HealMountDataOnly"
		}
		t.Run(name, func(t *testing.T) {
			_, root, db := testlibrary.Create(t)
			if _, err := db.Exec(`UPDATE Information SET uuid=?`, newUUID); err != nil {
				t.Fatal(err)
			}
			for id := int64(1); id <= 4; id++ {
				s19SeedTrack(t, db, id, float64(5000+id*500), "")
			}
			db.Close()

			db2Dir := filepath.Join(root, "Engine Library", "Database2")
			s19CreateHistoryDB(t, db2Dir, "33333333-3333-4333-8333-333333333333")
			overviewDir := filepath.Join(db2Dir, "OverviewData")
			for id := int64(1); id <= 4; id++ {
				s19WriteRGB(t, overviewDir, oldUUID, id, s19TestOverviewBlob(float64(5000+id*500), byte(20+id)))
			}
			// Empty curUUID directory created by Engine after resetUuid.
			if err := os.MkdirAll(filepath.Join(overviewDir, newUUID), 0o755); err != nil {
				t.Fatal(err)
			}

			var (
				r   Report
				err error
			)
			if dataOnly {
				r, err = HealMountDataOnly(root, 0)
			} else {
				r, err = HealMount(root, 0)
			}
			if err != nil || !r.Healthy {
				t.Fatalf("%s failed: %+v err=%v", name, r, err)
			}
			got, present := s19ReadMigratedJSON(t, r)
			if !present || got != 4 {
				t.Fatalf("rgb_overviews_migrated=(%d,%v), want (4,true); warnings=%v", got, present, r.Warnings)
			}
			if _, err := os.Stat(filepath.Join(overviewDir, oldUUID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old UUID dir still exists after whole-dir rename: err=%v", err)
			}
			for id := int64(1); id <= 4; id++ {
				if _, err := os.Stat(filepath.Join(overviewDir, newUUID, fmt.Sprintf("%d.rgb", id))); err != nil {
					t.Fatalf("missing migrated %d.rgb: %v", id, err)
				}
			}

			// Second pass must fast-skip cleanly and omit zero count in JSON.
			if dataOnly {
				r, err = HealMountDataOnly(root, 0)
			} else {
				r, err = HealMount(root, 0)
			}
			if err != nil || !r.Healthy {
				t.Fatalf("second pass failed: %+v err=%v", r, err)
			}
			if got2, present2 := s19ReadMigratedJSON(t, r); present2 || got2 != 0 {
				t.Fatalf("second pass expected omitted zero rgb_overviews_migrated, got (%d,%v)", got2, present2)
			}
		})
	}
}

func TestS19PartialMergePreservesNewerFilesAndChecksContext(t *testing.T) {
	const oldUUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const newUUID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	_, root, db := testlibrary.Create(t)
	if _, err := db.Exec(`UPDATE Information SET uuid=?`, newUUID); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 70; id++ {
		s19SeedTrack(t, db, id, float64(4000+id*100), "")
	}
	db.Close()

	overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
	newerPayload := s19TestOverviewBlob(4100, 99)
	s19WriteRGB(t, overviewDir, newUUID, 1, newerPayload)
	for id := int64(1); id <= 70; id++ {
		s19WriteRGB(t, overviewDir, oldUUID, id, s19TestOverviewBlob(float64(4000+id*100), 33))
	}

	// Cancelled context stops cleanly before migrating.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	rCancelled, err := HealMountDataOnlyContext(cancelledCtx, root, 0)
	if err != nil || !rCancelled.Healthy {
		t.Fatalf("cancelled pass failed: %+v err=%v", rCancelled, err)
	}
	if _, err := os.Stat(filepath.Join(overviewDir, newUUID, "2.rgb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled context unexpectedly migrated 2.rgb")
	}

	r, err := HealMountDataOnly(root, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("HealMountDataOnly failed: %+v err=%v", r, err)
	}
	got, _ := s19ReadMigratedJSON(t, r)
	if got != 69 {
		t.Fatalf("migrated=%d, want 69 (70 minus existing 1.rgb)", got)
	}
	actual1, err := os.ReadFile(filepath.Join(overviewDir, newUUID, "1.rgb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(actual1) != string(newerPayload) {
		t.Fatalf("existing newer 1.rgb in curUUID was overwritten")
	}
	for id := int64(2); id <= 70; id++ {
		if _, err := os.Stat(filepath.Join(overviewDir, newUUID, fmt.Sprintf("%d.rgb", id))); err != nil {
			t.Fatalf("missing merged %d.rgb: %v", id, err)
		}
	}

	// Mid-merge cancellation checks ctx.Err() every 64 files and resumes cleanly.
	const oldUUID2 = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaab"
	const newUUID2 = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbc"
	_, root2, db2 := testlibrary.Create(t)
	if _, err := db2.Exec(`UPDATE Information SET uuid=?`, newUUID2); err != nil {
		t.Fatal(err)
	}
	for id := int64(1); id <= 130; id++ {
		s19SeedTrack(t, db2, id, float64(4000+id*100), "")
	}
	db2.Close()
	ov2 := filepath.Join(root2, "Engine Library", "Database2", "OverviewData")
	s19WriteRGB(t, ov2, newUUID2, 1, newerPayload)
	for id := int64(1); id <= 130; id++ {
		s19WriteRGB(t, ov2, oldUUID2, id, s19TestOverviewBlob(float64(4000+id*100), 33))
	}
	midCtx, midCancel := context.WithCancel(context.Background())
	defer midCancel()
	batches := 0
	overviewMigrateBatch = func() {
		batches++
		if batches == 1 {
			midCancel()
		}
	}
	defer func() { overviewMigrateBatch = nil }()
	rMid, err := HealMountDataOnlyContext(midCtx, root2, 0)
	if err != nil || !rMid.Healthy || rMid.RGBOverviewsMigrated != 63 {
		t.Fatalf("mid-cancel got %+v err=%v, want RGBOverviewsMigrated=63", rMid, err)
	}
	overviewMigrateBatch = nil
	rFinish, err := HealMountDataOnly(root2, 0)
	if err != nil || !rFinish.Healthy || rFinish.RGBOverviewsMigrated != 66 {
		t.Fatalf("resume got %+v err=%v, want RGBOverviewsMigrated=66", rFinish, err)
	}
}

func TestS19RejectsMismatchedSamplesPerBinAndCorruptAndSymlinks(t *testing.T) {
	const oldUUID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	const newUUID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"

	t.Run("mismatch_samples_per_bin", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, newUUID); err != nil {
			t.Fatal(err)
		}
		// Own SPB=10000 -> tolerance max(16, 20)=20; candidate SPB=10500 (>20 diff) must be rejected.
		s19SeedTrack(t, db, 1, 10000, "")
		s19SeedTrack(t, db, 2, 12000, "")
		db.Close()

		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		s19WriteRGB(t, overviewDir, oldUUID, 1, s19TestOverviewBlob(10500, 44))
		s19WriteRGB(t, overviewDir, oldUUID, 2, s19TestOverviewBlob(12000, 44))

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 0 {
			t.Fatalf("mismatched samplesPerBin migrated=%d, want 0", got)
		}
		if _, err := os.Stat(filepath.Join(overviewDir, oldUUID, "1.rgb")); err != nil {
			t.Fatalf("rejected dir should stay untouched: %v", err)
		}
	})

	t.Run("within_tolerance_accepted", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, newUUID); err != nil {
			t.Fatal(err)
		}
		// Own SPB=1000 -> tolerance max(16, 2)=16; SPB=1010 (diff=10 <= 16) is accepted.
		s19SeedTrack(t, db, 1, 1000, "")
		db.Close()

		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		s19WriteRGB(t, overviewDir, oldUUID, 1, s19TestOverviewBlob(1010, 44))

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 1 {
			t.Fatalf("within-tolerance SPB migrated=%d, want 1", got)
		}
	})

	t.Run("corrupt_qcompress_rejected", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, newUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, "")
		db.Close()

		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		s19WriteRGB(t, overviewDir, oldUUID, 1, []byte("not-valid-qcompress-payload"))

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 0 {
			t.Fatalf("corrupt qCompress migrated=%d, want 0", got)
		}
	})

	t.Run("symlink_dir_ignored", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, newUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, "")
		db.Close()

		outside := filepath.Join(t.TempDir(), "outside")
		s19WriteRGB(t, outside, "target", 1, s19TestOverviewBlob(5000, 44))
		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		if err := os.MkdirAll(overviewDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(overviewDir, oldUUID)); err != nil {
			t.Fatal(err)
		}

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 0 {
			t.Fatalf("symlink dir migrated=%d, want 0", got)
		}
	})
}

func TestS19ExcludesSiblingDBAndHealthyNoOpAndTieWarn(t *testing.T) {
	const curUUID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	const histUUID = "ffffffff-ffff-4fff-8fff-ffffffffffff"

	t.Run("sibling_hm_db_excluded", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, curUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, "")
		db.Close()

		db2Dir := filepath.Join(root, "Engine Library", "Database2")
		s19CreateHistoryDB(t, db2Dir, histUUID)
		overviewDir := filepath.Join(db2Dir, "OverviewData")
		s19WriteRGB(t, overviewDir, histUUID, 1, s19TestOverviewBlob(5000, 55))

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 0 {
			t.Fatalf("sibling hm.db UUID dir was migrated (%d), want 0", got)
		}
		if _, err := os.Stat(filepath.Join(overviewDir, histUUID, "1.rgb")); err != nil {
			t.Fatalf("sibling hm.db OverviewData dir disappeared: %v", err)
		}
	})

	t.Run("healthy_library_noop", func(t *testing.T) {
		const extraUUID = "99999999-9999-4999-8999-999999999999"
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, curUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, "")
		db.Close()

		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		curPayload := s19TestOverviewBlob(5000, 77)
		s19WriteRGB(t, overviewDir, curUUID, 1, curPayload)
		s19WriteRGB(t, overviewDir, extraUUID, 1, s19TestOverviewBlob(5000, 11))

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 0 {
			t.Fatalf("healthy library migrated=%d, want 0", got)
		}
		if _, err := os.Stat(filepath.Join(overviewDir, extraUUID, "1.rgb")); err != nil {
			t.Fatalf("extra dir modified on healthy library: %v", err)
		}
	})

	t.Run("ambiguous_tie_warns_and_skips", func(t *testing.T) {
		const candA = "aaaa0000-0000-4000-8000-000000000001"
		const candB = "bbbb0000-0000-4000-8000-000000000002"
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, curUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, "")
		s19SeedTrack(t, db, 2, 6000, "")
		db.Close()

		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		for id := int64(1); id <= 2; id++ {
			s19WriteRGB(t, overviewDir, candA, id, s19TestOverviewBlob(float64(4000+id*1000), 11))
			s19WriteRGB(t, overviewDir, candB, id, s19TestOverviewBlob(float64(4000+id*1000), 22))
		}

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 0 {
			t.Fatalf("tied candidates migrated=%d, want 0", got)
		}
		if len(r.Warnings) == 0 {
			t.Fatalf("expected warning on tied candidate directories")
		}
	})
}

func TestS19ReadOnlyAndInUseUntouched(t *testing.T) {
	const oldUUID = "12121212-1212-4121-8121-121212121212"
	const newUUID = "34343434-3434-4343-8343-343434343434"

	t.Run("read_only_mount", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, newUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, "")
		db.Close()

		db2Dir := filepath.Join(root, "Engine Library", "Database2")
		overviewDir := filepath.Join(db2Dir, "OverviewData")
		s19WriteRGB(t, overviewDir, oldUUID, 1, s19TestOverviewBlob(5000, 33))
		if err := os.Chmod(filepath.Join(db2Dir, "m.db"), 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(db2Dir, "m.db"), 0o644) })

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.ReadOnly {
			t.Fatalf("expected read-only report, got %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 0 {
			t.Fatalf("read-only mount migrated=%d, want 0", got)
		}
		if _, err := os.Stat(filepath.Join(overviewDir, oldUUID, "1.rgb")); err != nil {
			t.Fatalf("read-only mount modified OverviewData: %v", err)
		}
	})

	t.Run("library_in_use", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, newUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, "")
		db.Close()

		dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		s19WriteRGB(t, overviewDir, oldUUID, 1, s19TestOverviewBlob(5000, 33))

		f, err := os.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sleep", "60")
		cmd.ExtraFiles = []*os.File{f}
		if err := cmd.Start(); err != nil {
			f.Close()
			t.Fatal(err)
		}
		f.Close()
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

		if _, err := HealMountDataOnly(root, 0); !errors.Is(err, errLibraryInUse) {
			t.Fatalf("expected errLibraryInUse, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(overviewDir, oldUUID, "1.rgb")); err != nil {
			t.Fatalf("in-use library modified OverviewData: %v", err)
		}
	})
}

func TestS19DesktopOriginDatabaseUuidRecovery(t *testing.T) {
	const desktopUUID = "56565656-5656-4565-8565-565656565656"
	const decoyUUID = "78787878-7878-4787-8787-787878787878"
	const curUUID = "90909090-9090-4909-8909-909090909090"

	t.Run("track_originDatabaseUuid_breaks_tie", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, curUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, desktopUUID)
		s19SeedTrack(t, db, 2, 6000, desktopUUID)
		db.Close()

		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		for id := int64(1); id <= 2; id++ {
			s19WriteRGB(t, overviewDir, desktopUUID, id, s19TestOverviewBlob(float64(4000+id*1000), 66))
			s19WriteRGB(t, overviewDir, decoyUUID, id, s19TestOverviewBlob(float64(4000+id*1000), 11))
		}

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 2 {
			t.Fatalf("migrated=%d, want 2 (warnings=%v)", got, r.Warnings)
		}
		got1, err := os.ReadFile(filepath.Join(overviewDir, curUUID, "1.rgb"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got1) != string(s19TestOverviewBlob(5000, 66)) {
			t.Fatalf("recovered wrong candidate directory")
		}
		if _, err := os.Stat(filepath.Join(overviewDir, decoyUUID, "1.rgb")); err != nil {
			t.Fatalf("decoy dir should remain untouched: %v", err)
		}
	})

	t.Run("playlist_entity_databaseUuid_breaks_tie", func(t *testing.T) {
		_, root, db := testlibrary.Create(t)
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, curUUID); err != nil {
			t.Fatal(err)
		}
		s19SeedTrack(t, db, 1, 5000, "")
		if _, err := db.Exec(`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId) VALUES(1,'Set',0,1,0)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(1,1,1,?,0,0)`, desktopUUID); err != nil {
			t.Fatal(err)
		}
		db.Close()

		overviewDir := filepath.Join(root, "Engine Library", "Database2", "OverviewData")
		s19WriteRGB(t, overviewDir, desktopUUID, 1, s19TestOverviewBlob(5000, 88))
		s19WriteRGB(t, overviewDir, decoyUUID, 1, s19TestOverviewBlob(5000, 12))

		r, err := HealMountDataOnly(root, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("heal failed: %+v err=%v", r, err)
		}
		if got, _ := s19ReadMigratedJSON(t, r); got != 1 {
			t.Fatalf("migrated=%d, want 1 (warnings=%v)", got, r.Warnings)
		}
		got1, err := os.ReadFile(filepath.Join(overviewDir, curUUID, "1.rgb"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got1) != string(s19TestOverviewBlob(5000, 88)) {
			t.Fatalf("recovered wrong candidate directory")
		}
	})
}
