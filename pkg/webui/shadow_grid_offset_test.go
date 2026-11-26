package webui

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

	t0 := time.Now()
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

	// Pruning the peer clears RTT and pending UDP ping state.
	a.mu.Lock()
	a.peers["127.0.0.1"] = t0
	a.prunePeers(t0.Add(shadowPeerSeenTimeout + 2*time.Second))
	if len(a.peerRTT) != 0 || len(a.udpPings) != 0 || len(a.udpPingTries) != 0 {
		t.Fatalf("prunePeers left UDP RTT state: rtt=%v pings=%v tries=%v", a.peerRTT, a.udpPings, a.udpPingTries)
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
	if len(sh2.peerRTT) != 0 {
		t.Fatalf("peerRTT not pruned: %+v", sh2.peerRTT)
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
