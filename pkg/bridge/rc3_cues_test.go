package bridge_test

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// BridgeAnalysis may be absent for a bridge-owned row when the state database
// hit its size cap. That row must still retain a deck edit through a staged
// rebuild; the bridge-owned Track/BridgeState identity is sufficient.
func TestLargeReexportKeepsDeckCueWhenBridgeAnalysisCapped(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	root := guardStick(t, 20)
	restoreLarge := bridge.SetLargeExportForTest()
	defer restoreLarge()

	live := dbOpen(t, root)
	if _, err := live.Exec(`DELETE FROM bridge.BridgeAnalysis WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=3)`); err != nil {
		t.Fatal(err)
	}
	publishSetB(t, live, 3, 30000.5)
	live.Close()
	analysis := filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT")
	b, err := os.ReadFile(analysis)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if err := os.WriteFile(analysis, b, 0o644); err != nil {
		t.Fatal(err)
	}
	changed := time.Now().Add(time.Hour)
	if err := os.Chtimes(analysis, changed, changed); err != nil {
		t.Fatal(err)
	}
	guardNewExport(t, root)

	if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("start background update: %+v %v", r, err)
	}
	if r, err := bridge.ImportInBackground(context.Background(), root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("publish background update: %+v %v", r, err)
	}

	db := dbOpen(t, root)
	defer db.Close()
	_, cues, at := publishHotB(t, db, 3)
	if got := math.Float64frombits(binary.BigEndian.Uint64(cues[at:])); got != 30000.5 {
		t.Fatalf("deck cue was lost without BridgeAnalysis: got %v, want 30000.5", got)
	}
}
