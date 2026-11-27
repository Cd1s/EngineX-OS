package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func runGuardianDataOnly(t *testing.T, mount string) guardianResult {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--heal-mount", mount, "--data-only", "--ui-dir", t.TempDir()}, &out, &errOut); code != 0 {
		t.Fatalf("guardian exited %d: %s", code, &errOut)
	}
	var rows []guardianResult
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("invalid guardian report: %v (raw=%s)", err, out.String())
	}
	return rows[0]
}

func TestS4GuardianMP3GridAfterRestore(t *testing.T) {
	mount := t.TempDir()
	seedPreR4MP3Mount(t, mount)
	first := runGuardianDataOnly(t, mount)
	if first.MP3Grids == nil || first.MP3Grids.Corrected != 1 {
		t.Fatalf("fixture did not repair exactly one grid: %+v", first.MP3Grids)
	}
	want := readFirstBeatOffset(t, mount)
	if err := os.Truncate(filepath.Join(mount, "Engine Library", "Database2", "m.db"), 0); err != nil {
		t.Fatal(err)
	}
	second := runGuardianDataOnly(t, mount)
	if !second.RestoredFromBackup || second.MP3Grids == nil {
		t.Fatalf("database or grid check missing from second mount: %+v", second)
	}
	if got := readFirstBeatOffset(t, mount); got != want {
		t.Fatalf("restored grid offset=%g, want %g, grid pass bypassed=%v", got, want, second.MP3Grids.Bypassed)
	}

	// Restoring an older snapshot that predates .enginex-mp3-grid-stamp must
	// invalidate the obsolete stamp so FixMP3Grids repairs the restored DB
	// and publishes the repaired state without double-shifting on later mounts.
	t.Run("pre_stamp_snapshot_invalidates_obsolete_stamp", func(t *testing.T) {
		m2 := t.TempDir()
		seedPreR4MP3Mount(t, m2)
		// Direct dbhealer heal snapshots the pre-grid DB (beat0=5000, no stamp).
		if r, err := dbhealer.HealMountDataOnly(m2, 0); err != nil || !r.Healthy {
			t.Fatalf("pre-stamp heal: %+v %v", r, err)
		}
		// Run MP3 grid repair separately so the completion stamp exists on disk
		// while the saved snapshot predates it.
		if mr, err := enginedb.FixMP3Grids(context.Background(), m2, enginedb.MP3GridOptions{}); err != nil || mr.Corrected != 1 {
			t.Fatalf("standalone mp3 fix: %+v %v", mr, err)
		}
		wantOffset := readFirstBeatOffset(t, m2)
		if err := os.Truncate(filepath.Join(m2, "Engine Library", "Database2", "m.db"), 0); err != nil {
			t.Fatal(err)
		}
		rec := runGuardianDataOnly(t, m2)
		if !rec.RestoredFromBackup || rec.MP3Grids == nil || rec.MP3Grids.Corrected != 1 {
			t.Fatalf("pre-stamp restore did not re-run MP3 grid repair: %+v", rec)
		}
		if got := readFirstBeatOffset(t, m2); got != wantOffset {
			t.Fatalf("post-restore offset=%g, want %g", got, wantOffset)
		}
		// A second truncation must restore the newly published post-grid snapshot
		// and preserve the stamp so the beat grid is never double-shifted.
		if err := os.Truncate(filepath.Join(m2, "Engine Library", "Database2", "m.db"), 0); err != nil {
			t.Fatal(err)
		}
		rec2 := runGuardianDataOnly(t, m2)
		if !rec2.RestoredFromBackup || rec2.MP3Grids == nil || !rec2.MP3Grids.Bypassed || rec2.MP3Grids.Corrected != 0 {
			t.Fatalf("second restore must keep stamp and avoid double-shift: %+v", rec2)
		}
		if got := readFirstBeatOffset(t, m2); got != wantOffset {
			t.Fatalf("second restore shifted offset to %g, want %g", got, wantOffset)
		}
	})
}

func TestS4GuardianQueuedEditSurvivesLogicalRestore(t *testing.T) {
	mount := t.TempDir()
	seedPreR4MP3Mount(t, mount)
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	connect := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := connect()
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for id := 2; id <= 30; id++ {
		name := fmt.Sprintf("review-%d.wav", id)
		if err := os.WriteFile(filepath.Join(mount, "Music", name), []byte("review"), 0o600); err != nil {
			t.Fatal(err)
		}
		exec("INSERT INTO Track(id,path,filename,isAnalyzed) VALUES(?,?,?,0)", id, "../Music/"+name, name)
	}
	for id := 1; id <= 3; id++ {
		exec("INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(?,?,0,1,?,1,1)", id, fmt.Sprint(id), (id+1)%4)
	}
	for id := 1; id <= 30; id++ {
		next := id + 1
		if id%10 == 0 {
			next = 0
		}
		exec("INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,?,?,'cli-mp3',?,0)", id, 1+(id-1)/10, id, next)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runGuardianDataOnly(t, mount)
	db = connect()
	exec("DELETE FROM PlaylistEntity")
	exec("DELETE FROM Playlist")
	exec("DELETE FROM Track WHERE id>5")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	const edited = "queued review edit"
	if err := enginedb.QueuePending(mount, func(p *enginedb.Pending) {
		p.TrackMetadata = append(p.TrackMetadata, enginedb.PendingTrackMetadata{TrackID: 2, Comment: edited})
	}); err != nil {
		t.Fatal(err)
	}
	if !runGuardianDataOnly(t, mount).RestoredFromBackup {
		t.Fatal("fixture did not trigger logical recovery")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), enginedb.PendingFile)); !os.IsNotExist(err) {
		t.Fatalf("queue state unexpected: %v", err)
	}
	db = connect()
	var comment string
	if err := db.QueryRow("SELECT COALESCE(comment,'') FROM Track WHERE id=2").Scan(&comment); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if comment != edited {
		t.Fatalf("queue consumed, surviving track comment=%q, want %q", comment, edited)
	}

	// Verify the replayed edit was also captured in the post-replay snapshot.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if !runGuardianDataOnly(t, mount).RestoredFromBackup {
		t.Fatal("second truncation did not restore from backup")
	}
	db = connect()
	defer db.Close()
	if err := db.QueryRow("SELECT COALESCE(comment,'') FROM Track WHERE id=2").Scan(&comment); err != nil {
		t.Fatal(err)
	}
	if comment != edited {
		t.Fatalf("replayed queued edit lost after later structural restore: got %q, want %q", comment, edited)
	}
}
