package webui

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Heartbeats and steady playback must not advance the version: the display QML
// parses a reply only when the version (or the compose digest) changes.
func TestTakePeerBumpsOnlyWhenThePictureChanges(t *testing.T) {
	sh := newShadowState()
	t0 := time.Now()
	cue := 1234.5
	base := deckState{Deck: 2, Key: "k", Title: "T", Playing: true, PositionMS: 1000, Pitch: 0, BPM: 120, CurrentBPM: 120, CuePos: &cue, Cues: []shadowCue{{I: 1, P: 4800, N: "A", C: "#fff"}}, Link: "L"}
	seq := int64(1)
	take := func(d deckState, at time.Duration) uint64 {
		t.Helper()
		seq++
		d.Seq = seq
		sh.takePeer("192.0.2.7", []deckState{d}, t0.Add(at))
		return sh.ver
	}
	v := take(base, 0)
	if v == 0 {
		t.Fatal("a new deck must bump")
	}
	steady := base
	steady.PositionMS, steady.Beat, steady.CurrentBPM, steady.Dbg = 1500, 2.5, 120, "x"
	steady.Cues = []shadowCue{{I: 1, P: 4800, N: "A", C: "#fff"}} // a new slice, same cues
	c2 := cue
	steady.CuePos = &c2
	if take(steady, 500*time.Millisecond) != v {
		t.Fatal("steady playback / heartbeat bumped")
	}
	if p := sh.peerDecks[2]; p.at != t0.Add(500*time.Millisecond) || p.PositionMS != 1500 || p.anchorPos != 1000 {
		t.Fatalf("liveness or anchor: %+v", p)
	}
	// Small drifts accumulate against the anchored line, not the previous sample.
	steady.PositionMS = 2009 // +9 ms at 1 s: sampling noise
	if take(steady, time.Second) != v {
		t.Fatal("9 ms drift bumped")
	}
	steady.PositionMS = 2518 // +18 ms at 1.5 s: each step was only 9 ms (a jog nudge, clock drift)
	if v2 := take(steady, 1500*time.Millisecond); v2 == v {
		t.Fatal("accumulated drift never woke the QML")
	} else {
		v = v2
	}
	if p := sh.peerDecks[2]; p.anchorPos != 2518 || p.anchorAt != t0.Add(1500*time.Millisecond) {
		t.Fatalf("re-anchored: %+v", p)
	}
	for name, edit := range map[string]func(*deckState){
		"pause":  func(d *deckState) { d.Playing = false },
		"pitch":  func(d *deckState) { d.Pitch = 0.5 },
		"tempo":  func(d *deckState) { d.CurrentBPM = 120.01 },
		"title":  func(d *deckState) { d.Title = "U" },
		"cue":    func(d *deckState) { d.Cues = []shadowCue{{I: 1, P: 4801, N: "A", C: "#fff"}} },
		"cuePos": func(d *deckState) { x := 1.0; d.CuePos = &x },
		"jump":   func(d *deckState) { d.Jumps++ },
		"master": func(d *deckState) { d.Master = new(true) },
		"track":  func(d *deckState) { d.Key = "k2" },
	} {
		d := steady
		d.PositionMS = 2518 + 500 // on the line at 2 s
		edit(&d)
		if take(d, 2*time.Second) == v {
			t.Fatalf("%s did not bump", name)
		}
		v = sh.ver
		take(steady, 2*time.Second) // back to the base picture (bumps too)
		v = sh.ver
	}
	paused := steady
	paused.Playing = false
	v = take(paused, 3*time.Second)
	paused.PositionMS += 0.2 // float noise of a paused deck
	if take(paused, 3500*time.Millisecond) != v {
		t.Fatal("paused heartbeat bumped")
	}
	paused.PositionMS += 5 // a nudge while paused
	if take(paused, 4*time.Second) == v {
		t.Fatal("paused seek did not bump")
	}
	v = sh.ver
	seq++
	sh.takePeer("192.0.2.7", nil, t0.Add(5*time.Second)) // goodbye
	if sh.ver == v || len(sh.peerDecks) != 0 {
		t.Fatal("an eject did not bump")
	}
}

// The QML pings between posts; the decks stay live and keep moving for the peer.
func TestDecksAlivePingKeepsOwnDecksLive(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	ping := func() int {
		w := httptest.NewRecorder()
		s.decksAlive(w, httptest.NewRequest("POST", "/v1/enginex/decks/alive", nil))
		return w.Code
	}
	if c := ping(); c != http.StatusConflict {
		t.Fatalf("ping before any post: %d, want 409 (ask for a full post)", c)
	}
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader(fmt.Sprintf(`[{"deck":1,"playing":true,"positionMs":5000,"pitch":10,"bpm":120,"beat":1,"master":true,"at":%d}]`, time.Now().UnixMilli()))))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.shadow.mu.Lock()
	seq := s.shadow.own[0].Seq
	// The last post is 3 s old but pings came since: the peer still extrapolates.
	s.shadow.own[0].At -= 3000
	s.shadow.ownAt = time.Now().Add(-time.Second)
	s.shadow.ownExport.At = s.shadow.ownExport.At.Add(-3 * time.Second)
	s.shadow.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	if c := ping(); c != http.StatusNoContent {
		t.Fatalf("ping: %d", c)
	}
	s.shadow.mu.Lock()
	out := s.shadow.ownForPeer(time.Now())
	newSeq, ex := s.shadow.own[0].Seq, s.shadow.ownExport
	s.shadow.mu.Unlock()
	if newSeq <= seq || len(out) != 1 || out[0].Stale {
		t.Fatalf("ping: seq %d->%d %+v", seq, newSeq, out)
	}
	if p := out[0].PositionMS; p < 5000+3000*1.1-50 || p > 5000+3000*1.1+100 {
		t.Fatalf("alive deck not carried on: %v", p)
	}
	if time.Since(ex.At) > time.Second || ex.Beat < 0 || ex.Beat >= 4 {
		t.Fatalf("export not carried: %+v", ex)
	}
	// Silent for 2 s: stale, and the playhead stops 2 s after the last word.
	s.shadow.mu.Lock()
	s.shadow.ownAt = time.Now().Add(-2500 * time.Millisecond)
	out = s.shadow.ownForPeer(time.Now())
	s.shadow.mu.Unlock()
	if p := out[0].PositionMS; !out[0].Stale || p < 5000+2500*1.1-100 || p > 5000+2500*1.1+100 {
		t.Fatalf("silent deck: %+v", out[0])
	}
	if c := ping(); c != http.StatusConflict {
		t.Fatalf("ping after 2 s of silence: %d, want 409", c)
	}
}

// The X-Shadow-State header changes with the version and with what compose
// derives from time (stale), never on a heartbeat.
func TestShadowStateHeaderSkipsHeartbeats(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowMode=cdj")
	wantTempo := 130.17
	header := func() string {
		t.Helper()
		r, err := http.Get(srv.URL + "/v1/enginex/shadow")
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ Decks []deckState }
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if len(v.Decks) != 1 || v.Decks[0].CurrentBPM != wantTempo {
			t.Fatalf("exact tempo: %+v, want %v", v.Decks, wantTempo)
		}
		return r.Header.Get("X-Shadow-State")
	}
	now := time.Now()
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", BPM: 128, Pitch: 1.7, CurrentBPM: 130.17, Playing: true, PositionMS: 1000, Seq: 1}}, now)
	s.shadow.mu.Unlock()
	h1 := header()
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", BPM: 128, Pitch: 1.7, CurrentBPM: 130.17, Playing: true, PositionMS: 1300, Seq: 2}}, now.Add(300*time.Millisecond))
	s.shadow.mu.Unlock()
	if h2 := header(); h2 != h1 || h1 == "" {
		t.Fatalf("heartbeat changed the header: %q -> %q", h1, h2)
	}
	// A tempo-only update must wake the display even when rounded pitch is
	// unchanged. Reconstructing it from 128 BPM and 1.7% would show 130.18.
	wantTempo = 130.18
	s.shadow.mu.Lock()
	d := s.shadow.peerDecks[2].deckState
	d.CurrentBPM, d.Seq = wantTempo, 3
	s.shadow.takePeer("192.0.2.7", []deckState{d}, now.Add(300*time.Millisecond))
	s.shadow.mu.Unlock()
	if h := header(); h == h1 {
		t.Fatal("exact tempo change kept the header")
	} else {
		h1 = h
	}
	s.shadow.mu.Lock()
	p := s.shadow.peerDecks[2]
	p.at = time.Now().Add(-3 * time.Second) // silent peer: compose marks it stale
	s.shadow.peerDecks[2] = p
	s.shadow.mu.Unlock()
	if h3 := header(); h3 == h1 {
		t.Fatal("a stale deck kept the header")
	}
}

// A display retry while the source still analyses must not wake the QML.
func TestDisplayRetryBumpsOnlyOnReadiness(t *testing.T) {
	pending := &shadowDisplayAsset{DetailPending: true}
	ready := &shadowDisplayAsset{Detail: true}
	for _, c := range []struct {
		name     string
		old, new *shadowDisplayEntry
		want     bool
	}{
		{"first fetch", nil, &shadowDisplayEntry{}, true},
		{"first asset", &shadowDisplayEntry{}, &shadowDisplayEntry{asset: pending}, true},
		{"still pending", &shadowDisplayEntry{asset: pending}, &shadowDisplayEntry{asset: &shadowDisplayAsset{DetailPending: true}}, false},
		{"detail arrived", &shadowDisplayEntry{asset: pending}, &shadowDisplayEntry{asset: ready}, true},
		{"failed retry", &shadowDisplayEntry{}, &shadowDisplayEntry{}, false},
	} {
		if got := displayReadinessChanged(c.old, c.new); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Detail readiness reaches the QML through the header digest and displayDetail,
// so it neither polls the display JSON nor needs a version bump.
func TestShadowStateHeaderCarriesDisplayDetail(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowMode=cdj")
	get := func() (string, []deckState) {
		t.Helper()
		r, err := http.Get(srv.URL + "/v1/enginex/shadow")
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ Decks []deckState }
		json.NewDecoder(r.Body).Decode(&v)
		r.Body.Close()
		return r.Header.Get("X-Shadow-State"), v.Decks
	}
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Seq: 1}}, time.Now())
	p := s.shadow.peerDecks[2]
	s.shadow.mu.Unlock()
	id := displayPeerID(p)
	set := func(a *shadowDisplayAsset) {
		a.Key = "a"
		s.shadow.display.mu.Lock()
		s.shadow.display.entries = map[string]*shadowDisplayEntry{id: {asset: a, at: time.Now(), retry: time.Now().Add(time.Hour)}}
		s.shadow.display.mu.Unlock()
	}
	set(&shadowDisplayAsset{DetailPending: true})
	h1, d1 := get()
	ver := s.shadow.ver
	set(&shadowDisplayAsset{Detail: true})
	h2, d2 := get()
	if h1 == h2 || len(d1) != 1 || len(d2) != 1 || d1[0].DisplayDetail || !d2[0].DisplayDetail || s.shadow.ver != ver {
		t.Fatalf("detail readiness: %q %+v -> %q %+v (ver %d -> %d)", h1, d1, h2, d2, ver, s.shadow.ver)
	}
}

func TestShadowLongPollHoldsQuietAnswers(t *testing.T) {
	old := shadowQuietPoll
	shadowQuietPoll = 400 * time.Millisecond
	defer func() { shadowQuietPoll = old }()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowMode=cdj")
	poll := func() (string, time.Duration) {
		t.Helper()
		s.shadow.mu.Lock()
		ver := s.shadow.ver
		s.shadow.mu.Unlock()
		start := time.Now()
		r, err := http.Get(fmt.Sprintf("%s/v1/enginex/shadow?ver=%d", srv.URL, ver))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r.Header.Get("X-Shadow-State"), time.Since(start)
	}
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Playing: true, PositionMS: 1000, Seq: 1}}, time.Now())
	s.shadow.mu.Unlock()
	h1, took := poll()
	if took < 350*time.Millisecond {
		t.Fatalf("a quiet poll answered after %v, want the full hold", took)
	}
	// A peer deck about to turn stale: the hold ends when it does and the header shows it.
	s.shadow.mu.Lock()
	p := s.shadow.peerDecks[2]
	p.at = time.Now().Add(-1850 * time.Millisecond)
	s.shadow.peerDecks[2] = p
	s.shadow.mu.Unlock()
	h2, took := poll()
	if took > 300*time.Millisecond || h2 == h1 || !strings.Contains(h2, ":true:13:true:") {
		t.Fatalf("stale transition: %v %q -> %q", took, h1, h2)
	}
	// A real change still answers at once.
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.shadow.mu.Lock()
		s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "b", Seq: 2}}, time.Now())
		s.shadow.mu.Unlock()
	}()
	if _, took := poll(); took > 300*time.Millisecond {
		t.Fatalf("a bump waited %v", took)
	}
	// CDJ (metadata-only) decks refresh positions at 120 ms.
	s.shadow.mu.Lock()
	s.shadow.metaOnlyPoll = true
	s.shadow.mu.Unlock()
	if d := s.shadow.pollWait(false, time.Now()); d != 120*time.Millisecond {
		t.Fatalf("CDJ poll wait %v", d)
	}
	if d := s.shadow.pollWait(true, time.Now()); d != 120*time.Millisecond {
		t.Fatalf("legacy poll wait %v", d)
	}
}

// An update that took longer than the peer's quickest one carries a playing deck
// on by the extra time; a clock that is simply ahead, or an old sender, does not.
func TestTakePeerCarriesALateUpdateOn(t *testing.T) {
	sh := newShadowState()
	t0 := time.Now()
	take := func(seq int64, pos float64, sent, arrived time.Duration, old bool) float64 {
		t.Helper()
		d := deckState{Deck: 2, Key: "k", Playing: true, PositionMS: pos, Pitch: 50, Seq: seq}
		if !old {
			d.Sent = t0.Add(5*time.Second + sent).UnixMilli() // the peer's clock is 5 s ahead
		}
		sh.takePeer("192.0.2.7", []deckState{d}, t0.Add(arrived))
		if sh.peerDecks[2].Sent != 0 {
			t.Fatal("Sent reached the picture")
		}
		return sh.peerDecks[2].PositionMS
	}
	if got := take(1, 1000, 0, 2*time.Millisecond, false); got != 1000 {
		t.Fatalf("first update moved to %v", got)
	}
	// Sent on time, 100 ms late at +50 %: 150 ms on (less the 1 ms/s allowance).
	if got := take(2, 1750, 500*time.Millisecond, 602*time.Millisecond, false); got < 1898 || got > 1900 {
		t.Fatalf("late update at %v, want ~1900", got)
	}
	if got := take(3, 2500, time.Second, 1002*time.Millisecond, false); got != 2500 {
		t.Fatalf("quick update moved to %v", got)
	}
	if got := take(4, 3250, 0, 1700*time.Millisecond, true); got != 3250 {
		t.Fatalf("an old sender's update moved to %v", got)
	}
	// The peer's clock stepped 2 s: not a 2 s trip.
	if got := take(5, 4000, -time.Second, 1900*time.Millisecond, false); got != 4000 {
		t.Fatalf("stepped clock moved the deck to %v", got)
	}
}

// Two players' clocks: the peer's audio runs -165 ppm against our system clock
// and its pitch is rounded. Carried on at the pitch alone the picture is 100 ms
// off after ten minutes, or steps 12 ms every minute.
func TestTakePeerMeasuresTheDecksSpeed(t *testing.T) {
	sh := newShadowState()
	t0 := time.Now()
	const pitch, skew = 3.12, -165e-6
	speed := (1 + pitch/100) * (1 + skew)
	noise := []float64{0, 0.8, -0.6, 0.3, -0.9, 0.5} // ms: the sender's line
	nudge := 0.0
	truth := func(at time.Duration) float64 { return 1000 + nudge + float64(at.Microseconds())/1000*speed }
	var seq int64
	var p peerDeck
	at := time.Duration(0)
	// The peer's QML posts its line every 5 s; its suite carries that on at the
	// pitch with every ping (500 ms).
	run := func(until time.Duration, off float64, late time.Duration) {
		t.Helper()
		for ; at <= until; at += 500 * time.Millisecond {
			seq++
			post := at.Truncate(5 * time.Second)
			pos := truth(post) + noise[int(post/(5*time.Second))%len(noise)] + float64((at-post).Milliseconds())*(1+pitch/100)
			d := deckState{Deck: 2, Key: "k", Playing: true, Pitch: pitch, PositionMS: pos + off, Seq: seq, Sent: t0.Add(at).UnixMilli()}
			sh.takePeer("192.0.2.7", []deckState{d}, t0.Add(at+late))
			p = sh.peerDecks[2]
		}
	}
	near := func(what string, got, want, tol float64) {
		t.Helper()
		if math.Abs(got-want) > tol {
			t.Fatalf("%s: %v, want %v +-%v", what, got, want, tol)
		}
	}
	run(35500*time.Millisecond, 0, 0)
	if p.skew != 0 {
		t.Fatalf("measured over less than 30 s: %v", p.skew)
	}
	// 20 ms late and 8 ms off: carried on, not measured from.
	run(39*time.Second, 8, 20*time.Millisecond)
	if p.skew != 0 {
		t.Fatalf("measured from late updates: %v", p.skew)
	}
	run(40*time.Second, 0, 0)
	near("skew after 40 s", p.skew, skew, 60e-6)
	v := sh.ver
	run(5*time.Minute, 0, 0)
	near("skew after 5 min", p.skew, skew, 15e-6)
	pos, rate := p.carried(0)
	near("line after 5 min", pos, truth(at-500*time.Millisecond), 2)
	near("rate", rate, speed, 15e-6*speed+1e-6)
	if sh.ver-v > 4 {
		t.Fatalf("%d bumps in 5 min of steady play", sh.ver-v)
	}
	// A jog nudge: a new line, measured anew; the speed found so far stays.
	nudge, v = 40, sh.ver
	was := p.skew
	run(at, 0, 0)
	if sh.ver == v || p.skew != was || !p.refAt.IsZero() {
		t.Fatalf("nudge: ver %d -> %d, skew %v -> %v, ref %v", v, sh.ver, was, p.skew, p.refAt)
	}
	run(10*time.Minute, 0, 0)
	near("skew after 10 min", p.skew, skew, 15e-6)
	last := at - 500*time.Millisecond
	near("anchored line after 10 min", p.anchorPos+float64(t0.Add(last).Sub(p.anchorAt).Microseconds())/1000*p.speed(), truth(last), 3)
	// Another track at another pitch starts with the clock's part of it.
	sh.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "k2", Playing: true, Seq: seq + 1, Sent: t0.Add(at).UnixMilli()}}, t0.Add(at))
	if _, rate := sh.peerDecks[2].carried(0); math.Abs(rate-(1+skew)) > 16e-6 {
		t.Fatalf("next track's rate %v", rate)
	}
}

// The source's pitch range and key lock travel from its QML post to the peer's
// shadow reply; a deck that sent neither (a CDJ, an older sender) has neither.
func TestShadowCarriesRangeAndKeyLock(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1")
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader(fmt.Sprintf(`[{"deck":1,"range":100,"keylock":false,"at":%d}]`, time.Now().UnixMilli()))))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.shadow.mu.Lock()
	out := s.shadow.ownForPeer(time.Now())
	on := true
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Range: 8, Seq: 1}, {Deck: 4, Key: "b", Seq: 1}}, time.Now())
	ver := s.shadow.ver
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Range: 100, Seq: 2}, {Deck: 4, Key: "b", Seq: 2}}, time.Now())
	ver2 := s.shadow.ver
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Range: 100, KeyLock: &on, Seq: 3}, {Deck: 4, Key: "b", Seq: 3}}, time.Now())
	ver3 := s.shadow.ver
	s.shadow.mu.Unlock()
	if ver2 == ver || ver3 == ver2 {
		t.Fatalf("a new range or key lock must wake the QML: ver %d %d %d", ver, ver2, ver3)
	}
	if len(out) != 1 || out[0].Range != 100 || out[0].KeyLock == nil || *out[0].KeyLock {
		t.Fatalf("own deck for the peer: %+v", out)
	}
	r, err := http.Get(srv.URL + "/v1/enginex/shadow")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var v struct{ Decks []deckState }
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil || len(v.Decks) != 2 {
		t.Fatalf("%v %+v", err, v.Decks)
	}
	for _, d := range v.Decks {
		if d.Deck == 2 && (d.Range != 100 || d.KeyLock == nil || !*d.KeyLock) || d.Deck == 4 && (d.Range != 0 || d.KeyLock != nil) {
			t.Fatalf("shadow reply: %+v", d)
		}
	}
}

// The display-only shadow reply carries KeyName and Lead from the source deck,
// and wakes the QML when either changes.
func TestShadowDisplayCarriesKeyNameAndLead(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowLegacyEngine=0")
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader(fmt.Sprintf(`[{"deck":1,"keyName":"8B","lead":true,"at":%d}]`, time.Now().UnixMilli()))))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.shadow.mu.Lock()
	out := s.shadow.ownForPeer(time.Now())
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", KeyName: "-", Seq: 1}}, time.Now())
	ver1 := s.shadow.ver
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", KeyName: "9B", Seq: 2}}, time.Now())
	ver2 := s.shadow.ver
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", KeyName: "9B", Lead: new(true), Seq: 3}}, time.Now())
	ver3 := s.shadow.ver
	s.shadow.mu.Unlock()
	if len(out) != 1 || out[0].KeyName != "8B" || out[0].Lead == nil || !*out[0].Lead {
		t.Fatalf("own deck for peer: %+v", out)
	}
	if ver2 == ver1 || ver3 == ver2 {
		t.Fatalf("keyName or lead change must bump ver: %d %d %d", ver1, ver2, ver3)
	}
	r, err := http.Get(srv.URL + "/v1/enginex/shadow")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var v struct{ Decks []deckState }
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil || len(v.Decks) != 1 || v.Decks[0].KeyName != "9B" || v.Decks[0].Lead == nil || !*v.Decks[0].Lead {
		t.Fatalf("shadow reply: %v %+v", err, v.Decks)
	}
}

// A shadow deck left holding master is told to the peer: the change wakes its
// QML, and its shadow reply names the deck.
func TestShadowCarriesShadowMaster(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1")
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader(fmt.Sprintf(`[{"deck":1,"shadowMaster":2,"at":%d}]`, time.Now().UnixMilli()))))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.shadow.mu.Lock()
	out := s.shadow.ownForPeer(time.Now())
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Seq: 1}}, time.Now())
	ver := s.shadow.ver
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, ShadowMaster: 1, Seq: 2}}, time.Now())
	ver2 := s.shadow.ver
	s.shadow.mu.Unlock()
	if len(out) != 1 || out[0].ShadowMaster != 2 || ver2 == ver {
		t.Fatalf("own deck for the peer: %+v, ver %d -> %d", out, ver, ver2)
	}
	r, err := http.Get(srv.URL + "/v1/enginex/shadow")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var v struct{ Decks []deckState }
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil || len(v.Decks) != 1 || v.Decks[0].ShadowMaster != 1 {
		t.Fatalf("shadow reply: %v %+v", err, v.Decks)
	}
}

// The key the source's header writes travels from its QML post to the peer's
// shadow reply, and a change of it wakes the peer's QML.
func TestShadowCarriesKeyName(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1")
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader(fmt.Sprintf(`[{"deck":1,"keyName":"-","at":%d}]`, time.Now().UnixMilli()))))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.shadow.mu.Lock()
	out := s.shadow.ownForPeer(time.Now())
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", KeyName: "-", Seq: 1}}, time.Now())
	ver := s.shadow.ver
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", KeyName: "8B", Seq: 2}}, time.Now())
	ver2 := s.shadow.ver
	s.shadow.mu.Unlock()
	if len(out) != 1 || out[0].KeyName != "-" {
		t.Fatalf("own deck for the peer: %+v", out)
	}
	if ver2 == ver {
		t.Fatalf("a new key must wake the QML: ver %d %d", ver, ver2)
	}
	r, err := http.Get(srv.URL + "/v1/enginex/shadow")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var v struct{ Decks []deckState }
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil || len(v.Decks) != 1 || v.Decks[0].KeyName != "8B" {
		t.Fatalf("shadow reply: %v %+v", err, v.Decks)
	}
}

// Two standalone players share master over Engine Link, so the shadow reply
// clears Master (the copy must not take it). The header's LEAD has its own flag,
// the source deck's DeckIsMaster, and a change of it wakes the QML.
func TestShadowMasterLeadJSONRoundTrip(t *testing.T) {
	for _, input := range []string{
		`{"deck":2,"master":true,"lead":false}`,
		`{"deck":2,"master":false,"lead":true}`,
		`{"deck":2}`,
	} {
		t.Run(input, func(t *testing.T) {
			var first, second deckState
			if err := json.Unmarshal([]byte(input), &first); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			var want, got map[string]json.RawMessage
			if err := json.Unmarshal([]byte(input), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"master", "lead"} {
				if string(got[field]) != string(want[field]) {
					t.Fatalf("%s: want %s, got %s in %s", field, want[field], got[field], encoded)
				}
			}
			if err := json.Unmarshal(encoded, &second); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if !samePeerPicture(peerDeck{deckState: first, at: now}, second, now) {
				t.Fatal("the same decoded values must not wake the QML again")
			}
		})
	}
}

func TestShadowReplyTellsLead(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowLegacyEngine=1")
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Master: new(true), Lead: new(false), Seq: 1}, {Deck: 4, Key: "b", Master: new(true), Lead: new(false), Seq: 1}}, time.Now())
	ver := s.shadow.ver
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Master: new(true), Lead: new(true), Seq: 2}, {Deck: 4, Key: "b", Master: new(true), Lead: new(false), Seq: 2}}, time.Now())
	woke := s.shadow.ver != ver
	s.shadow.mu.Unlock()
	if !woke {
		t.Fatal("LEAD on the source must wake the QML")
	}
	r, err := http.Get(srv.URL + "/v1/enginex/shadow")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var v struct{ Decks []deckState }
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil || len(v.Decks) != 2 {
		t.Fatalf("%v %+v", err, v.Decks)
	}
	for _, d := range v.Decks {
		if d.Master == nil || *d.Master || (d.Lead != nil && *d.Lead) != (d.Deck == 2) {
			t.Fatalf("shadow reply: %+v", d)
		}
	}
}

// The source's loop, key shift, Stems icon and slip line reach the shadow deck's
// header, and a change of any of them wakes the QML.
func TestShadowReplyTellsDeckDisplayState(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowLegacyEngine=1")
	steps := []deckState{
		{Deck: 2, Key: "a"},
		{Deck: 2, Key: "a", Loop: true},
		{Deck: 2, Key: "a", Loop: true, KeyOffset: -2},
		{Deck: 2, Key: "a", Loop: true, KeyOffset: -2, Stems: true},
		{Deck: 2, Key: "a", Loop: true, KeyOffset: -2, Stems: true, Slip: 44100},
		{Deck: 2, Key: "a", Loop: true, KeyOffset: -2, Stems: true, Slip: 55125},
	}
	s.shadow.mu.Lock()
	for i, d := range steps {
		ver := s.shadow.ver
		d.Seq = int64(i + 1)
		s.shadow.takePeer("192.0.2.7", []deckState{d}, time.Now())
		if i > 0 && s.shadow.ver == ver {
			s.shadow.mu.Unlock()
			t.Fatalf("step %d did not wake the QML: %+v", i, d)
		}
	}
	s.shadow.mu.Unlock()
	r, err := http.Get(srv.URL + "/v1/enginex/shadow")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var v struct{ Decks []deckState }
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil || len(v.Decks) != 1 {
		t.Fatalf("%v %+v", err, v.Decks)
	}
	if d := v.Decks[0]; !d.Loop || d.KeyOffset != -2 || !d.Stems || d.Slip != 55125 {
		t.Fatalf("shadow reply: %+v", d)
	}
}

func TestShadowIdleReplyKeepsActiveSnapshots(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	if err := os.WriteFile(fourDeckRolePath, []byte("13"), 0600); err != nil {
		t.Fatal(err)
	}
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowLegacyEngine=1")
	sh := s.shadow
	put := func(d deckState) uint64 {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		sh.takePeer("192.0.2.7", []deckState{d}, time.Now())
		return sh.ver
	}
	get := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.shadowGet(w, httptest.NewRequest("GET", "/v1/enginex/shadow"+query, nil))
		return w
	}
	decks := func(w *httptest.ResponseRecorder) []deckState {
		t.Helper()
		var v struct {
			DisplayOnly *bool       `json:"displayOnly"`
			Decks       []deckState `json:"decks"`
		}
		if err := json.NewDecoder(w.Body).Decode(&v); err != nil || v.DisplayOnly == nil || *v.DisplayOnly || w.Header().Get("X-Shadow-Unchanged") != "" {
			t.Fatalf("expected a full Engine response: %+v, %v", v, err)
		}
		return v.Decks
	}

	d := deckState{Deck: 2, Link: "peer", Seq: 1}
	ver := put(d)
	query := fmt.Sprintf("?ver=%d", ver)
	if w := get(query); len(decks(w)) != 1 || w.Header().Get("X-Shadow-Idle") != "1" {
		t.Fatal("an old client must receive a complete empty snapshot")
	}
	if w := get(query + "&idle=1"); strings.TrimSpace(w.Body.String()) != `{"unchanged":true}` || w.Header().Get("X-Shadow-Unchanged") != "1" {
		t.Fatalf("short response must contain only the unchanged marker: %s", w.Body.String())
	}
	d.Key, d.Seq = "a", 2
	sh.mu.Lock()
	sh.busy["peer:a"] = true
	sh.mu.Unlock()
	ver = put(d)
	query = fmt.Sprintf("?ver=%d&idle=1", ver)
	if v := decks(get(query)); len(v) != 1 || !v[0].Loading {
		t.Fatalf("copy loading must reach QML: %+v", v)
	}
	sh.mu.Lock()
	sh.files["peer:a"] = "/copy.mp3"
	sh.mu.Unlock()
	if v := decks(get(query)); len(v) != 1 || v[0].Path != "/copy.mp3" || v[0].Loading {
		t.Fatalf("copy readiness without a version change must reach QML: %+v", v)
	}
	d.Playing, d.Seq = true, 3
	ver = put(d)
	query = fmt.Sprintf("?ver=%d&idle=1", ver)
	if v := decks(get(query)); len(v) != 1 || !v[0].Playing || v[0].Seq != 3 {
		t.Fatalf("active play and drift state must reach QML: %+v", v)
	}
}

// #201: Shadow Deck Mode switches live. A long poll held in All CDJ Mode answers
// as soon as the mode changes, and the answer (displayOnly, headers) is in the
// new mode, not the one read before the wait.
func TestShadowLongPollFollowsALiveModeSwitch(t *testing.T) {
	old := shadowQuietPoll
	shadowQuietPoll = 3 * time.Second
	defer func() { shadowQuietPoll = old }()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowMode=cdj")
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Playing: true, PositionMS: 1000, Seq: 1}}, time.Now())
	ver := s.shadow.ver
	s.shadow.mu.Unlock()
	posted := make(chan struct{})
	go func() {
		defer close(posted)
		time.Sleep(100 * time.Millisecond)
		uiPost(t, srv.URL, "shadowMode=engine")
	}()
	// The poll answers while the POST handler is still reading fourDeckRolePath:
	// let it finish before the deferred restores above run.
	defer func() { <-posted }()
	start := time.Now()
	r, err := http.Get(fmt.Sprintf("%s/v1/enginex/shadow?ver=%d", srv.URL, ver))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	took := time.Since(start)
	var body struct {
		DisplayOnly bool `json:"displayOnly"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if took > 1500*time.Millisecond || body.DisplayOnly || r.Header.Get("X-Shadow-State") != "" {
		t.Fatalf("mode switch during a held poll: answered after %v, displayOnly=%v, X-Shadow-State=%q", took, body.DisplayOnly, r.Header.Get("X-Shadow-State"))
	}
}
