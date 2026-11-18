package webui

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestShadowPositionFeedWritesSeqlockRecord(t *testing.T) {
	old := shadowPositionFeedPath
	shadowPositionFeedPath = filepath.Join(t.TempDir(), "feed")
	defer func() { shadowPositionFeedPath = old }()
	if err := shadowPositionFeedWrite(shadowPositionFeedRequest{Deck: 2, Key: "track-a", Active: true, PositionSamples: 44100, RateSamplesPerSec: 48000}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(shadowPositionFeedPath)
	if err != nil {
		t.Fatal(err)
	}
	off := 48
	if got := binary.LittleEndian.Uint32(b[off : off+4]); got == 0 || got&1 != 0 {
		t.Fatalf("sequence is not a stable even value: %d", got)
	}
	if got := binary.LittleEndian.Uint32(b[off+4 : off+8]); got != 1 {
		t.Fatalf("active=%d", got)
	}
	if got := binary.LittleEndian.Uint32(b[off+8 : off+12]); got != 2 {
		t.Fatalf("deck=%d", got)
	}
	if got := binary.LittleEndian.Uint64(b[off+40 : off+48]); got == 0 {
		t.Fatal("missing monotonic timestamp")
	}
}

// The record is dated when stream-sync extrapolated the position, not when the follower's
// request arrived: the hook carries it on from there. A time that cannot be right is now.
func TestShadowPositionFeedDatesTheRecordByItsPosition(t *testing.T) {
	old := shadowPositionFeedPath
	shadowPositionFeedPath = filepath.Join(t.TempDir(), "feed")
	defer func() { shadowPositionFeedPath = old }()
	raw := func() int64 {
		var ts unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &ts); err != nil {
			t.Fatal(err)
		}
		return ts.Nano()
	}
	for _, c := range []struct {
		name string
		at   int64
		age  time.Duration
	}{
		{"300 ms old", time.Now().Add(-300 * time.Millisecond).UnixMilli(), 300 * time.Millisecond},
		{"no time", 0, 0},
		{"ahead", time.Now().Add(time.Minute).UnixMilli(), 0},
		{"a minute old", time.Now().Add(-time.Minute).UnixMilli(), 0},
	} {
		before := raw()
		if err := shadowPositionFeedWrite(shadowPositionFeedRequest{Deck: 1, Key: "k", Active: true, PositionSamples: 1, RateSamplesPerSec: 44100, At: c.at}); err != nil {
			t.Fatal(err)
		}
		after := raw()
		b, _ := os.ReadFile(shadowPositionFeedPath)
		got := int64(binary.LittleEndian.Uint64(b[40:48]))
		// The two clocks are read a moment apart and At is in whole milliseconds.
		if lo, hi := before-int64(c.age)-int64(5*time.Millisecond), after-int64(c.age)+int64(5*time.Millisecond); got < lo || got > hi {
			t.Errorf("%s: dated %d ns, want %d..%d", c.name, got, lo, hi)
		}
	}
	// A position that is no number switches the deck's feed off instead of sending the hook there.
	if err := shadowPositionFeedWrite(shadowPositionFeedRequest{Deck: 1, Key: "k", Active: true, PositionSamples: math.NaN()}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(shadowPositionFeedPath)
	if seq, active := binary.LittleEndian.Uint32(b[:4]), binary.LittleEndian.Uint32(b[4:8]); active != 0 || seq&1 != 0 {
		t.Fatalf("NaN position: active=%d seq=%d", active, seq)
	}
}
