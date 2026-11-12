package stems

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// announceOnLoopback starts a renderer beacon and points discovery at it.
func announceOnLoopback(t *testing.T, ctx context.Context, port int) {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go Announce(ctx, pc, Beacon{Port: port, Backend: "htdemucs", Hostname: "home-pc"})
	old, oldMDNS := discoverTargets, mdnsTargets
	discoverTargets = func() []*net.UDPAddr { return []*net.UDPAddr{pc.LocalAddr().(*net.UDPAddr)} }
	mdnsTargets = func() []*net.UDPAddr { return nil }
	t.Cleanup(func() { discoverTargets, mdnsTargets = old, oldMDNS })
}

// The same renderer is found through mDNS alone.
func TestDiscoverOverMDNS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	announceOnLoopback(t, ctx, 17890)
	probe := discoverTargets()
	discoverTargets, mdnsTargets = func() []*net.UDPAddr { return nil }, func() []*net.UDPAddr { return probe }
	endpoint, b, err := DiscoverLANServer(ctx, 350*time.Millisecond)
	if err != nil || endpoint != "http://127.0.0.1:17890" || b.Backend != "htdemucs" || b.Hostname != "home-pc" || b.Version != 2 {
		t.Fatalf("%q %+v %v", endpoint, b, err)
	}
}

func TestMDNSMessages(t *testing.T) {
	query := append([]byte{0, 9, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}, mdnsQuestion()...)
	if id, ok := mdnsQuery(query); !ok || id != 9 {
		t.Fatalf("own query: %d %v", id, ok)
	}
	other := append([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}, appendName(nil, "_airplay", "_tcp", "local")...)
	if _, ok := mdnsQuery(append(other, 0, 12, 0, 1)); ok {
		t.Fatal("answered another service")
	}
	ans := mdnsAnswer(9, true, Beacon{Port: 17890, Backend: "hpss", Hostname: "Studio.lan", Version: 2}, net.IPv4(192, 168, 1, 20))
	if _, ok := mdnsQuery(ans); ok {
		t.Fatal("a response was taken for a query")
	}
	if ans[0] != 0 || ans[1] != 9 || ans[7] != 4 {
		t.Fatalf("header % x", ans[:12])
	}
	b, ok := parseMDNS(ans)
	if !ok || b.Port != 17890 || b.Backend != "hpss" || b.Hostname != "Studio.lan" || b.Service != "enginex-stemsd" {
		t.Fatalf("%+v %v", b, ok)
	}
	// Compressed names, as other responders send them.
	msg := append(make([]byte, 12), appendName(nil, "a", "b")...)
	msg = append(msg, 0xc0, 12)
	if name, next, ok := readName(msg, len(msg)-2); !ok || name != "a.b" || next != len(msg) {
		t.Fatalf("%q %d %v", name, next, ok)
	}
	if _, _, ok := readName([]byte{0xc0, 0}, 0); ok {
		t.Fatal("pointer loop accepted")
	}
}

func TestDiscoverLANServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	announceOnLoopback(t, ctx, 17890)
	endpoint, b, err := DiscoverLANServer(ctx, 350*time.Millisecond)
	if err != nil || endpoint != "http://127.0.0.1:17890" || b.Service != "enginex-stemsd" || b.Version != 2 || b.Backend != "htdemucs" || b.Hostname != "home-pc" {
		t.Fatalf("%q %+v %v", endpoint, b, err)
	}
	// Nothing answering: the probe gives up at the timeout.
	discoverTargets = func() []*net.UDPAddr { return []*net.UDPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: 9}} }
	start := time.Now()
	if _, _, err = DiscoverLANServer(ctx, 200*time.Millisecond); err == nil || time.Since(start) > time.Second {
		t.Fatalf("silent LAN: %v after %v", err, time.Since(start))
	}
}

func readLANState(t *testing.T, path string) LANState {
	t.Helper()
	var st LANState
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestFindLANDiscoversCachesAndFallsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &Backend{Slot: make(chan struct{}, 1), Name: "htdemucs"}
	srv := httptest.NewServer(b)
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	announceOnLoopback(t, ctx, port)
	state := filepath.Join(t.TempDir(), LANStateName)

	endpoint, ok := FindLAN(ctx, state, 350*time.Millisecond)
	if st := readLANState(t, state); !ok || endpoint != srv.URL || !st.Online || st.Backend != "htdemucs" || st.Endpoint != srv.URL {
		t.Fatalf("discovery: %q %v %+v", endpoint, ok, st)
	}
	// A busy renderer is skipped; the deck renders locally instead of queueing.
	b.Slot <- struct{}{}
	if _, ok = FindLAN(ctx, state, 350*time.Millisecond); ok {
		t.Fatal("busy renderer was chosen")
	}
	<-b.Slot
	// Cached address answers /healthz: no probe needed.
	discoverTargets = func() []*net.UDPAddr { return nil }
	if endpoint, ok = FindLAN(ctx, state, 350*time.Millisecond); !ok || endpoint != srv.URL {
		t.Fatalf("cached: %q %v", endpoint, ok)
	}
	// Computer switched off: quick miss, last renderer kept for the console.
	srv.Close()
	start := time.Now()
	if _, ok = FindLAN(ctx, state, 350*time.Millisecond); ok || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("offline: %v after %v", ok, time.Since(start))
	}
	if st := readLANState(t, state); st.Online || st.Endpoint != srv.URL || st.Hostname == "" {
		t.Fatalf("offline state %+v", st)
	}
}

// A renderer without a credential serves LAN callers only.
func TestOpenBackendIsLANOnly(t *testing.T) {
	b := &Backend{Slot: make(chan struct{}, 2)}
	for addr, want := range map[string]int{"192.168.1.20:5000": 200, "100.101.1.2:5000": 200, "203.0.113.5:5000": 403} {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d want %d", addr, w.Code, want)
		}
		if want == 200 && !strings.Contains(w.Body.String(), `"sessions":2`) {
			t.Fatalf("healthz %s", w.Body)
		}
	}
}

// A renderer that cannot be dialled leaves Engine's stream untouched.
func TestProxyUnreachable(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	in := strings.NewReader("numChannels=2\n\n")
	err := Proxy(context.Background(), "http://"+addr, "", in, io.Discard)
	if !errors.Is(err, ErrUnreachable) || in.Len() != len("numChannels=2\n\n") {
		t.Fatalf("%v, %d bytes left", err, in.Len())
	}
}

// A cloud relay cached in stems-lan.json (or answering generic {"ok":true} on
// /healthz while requiring Bearer auth on /v1/engine) must never be chosen by
// FindLAN as a passwordless home renderer (BH-GF2-5).
func TestFindLANDoesNotUseRelayHealthzOrCachedRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old, oldMDNS := discoverTargets, mdnsTargets
	discoverTargets = func() []*net.UDPAddr { return nil }
	mdnsTargets = func() []*net.UDPAddr { return nil }
	t.Cleanup(func() { discoverTargets, mdnsTargets = old, oldMDNS })

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer relay.Close()

	dir := t.TempDir()
	state := filepath.Join(dir, LANStateName)
	// Simulate pre-5.3.0.5 stems-lan.json written by probeRelayStems (no Relay flag).
	raw, _ := json.Marshal(LANState{
		Endpoint: relay.URL,
		Hostname: "relay-worker",
		Backend:  "htdemucs",
		Online:   true,
		Checked:  time.Now().UTC(),
	})
	if err := os.WriteFile(state, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if endpoint, ok := FindLAN(ctx, state, 150*time.Millisecond); ok {
		err := Proxy(ctx, endpoint, "", strings.NewReader("numChannels=2\nsampleRate=44100\nmaxBlockSize=4\n\naction=done\n\n"), io.Discard)
		t.Fatalf("FindLAN chose relay %q as passwordless LAN renderer (Proxy err=%v)", endpoint, err)
	}

	// Relay:true cached by probeRelayStems is preserved when no LAN renderer answers,
	// and falls through cleanly to a real LAN enginex-stemsd when one announces.
	raw, _ = json.Marshal(LANState{
		Endpoint: relay.URL,
		Hostname: "relay-worker",
		Backend:  "htdemucs",
		Online:   true,
		Relay:    true,
		Checked:  time.Now().UTC(),
	})
	if err := os.WriteFile(state, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if endpoint, ok := FindLAN(ctx, state, 100*time.Millisecond); ok || endpoint != "" {
		t.Fatalf("FindLAN with Relay:true and no LAN server = (%q, %v)", endpoint, ok)
	}
	if st := readLANState(t, state); !st.Relay || !st.Online || st.Endpoint != relay.URL {
		t.Fatalf("Relay:true state was clobbered: %+v", st)
	}

	lanSrv := httptest.NewServer(&Backend{Slot: make(chan struct{}, 1), Name: "htdemucs"})
	defer lanSrv.Close()
	port, _ := strconv.Atoi(lanSrv.URL[strings.LastIndex(lanSrv.URL, ":")+1:])
	announceOnLoopback(t, ctx, port)
	if endpoint, ok := FindLAN(ctx, state, 250*time.Millisecond); !ok || endpoint != lanSrv.URL {
		t.Fatalf("FindLAN did not discover real LAN server behind cached relay: (%q, %v), want %q", endpoint, ok, lanSrv.URL)
	}
}
