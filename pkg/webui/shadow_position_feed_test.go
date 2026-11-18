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
	off := shadowPositionFeedRecordSize
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
		// The hook's side of the contract (2026-11-19): a record is never dated more than
		// 2 s before its write, so a record 5 s old is one this process no longer writes.
		{"1.9 s old", time.Now().Add(-1900 * time.Millisecond).UnixMilli(), 1900 * time.Millisecond},
		{"2.1 s old", time.Now().Add(-2100 * time.Millisecond).UnixMilli(), 0},
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

// A cue before sample 0 and a scratch backwards are positions like any other; a loop is
// sent only when it is one.
func TestShadowPositionFeedTakesNegativeValuesAndALoop(t *testing.T) {
	old := shadowPositionFeedPath
	shadowPositionFeedPath = filepath.Join(t.TempDir(), "feed")
	defer func() { shadowPositionFeedPath = old }()
	f64 := func(b []byte) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b)) }
	write := func(r shadowPositionFeedRequest) []byte {
		t.Helper()
		if err := shadowPositionFeedWrite(r); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(shadowPositionFeedPath)
		if err != nil || len(b) != 4*64 {
			t.Fatalf("feed file: %d bytes, %v", len(b), err)
		}
		return b[2*64 : 3*64]
	}
	b := write(shadowPositionFeedRequest{Deck: 3, Key: "k", Active: true, PositionSamples: -2000, RateSamplesPerSec: -88200, LoopInSamples: 1000, LoopOutSamples: 5000})
	if active, pos, rate, in, out := binary.LittleEndian.Uint32(b[4:]), f64(b[24:]), f64(b[32:]), f64(b[48:]), f64(b[56:]); active != 1 || pos != -2000 || rate != -88200 || in != 1000 || out != 5000 {
		t.Fatalf("active %d position %v rate %v loop %v..%v", active, pos, rate, in, out)
	}
	for _, loop := range [][2]float64{{5000, 1000}, {-1, 5000}, {1000, 1000}, {math.NaN(), 5000}, {1000, math.Inf(1)}} {
		b = write(shadowPositionFeedRequest{Deck: 3, Key: "k", Active: true, PositionSamples: 1, LoopInSamples: loop[0], LoopOutSamples: loop[1]})
		if in, out := f64(b[48:]), f64(b[56:]); in != 0 || out != 0 || binary.LittleEndian.Uint32(b[4:]) != 1 {
			t.Errorf("loop %v: sent %v..%v", loop, in, out)
		}
	}
}
