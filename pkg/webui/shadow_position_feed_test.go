package webui

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
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
