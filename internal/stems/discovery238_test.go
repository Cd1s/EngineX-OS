package stems

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// lanRenderers238 starts a home renderer per backend name, announces each to
// discovery in that order, and returns their base URLs.
func lanRenderers238(t *testing.T, ctx context.Context, backends ...string) []string {
	t.Helper()
	var urls []string
	var targets []*net.UDPAddr
	for _, name := range backends {
		srv := httptest.NewServer(&Backend{Slot: make(chan struct{}, 1), Name: name})
		t.Cleanup(srv.Close)
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go Announce(ctx, pc, Beacon{Port: srv.Listener.Addr().(*net.TCPAddr).Port, Backend: name, Hostname: name + "-pc"})
		urls, targets = append(urls, srv.URL), append(targets, pc.LocalAddr().(*net.UDPAddr))
	}
	old, oldMDNS := discoverTargets, mdnsTargets
	discoverTargets = func() []*net.UDPAddr { return targets }
	mdnsTargets = func() []*net.UDPAddr { return nil }
	t.Cleanup(func() { discoverTargets, mdnsTargets = old, oldMDNS })
	return urls
}

func cacheRenderer238(t *testing.T, st LANState) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), LANStateName)
	b, _ := json.Marshal(st)
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return state
}

// A renderer that only has hpss must not hide one with the neural model
// (#238 review): neither the cached one nor the first to answer ends the
// search, for stems-processor (FindLANRenderer) and for the deck's status
// (ProbeLAN) alike.
func TestLANChoiceLooksPastHPSSForTheNeuralModel(t *testing.T) {
	for _, cached := range []bool{true, false} {
		name := "hpss answers first"
		if cached {
			name = "hpss cached"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			urls := lanRenderers238(t, ctx, "hpss", Neural)
			last := LANState{}
			if cached {
				last = LANState{Endpoint: urls[0], Backend: "hpss", Online: true}
			}
			got, ok := FindLANRenderer(ctx, cacheRenderer238(t, last), 250*time.Millisecond)
			if !ok || got.Endpoint != urls[1] || got.Backend != Neural {
				t.Fatalf("FindLANRenderer chose %q (%s, %v) with the neural renderer %q in reach", got.Endpoint, got.Backend, ok, urls[1])
			}
			if got, ok = ProbeLAN(ctx, &last, 250*time.Millisecond); !ok || got.Endpoint != urls[1] || got.Backend != Neural {
				t.Fatalf("ProbeLAN chose %q (%s, %v) with the neural renderer %q in reach", got.Endpoint, got.Backend, ok, urls[1])
			}
		})
	}
}

// The search must not slow the usual case down: a cached renderer with the
// neural model is taken without waiting for discovery, and one that only has
// hpss is still what is reported when no other answers.
func TestLANChoiceKeepsTheCachedRenderer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const wait = 600 * time.Millisecond // the discovery window
	for _, backend := range []string{Neural, "hpss"} {
		t.Run(backend, func(t *testing.T) {
			srv := httptest.NewServer(&Backend{Slot: make(chan struct{}, 1), Name: backend})
			defer srv.Close()
			lanRenderers238(t, ctx) // nobody answers discovery
			state := cacheRenderer238(t, LANState{Endpoint: srv.URL, Backend: backend, Online: true})
			start := time.Now()
			got, ok := FindLANRenderer(ctx, state, wait)
			if !ok || got.Endpoint != srv.URL || got.Backend != backend {
				t.Fatalf("chose %q (%s, %v), want the cached %q", got.Endpoint, got.Backend, ok, srv.URL)
			}
			if d := time.Since(start); backend == Neural && d >= wait/2 {
				t.Fatalf("the cached neural renderer took %v: it waited for discovery", d)
			}
		})
	}
}
