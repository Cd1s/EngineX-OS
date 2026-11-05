package prolink_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

// rb-bridge must not see the deck's own export: it copied the deck's stick onto itself.
func TestDiscoveryIgnoresOwnPackets(t *testing.T) {
	d := prolink.NewDiscovery()
	d.SetIgnoreLocal(true)
	ctx, cancel := context.WithCancel(t.Context())
	seen := make(chan byte, 16)
	stopped := make(chan error, 1)
	go func() {
		stopped <- d.Listen(ctx, net.IPv4(127, 62, 0, 1), func(dev prolink.Device) { seen <- dev.Number })
	}()
	defer func() { cancel(); <-stopped }()
	own, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}) // a local interface address
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	peer := simSocket(t, 2) // 127.63.0.2, not an interface address
	to := &net.UDPAddr{IP: net.IPv4(127, 62, 0, 1), Port: 50000}
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("the remote player was never seen")
		case <-tick.C:
			own.WriteToUDP(simKeepalive(4), to)
			peer.WriteToUDP(simKeepalive(3), to)
		case n := <-seen:
			if n != 3 {
				t.Fatalf("own packet (player %d) reached the listener", n)
			}
			return
		}
	}
}
