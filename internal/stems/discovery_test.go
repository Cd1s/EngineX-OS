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
	"sync/atomic"
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

func TestFindLANSkipsBusyResponderForIdlePeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	busyBackend := &Backend{Slot: make(chan struct{}, 1), Name: "htdemucs"}
	busyBackend.Slot <- struct{}{}
	busySrv := httptest.NewServer(busyBackend)
	defer busySrv.Close()
	busyPort, _ := strconv.Atoi(busySrv.URL[strings.LastIndex(busySrv.URL, ":")+1:])

	idleBackend := &Backend{Slot: make(chan struct{}, 1), Name: "htdemucs"}
	idleSrv := httptest.NewServer(idleBackend)
	defer idleSrv.Close()
	idlePort, _ := strconv.Atoi(idleSrv.URL[strings.LastIndex(idleSrv.URL, ":")+1:])

	pcBusy, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go Announce(ctx, pcBusy, Beacon{Port: busyPort, Backend: "htdemucs", Hostname: "busy-pc"})

	pcIdle, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer pcIdle.Close()
		buf := make([]byte, 1500)
		n, from, err := pcIdle.ReadFrom(buf)
		if err != nil || string(buf[:n]) != discoverProbe {
			return
		}
		time.Sleep(25 * time.Millisecond)
		reply, _ := json.Marshal(Beacon{Service: beaconService, Version: 2, Port: idlePort, Backend: "htdemucs", Hostname: "idle-pc"})
		pcIdle.WriteTo(reply, from)
	}()

	old, oldMDNS := discoverTargets, mdnsTargets
	discoverTargets = func() []*net.UDPAddr {
		return []*net.UDPAddr{pcBusy.LocalAddr().(*net.UDPAddr), pcIdle.LocalAddr().(*net.UDPAddr)}
	}
	mdnsTargets = func() []*net.UDPAddr { return nil }
	t.Cleanup(func() { discoverTargets, mdnsTargets = old, oldMDNS })

	state := filepath.Join(t.TempDir(), LANStateName)
	endpoint, ok := FindLAN(ctx, state, 300*time.Millisecond)
	if !ok || endpoint != idleSrv.URL {
		t.Fatalf("FindLAN = (%q, %v), want (%q, true)", endpoint, ok, idleSrv.URL)
	}
}

func TestFindLANRetriesLostWiFiBroadcastAndUsesMDNSARecord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lanBackend := &Backend{Slot: make(chan struct{}, 1), Name: "htdemucs"}
	lanSrv := httptest.NewServer(lanBackend) // bound on 127.0.0.1:<port>
	defer lanSrv.Close()
	port, _ := strconv.Atoi(lanSrv.URL[strings.LastIndex(lanSrv.URL, ":")+1:])

	// 1. Simulate 802.11 Wi-Fi dropping the initial broadcast probe frame; the retry must pair.
	pcLossy, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pcLossy.Close()
	go func() {
		buf := make([]byte, 1500)
		seenProbes := 0
		for {
			n, from, err := pcLossy.ReadFrom(buf)
			if err != nil {
				return
			}
			if string(buf[:n]) == discoverProbe {
				seenProbes++
				if seenProbes == 1 {
					continue // first Wi-Fi broadcast frame lost
				}
				reply, _ := json.Marshal(Beacon{Service: beaconService, Version: 2, Port: port, Backend: "htdemucs", Hostname: "gpu-pc"})
				pcLossy.WriteTo(reply, from)
				return
			}
		}
	}()

	old, oldMDNS := discoverTargets, mdnsTargets
	discoverTargets = func() []*net.UDPAddr { return []*net.UDPAddr{pcLossy.LocalAddr().(*net.UDPAddr)} }
	mdnsTargets = func() []*net.UDPAddr { return nil }
	t.Cleanup(func() { discoverTargets, mdnsTargets = old, oldMDNS })

	state1 := filepath.Join(t.TempDir(), LANStateName)
	if endpoint, ok := FindLAN(ctx, state1, 300*time.Millisecond); !ok || endpoint != lanSrv.URL {
		t.Fatalf("FindLAN after single lost Wi-Fi broadcast = (%q, %v), want (%q, true)", endpoint, ok, lanSrv.URL)
	}

	// 2. mDNS A-record validation:
	// (a) A packet from 127.0.0.2 advertising loopback 127.0.0.1 or an off-subnet private IP
	// must NOT make the deck probe that A-record IP; it falls back to pkt.from.IP (127.0.0.2).
	var victimHits atomic.Int32
	victimSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		victimHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"busy":false,"backend":"htdemucs","protocol":"engine-5.0.4"}`))
	}))
	defer victimSrv.Close()
	victimPort, _ := strconv.Atoi(victimSrv.URL[strings.LastIndex(victimSrv.URL, ":")+1:])

	var spoofOffSubnet atomic.Bool
	pcSpoof, err := net.ListenPacket("udp4", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pcSpoof.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pcSpoof.ReadFrom(buf)
			if err != nil {
				return
			}
			if id, ok := mdnsQuery(buf[:n]); ok {
				spoofIP := net.IPv4(127, 0, 0, 1)
				spoofPort := victimPort
				if spoofOffSubnet.Load() {
					spoofIP = net.IPv4(10, 254, 254, 9)
					spoofPort = 17890
				}
				ans := mdnsAnswer(id, true, Beacon{Port: spoofPort, Backend: "htdemucs", Hostname: "spoof-pc", Version: 2}, spoofIP)
				pcSpoof.WriteTo(ans, from)
			}
		}
	}()

	discoverTargets = func() []*net.UDPAddr { return nil }
	mdnsTargets = func() []*net.UDPAddr { return []*net.UDPAddr{pcSpoof.LocalAddr().(*net.UDPAddr)} }

	state2 := filepath.Join(t.TempDir(), LANStateName)
	if endpoint, ok := FindLAN(ctx, state2, 120*time.Millisecond); ok || victimHits.Load() != 0 {
		t.Fatalf("spoofed loopback mDNS A record probed 127.0.0.1: endpoint=%q ok=%v victimHits=%d", endpoint, ok, victimHits.Load())
	}
	spoofOffSubnet.Store(true)
	var seenEndpoint string
	_ = discoverLANServers(ctx, 120*time.Millisecond, func(ep string, _ Beacon) bool {
		seenEndpoint = ep
		return true
	})
	if seenEndpoint != "http://127.0.0.2:17890" {
		t.Fatalf("off-subnet mDNS A record endpoint = %q, want fallback to packet source http://127.0.0.2:17890", seenEndpoint)
	}
	_, sub50, _ := net.ParseCIDR("192.168.50.0/24")
	if !validMDNSRecordIP(net.IPv4(10, 20, 30, 40), net.IPv4(10, 20, 30, 40), nil) ||
		!validMDNSRecordIP(net.IPv4(192, 168, 50, 9), net.IPv4(192, 168, 1, 1), []*net.IPNet{sub50}) ||
		validMDNSRecordIP(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), []*net.IPNet{sub50}) ||
		validMDNSRecordIP(net.IPv4(10, 99, 0, 5), net.IPv4(192, 168, 50, 1), []*net.IPNet{sub50}) {
		t.Fatal("validMDNSRecordIP accepted loopback/off-subnet IP or rejected matching/local-subnet IP")
	}

	// (b) When the mDNS A record falls inside localLANIPv4Nets() (non-loopback local subnet),
	// a reflected packet from 127.0.0.2 resolves to that local-subnet listener IP.
	if nets := localLANIPv4Nets(); len(nets) > 0 {
		ln, err := net.Listen("tcp4", net.JoinHostPort(nets[0].IP.String(), "0"))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		srvReal := &http.Server{Handler: &Backend{Slot: make(chan struct{}, 1), Name: "htdemucs"}}
		defer srvReal.Close()
		go srvReal.Serve(ln)
		realPort := ln.Addr().(*net.TCPAddr).Port
		wantRealURL := "http://" + net.JoinHostPort(nets[0].IP.String(), strconv.Itoa(realPort))

		pcReflector, err := net.ListenPacket("udp4", "127.0.0.2:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pcReflector.Close()
		go func() {
			buf := make([]byte, 1500)
			for {
				n, from, err := pcReflector.ReadFrom(buf)
				if err != nil {
					return
				}
				if id, ok := mdnsQuery(buf[:n]); ok {
					ans := mdnsAnswer(id, true, Beacon{Port: realPort, Backend: "htdemucs", Hostname: "gpu-pc", Version: 2}, nets[0].IP)
					pcReflector.WriteTo(ans, from)
					return
				}
			}
		}()
		mdnsTargets = func() []*net.UDPAddr { return []*net.UDPAddr{pcReflector.LocalAddr().(*net.UDPAddr)} }
		state2b := filepath.Join(t.TempDir(), LANStateName)
		if endpoint, ok := FindLAN(ctx, state2b, 300*time.Millisecond); !ok || endpoint != wantRealURL {
			t.Fatalf("FindLAN via local-subnet mDNS A record = (%q, %v), want (%q, true)", endpoint, ok, wantRealURL)
		}
	}

	// 3. ProbeLANState (called by deck UI / Web Console when idle) pairs immediately and writes stems-lan.json.
	announceOnLoopback(t, ctx, port)
	state3 := filepath.Join(t.TempDir(), LANStateName)
	st := ProbeLANState(ctx, state3, 250*time.Millisecond)
	if st == nil || !st.Online || st.Endpoint != lanSrv.URL || st.Backend != "htdemucs" {
		t.Fatalf("ProbeLANState failed to pair idle deck: %+v", st)
	}

	// 4. ARP table + RememberPeerHint build unicast DiscoveryPort targets for AP-isolated Wi-Fi.
	arpFile := filepath.Join(t.TempDir(), "arp")
	arpContent := "IP address       HW type     Flags       HW address            Mask     Device\n" +
		"192.168.88.42    0x1         0x2         aa:bb:cc:dd:ee:ff     *        wlan0\n" +
		"192.168.88.99    0x1         0x0         00:00:00:00:00:00     *        wlan0\n"
	if err := os.WriteFile(arpFile, []byte(arpContent), 0o600); err != nil {
		t.Fatal(err)
	}
	origARP := arpTablePath
	arpTablePath = arpFile
	t.Cleanup(func() {
		arpTablePath = origARP
		peerHintsMu.Lock()
		peerHints = nil
		peerHintsMu.Unlock()
	})
	RememberPeerHint("192.168.88.55:54321")
	uTargets := unicastPeerTargets()
	gotIPs := map[string]bool{}
	for _, ut := range uTargets {
		if ut.Port == DiscoveryPort {
			gotIPs[ut.IP.String()] = true
		}
	}
	if !gotIPs["192.168.88.42"] || !gotIPs["192.168.88.55"] || gotIPs["192.168.88.99"] {
		t.Fatalf("unexpected unicast peer targets from ARP + RememberPeerHint: %+v", gotIPs)
	}
}
