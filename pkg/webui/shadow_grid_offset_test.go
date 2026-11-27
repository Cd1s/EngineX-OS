package webui

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// stepWallClock returns a copy of t whose wall clock stepped by deltaSec while
// keeping its monotonic reading unchanged (simulating a local NTP clock step).
func stepWallClock(t *testing.T, base time.Time, deltaSec int64) time.Time {
	t.Helper()
	type rawTime struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	rt := *(*rawTime)(unsafe.Pointer(&base))
	const hasMonotonic = uint64(1) << 63
	const nsecShift = 30
	if rt.wall&hasMonotonic == 0 {
		t.Fatal("base time has no monotonic clock reading")
	}
	sec := int64((rt.wall &^ hasMonotonic) >> nsecShift)
	nsec := rt.wall & ((1 << nsecShift) - 1)
	rt.wall = hasMonotonic | (uint64(sec+deltaSec) << nsecShift) | nsec
	out := *(*time.Time)(unsafe.Pointer(&rt))
	if out.Sub(base) != 0 {
		t.Fatalf("stepWallClock changed monotonic clock by %v", out.Sub(base))
	}
	if diff := out.UnixMilli() - base.UnixMilli(); diff != deltaSec*1000 {
		t.Fatalf("stepWallClock wall delta = %d ms, want %d ms", diff, deltaSec*1000)
	}
	return out
}

func TestShadowLegacyCarryPreservesMonotonicClockAcrossWallStep(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	pAt := time.Now()
	s.shadow.mu.Lock()
	s.shadow.files["peer:"+stableTrackKey("k")] = "/tmp/already-copied.wav"
	s.shadow.peerDecks[2] = peerDeck{
		deckState: deckState{Deck: 2, Key: "k", Link: "link-A", Playing: true, PositionMS: 1000, Pitch: 0, BPM: 128, CurrentBPM: 128},
		ip:        "192.0.2.7",
		at:        pAt,
		anchorPos: 1000,
		anchorAt:  pAt,
	}
	s.shadow.mu.Unlock()

	// Same-millisecond wake right after receipt must never walk backward below p.PositionMS.
	sameMSNow := pAt.Add(20 * time.Microsecond)
	outSameMS, _, _, _ := s.shadowLegacy("13", sameMSNow, false)
	if len(outSameMS) != 1 || outSameMS[0].PositionMS < 1000 {
		t.Fatalf("same-ms wake walked backward: %+v", outSameMS)
	}

	// Normal monotonic advance: carried position matches d.At to the exact ms.
	steadyNow := pAt.Add(50*time.Millisecond + 600*time.Microsecond)
	outSteady, _, _, _ := s.shadowLegacy("13", steadyNow, false)
	if len(outSteady) != 1 {
		t.Fatalf("steady shadowLegacy returned %d decks, want 1", len(outSteady))
	}
	subMS := time.Duration(steadyNow.Nanosecond()) % time.Millisecond
	wantSteady := 1000 + float64((steadyNow.Sub(pAt)-subMS).Microseconds())/1000.0
	if outSteady[0].At != steadyNow.UnixMilli() || math.Abs(outSteady[0].PositionMS-wantSteady) > 0.01 {
		t.Fatalf("steady carry: pos=%.3f at=%d, want pos=%.3f at=%d", outSteady[0].PositionMS, outSteady[0].At, wantSteady, steadyNow.UnixMilli())
	}

	// +1 s and -1 s wall-clock steps with only 50 ms monotonic elapsed: deck is still
	// fresh and must stay at ~1050 ms, never jumping +/-1000 ms.
	for _, deltaSec := range []int64{1, -1} {
		steppedNow := stepWallClock(t, steadyNow, deltaSec)
		out, _, _, _ := s.shadowLegacy("13", steppedNow, false)
		if len(out) != 1 {
			t.Fatalf("wall step %+ds: shadowLegacy returned %d decks, want 1", deltaSec, len(out))
		}
		if out[0].At != steppedNow.UnixMilli() {
			t.Fatalf("wall step %+ds: d.At = %d, want %d", deltaSec, out[0].At, steppedNow.UnixMilli())
		}
		if math.Abs(out[0].PositionMS-wantSteady) > 0.01 {
			t.Fatalf("wall step %+ds corrupted carried position: got %.3f ms, want %.3f ms", deltaSec, out[0].PositionMS, wantSteady)
		}
	}
}

func TestSlowHTTPPollDoesNotAdvanceColdUDPCopy(t *testing.T) {
	// Reproduce the slow-HTTP / fast-UDP mismatch: an 80 ms HTTP /peer/decks handler
	// must NOT be treated as UDP transit when a 2 ms one-way UDP push arrives.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		nowMS := time.Now().UnixMilli()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decks": []deckState{{
				Deck: 2, Key: "k", Link: "link-A", Playing: true,
				PositionMS: 1000, Pitch: 0, Seq: 1, Sent: nowMS,
			}},
		})
	}))
	defer srv.Close()

	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.mu.Lock()
	s.shadow.peers[host] = time.Now()
	s.shadow.peerPort = port
	s.shadow.mu.Unlock()

	pollStart := time.Now()
	s.shadowPollPeers()
	if took := time.Since(pollStart); took < 70*time.Millisecond {
		t.Fatalf("slow HTTP poll took only %v, test fixture did not delay", took)
	}

	// Cold UDP update arrives 2 ms after sender stamped Sent (true source pos at arrival = 5002 ms).
	sentAt := time.Now()
	arrivedAt := sentAt.Add(2 * time.Millisecond)
	pkt, err := json.Marshal([]deckState{{
		Deck: 2, Key: "k", Link: "link-A", Playing: true,
		PositionMS: 5000, Pitch: 0, Seq: 10, Sent: sentAt.UnixMilli(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.shadow.receive(append([]byte(shadowDecksMagic), pkt...), host, arrivedAt)

	s.shadow.mu.Lock()
	gotPos := s.shadow.peerDecks[2].PositionMS
	s.shadow.mu.Unlock()
	// True source position at arrival is 5002 ms; cold UDP halfRTT=0 leaves 5000 ms (-2 ms).
	// Using the 80 ms HTTP poll as RTT pushed this +18 ms ahead to 5020 ms.
	if errMS := gotPos - 5002.0; math.Abs(errMS) > 3.0 {
		t.Fatalf("slow HTTP poll contaminated cold UDP copy: got %.3f ms (err %+.3f ms vs true 5002.0 ms)", gotPos, errMS)
	}
}

func TestUDPPingPongMeasuresTransitIndependentOfSlowHTTP(t *testing.T) {
	aConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer aConn.Close()
	bConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer bConn.Close()

	a := newShadowState()
	b := newShadowState()
	bPort := bConn.LocalAddr().(*net.UDPAddr).Port

	t0 := time.Now()
	// Register peers via valid discovery beacons so echo replies are permitted.
	a.receive([]byte("EngineX-shadow 1"), "127.0.0.1", t0)
	b.receive([]byte("EngineX-shadow 1"), "127.0.0.1", t0)

	// Drive one real UDP ping -> peer handleUDP echo -> sender handleUDP pong with
	// synthetic 4 ms RTT (2 ms one-way), plus an initial 30 ms spike that converges down.
	exchange := func(tSend time.Time, rtt time.Duration) {
		a.sendUDPPing(aConn, "127.0.0.1", bPort, tSend)
		buf := make([]byte, 128)
		bConn.SetReadDeadline(time.Now().Add(time.Second))
		n, fromA, err := bConn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("peer read ping: %v", err)
		}
		b.handleUDP(bConn, buf[:n], fromA, tSend.Add(rtt/2))

		aConn.SetReadDeadline(time.Now().Add(time.Second))
		n, fromB, err := aConn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("sender read pong: %v", err)
		}
		a.handleUDP(aConn, buf[:n], fromB, tSend.Add(rtt))
	}

	// Cold peer before first pong: halfRTT must be 0.
	if got := a.peerHalfRTT("127.0.0.1", t0); got != 0 {
		t.Fatalf("cold peerHalfRTT = %v, want 0", got)
	}

	// First cold ping hits a 30 ms spike; second ping sees true 4 ms wire RTT -> 2 ms halfRTT.
	exchange(t0, 30*time.Millisecond)
	if got := a.peerHalfRTT("127.0.0.1", t0.Add(30*time.Millisecond)); math.Abs(got-15.0) > 0.1 {
		t.Fatalf("spike halfRTT = %.3f, want ~15.0", got)
	}
	exchange(t0.Add(250*time.Millisecond), 4*time.Millisecond)
	if got := a.peerHalfRTT("127.0.0.1", t0.Add(254*time.Millisecond)); math.Abs(got-2.0) > 0.1 {
		t.Fatalf("converged halfRTT = %.3f, want ~2.0", got)
	}

	// Unmatched / replayed / stale pongs must be ignored.
	fromB := bConn.LocalAddr().(*net.UDPAddr)
	a.handleUDP(aConn, []byte(shadowPongMagic+"bogus"), fromB, t0.Add(260*time.Millisecond))
	a.sendUDPPing(aConn, "127.0.0.1", bPort, t0.Add(500*time.Millisecond))
	buf := make([]byte, 128)
	bConn.SetReadDeadline(time.Now().Add(time.Second))
	n, fromA, err := bConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	b.handleUDP(bConn, buf[:n], fromA, t0.Add(750*time.Millisecond))
	aConn.SetReadDeadline(time.Now().Add(time.Second))
	n, fromB, err = aConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	// Stale pong (> 400 ms) must not overwrite the 4 ms RTT.
	a.handleUDP(aConn, buf[:n], fromB, t0.Add(1000*time.Millisecond))
	if got := a.peerHalfRTT("127.0.0.1", t0.Add(1000*time.Millisecond)); got > 2.2 {
		t.Fatalf("stale pong polluted halfRTT: got %.3f", got)
	}

	// Receiving a UDP deck push now compensates by ~2 ms (not 0 or 20 ms).
	sentAt := t0.Add(1200 * time.Millisecond)
	arrivedAt := sentAt.Add(2 * time.Millisecond)
	a.mu.Lock()
	a.takePeer("127.0.0.1", []deckState{{
		Deck: 2, Key: "k", Link: "link-A", Playing: true,
		PositionMS: 5000, Pitch: 0, Seq: 20, Sent: sentAt.UnixMilli(),
	}}, arrivedAt)
	gotPos := a.peerDecks[2].PositionMS
	a.mu.Unlock()
	if math.Abs(gotPos-5002.0) > 0.25 {
		t.Fatalf("warm UDP copy pos = %.3f ms, want ~5002.0 ms", gotPos)
	}

	// Pruning the peer clears RTT and echo peer state.
	a.mu.Lock()
	a.peers["127.0.0.1"] = t0
	a.prunePeers(t0.Add(shadowPeerSeenTimeout + 2*time.Second))
	if p := a.findEchoPeer("127.0.0.1"); p != nil {
		t.Fatalf("prunePeers left UDP echo peer state: %+v", p)
	}
	a.mu.Unlock()
}

func TestPeerHalfRTTCompensatesMinTransitAndBoundsOutliers(t *testing.T) {
	sh := newShadowState()
	t0 := time.Now()

	// Before any RTT measurement, halfRTT is 0 (legacy behaviour unchanged).
	if got := sh.peerHalfRTT("192.0.2.7", t0); got != 0 {
		t.Fatalf("unmeasured halfRTT = %v, want 0", got)
	}

	// First UDP echo is 8 ms; second is 4 ms -> halfRTT = 2 ms.
	sh.recordPeerRTT("192.0.2.7", 8.0, t0)
	if got := sh.peerHalfRTT("192.0.2.7", t0); math.Abs(got-4.0) > 1e-6 {
		t.Fatalf("initial halfRTT = %v, want 4.0", got)
	}
	sh.recordPeerRTT("192.0.2.7", 4.0, t0.Add(250*time.Millisecond))
	if got := sh.peerHalfRTT("192.0.2.7", t0.Add(250*time.Millisecond)); math.Abs(got-2.0) > 1e-6 {
		t.Fatalf("min halfRTT = %v, want 2.0", got)
	}
	// A transient spike does not jump the running minimum.
	sh.recordPeerRTT("192.0.2.7", 40.0, t0.Add(500*time.Millisecond))
	if got := sh.peerHalfRTT("192.0.2.7", t0.Add(500*time.Millisecond)); got > 2.2 {
		t.Fatalf("spike jumped halfRTT to %v", got)
	}

	// takePeer adds (excess + halfRTT) * speed to a playing deck, even on the quickest packet (excess == 0).
	d := deckState{Deck: 2, Key: "k", Link: "link-A", Playing: true, PositionMS: 1000, Pitch: 50, Seq: 1, Sent: t0.Add(500 * time.Millisecond).UnixMilli()}
	sh.takePeer("192.0.2.7", []deckState{d}, t0.Add(502*time.Millisecond))
	gotPos := sh.peerDecks[2].PositionMS
	wantPos := 1000 + 2.0125*1.5 // ~3.02 ms at +50%
	if math.Abs(gotPos-wantPos) > 0.15 {
		t.Fatalf("quickest update pos = %.3f, want ~%.3f (halfRTT not compensated)", gotPos, wantPos)
	}

	// A paused deck is never moved by halfRTT.
	paused := deckState{Deck: 2, Key: "k", Link: "link-A", Playing: false, PositionMS: 2000, Pitch: 50, Seq: 2, Sent: t0.Add(750 * time.Millisecond).UnixMilli()}
	sh.takePeer("192.0.2.7", []deckState{paused}, t0.Add(752*time.Millisecond))
	if got := sh.peerDecks[2].PositionMS; got != 2000 {
		t.Fatalf("paused deck moved by halfRTT: %v", got)
	}

	// Huge RTT is bounded by shadowHalfRTTMaxMS.
	sh2 := newShadowState()
	sh2.recordPeerRTT("192.0.2.8", 200.0, t0)
	if got := sh2.peerHalfRTT("192.0.2.8", t0); got != shadowHalfRTTMaxMS {
		t.Fatalf("bounded halfRTT = %v, want %v", got, shadowHalfRTTMaxMS)
	}

	// Expired RTT returns 0 and is pruned.
	if got := sh2.peerHalfRTT("192.0.2.8", t0.Add(shadowPeerSeenTimeout+time.Second)); got != 0 {
		t.Fatalf("stale halfRTT = %v, want 0", got)
	}
	sh2.prunePeers(t0.Add(shadowPeerSeenTimeout + time.Second))
	if p := sh2.findEchoPeer("192.0.2.8"); p != nil {
		t.Fatalf("peerRTT not pruned: %+v", p)
	}
}

func TestTakePeerSkewStillMeasuresWithHalfRTTAboveSkewLateThreshold(t *testing.T) {
	sh := newShadowState()
	t0 := time.Now()
	const pitch, skew = 0.0, -200e-6
	speed := 1 + skew
	// 12 ms RTT -> 6 ms halfRTT (> shadowSkewLateMS = 2 ms). Skew regression must still use excess jitter.
	sh.recordPeerRTT("192.0.2.9", 12.0, t0)
	var seq int64
	for at := time.Duration(0); at <= 40*time.Second; at += 500 * time.Millisecond {
		seq++
		now := t0.Add(at + 6*time.Millisecond)
		sh.recordPeerRTT("192.0.2.9", 12.0, now)
		d := deckState{
			Deck:       2,
			Key:        "k",
			Link:       "link-C",
			Playing:    true,
			Pitch:      pitch,
			PositionMS: 1000 + float64(at.Microseconds())/1000*speed,
			Seq:        seq,
			Sent:       t0.Add(at).UnixMilli(),
		}
		sh.takePeer("192.0.2.9", []deckState{d}, now)
	}
	got := sh.peerDecks[2].skew
	if math.Abs(got-skew) > 60e-6 {
		t.Fatalf("skew with 6 ms halfRTT = %g, want %g +-60e-6", got, skew)
	}
}

func TestTakePeerPausedFollowUsesTighterDriftDeadband(t *testing.T) {
	t0 := time.Now()
	base := deckState{Deck: 2, Key: "k", Link: "L", Playing: true, PositionMS: 1000, Pitch: 0, BPM: 128, CurrentBPM: 128}

	// Legacy playing-copy mode keeps shadowPlayingDriftMS (12 ms): 6 ms drift stays quiet.
	legacy := newShadowState()
	legacy.pausedFollow.Store(false)
	d := base
	d.Seq = 1
	legacy.takePeer("192.0.2.7", []deckState{d}, t0)
	vLegacy := legacy.ver
	d.Seq = 2
	d.PositionMS = 1506 // +6 ms drift at 500 ms
	legacy.takePeer("192.0.2.7", []deckState{d}, t0.Add(500*time.Millisecond))
	if legacy.ver != vLegacy {
		t.Fatalf("legacy mode bumped on 6 ms drift (want <= %v ms quiet)", shadowPlayingDriftMS)
	}

	// Paused-follow mode uses shadowPausedFollowDriftMS: within deadband stays quiet, beyond deadband re-anchors.
	pf := newShadowState()
	pf.pausedFollow.Store(true)
	d = base
	d.Seq = 1
	pf.takePeer("192.0.2.7", []deckState{d}, t0)
	vPF := pf.ver
	d.Seq = 2
	d.PositionMS = 1500 + shadowPausedFollowDriftMS*0.5
	pf.takePeer("192.0.2.7", []deckState{d}, t0.Add(500*time.Millisecond))
	if pf.ver != vPF {
		t.Fatalf("paused-follow bumped inside deadband (%.2f ms <= %.2f ms)", shadowPausedFollowDriftMS*0.5, shadowPausedFollowDriftMS)
	}
	d.Seq = 3
	d.PositionMS = 2000 + shadowPausedFollowDriftMS + 0.5
	pf.takePeer("192.0.2.7", []deckState{d}, t0.Add(time.Second))
	if pf.ver == vPF {
		t.Fatalf("paused-follow did not re-anchor beyond deadband (%.2f ms > %.2f ms)", shadowPausedFollowDriftMS+0.5, shadowPausedFollowDriftMS)
	}
	if pf.peerDecks[2].anchorPos != d.PositionMS {
		t.Fatalf("paused-follow anchorPos = %v, want %v", pf.peerDecks[2].anchorPos, d.PositionMS)
	}
}

func TestShadowPositionFeedAgePreservesSubMillisecondElapsed(t *testing.T) {
	at := int64(1_700_000_000_000)
	now := time.UnixMilli(at).Add(940 * time.Microsecond)
	got := shadowPositionFeedAge(at, now)
	if got != 940*time.Microsecond {
		t.Fatalf("shadowPositionFeedAge = %v, want 940us (sub-ms truncation lost feed age)", got)
	}
}

func TestUIGetSyncsPausedFollowWithoutLoopPolling(t *testing.T) {
	oldState, oldAlive := hookFeatureStatePath, engineAlive
	stateFile := filepath.Join(t.TempDir(), "hook-features")
	hookFeatureStatePath = stateFile
	engineAlive = func(pid int) bool { return pid == 4242 }
	defer func() {
		hookFeatureStatePath, engineAlive = oldState, oldAlive
	}()

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	if err := os.WriteFile(stateFile, []byte("pid=4242\nbuild=\noff=\non=shadow-paused-follow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/enginex/ui", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !s.shadow.pausedFollow.Load() {
		t.Fatalf("GET /v1/enginex/ui did not sync pausedFollow=true: code=%d pausedFollow=%v", w.Code, s.shadow.pausedFollow.Load())
	}

	if err := os.WriteFile(stateFile, []byte("pid=4242\nbuild=\noff=shadow-paused-follow\non=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/v1/enginex/ui", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK || s.shadow.pausedFollow.Load() {
		t.Fatalf("GET /v1/enginex/ui did not sync pausedFollow=false: code=%d pausedFollow=%v", w.Code, s.shadow.pausedFollow.Load())
	}
}

func TestUDPEchoKnownPeersOnlyRateLimitedAndLockFree(t *testing.T) {
	aConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer aConn.Close()
	bConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer bConn.Close()

	a := newShadowState()
	b := newShadowState()
	aAddr := aConn.LocalAddr().(*net.UDPAddr)
	bAddr := bConn.LocalAddr().(*net.UDPAddr)
	t0 := time.Now()

	readReply := func(conn *net.UDPConn, wait time.Duration) (string, bool) {
		buf := make([]byte, 128)
		conn.SetReadDeadline(time.Now().Add(wait))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", false
		}
		return string(buf[:n]), true
	}

	// 1. Unknown peer ping must be dropped (no echo reply).
	b.handleUDP(bConn, []byte(shadowPingMagic+"1a2b"), aAddr, t0)
	if got, ok := readReply(aConn, 40*time.Millisecond); ok {
		t.Fatalf("answered UDP ping from unknown peer: %q", got)
	}

	// 2. Known peer gets a reply, bounded to at most one reply per reply gap.
	b.receive([]byte("EngineX-shadow 1"), "127.0.0.1", t0)
	b.handleUDP(bConn, []byte(shadowPingMagic+"1111"), aAddr, t0)
	if got, ok := readReply(aConn, 200*time.Millisecond); !ok || got != shadowPongMagic+"1111" {
		t.Fatalf("known peer first ping reply = (%q, %v), want %q", got, ok, shadowPongMagic+"1111")
	}
	// Rapid flood within 200 ms must be dropped.
	for _, off := range []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 150 * time.Millisecond} {
		b.handleUDP(bConn, []byte(shadowPingMagic+"2222"), aAddr, t0.Add(off))
	}
	if got, ok := readReply(aConn, 40*time.Millisecond); ok {
		t.Fatalf("rate-limited UDP ping was answered: %q", got)
	}
	// After the reply interval elapses, a known peer is answered again.
	b.handleUDP(bConn, []byte(shadowPingMagic+"3333"), aAddr, t0.Add(250*time.Millisecond))
	if got, ok := readReply(aConn, 200*time.Millisecond); !ok || got != shadowPongMagic+"3333" {
		t.Fatalf("post-interval ping reply = (%q, %v), want %q", got, ok, shadowPongMagic+"3333")
	}

	// 3. Receive path (both ping reply and pong handling) must be lock-free under sh.mu.
	a.receive([]byte("EngineX-shadow 1"), "127.0.0.1", t0.Add(500*time.Millisecond))
	tSend := t0.Add(500 * time.Millisecond)
	a.sendUDPPing(aConn, "127.0.0.1", bAddr.Port, tSend)
	pingPkt, ok := readReply(bConn, 200*time.Millisecond)
	if !ok {
		t.Fatal("did not receive sent UDP ping")
	}

	done := make(chan struct{})
	a.mu.Lock()
	b.mu.Lock()
	go func() {
		defer close(done)
		b.handleUDP(bConn, []byte(pingPkt), aAddr, tSend.Add(2*time.Millisecond))
		pongPkt, ok := readReply(aConn, 100*time.Millisecond)
		if ok {
			a.handleUDP(aConn, []byte(pongPkt), bAddr, tSend.Add(4*time.Millisecond))
		}
	}()
	select {
	case <-done:
		b.mu.Unlock()
		a.mu.Unlock()
	case <-time.After(250 * time.Millisecond):
		b.mu.Unlock()
		a.mu.Unlock()
		<-done
		t.Fatal("UDP ping/pong receive path blocked on sh.mu (not lock-free)")
	}
	if got := a.peerHalfRTT("127.0.0.1", tSend.Add(4*time.Millisecond)); math.Abs(got-2.0) > 0.1 {
		t.Fatalf("lock-free pong halfRTT = %.3f ms, want ~2.0 ms", got)
	}

	// 4. Pruned peer is no longer eligible for UDP echo replies.
	b.mu.Lock()
	b.prunePeers(tSend.Add(shadowPeerSeenTimeout + time.Second))
	b.mu.Unlock()
	b.handleUDP(bConn, []byte(shadowPingMagic+"4444"), aAddr, tSend.Add(shadowPeerSeenTimeout+time.Second))
	if got, ok := readReply(aConn, 40*time.Millisecond); ok {
		t.Fatalf("answered UDP ping from pruned peer: %q", got)
	}
}

func TestPausedFollowWireAndSuiteLifecycle(t *testing.T) {
	oldState, oldDisable, oldAlive := hookFeatureStatePath, hookDisablePath, engineAlive
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "hook-features")
	disableFile := filepath.Join(dir, "hook-disable.conf")
	hookFeatureStatePath = stateFile
	hookDisablePath = disableFile
	livePID := 4242
	engineAlive = func(pid int) bool { return pid == livePID }
	defer func() {
		hookFeatureStatePath, hookDisablePath, engineAlive = oldState, oldDisable, oldAlive
	}()

	uiDir := filepath.Join(dir, "ui")
	stateDir := filepath.Join(dir, "state")

	// Model the exact QML semantics in tools/ui-mod/build_packs.py:
	//   if (Array.isArray(v.featuresOff)) scFeaturesOff = v.featuresOff
	//   if (v.shadowPausedFollow !== undefined) scHookFollows = v.shadowPausedFollow === true
	//   scPausedShadowFollow = scHookFollows && !scOff("shadow-paused-follow")
	type qmlUI struct {
		featuresOff []string
		hookFollows bool
	}
	apply := func(ui *qmlUI, raw []byte) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal /v1/enginex/ui: %v (%s)", err, raw)
		}
		if rawOff, ok := m["featuresOff"]; ok {
			if arr, ok := rawOff.([]any); ok {
				off := make([]string, 0, len(arr))
				for _, item := range arr {
					if s, ok := item.(string); ok {
						off = append(off, s)
					}
				}
				ui.featuresOff = off
			}
		}
		if rawFollow, ok := m["shadowPausedFollow"]; ok {
			b, _ := rawFollow.(bool)
			ui.hookFollows = b
		}
		return m
	}
	uiPausedFollow := func(ui *qmlUI) bool {
		return ui.hookFollows && !slices.Contains(ui.featuresOff, "shadow-paused-follow")
	}

	callUI := func(s *Server, method, target string, ui *qmlUI, want bool, step string) {
		t.Helper()
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s (%s %s): status=%d body=%s", step, method, target, w.Code, w.Body.String())
		}
		m := apply(ui, w.Body.Bytes())
		if _, ok := m["shadowPausedFollow"]; !ok {
			t.Fatalf("%s (%s): wire JSON omitted explicit shadowPausedFollow: %s", step, method, w.Body.String())
		}
		rawOff, ok := m["featuresOff"]
		if !ok {
			t.Fatalf("%s (%s): wire JSON omitted explicit featuresOff: %s", step, method, w.Body.String())
		}
		if _, isArr := rawOff.([]any); !isArr {
			t.Fatalf("%s (%s): wire JSON featuresOff is not array (%T): %s", step, method, rawOff, w.Body.String())
		}
		gotUI := uiPausedFollow(ui)
		gotSuite := s.shadow.pausedFollow.Load()
		if gotUI != want || gotSuite != want {
			t.Fatalf("%s (%s): UI=%v suite=%v, want both %v (body=%s)", step, method, gotUI, gotSuite, want, w.Body.String())
		}
	}

	newSrv := func() *Server {
		return New(Config{UIDir: uiDir, StateDir: stateDir}, nil)
	}

	// 1. Fresh UI with feature ON, persist a setting so reboot persistence is exercised too.
	if err := os.WriteFile(stateFile, []byte("pid=4242\nbuild=\noff=\non=shadow-paused-follow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newSrv()
	ui := &qmlUI{}
	callUI(s, http.MethodGet, "/v1/enginex/ui", ui, true, "initial on")
	callUI(s, http.MethodPost, "/v1/enginex/ui?fourDeckWaveform=1", ui, true, "post on")

	// 2. Toggle OFF while UI stays warm: wire must explicitly send shadowPausedFollow:false.
	if err := os.WriteFile(stateFile, []byte("pid=4242\nbuild=\noff=shadow-paused-follow\non=shadow-paused-follow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callUI(s, http.MethodGet, "/v1/enginex/ui", ui, false, "warm toggle off")
	callUI(s, http.MethodPost, "/v1/enginex/ui?fourDeckWaveform=1", ui, false, "warm post off")

	// 3. Re-enable while UI stays warm: wire must explicitly send featuresOff:[] so UI clears stale off entry.
	if err := os.WriteFile(stateFile, []byte("pid=4242\nbuild=\noff=\non=shadow-paused-follow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callUI(s, http.MethodGet, "/v1/enginex/ui", ui, true, "warm toggle back on")

	// 4. stream-sync re-creation with the same warm UI (both ON and OFF states).
	s.Close()
	s = newSrv()
	callUI(s, http.MethodGet, "/v1/enginex/ui", ui, true, "after stream-sync recreate (on)")
	if err := os.WriteFile(stateFile, []byte("pid=4242\nbuild=\noff=shadow-paused-follow\non=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = newSrv()
	callUI(s, http.MethodGet, "/v1/enginex/ui", ui, false, "after stream-sync recreate (off)")

	// 5. Fresh UI with feature ON, then hook-state file disappears: wire must explicitly send false + [].
	if err := os.WriteFile(stateFile, []byte("pid=4242\nbuild=\noff=\non=shadow-paused-follow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ui = &qmlUI{}
	callUI(s, http.MethodGet, "/v1/enginex/ui", ui, true, "fresh ui on before removal")
	if err := os.Remove(stateFile); err != nil {
		t.Fatal(err)
	}
	callUI(s, http.MethodGet, "/v1/enginex/ui", ui, false, "hook-state removed while ui warm")

	// 6. Reboot-like restart: new Engine PID, fresh stream-sync, fresh UI, persisted uiSettings intact.
	s.Close()
	livePID = 9001
	for _, tc := range []struct {
		name  string
		state string
		want  bool
	}{
		{"reboot off", "pid=9001\nbuild=\noff=shadow-paused-follow\non=\n", false},
		{"reboot on", "pid=9001\nbuild=\noff=\non=shadow-paused-follow\n", true},
	} {
		if err := os.WriteFile(stateFile, []byte(tc.state), 0o644); err != nil {
			t.Fatal(err)
		}
		s = newSrv()
		freshUI := &qmlUI{}
		callUI(s, http.MethodGet, "/v1/enginex/ui", freshUI, tc.want, tc.name)
		// Verify persisted fourDeckWaveform setting survived the reboot-like restart.
		req := httptest.NewRequest(http.MethodGet, "/v1/enginex/ui", nil)
		req.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		var persisted struct {
			FourDeckWaveform bool `json:"fourDeckWaveform"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &persisted); err != nil || !persisted.FourDeckWaveform {
			t.Fatalf("%s: persisted fourDeckWaveform lost: %s", tc.name, w.Body.String())
		}
		s.Close()
	}
}

func TestUDPPingPongOverlappingSendAndReceive(t *testing.T) {
	aConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer aConn.Close()
	bConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer bConn.Close()

	a := newShadowState()
	bAddr := bConn.LocalAddr().(*net.UDPAddr)
	t0 := time.Now()
	a.receive([]byte("EngineX-shadow 1"), "127.0.0.1", t0)
	p := a.findEchoPeer("127.0.0.1")
	if p == nil {
		t.Fatal("missing echo peer")
	}

	// Deterministic overlap: send seq1 at t0, overwrite with seq2 at t0+250ms before
	// pong1 arrives. A late pong1 must be dropped without clobbering seq2, and pong2
	// must then measure against seq2's own send timestamp.
	readPingSeq := func() string {
		t.Helper()
		buf := make([]byte, 128)
		bConn.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := bConn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read ping: %v", err)
		}
		return string(buf[len(shadowPingMagic):n])
	}
	if !a.sendUDPPingPeer(aConn, p, bAddr.Port, t0) {
		t.Fatal("first ping rate-limited")
	}
	seq1 := readPingSeq()
	t2 := t0.Add(250 * time.Millisecond)
	if !a.sendUDPPingPeer(aConn, p, bAddr.Port, t2) {
		t.Fatal("second ping rate-limited")
	}
	seq2 := readPingSeq()

	a.handleUDP(aConn, []byte(shadowPongMagic+seq1), bAddr, t2.Add(2*time.Millisecond))
	if got := a.peerHalfRTT("127.0.0.1", t2.Add(2*time.Millisecond)); got != 0 {
		t.Fatalf("superseded pong1 recorded halfRTT = %.3f ms, want 0", got)
	}
	a.handleUDP(aConn, []byte(shadowPongMagic+seq2), bAddr, t2.Add(8*time.Millisecond))
	if got := a.peerHalfRTT("127.0.0.1", t2.Add(8*time.Millisecond)); math.Abs(got-4.0) > 0.05 {
		t.Fatalf("pong2 halfRTT = %.3f ms, want 4.0 ms (pong1 must not have cleared seq2)", got)
	}

	// Concurrent overlap stress: sender emits probes spaced 205 ms apart while receiver
	// delivers every pong with a true RTT of 207 ms (halfRTT clamped to shadowHalfRTTMaxMS).
	// If nonce and send timestamp ever tear across send(k+1) and handleUDP(pong_k), the
	// receiver computes (t_k + 207 ms) - (t_k + 205 ms) = 2 ms -> halfRTT = 1.0 ms.
	a2 := newShadowState()
	tBase := time.Now()
	a2.receive([]byte("EngineX-shadow 1"), "127.0.0.1", tBase)
	p2 := a2.findEchoPeer("127.0.0.1")

	stopDrain := make(chan struct{})
	var drainWG sync.WaitGroup
	drainWG.Add(1)
	go func() {
		defer drainWG.Done()
		buf := make([]byte, 128)
		for {
			select {
			case <-stopDrain:
				return
			default:
			}
			bConn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			_, _, _ = bConn.ReadFromUDP(buf)
		}
	}()

	type probe struct {
		seq   uint64
		tSend time.Time
	}
	const iters = 15000
	ch := make(chan probe, 1)
	var badHalfRTTMilli atomic.Int64
	var rxWG sync.WaitGroup
	rxWG.Add(1)
	go func() {
		defer rxWG.Done()
		var pkt [len(shadowPongMagic) + 16]byte
		copy(pkt[:], shadowPongMagic)
		for pr := range ch {
			n := len(shadowPongMagic) + len(strconv.AppendUint(pkt[len(shadowPongMagic):len(shadowPongMagic)], pr.seq, 16))
			tRecv := pr.tSend.Add(207 * time.Millisecond)
			a2.handleUDP(aConn, pkt[:n], bAddr, tRecv)
			if got := a2.peerHalfRTT("127.0.0.1", tRecv); got > 0 && got < shadowHalfRTTMaxMS-0.5 {
				badHalfRTTMilli.Store(int64(got * 1000))
			}
		}
	}()

	for i := 1; i <= iters; i++ {
		tSend := tBase.Add(time.Duration(i) * 205 * time.Millisecond)
		if !a2.sendUDPPingPeer(aConn, p2, bAddr.Port, tSend) {
			t.Fatalf("send %d unexpectedly skipped", i)
		}
		select {
		case ch <- probe{seq: uint64(i), tSend: tSend}:
		default:
		}
	}
	close(ch)
	rxWG.Wait()
	close(stopDrain)
	drainWG.Wait()

	if bad := badHalfRTTMilli.Load(); bad != 0 {
		t.Fatalf("overlapping send/receive tore nonce+timestamp and recorded bogus halfRTT = %.3f ms (true RTT was 207 ms)", float64(bad)/1000.0)
	}
}

func TestUDPPingPongAcceptedZeroAllocs(t *testing.T) {
	aConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer aConn.Close()
	bConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer bConn.Close()
	bAddr := bConn.LocalAddr().(*net.UDPAddr)

	const runs = 64
	const total = runs + 1 // 1 warmup + runs measured by testing.AllocsPerRun
	states := make([]*shadowState, total)
	pongs := make([][]byte, total)
	t0 := time.Now()
	sentAt := t0.Add(time.Second)
	recvAt := sentAt.Add(4 * time.Millisecond)
	buf := make([]byte, 128)
	for i := range total {
		sh := newShadowState()
		sh.receive([]byte("EngineX-shadow 1"), "127.0.0.1", t0)
		p := sh.findEchoPeer("127.0.0.1")
		if p == nil || !sh.sendUDPPingPeer(aConn, p, bAddr.Port, sentAt) {
			t.Fatalf("arm ping %d failed", i)
		}
		bConn.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := bConn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read ping %d: %v", i, err)
		}
		rest := buf[len(shadowPingMagic):n]
		pong := make([]byte, len(shadowPongMagic)+len(rest))
		copy(pong, shadowPongMagic)
		copy(pong[len(shadowPongMagic):], rest)
		states[i] = sh
		pongs[i] = pong
	}

	peerAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: bAddr.Port}
	idx := 0
	allocs := testing.AllocsPerRun(runs, func() {
		sh := states[idx]
		pkt := pongs[idx]
		idx++
		sh.handleUDP(nil, pkt, peerAddr, recvAt)
	})
	if idx != total {
		t.Fatalf("ran %d times, want %d", idx, total)
	}
	for i, sh := range states {
		if got := sh.peerHalfRTT("127.0.0.1", recvAt); math.Abs(got-2.0) > 0.05 {
			t.Fatalf("state %d accepted UDP pong halfRTT = %.3f ms, want 2.0 ms", i, got)
		}
	}
	if allocs != 0 {
		t.Fatalf("accepted UDP pong allocated %.2f allocs/run, want 0", allocs)
	}
}

func TestUDPPingPongPrunePreservesNewerSample(t *testing.T) {
	sh := newShadowState()
	t0 := time.Now()
	sh.receive([]byte("EngineX-shadow 1"), "127.0.0.1", t0)

	// Poll takes its snapshot tPoll, then a concurrent UDP pong records a fresh
	// 4 ms RTT (half RTT = 2 ms) 1 ms later before prunePeers(tPoll) runs.
	tPoll := t0.Add(time.Second)
	tSample := tPoll.Add(time.Millisecond)
	sh.recordPeerRTT("127.0.0.1", 4.0, tSample)

	// Both peerHalfRTT(tPoll) and prunePeers(tPoll) must treat the newer sample as fresh.
	if got := sh.peerHalfRTT("127.0.0.1", tPoll); math.Abs(got-2.0) > 0.05 {
		t.Fatalf("peerHalfRTT(tPoll) before prune = %.3f ms, want 2.0 ms", got)
	}
	sh.prunePeers(tPoll)
	if got := sh.peerHalfRTT("127.0.0.1", tSample); math.Abs(got-2.0) > 0.05 {
		t.Fatalf("prunePeers(tPoll) cleared newer sample: halfRTT = %.3f ms, want 2.0 ms", got)
	}

	// Slightly out-of-order recordPeerRTT must keep running-min instead of resetting.
	sh.recordPeerRTT("127.0.0.1", 20.0, tPoll)
	if got := sh.peerHalfRTT("127.0.0.1", tSample); math.Abs(got-2.0) > 0.05 {
		t.Fatalf("out-of-order recordPeerRTT reset running-min: halfRTT = %.3f ms, want 2.0 ms", got)
	}

	// Direct prune across 40-bit ms wrap: preserve fresh wrapped sample and 1 ms ahead
	// across wrap, and clear expired wrapped & ancient half-ring samples.
	const ring = uint64(1) << udpRTTAtBits
	p := sh.findEchoPeer("127.0.0.1")

	// Sample 1 ms ahead right across the 40-bit wrap (nowAt = ring-1, sampleAt = 0): fresh.
	p.rttState.Store(packUDPRTT(1, 4000, 0))
	sh.boot = t0.Add(-time.Duration(ring-1) * time.Millisecond)
	sh.prunePeers(t0)
	if got := sh.peerHalfRTT("127.0.0.1", t0); math.Abs(got-2.0) > 0.05 {
		t.Fatalf("prunePeers cleared sample 1 ms ahead across 40-bit wrap: halfRTT = %.3f ms, want 2.0 ms", got)
	}

	// Legitimate fresh wrap (sample 5 ms before wrap, now 5 ms after wrap -> age 10 ms): fresh.
	p.rttState.Store(packUDPRTT(1, 4000, int64(ring-5)))
	sh.boot = t0.Add(-5 * time.Millisecond)
	sh.prunePeers(t0)
	if got := sh.peerHalfRTT("127.0.0.1", t0); math.Abs(got-2.0) > 0.05 {
		t.Fatalf("prunePeers cleared fresh wrapped sample: halfRTT = %.3f ms, want 2.0 ms", got)
	}

	// Legitimate expired wrap (sample 10 s before wrap, now 6 s after wrap -> age 16 s): expired.
	p.rttState.Store(packUDPRTT(1, 4000, int64(ring-10000)))
	sh.boot = t0.Add(-6000 * time.Millisecond)
	sh.prunePeers(t0)
	if got := sh.peerHalfRTT("127.0.0.1", t0); got != 0 {
		t.Fatalf("prunePeers kept expired wrapped sample: halfRTT = %.3f ms, want 0", got)
	}
}

func TestUDPPingPongAgeWrapWindow(t *testing.T) {
	const ring = uint64(1) << udpRTTAtBits
	timeoutMS := int64(shadowPeerSeenTimeout / time.Millisecond)

	if udpRTTExpired(ring-1, 0, timeoutMS) {
		t.Fatal("sample 1 ms ahead across 40-bit wrap marked expired")
	}
	if udpRTTExpired(5, ring-5, timeoutMS) {
		t.Fatal("legitimate 10 ms wrap marked expired")
	}
	if !udpRTTExpired(6000, ring-10000, timeoutMS) {
		t.Fatal("legitimate 16 s wrap not expired")
	}
	if !udpRTTExpired(1000, 1000+udpRTTFutureWindowMS+1, timeoutMS) {
		t.Fatal("sample outside small future window not expired")
	}
	if !udpRTTExpired(1<<39, 0, timeoutMS) {
		t.Fatal("ancient half-ring sample not expired")
	}
}
