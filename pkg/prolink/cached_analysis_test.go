package prolink_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestCachedAnalysisReadOnlyAndIdentity(t *testing.T) {
	usb := t.TempDir()
	testfixture.Export(t, usb) // same rekordbox disk as the CDJ-3000 simulator
	player := newFakePlayer(t, usb)
	m := &prolink.MediaCache{Dir: t.TempDir(), Portmap: player.port()}
	d := prolink.NewDiscovery()
	d.SetMediaCache(m)
	d.Observe(prolink.Device{Address: "127.0.0.1", Number: 2, PacketType: 6})
	dev := d.Observe(prolink.Device{Address: "127.0.0.9", Number: 1, PacketType: 10, TrackID: 1, SourcePlayer: 2, SourceSlot: 3})
	player.Close() // all subsequent reads must work without any network service
	a, title, artist := d.CachedAnalysis(dev)
	if a == nil || title == "" || len(a.Cues) == 0 || len(a.Waveforms) == 0 {
		t.Fatalf("missing cached analysis: %v %q %q", a, title, artist)
	}
	b, bt, ba := m.CachedAnalysis(dev, net.ParseIP("127.0.0.1"))
	if b == nil || bt != title || ba != artist || len(b.Beats) != len(a.Beats) || len(b.Cues) != len(a.Cues) {
		t.Fatalf("disk and memory caches differ: %+v %q %q", b, bt, ba)
	}
	for _, change := range []func(*prolink.Device){
		func(v *prolink.Device) { v.Address = "127.0.0.8" },
		func(v *prolink.Device) { v.Number = 3 },
		func(v *prolink.Device) { v.TrackID = 2 },
		func(v *prolink.Device) { v.SourcePlayer = 3 },
		func(v *prolink.Device) { v.SourceSlot = 2 },
	} {
		wrong := dev
		change(&wrong)
		if a, _, _ := d.CachedAnalysis(wrong); a != nil {
			t.Fatalf("borrowed another track: %+v", wrong)
		}
	}
	if a, _, _ := m.CachedAnalysis(dev, net.ParseIP("127.0.0.2")); a != nil {
		t.Fatal("disk cache borrowed another host's drive")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "127.0.0.1", "2-3", "MUSIC")); !os.IsNotExist(err) {
		t.Fatalf("audio was cached: %v", err)
	}
}
