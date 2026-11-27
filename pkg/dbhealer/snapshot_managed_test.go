package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	_ "modernc.org/sqlite"
)

// Restored #246 (fills the v2 snapshot gap in pkg/dbhealer/normalization_test.go):
// HealMountDataOnly reads bridge-managed track IDs from v2 .rb-bridge-snapshot (with NO legacy
// .rb-bridge-state.db present) and leaves bridge-managed NFD paths alone while normalizing
// unmanaged tracks.
// Guards pkg/dbhealer/db.go:581 (`rbstate.Load` in `bridgeManagedTracks`).
func TestRestored_Issue246_DBHealerRespectsV2SnapshotManagedTracks(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	writeFile := func(rel string) {
		p := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 32), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Track 10: unmanaged NFD path whose NFC file exists -> MUST be normalized.
	// Track 20: bridge-managed via v2 .rb-bridge-snapshot (NO .rb-bridge-state.db) -> MUST NOT be normalized.
	unmanagedNFD := "../Music/Cafe\u0301.mp3"
	unmanagedNFC := "../Music/Café.mp3"
	managedNFD := "../Music/Bridge\u0301.mp3"
	managedNFC := "../Music/Bridgé.mp3"
	writeFile(unmanagedNFC)
	writeFile(managedNFC)
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(10,?,?,32),(20,?,?,32)`,
		unmanagedNFD, filepath.Base(unmanagedNFD), managedNFD, filepath.Base(managedNFD)); err != nil {
		t.Fatal(err)
	}
	var libUUID string
	if err := db.QueryRow(`SELECT uuid FROM Information`).Scan(&libUUID); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snap := rbstate.New(libUUID)
	snap.Tracks[rbstate.FoldPath("Music/Bridgé.mp3")] = &rbstate.Track{
		Key:      rbstate.FoldPath("Music/Bridgé.mp3"),
		EngineID: 20,
		Rel:      "Music/Bridge\u0301.mp3",
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}

	got, err := HealMountDataOnly(root, 0)
	if err != nil {
		t.Fatalf("HealMountDataOnly: %v", err)
	}
	if got.PathsNormalized != 1 {
		t.Fatalf("PathsNormalized=%d, want 1 (only unmanaged track 10): %+v", got.PathsNormalized, got)
	}
	check, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var p10, p20 string
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=10`).Scan(&p10); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=20`).Scan(&p20); err != nil {
		t.Fatal(err)
	}
	if p10 != unmanagedNFC {
		t.Fatalf("unmanaged track 10 path=%q, want %q", p10, unmanagedNFC)
	}
	if p20 != managedNFD {
		t.Fatalf("v2-snapshot-managed track 20 was unexpectedly rewritten to %q (want %q)", p20, managedNFD)
	}
}

func TestDamagedV2SnapshotProtectsBridgeManagedDropboxTrack(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	full := filepath.Join(root, "Music", "BridgeTrack.mp3")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("audio-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,isAnalyzed) VALUES(20,'../Music/BridgeTrack.mp3','BridgeTrack.mp3',10,'Dropbox','streaming://Dropbox/Track/20',5,1)`,
	); err != nil {
		t.Fatal(err)
	}
	var libUUID string
	if err := db.QueryRow(`SELECT uuid FROM Information`).Scan(&libUUID); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snap := rbstate.New(libUUID)
	snap.Tracks[rbstate.FoldPath("Music/BridgeTrack.mp3")] = &rbstate.Track{
		Key:      rbstate.FoldPath("Music/BridgeTrack.mp3"),
		EngineID: 20,
		Rel:      "Music/BridgeTrack.mp3",
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	snapPath := filepath.Join(dbDir, rbstate.SnapshotName)
	raw, err := os.ReadFile(snapPath)
	if err != nil || len(raw) < 2 {
		t.Fatalf("ReadFile snapshot: len=%d err=%v", len(raw), err)
	}
	if err := os.WriteFile(snapPath, raw[:len(raw)-1], 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.DropboxLocalized != 0 {
		t.Fatalf("damaged v2 snapshot localized dropbox track: got=%+v err=%v", got, err)
	}
	check, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var src, uri string
	if err := check.QueryRow(`SELECT COALESCE(streamingSource,''), COALESCE(uri,'') FROM Track WHERE id=20`).Scan(&src, &uri); err != nil {
		t.Fatal(err)
	}
	if src != "Dropbox" || uri != "streaming://Dropbox/Track/20" {
		t.Fatalf("damaged snapshot rewrote Track 20: src=%q uri=%q", src, uri)
	}
}

func TestBridgeManagedTracksContextHonorsDeadlineOnLargeSnapshot(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	var libUUID string
	if err := db.QueryRow(`SELECT uuid FROM Information`).Scan(&libUUID); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snap := rbstate.New(libUUID)
	for i := 1; i <= 60000; i++ {
		rel := fmt.Sprintf("Music/Track-%05d.mp3", i)
		key := rbstate.FoldPath(rel)
		snap.Tracks[key] = &rbstate.Track{
			Key:      key,
			EngineID: int64(i),
			Rel:      rel,
			Native:   true,
		}
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ids, err := bridgeManagedTracksContext(canceled, root)
	if !errors.Is(err, context.Canceled) || len(ids) != 0 {
		t.Fatalf("canceled bridgeManagedTracksContext: len=%d err=%v, want context.Canceled", len(ids), err)
	}

	tightCtx, cancelTight := context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancelTight()
	deadline, _ := tightCtx.Deadline()
	ids, err = bridgeManagedTracksContext(tightCtx, root)
	overrun := time.Since(deadline)
	if err == nil {
		t.Fatalf("60k snapshot exceeded 2ms budget without cancellation error: len=%d overrun=%v", len(ids), overrun)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error on tight deadline: %v", err)
	}
	if overrun >= 50*time.Millisecond {
		t.Fatalf("large snapshot exceeded tight deadline by %v (>= 50ms)", overrun)
	}
}
