package webui

import (
	"encoding/binary"
	"math"
	"os"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Shadow grid/CUE, Link fill: the grid left for the hook runs from sample 0,
// as Engine's own analysis does, not from rekordbox's first beat (0.5 s here): a CUE in
// that intro played with no grid under it and the shadow copy went off the main deck.
func TestRekordboxLinkGridCoversTheIntro(t *testing.T) {
	old := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	t.Cleanup(func() { rbxAnalysisDir = old })
	tr := rekordbox.Track{ID: 1, SampleRate: 48000, SampleCount: 3 * 48000, Duration: 3, BPM: 120}
	rbxStoreAnalysis(7, tr, rbxAnalysis(analysedRbxLink{}, 1), false)
	b, err := os.ReadFile(rbxAnalysisPath(7))
	if err != nil {
		t.Fatal(err)
	}
	blobs := rbxBlobs(b)
	if len(blobs) != 5 {
		t.Fatalf("%d blobs", len(blobs))
	}
	raw, err := enginedb.QUncompress(blobs[2], 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	off := 17
	for g := 0; g < 2; g++ {
		n := int(binary.BigEndian.Uint64(raw[off:]))
		first := math.Float64frombits(binary.LittleEndian.Uint64(raw[off+8:]))
		if first > 0 || first <= -24000 || n != 3 {
			t.Fatalf("grid %d: %d markers from sample %v: the intro before 0.5 s has no grid", g, n, first)
		}
		off += 8 + 24*n
	}
}
