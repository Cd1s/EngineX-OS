package bridge

import (
	"os"
	"testing"
	"time"
)

// The queue and the deck-activity file are kept as they were (design §1.5): the old test,
// moved. Its two other tests are fill_test.go's TestFillServesThePriorityQueueFirst.
func TestPriorityQueueCoalescePermissionsAndActivity(t *testing.T) {
	priDir := t.TempDir()
	restore := SetPriorityDirForTest(priDir)
	defer restore()

	mount := t.TempDir()
	q := NewPriorityQueue()

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

	pending, maxSeq := PendingPriority(mount)
	if maxSeq != seq2 || len(pending) != 2 || pending[0].Path != "../Contents/TrackB.mp3" || pending[1].Path != "../Contents/TrackA.mp3" {
		t.Fatalf("pending=%+v maxSeq=%d", pending, maxSeq)
	}
	if err := AckPriority(mount, pending[:1]); err != nil {
		t.Fatal(err)
	}
	pending, _ = PendingPriority(mount)
	if len(pending) != 1 || pending[0].Path != "../Contents/TrackA.mp3" {
		t.Fatalf("after partial ack: pending=%+v", pending)
	}

	// Deck activity: active edge takes effect immediately, idle requires 2 spaced samples.
	t0 := time.Now()
	if !q.UpdateDeckActivity(true, t0) || !IsDeckActive() {
		t.Fatal("expected active deck immediately")
	}
	if !q.UpdateDeckActivity(false, t0.Add(500*time.Millisecond)) || !IsDeckActive() {
		t.Fatal("1 idle sample must keep deck active")
	}
	if !q.UpdateDeckActivity(false, t0.Add(600*time.Millisecond)) || !IsDeckActive() {
		t.Fatal("closely spaced idle sample must not count as 2nd sample")
	}
	if q.UpdateDeckActivity(false, t0.Add(1000*time.Millisecond)) || IsDeckActive() {
		t.Fatal("2nd spaced idle sample should return deck to idle")
	}
}
