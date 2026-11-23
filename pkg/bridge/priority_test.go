package bridge_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestPriorityQueueCoalescePermissionsAndActivity(t *testing.T) {
	priDir := t.TempDir()
	restore := bridge.SetPriorityDirForTest(priDir)
	defer restore()

	mount := t.TempDir()
	q := bridge.NewPriorityQueue()

	seq1, err := q.Enqueue(mount, "../Contents/TrackA.mp3", "deck", 1)
	if err != nil || seq1 == 0 {
		t.Fatalf("Enqueue TrackA: seq=%d err=%v", seq1, err)
	}
	// Identical head edge coalesces without incrementing seq.
	seqDup, err := q.Enqueue(mount, "Contents/TrackA.mp3", "deck", 1)
	if err != nil || seqDup != seq1 {
		t.Fatalf("duplicate head edge: got seq=%d want %d err=%v", seqDup, seq1, err)
	}
	seq2, err := q.Enqueue(mount, "../Contents/TrackB.mp3", "preview", 0)
	if err != nil || seq2 <= seq1 {
		t.Fatalf("Enqueue TrackB: seq=%d err=%v", seq2, err)
	}
	// Verify file mode is 0600 inside priDir, never touching /run.
	entries, err := os.ReadDir(priDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("priority dir entries=%v err=%v", entries, err)
	}
	if info, err := entries[0].Info(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("priority record mode=%v err=%v, want 0600", info.Mode().Perm(), err)
	}

	pending, maxSeq := bridge.PendingPriority(mount)
	if maxSeq != seq2 || len(pending) != 2 || pending[0].Path != "../Contents/TrackB.mp3" || pending[1].Path != "../Contents/TrackA.mp3" {
		t.Fatalf("pending=%+v maxSeq=%d", pending, maxSeq)
	}
	if err := bridge.AckPriority(mount, pending[:1]); err != nil {
		t.Fatal(err)
	}
	pending, _ = bridge.PendingPriority(mount)
	if len(pending) != 1 || pending[0].Path != "../Contents/TrackA.mp3" {
		t.Fatalf("after partial ack: pending=%+v", pending)
	}

	// Deck activity: active edge takes effect immediately, idle requires 2 spaced samples.
	t0 := time.Now()
	if !q.UpdateDeckActivity(true, t0) || !bridge.IsDeckActive() {
		t.Fatal("expected active deck immediately")
	}
	if !q.UpdateDeckActivity(false, t0.Add(500*time.Millisecond)) || !bridge.IsDeckActive() {
		t.Fatal("1 idle sample must keep deck active")
	}
	if !q.UpdateDeckActivity(false, t0.Add(600*time.Millisecond)) || !bridge.IsDeckActive() {
		t.Fatal("closely spaced idle sample must not count as 2nd sample")
	}
	if q.UpdateDeckActivity(false, t0.Add(1000*time.Millisecond)) || bridge.IsDeckActive() {
		t.Fatal("2nd spaced idle sample should return deck to idle")
	}

	// Eject removes both record and ack.
	q.RemoveVolume(mount)
	if p, _ := bridge.PendingPriority(mount); len(p) != 0 {
		t.Fatalf("after RemoveVolume: pending=%+v", p)
	}
}

func TestPriorityImportsTargetTrackFirstInBackground(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	priDir := t.TempDir()
	restore := bridge.SetPriorityDirForTest(priDir)
	defer restore()

	root := t.TempDir()
	testfixture.LargeExport(t, root, 12)
	// Fast first import at mount: leaves all 12 tracks as fast: in stage/live.
	// Use a tiny deadline so tracks stay fast/deferred and go to background.
	res, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{
		Fast:     true,
		FastCues: true,
	}, false)
	if err != nil || res.Added != 12 {
		t.Fatalf("fast mount: res=%+v err=%v", res, err)
	}

	// Enqueue the last track (#12) as highest priority.
	targetRel := testfixture.LargeTrackPath(12)
	q := bridge.NewPriorityQueue()
	seq, err := q.Enqueue(root, "../"+targetRel, "deck", 1)
	if err != nil {
		t.Fatal(err)
	}

	// Run a single SyncResolved with PriorityPaths + MaxTracks=1 to verify
	// track #12 is imported first with full PerformanceData and non-fast state,
	// leaving the other 11 tracks deferred.
	lib, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	sres, err := enginedb.Sync(context.Background(), root, lib, enginedb.Options{
		Resume:        true,
		PriorityPaths: []string{"../" + targetRel},
		MaxTracks:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sres.Updated != 1 || sres.Deferred != 11 {
		t.Fatalf("priority sync: updated=%d deferred=%d, want 1 and 11", sres.Updated, sres.Deferred)
	}

	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ATTACH DATABASE ? AS bridge`, filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db")); err != nil {
		t.Fatal(err)
	}
	var fp string
	var analyzed int
	var beatLen, cueLen, ovLen int
	err = db.QueryRow(`SELECT s.fingerprint, t.isAnalyzed, length(p.beatData), length(p.quickCues), length(p.overviewWaveFormData)
		FROM bridge.BridgeState s
		JOIN Track t ON t.id = s.trackId
		JOIN PerformanceData p ON p.trackId = t.id
		WHERE s.path = ?`, "../"+targetRel).Scan(&fp, &analyzed, &beatLen, &cueLen, &ovLen)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(fp, "fast:") || analyzed != 1 || beatLen == 0 || cueLen == 0 || ovLen == 0 {
		t.Fatalf("priority track not fully imported: fp=%q analyzed=%d beat=%d cue=%d ov=%d", fp, analyzed, beatLen, cueLen, ovLen)
	}
	// Track #1 must still be fast: (untouched by the 1-track priority burst).
	var firstFP string
	if err := db.QueryRow(`SELECT fingerprint FROM bridge.BridgeState WHERE path = ?`, "../"+testfixture.LargeTrackPath(1)).Scan(&firstFP); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(firstFP, "fast:") {
		t.Fatalf("non-priority track #1 was imported out of turn: fp=%q", firstFP)
	}
	// Verify ack tracking through PendingPriority.
	if err := bridge.AckPriority(root, []bridge.PriorityItem{{Path: "../" + targetRel, Seq: seq}}); err != nil {
		t.Fatal(err)
	}
	if pending, _ := bridge.PendingPriority(root); len(pending) != 0 {
		t.Fatalf("priority item remained pending after ack: %+v", pending)
	}
}

func TestImportInBackgroundConsumesAndAcksPriority(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	priDir := t.TempDir()
	restore := bridge.SetPriorityDirForTest(priDir)
	defer restore()

	root := t.TempDir()
	testfixture.LargeExport(t, root, 6)
	defer bridge.SetLargeExportForTest()()
	// First mount starts background fast build; second mount publishes fast rows and starts file-phase update stage.
	if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	if _, err := bridge.ImportInBackground(context.Background(), root, nil); err != nil {
		t.Fatalf("fast build: %v", err)
	}
	if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("publish fast mount: %+v %v", r, err)
	}

	targetRel := testfixture.LargeTrackPath(6)
	q := bridge.NewPriorityQueue()
	seq, err := q.Enqueue(root, "../"+targetRel, "preview", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Also enqueue a missing path to verify bounded retry does not stall background completion.
	if _, err := q.Enqueue(root, "../Contents/does-not-exist.mp3", "focus", 0); err != nil {
		t.Fatal(err)
	}

	res, err := bridge.ImportInBackground(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("ImportInBackground: %v", err)
	}
	if res.Deferred != 0 {
		t.Fatalf("ImportInBackground left %d tracks deferred", res.Deferred)
	}
	// Before publishing the stage at remount, the prioritized track's .rgb overview
	// must already be mirrored into the live Database2/OverviewData/<uuid>/ directory.
	liveDir := filepath.Join(root, "Engine Library", "Database2")
	ldb, err := sql.Open("sqlite", filepath.Join(liveDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	var liveUUID string
	var liveTrackID int64
	if err := ldb.QueryRow(`SELECT uuid FROM Information WHERE id = 1`).Scan(&liveUUID); err != nil {
		ldb.Close()
		t.Fatal(err)
	}
	if err := ldb.QueryRow(`SELECT id FROM Track WHERE path = ?`, "../"+targetRel).Scan(&liveTrackID); err != nil {
		ldb.Close()
		t.Fatal(err)
	}
	ldb.Close()
	mirroredRGB := filepath.Join(liveDir, "OverviewData", liveUUID, strconv.FormatInt(liveTrackID, 10)+".rgb")
	if fi, err := os.Stat(mirroredRGB); err != nil || fi.Size() <= 12 {
		t.Fatalf("expected prioritized live .rgb before stage publish at %s: fi=%v err=%v", mirroredRGB, fi, err)
	}
	// Publish the finished stage and confirm target track is analyzed.
	if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil {
		t.Fatalf("publish mount: %v", err)
	}
	ack := bridge.ReadPriorityAck(root)
	if ack.AckSeq < seq {
		t.Fatalf("priority ack seq=%d, want >= %d", ack.AckSeq, seq)
	}
}
