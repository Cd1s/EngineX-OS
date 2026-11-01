package prolink_test

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/prolink"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
)

func TestDiscoveryStatusAndExpiry(t *testing.T) {
	b := make([]byte, 54)
	copy(b, "Qspt1WmJOL")
	b[10] = 6
	copy(b[12:], "rekordbox")
	b[0x24] = 17
	ip := net.IPv4(192, 168, 1, 2)
	d, e := prolink.ParsePacket(b, 50000, ip)
	if e != nil || d.Name != "rekordbox" || d.Number != 17 {
		t.Fatal(d, e)
	}
	discovery := prolink.NewDiscovery()
	discovery.Observe(d)
	if len(discovery.Devices(time.Now())) != 1 || len(discovery.Devices(time.Now().Add(11*time.Second))) != 0 {
		t.Fatal("discovery TTL")
	}
	status := make([]byte, 0xd4)
	copy(status, "Qspt1WmJOL")
	status[10] = 0xa
	copy(status[12:], "CDJ-3000")
	status[0x21] = 1
	status[0x28] = 17
	status[0x29] = 3
	binary.BigEndian.PutUint32(status[0x2c:], 42)
	d, e = prolink.ParsePacket(status, 50002, ip)
	if e != nil || d.TrackID != 42 || d.SourcePlayer != 17 || d.SourceSlot != 3 {
		t.Fatal(d, e)
	}
	for _, bad := range [][]byte{nil, b[:20], status[:54], []byte("untrusted UDP")} {
		if _, e = prolink.ParsePacket(bad, 50002, ip); e == nil {
			t.Fatal("accepted bad packet")
		}
	}
}

type source struct{}

func (source) Library(context.Context, prolink.Device) (*rekordbox.Library, error) {
	return &rekordbox.Library{Tracks: []rekordbox.Track{{ID: 1, Path: "Music/a.wav"}}}, nil
}

type sink struct{ called bool }

func (s *sink) Import(_ context.Context, _ prolink.Device, l *rekordbox.Library) error {
	s.called = len(l.Tracks) == 1
	return nil
}
func TestLibrarySyncInterface(t *testing.T) {
	s := &sink{}
	if e := prolink.Sync(context.Background(), prolink.Device{}, source{}, s); e != nil || !s.called {
		t.Fatal(e, s)
	}
}
