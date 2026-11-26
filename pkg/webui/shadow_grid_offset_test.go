package webui

import (
	"math"
	"testing"
	"time"
)

func TestPeerHalfRTTCompensatesMinTransitAndBoundsOutliers(t *testing.T) {
	sh := newShadowState()
	t0 := time.Now()

	// Before any RTT measurement, halfRTT is 0 (legacy behaviour unchanged).
	if got := sh.peerHalfRTT("link-A", "192.0.2.7", t0); got != 0 {
		t.Fatalf("unmeasured halfRTT = %v, want 0", got)
	}

	// First HTTP poll includes TCP handshake (8 ms); second keep-alive poll is 4 ms -> halfRTT = 2 ms.
	sh.recordPeerRTT("192.0.2.7", "link-A", 8.0, t0)
	if got := sh.peerHalfRTT("link-A", "192.0.2.7", t0); math.Abs(got-4.0) > 1e-6 {
		t.Fatalf("initial halfRTT = %v, want 4.0", got)
	}
	sh.recordPeerRTT("192.0.2.7", "link-A", 4.0, t0.Add(250*time.Millisecond))
	if got := sh.peerHalfRTT("link-A", "192.0.2.7", t0.Add(250*time.Millisecond)); math.Abs(got-2.0) > 1e-6 {
		t.Fatalf("min halfRTT = %v, want 2.0", got)
	}
	// A transient spike does not jump the running minimum.
	sh.recordPeerRTT("192.0.2.7", "link-A", 40.0, t0.Add(500*time.Millisecond))
	if got := sh.peerHalfRTT("link-A", "192.0.2.7", t0.Add(500*time.Millisecond)); got > 2.2 {
		t.Fatalf("spike jumped halfRTT to %v", got)
	}

	// takePeer adds (excess + halfRTT) * speed to a playing deck, even on the quickest packet (excess == 0).
	d := deckState{Deck: 2, Key: "k", Link: "link-A", Playing: true, PositionMS: 1000, Pitch: 50, Seq: 1, Sent: t0.Add(500 * time.Millisecond).UnixMilli()}
	sh.takePeer("192.0.2.7", []deckState{d}, t0.Add(502*time.Millisecond))
	gotPos := sh.peerDecks[2].PositionMS
	wantPos := 1000 + 2.125*1.5 // ~3.19 ms at +50%
	if math.Abs(gotPos-wantPos) > 0.3 {
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
	sh2.recordPeerRTT("192.0.2.8", "link-B", 200.0, t0)
	if got := sh2.peerHalfRTT("link-B", "192.0.2.8", t0); got != shadowHalfRTTMaxMS {
		t.Fatalf("bounded halfRTT = %v, want %v", got, shadowHalfRTTMaxMS)
	}

	// Expired RTT returns 0 and is pruned.
	if got := sh2.peerHalfRTT("link-B", "192.0.2.8", t0.Add(shadowPeerSeenTimeout+time.Second)); got != 0 {
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
	sh.recordPeerRTT("192.0.2.9", "link-C", 12.0, t0)
	var seq int64
	for at := time.Duration(0); at <= 40*time.Second; at += 500 * time.Millisecond {
		seq++
		now := t0.Add(at + 6*time.Millisecond)
		sh.recordPeerRTT("192.0.2.9", "link-C", 12.0, now)
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
