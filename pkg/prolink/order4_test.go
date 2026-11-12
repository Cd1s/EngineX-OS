package prolink

import (
	"bytes"
	"context"
	"encoding/binary"
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

func TestEngineBridgeClampsNegativePreRollCuesAndLoops(t *testing.T) {
	const rate = 44100.0
	var q bytes.Buffer
	binary.Write(&q, binary.BigEndian, uint64(8))
	for slot := 1; slot <= 8; slot++ {
		name := ""
		sample := float64(-1)
		switch slot {
		case 1:
			name = "LeadIn"
			sample = -22050 // -500 ms pre-roll hot cue
		case 2:
			name = "Drop"
			sample = 88200 // 2000 ms hot cue
		}
		q.WriteByte(byte(len(name)))
		q.WriteString(name)
		binary.Write(&q, binary.BigEndian, sample)
		q.Write([]byte{255, 255, 0, 0})
	}
	// main_cue at -4410 samples (-100 ms pre-roll), adjusted=1, default_cue=-1
	binary.Write(&q, binary.BigEndian, float64(-4410))
	q.WriteByte(1)
	binary.Write(&q, binary.BigEndian, float64(-1))

	cues, err := engineQuickCues(q.Bytes(), rate)
	if err != nil {
		t.Fatalf("engineQuickCues rejected negative pre-roll cue: %v", err)
	}
	if len(cues) != 3 || cues[0].HotCue != 1 || cues[0].TimeMS != 0 || cues[1].HotCue != 2 || cues[1].TimeMS != 2000 || cues[2].HotCue != 0 || cues[2].TimeMS != 0 {
		t.Fatalf("unexpected clamped cues: %+v", cues)
	}

	var lp bytes.Buffer
	binary.Write(&lp, binary.LittleEndian, uint64(3))
	writeLoop := func(name string, start, end float64) {
		lp.WriteByte(byte(len(name)))
		lp.WriteString(name)
		binary.Write(&lp, binary.LittleEndian, start)
		binary.Write(&lp, binary.LittleEndian, end)
		lp.Write([]byte{1, 1, 255, 0, 255, 0})
	}
	// Slot 1: spans pre-roll (-500 ms) into track (+1000 ms) -> clamps to 0..1000 ms and merges with HotCue 1
	writeLoop("LeadLoop", -22050, 44100)
	// Slot 2: purely pre-roll (-1000 ms .. -200 ms) -> skipped without failing
	writeLoop("PurePreRoll", -44100, -8820)
	// Slot 3: normal loop (4000 ms .. 8000 ms) -> preserved
	writeLoop("Chorus", 176400, 352800)

	merged, err := engineLoops(lp.Bytes(), rate, cues)
	if err != nil {
		t.Fatalf("engineLoops rejected negative pre-roll loop: %v", err)
	}
	var gotLead, gotChorus bool
	for _, c := range merged {
		if c.Name == "PurePreRoll" {
			t.Fatalf("purely pre-roll loop was not skipped: %+v", c)
		}
		if c.HotCue == 1 && c.Loop && c.TimeMS == 0 && c.EndMS == 1000 {
			gotLead = true
		}
		if c.Name == "Chorus" && c.Loop && c.TimeMS == 4000 && c.EndMS == 8000 {
			gotChorus = true
		}
	}
	if !gotLead || !gotChorus {
		t.Fatalf("expected clamped LeadLoop and preserved Chorus loop, got: %+v", merged)
	}
}

