package bridge

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestBackgroundPassOptionsIdleVsPlaying(t *testing.T) {
	t.Setenv("ENGINEX_RB_PRIORITY_DIR", t.TempDir())
	mount := t.TempDir()
	pri := newPriorityTracker()
	ctx := context.Background()

	// Idle deck: full-speed 256-track batch, path-sorted, zero pacing, yields on active transition.
	idleOpts, wasActive := backgroundPassOptions(ctx, mount, pri, importWindow, enginedb.Options{Resume: true})
	if wasActive {
		t.Fatal("expected idle deck")
	}
	if !idleOpts.SortByPath || idleOpts.MaxTracks != idleBatchTracks || idleOpts.ReadPause != nil || idleOpts.ReadChunk != 0 {
		t.Fatalf("unexpected idle options: SortByPath=%v MaxTracks=%d ReadPause=%v ReadChunk=%d",
			idleOpts.SortByPath, idleOpts.MaxTracks, idleOpts.ReadPause != nil, idleOpts.ReadChunk)
	}
	if !idleOpts.Deadline.IsZero() {
		t.Fatalf("expected zero deadline in default idle mode, got %v", idleOpts.Deadline)
	}
	if idleOpts.ShouldYield() {
		t.Fatal("idle ShouldYield unexpectedly true before deck activity")
	}

	// Deck becomes active: idle pass yields immediately; next pass switches to 64 KiB + 20 ms pacing.
	q := NewPriorityQueue()
	now := time.Now()
	q.UpdateDeckActivity(true, now)
	if !idleOpts.ShouldYield() {
		t.Fatal("idle ShouldYield did not fire immediately when deck became active")
	}

	playOpts, wasActive := backgroundPassOptions(ctx, mount, pri, importWindow, enginedb.Options{Resume: true})
	if !wasActive {
		t.Fatal("expected active deck")
	}
	if !playOpts.SortByPath || playOpts.MaxTracks != playingBatchTracks || playOpts.ReadChunk != enginedb.PlayingReadChunk || playOpts.ReadPause == nil {
		t.Fatalf("unexpected playing options: SortByPath=%v MaxTracks=%d ReadChunk=%d ReadPause=%v",
			playOpts.SortByPath, playOpts.MaxTracks, playOpts.ReadChunk, playOpts.ReadPause != nil)
	}

	// Two consecutive idle samples (>= 400 ms apart) restore idle full-speed mode.
	q.UpdateDeckActivity(false, now.Add(500*time.Millisecond))
	if !IsDeckActive() {
		t.Fatal("single idle sample cleared active state too early")
	}
	q.UpdateDeckActivity(false, now.Add(1000*time.Millisecond))
	if IsDeckActive() {
		t.Fatal("two spaced idle samples did not return to idle")
	}
}

func TestConfirmedRefsSkipsPacingWhenIdleAndPacesWhenPlaying(t *testing.T) {
	t.Setenv("ENGINEX_RB_PRIORITY_DIR", t.TempDir())
	root := t.TempDir()
	// Build a ~192 KiB .2EX PWV7 file (3 x 64 KiB chunks when playing).
	entries := 65000
	bodyLen := entries * 3
	secLen := 24 + bodyLen
	total := 28 + secLen
	raw := make([]byte, total)
	copy(raw[0:4], "PMAI")
	binary.BigEndian.PutUint32(raw[4:8], 28)
	binary.BigEndian.PutUint32(raw[8:12], uint32(total))
	copy(raw[28:32], "PWV7")
	binary.BigEndian.PutUint32(raw[32:36], 24)
	binary.BigEndian.PutUint32(raw[36:40], uint32(secLen))
	binary.BigEndian.PutUint32(raw[40:44], 3)
	binary.BigEndian.PutUint32(raw[44:48], uint32(entries))
	anlzRel := "PIONEER/USBANLZ/P001/ANLZ0000.2EX"
	anlzFull := filepath.Join(root, anlzRel)
	if err := os.MkdirAll(filepath.Dir(anlzFull), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anlzFull, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	lib := &rekordbox.Library{
		Tracks: []rekordbox.Track{{ID: 1, Path: "/Contents/A.mp3", AnalysisPath: "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"}},
	}
	keys := map[uint32]bool{1: true}

	// Idle: should not pay playingReadGap sleeps.
	defer func(old time.Duration) { playingReadGap = old }(playingReadGap)
	playingReadGap = 40 * time.Millisecond
	start := time.Now()
	refs, err := confirmedRefs(context.Background(), r, lib, keys, map[string]bool{})
	idleElapsed := time.Since(start)
	if err != nil || len(refs[1]) == 0 {
		t.Fatalf("idle confirmedRefs: refs=%+v err=%v", refs, err)
	}
	if idleElapsed >= playingReadGap {
		t.Fatalf("idle confirmedRefs unexpectedly paced: took %s (gap=%s)", idleElapsed, playingReadGap)
	}

	// Playing: must read in 64 KiB chunks (>= 3 chunks -> >= 3*playingReadGap).
	q := NewPriorityQueue()
	q.UpdateDeckActivity(true, time.Now())
	start = time.Now()
	refs, err = confirmedRefs(context.Background(), r, lib, keys, map[string]bool{})
	playElapsed := time.Since(start)
	if err != nil || len(refs[1]) == 0 {
		t.Fatalf("playing confirmedRefs: refs=%+v err=%v", refs, err)
	}
	if playElapsed < 3*playingReadGap {
		t.Fatalf("playing confirmedRefs did not pace 64 KiB chunks: took %s, want >= %s", playElapsed, 3*playingReadGap)
	}
}
