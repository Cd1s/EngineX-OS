package prolink_test

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestDiscoveryStatusAndExpiry(t *testing.T) {
	b := make([]byte, 54)
	copy(b, "Qspt1WmJOL")
	b[10] = 6
	copy(b[12:], "rekordbox\x00garbage") // bytes after the NUL are not part of the name
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

func TestObserveReturnsMergedStatusAndExpiresOldTrack(t *testing.T) {
	d := prolink.NewDiscovery()
	now := time.Now()
	status := prolink.Device{Address: "192.168.1.2", Number: 1, PacketType: 10, TrackID: 42, SourcePlayer: 2, SourceSlot: 3, LastSeen: now}
	d.Observe(status)
	status.PacketType = 6
	status.TrackID = 0
	status.SourcePlayer = 0
	status.SourceSlot = 0
	merged := d.Observe(status)
	if merged.TrackID != 42 || merged.SourcePlayer != 2 || merged.SourceSlot != 3 {
		t.Fatal("notification lost status", merged)
	}
	d.Devices(now.Add(11 * time.Second))
	if got := d.Observe(status); got.TrackID != 0 {
		t.Fatal("expired status was resurrected", got)
	}
}
