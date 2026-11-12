package prolink

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func localMedia(t *testing.T) (*MediaCache, Device, net.IP, string) {
	t.Helper()
	m := &MediaCache{Dir: t.TempDir()}
	dev := Device{Address: "127.0.0.1", Number: 1, SourcePlayer: 1, SourceSlot: 3, TrackID: 1}
	host := net.ParseIP(dev.Address)
	root := m.root(dev, host)
	testfixture.Export(t, root)
	testfixture.Write(t, root, ".host", []byte(host.String()))
	testfixture.Write(t, root, pdbPath+".stamp", []byte("first disc"))
	return m, dev, host, root
}

func TestMediaIdentityOwnerStampAndEnabled(t *testing.T) {
	m, dev, host, root := localMedia(t)
	version := m.MediaIdentity(dev, host)
	if a, _, _ := m.CachedAnalysisVersion(dev, host, version); a == nil {
		t.Fatal("valid cached analysis rejected")
	}
	testfixture.Write(t, root, pdbPath+".stamp", []byte("second disc"))
	if a, _, _ := m.CachedAnalysisVersion(dev, host, version); a != nil {
		t.Fatal("previous disc accepted")
	}
	version = m.MediaIdentity(dev, host)
	testfixture.Write(t, root, ".host", []byte("192.0.2.99"))
	if a, _, _ := m.CachedAnalysisVersion(dev, host, version); a != nil {
		t.Fatal("changed owner accepted")
	}
	testfixture.Write(t, root, ".host", []byte(host.String()))
	m.Enabled = func() bool { return false }
	if a, _, _ := m.CachedAnalysis(dev, host); a != nil || m.MediaIdentity(dev, host) != "" {
		t.Fatal("disabled cache read")
	}
}

func TestMediaCacheHostIsolationAndAtomicPublication(t *testing.T) {
	m, dev, host, root := localMedia(t)
	// publish into a fresh slot; existing material is a plain directory fixture.
	dev.SourcePlayer = 2
	root = m.root(dev, host)
	if err := m.publish(root, host, "disc1", testfixture.PDB()); err != nil {
		t.Fatal(err)
	}
	first := m.MediaIdentity(dev, host)
	old, err := os.Readlink(root)
	if err != nil {
		t.Fatal("slot was not atomically published", err)
	}
	if err := os.WriteFile(root+".pending", []byte("disc2"), 0600); err != nil {
		t.Fatal(err)
	}
	if m.MediaIdentity(dev, host) != "" {
		t.Fatal("replacement disc still exposes old waveform while fetching")
	}
	if err := m.publish(root, host, "disc2", testfixture.PDB()); err != nil {
		t.Fatal(err)
	}
	if first == m.MediaIdentity(dev, host) {
		t.Fatal("publication did not change medium identity")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), old)); !os.IsNotExist(err) {
		t.Fatal("old generation not removed")
	}
	other := net.ParseIP("127.0.0.2")
	if err := m.publish(m.root(dev, other), other, "other", testfixture.PDB()); err != nil {
		t.Fatal(err)
	}
	if m.MediaIdentity(dev, host) == "" || m.MediaIdentity(dev, other) == "" || m.root(dev, host) == m.root(dev, other) {
		t.Fatal("same-number players collide")
	}
}

func TestMediaCacheRejectsAnalysisPathTraversal(t *testing.T) {
	utf := func(s string) []byte {
		var b []byte
		for _, c := range utf16.Encode([]rune(s)) {
			b = binary.LittleEndian.AppendUint16(b, c)
		}
		return b
	}
	good := "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"
	for _, path := range []string{"../../../secret", "/../../secret", "/PIONEER/USBANLZ/../../../secret"} {
		t.Run(path, func(t *testing.T) {
			m, dev, host, root := localMedia(t)
			path += strings.Repeat("x", len(good)-len(path))
			pdb := bytes.ReplaceAll(testfixture.PDB(), utf(good), utf(path))
			if bytes.Equal(pdb, testfixture.PDB()) {
				t.Fatal("fixture path not replaced")
			}
			testfixture.Write(t, root, pdbPath, pdb)
			if a, _, _ := m.CachedAnalysis(dev, host); a != nil {
				t.Fatal("analysis traversal accepted")
			}
		})
	}
}

func TestMediaCacheRejectsSymlinkEscape(t *testing.T) {
	m, dev, host, root := localMedia(t)
	outside := filepath.Join(t.TempDir(), "outside.DAT")
	if err := os.WriteFile(outside, testfixture.DAT(), 0600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000")
	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		os.Remove(base + ext)
	}
	if err := os.Symlink(outside, base+".DAT"); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := m.CachedAnalysis(dev, host); a != nil {
		t.Fatal("analysis symlink escaped cache")
	}
}

func TestExporterCachedAnalysisAvoidsLifecycleMutex(t *testing.T) {
	m, dev, host, _ := localMedia(t)
	e := NewExporter(ExportConfig{MediaCache: m})
	e.mu.Lock() // SetEnabled(false)/Rebind hold this while waiting for workers.
	done := make(chan bool, 1)
	go func() { a, _, _ := e.CachedAnalysis(dev, host); done <- a != nil }()
	select {
	case ok := <-done:
		if !ok {
			t.Error("missing cached analysis")
		}
	case <-time.After(time.Second):
		t.Error("display read blocked on exporter shutdown")
	}
	e.mu.Unlock()
}

func TestMediaCachePublishesCompleteAnalysisGeneration(t *testing.T) {
	m, dev, host, _ := localMedia(t)
	dev.SourcePlayer = 2
	root := m.root(dev, host)
	files := map[string][]byte{"PIONEER/USBANLZ/P001/ANLZ0000.DAT": testfixture.DAT(), "PIONEER/USBANLZ/P001/ANLZ0000.EXT": testfixture.EXT()}
	if err := m.publish(root, host, "disc", testfixture.PDB(), mediaPublication{files: files}); err != nil {
		t.Fatal(err)
	}
	version := m.MediaIdentity(dev, host)
	if a, _, _ := m.CachedAnalysisVersion(dev, host, version); a == nil || len(a.Waveforms) == 0 {
		t.Fatal("published partial analysis")
	}
	extra := "PIONEER/USBANLZ/P002/OTHER.DAT"
	if err := m.publish(root, host, "disc", testfixture.PDB(), mediaPublication{preserve: true, files: map[string][]byte{extra: testfixture.DAT()}}); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := m.CachedAnalysis(dev, host); a == nil || len(a.Waveforms) == 0 {
		t.Fatal("new track publication lost prior analysis")
	}
	if _, err := os.Stat(filepath.Join(root, extra)); err != nil {
		t.Fatal(err)
	}
	version = m.MediaIdentity(dev, host)
	if err := m.publish(root, host, "disc", testfixture.PDB(), mediaPublication{files: map[string][]byte{"../outside": {1}}}); err == nil {
		t.Fatal("publication traversal accepted")
	}
	if version != m.MediaIdentity(dev, host) {
		t.Fatal("failed staging changed visible medium")
	}
}

func TestMediaLibraryCanceledParseCanRetry(t *testing.T) {
	m, dev, host, root := localMedia(t)
	version := m.MediaIdentity(dev, host)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.mu.Lock()
	if lib := m.library(ctx, root, version); lib != nil {
		t.Fatal("canceled parse succeeded")
	}
	if lib := m.library(context.Background(), root, version); lib == nil {
		t.Fatal("canceled parse poisoned PDB cache")
	}
	m.mu.Unlock()
}

func TestDisabledMediaKeepsDiscoveryTrackCache(t *testing.T) {
	d := NewDiscovery()
	d.SetMediaCache(&MediaCache{Dir: t.TempDir(), Enabled: func() bool { return false }})
	calls := 0
	d.SetResolver(func(Device) (*rekordbox.Analysis, string, string) { calls++; return nil, "local metadata", "" })
	dev := Device{Address: "127.0.0.1", Number: 1, SourcePlayer: 1, SourceSlot: 3, TrackID: 1, PacketType: 0x0a}
	for i := 0; i < 50; i++ {
		d.Observe(dev)
	}
	if calls != 1 {
		t.Fatalf("disabled share bypassed track cache %d times", calls)
	}
}

func TestMediaProbeFailureBackoff(t *testing.T) {
	m, dev, host, root := localMedia(t)
	at := time.Now()
	m.probes = map[string]mediaProbe{root: {at: at, ok: false}}
	for i := 0; i < 50; i++ {
		if a, _, _ := m.resolve(dev, host); a != nil {
			t.Fatal("failed probe returned analysis")
		}
	}
	if !m.probes[root].at.Equal(at) {
		t.Fatal("failed probe retried before backoff expired")
	}
}

func TestDotPioneerAnalysisAndNativeRekordboxExportGuard(t *testing.T) {
	// 1. MediaCache must accept .PIONEER/USBANLZ/... analysis paths from macOS HFS+ sticks.
	m, dev, host, _ := localMedia(t)
	dev.SourcePlayer = 3
	root := m.root(dev, host)
	lib, err := rekordbox.ParsePDB(bytes.NewReader(testfixture.PDB()), int64(len(testfixture.PDB())))
	if err != nil || len(lib.Tracks) == 0 {
		t.Fatalf("parse fixture pdb: %v", err)
	}
	for i := range lib.Tracks {
		lib.Tracks[i].AnalysisPath = strings.Replace(lib.Tracks[i].AnalysisPath, "/PIONEER/", "/.PIONEER/", 1)
	}
	dotPDB, err := buildPDB(lib.Tracks, lib.Playlists)
	if err != nil {
		t.Fatal(err)
	}
	dotRel := strings.TrimPrefix(lib.Tracks[0].AnalysisPath, "/")
	dotBase := strings.TrimSuffix(dotRel, filepath.Ext(dotRel))
	files := map[string][]byte{
		dotBase + ".DAT": testfixture.DAT(),
		dotBase + ".EXT": testfixture.EXT(),
	}
	if err := m.publish(root, host, "dot-disc", dotPDB, mediaPublication{files: files}); err != nil {
		t.Fatalf("publish .PIONEER analysis failed: %v", err)
	}
	if a, _, _ := m.CachedAnalysis(dev, host); a == nil || len(a.Waveforms) == 0 {
		t.Fatal("CachedAnalysis rejected .PIONEER/USBANLZ path")
	}

	// 2. SyncEngineToPioneerExport must leave Rekordbox 7 (exportLibrary.db) and .PIONEER sticks untouched.
	rb7Stick := t.TempDir()
	testfixture.Write(t, rb7Stick, "PIONEER/rekordbox/exportLibrary.db", []byte("SQLite format 3\x00"))
	testfixture.Write(t, rb7Stick, "Engine Library/Database2/m.db", []byte("not-opened-if-guarded"))
	if err := SyncEngineToPioneerExport(rb7Stick); err != nil {
		t.Fatalf("SyncEngineToPioneerExport on RB7 stick errored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rb7Stick, "PIONEER/rekordbox/export.pdb")); !os.IsNotExist(err) {
		t.Fatal("SyncEngineToPioneerExport wrote synthetic export.pdb onto RB7 exportLibrary.db stick")
	}

	// 3. startExport must accept a .PIONEER/rekordbox/export.pdb stick.
	dotStick := t.TempDir()
	testfixture.Export(t, dotStick)
	if err := os.Rename(filepath.Join(dotStick, "PIONEER"), filepath.Join(dotStick, ".PIONEER")); err != nil {
		t.Fatal(err)
	}
	exp := NewExporter(ExportConfig{
		Root: dotStick,
		IP:   net.IPv4(127, 0, 0, 1),
		MAC:  net.HardwareAddr{0x02, 0, 0, 0, 0, 1},
	})
	defer exp.Close()
	if err := exp.SetEnabled(true); err != nil && strings.Contains(err.Error(), "export.pdb") {
		t.Fatalf("Exporter rejected .PIONEER stick: %v", err)
	}
}
