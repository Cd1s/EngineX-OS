package prolink_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

// player serves root as a Pioneer player's USB slot, with the real server.
type player struct{ *prolink.NFSServer }

func newFakePlayer(t *testing.T, root string) player {
	s := &prolink.NFSServer{Root: root}
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return player{s}
}

func (p player) port() int { return p.PortmapPort() }

func TestMediaCacheReadsTrackFromPlayer(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	player := newFakePlayer(t, usb)
	cache := t.TempDir()
	on := true
	d := prolink.NewDiscovery()
	d.SetMediaCache(&prolink.MediaCache{Dir: cache, Portmap: player.port(), Enabled: func() bool { return on }})
	d.Observe(prolink.Device{Address: "127.0.0.1", Number: 2, PacketType: 6})

	status := prolink.Device{Address: "127.0.0.9", Number: 1, PacketType: 10, TrackID: 1, SourcePlayer: 2, SourceSlot: 3}
	obs := d.Observe(status)
	if obs.Title == "" || len(obs.Waveform) == 0 || len(obs.Beats) == 0 {
		t.Fatalf("track not read from the player: %+v", obs)
	}
	if _, err := os.Stat(filepath.Join(cache, "2-3", "PIONEER", "rekordbox", "export.pdb")); err != nil {
		t.Fatal(err)
	}

	// Another track on the same stick: export.pdb is not read again.
	cached := filepath.Join(cache, "2-3", "PIONEER", "rekordbox", "export.pdb")
	before, _ := os.Stat(cached)
	status.TrackID = 2
	d.Observe(status)
	if after, _ := os.Stat(cached); !os.SameFile(before, after) {
		t.Fatalf("export.pdb fetched again for an unchanged stick")
	}

	// A different stick (new mtime) is fetched again.
	pdb := filepath.Join(usb, "PIONEER", "rekordbox", "export.pdb")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(pdb, later, later); err != nil {
		t.Fatal(err)
	}
	status.TrackID = 3
	d.Observe(status)
	if after, _ := os.Stat(cached); os.SameFile(before, after) {
		t.Fatalf("export.pdb not refreshed after the stick changed")
	}

	// Off: nothing is read; SD slot or no export behaves the same.
	on = false
	off := prolink.NewDiscovery()
	off.SetMediaCache(&prolink.MediaCache{Dir: t.TempDir(), Portmap: player.port(), Enabled: func() bool { return on }})
	off.Observe(prolink.Device{Address: "127.0.0.1", Number: 2, PacketType: 6})
	if got := off.Observe(prolink.Device{Address: "127.0.0.9", Number: 1, PacketType: 10, TrackID: 1, SourcePlayer: 2, SourceSlot: 3}); got.Title != "" {
		t.Fatalf("read while disabled: %+v", got)
	}
}

func TestNFSClientErrors(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	player := newFakePlayer(t, usb)
	c := &prolink.NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: player.port()}
	ctx := t.Context()
	if _, _, err := c.Fetch(ctx, "/B/", "/PIONEER/rekordbox/export.pdb", 1<<20); err == nil {
		t.Fatal("unexported slot mounted")
	}
	if _, _, err := c.Fetch(ctx, "/C/", "/PIONEER/missing", 1<<20); err == nil {
		t.Fatal("missing file read")
	}
	if _, _, err := c.Fetch(ctx, "/C/", "/PIONEER/../etc/passwd", 1<<20); err == nil {
		t.Fatal("dot-dot path accepted")
	}
	if _, _, err := c.Fetch(ctx, "/C/", "/PIONEER/rekordbox/export.pdb", 16); err == nil {
		t.Fatal("size limit ignored")
	}
	data, _, err := c.Fetch(ctx, "/C/", "/PIONEER/rekordbox/export.pdb", 1<<26)
	want, _ := os.ReadFile(filepath.Join(usb, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil || string(data) != string(want) {
		t.Fatalf("fetch: %v (%d of %d bytes)", err, len(data), len(want))
	}
	// Nothing listening: gives up within the per-attempt timeout.
	dead := &prolink.NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: 9, Timeout: 20 * time.Millisecond}
	if _, _, err := dead.Lookup(ctx, "/C/", "/x"); err == nil {
		t.Fatal("no error without a player")
	}
	if _, err := prolink.SlotExport(1); err == nil {
		t.Fatal("CD slot has no export")
	}
}

func TestStageCopiesLibraryWithinBudget(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb)
	player := newFakePlayer(t, usb)
	c := &prolink.NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: player.port()}
	dst := t.TempDir()
	n, err := prolink.Stage(t.Context(), c, "/C/", dst, 1<<30)
	if err != nil || n == 0 {
		t.Fatalf("stage: %d %v", n, err)
	}
	// Staged under the export's own spelling (/Music/...), whatever the stick's case.
	for rel, src := range map[string]string{"PIONEER/rekordbox/export.pdb": "", "PIONEER/USBANLZ/P001/ANLZ0000.DAT": "", "Music/two.flac": "MUSIC/two.flac", "Music/five.wav": "MUSIC/five.wav"} {
		if src == "" {
			src = rel
		}
		want, _ := os.ReadFile(filepath.Join(usb, src))
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil || string(got) != string(want) {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	// Again: everything is already there.
	if n, err := prolink.Stage(t.Context(), c, "/C/", dst, 1<<30); err != nil || n != 0 {
		t.Fatalf("restage: %d %v", n, err)
	}
	// A tiny budget copies nothing but still writes export.pdb.
	small := t.TempDir()
	if n, err := prolink.Stage(t.Context(), c, "/C/", small, 1); err != nil || n != 0 {
		t.Fatalf("budget: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(small, "PIONEER", "rekordbox", "export.pdb")); err != nil {
		t.Fatal(err)
	}
	if _, ok := prolink.SafeRel("/../../etc/passwd"); ok {
		t.Fatal("escaping path accepted")
	}
}

// The server is read-only and confined to its root: no write procedure, no
// "..", no symlink out, no stale or forged handle.
func TestNFSServerConfinement(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	outside := t.TempDir()
	testfixture.Write(t, outside, "secret.txt", []byte("secret"))
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	p := newFakePlayer(t, root)
	c := &prolink.NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: p.port(), Timeout: 200 * time.Millisecond}
	ctx := t.Context()
	for _, bad := range []string{"/link/secret.txt", "/file-link", "/PIONEER/../link/secret.txt"} {
		if _, _, err := c.Fetch(ctx, "/C/", bad, 1<<20); err == nil {
			t.Errorf("%s readable", bad)
		}
	}
	// Case-insensitive like FAT: the export says /Music, the stick has MUSIC.
	got, _, err := c.Fetch(ctx, "/C/", "/Music/two.flac", 1<<26)
	want, _ := os.ReadFile(filepath.Join(root, "MUSIC", "two.flac"))
	if err != nil || string(got) != string(want) {
		t.Fatalf("case-insensitive read: %v", err)
	}
	// A restart invalidates every handle the old server gave out.
	fh, _, err := c.Lookup(ctx, "/C/", "/PIONEER/rekordbox/export.pdb")
	if err != nil {
		t.Fatal(err)
	}
	if st := prolink.NFSStatusForTest(p.NFSServer, 6, fh); st != 0 {
		t.Fatalf("read with a fresh handle: %d", st)
	}
	forged := append([]byte(nil), fh...)
	forged[7] ^= 0x40
	if st := prolink.NFSStatusForTest(p.NFSServer, 6, forged); st != 70 {
		t.Fatalf("forged handle: %d", st)
	}
	for _, proc := range []uint32{2, 8, 9, 10, 11, 14, 15} {
		if st := prolink.NFSStatusForTest(p.NFSServer, proc, fh); st != 30 {
			t.Fatalf("proc %d: %d, want read-only", proc, st)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "secret" {
		t.Fatal("outside file changed")
	}
}
