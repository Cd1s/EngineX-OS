package stems

import (
	"context"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProbeLANDiscoversIdleWorkerAndRechecksCachedEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := &Backend{Slot: make(chan struct{}, 1), Name: "htdemucs"}
	srv := httptest.NewServer(backend)
	defer srv.Close()
	port, err := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}

	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go Announce(ctx, pc, Beacon{Port: port, Backend: "htdemucs", Hostname: "primego-gpu"})

	oldDisc, oldMDNS := discoverTargets, mdnsTargets
	discoverTargets = func() []*net.UDPAddr { return []*net.UDPAddr{pc.LocalAddr().(*net.UDPAddr)} }
	mdnsTargets = func() []*net.UDPAddr { return nil }
	t.Cleanup(func() { discoverTargets, mdnsTargets = oldDisc, oldMDNS })

	got, ok := ProbeLAN(ctx, nil, 500*time.Millisecond)
	if !ok || got.Endpoint != srv.URL || got.Backend != "htdemucs" {
		t.Fatalf("ProbeLAN(nil) = (%+v, %v), want (%s, true)", got, ok, srv.URL)
	}

	// Once known, ProbeLAN verifies /healthz directly even with UDP discovery off.
	discoverTargets = func() []*net.UDPAddr { return nil }
	got2, ok2 := ProbeLAN(ctx, &got, 500*time.Millisecond)
	if !ok2 || got2.Endpoint != srv.URL {
		t.Fatalf("ProbeLAN(cached) = (%+v, %v), want (%s, true)", got2, ok2, srv.URL)
	}
}
