package prolink

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestStageCountsExistingFilesTowardBudget(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	srv := &NFSServer{Root: usb}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c := &NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: srv.PortmapPort()}
	dst := t.TempDir()

	// Pre-populate dst with 4 KiB of staged content exceeding a 2 KiB budget.
	pre := filepath.Join(dst, "Music", "preexisting.bin")
	if err := os.MkdirAll(filepath.Dir(pre), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pre, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	copied, err := Stage(context.Background(), c, "/C/", dst, 2048)
	if err != nil {
		t.Fatalf("Stage failed: %v", err)
	}
	if copied != 0 {
		t.Fatalf("Stage copied %d new files despite existing dst files already exceeding max budget", copied)
	}
}

func TestAnnounceLoopUpdatesFirstFlagAndRechecksPDBBeforeClaim(t *testing.T) {
	now := time.Now()
	r := &exportRun{
		probed:  true,
		mounted: true,
		ip:      net.IPv4(127, 63, 0, 1),
		mac:     net.HardwareAddr{2, 0, 0, 0, 0, 1},
		peers: map[string]exportPeer{
			"expired": {number: 4, seen: now.Add(-11 * time.Second)},
		},
	}
	r.chooseLocked(now)
	if !r.first || r.announcement()[0x25] != 2 {
		t.Fatalf("after stale peer expired, r.first=%v b[0x25]=%d, want true and 2", r.first, r.announcement()[0x25])
	}
	r.peers["active"] = exportPeer{number: 1, seen: now}
	r.chooseLocked(now)
	if r.first || r.announcement()[0x25] != 1 {
		t.Fatalf("after peer joined, r.first=%v b[0x25]=%d, want false and 1", r.first, r.announcement()[0x25])
	}

	// If export.pdb changes during the initial probe window (< 1.5s), timer.C must not claim a player number.
	root := t.TempDir()
	testfixture.Export(t, root)
	cfg := testExportConfig(root)
	cfg.ProbeDuration = 80 * time.Millisecond
	e := NewExporter(cfg)
	defer e.Close()
	if err := e.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb"), future, future); err != nil {
		t.Fatal(err)
	}
	time.Sleep(130 * time.Millisecond)
	if num := e.Status().Number; num != 0 {
		t.Fatalf("Exporter claimed player %d despite export.pdb changing before probe timer fired", num)
	}
}
