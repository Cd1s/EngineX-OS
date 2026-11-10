package webui

import (
	"encoding/json"
	"fmt"
	"io"
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
	base := deckState{Deck: 2, Key: "k", Title: "T", Playing: true, PositionMS: 1000, Pitch: 0, BPM: 120, CuePos: &cue, Cues: []shadowCue{{I: 1, P: 4800, N: "A", C: "#fff"}}, Link: "L"}
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
	steady.PositionMS, steady.Beat, steady.CurrentBPM, steady.Dbg = 1500, 2.5, 120.01, "x"
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
	steady.PositionMS = 2018 // +18 ms at 1 s: sampling noise
	if take(steady, time.Second) != v {
		t.Fatal("18 ms drift bumped")
	}
	steady.PositionMS = 2536 // +36 ms at 1.5 s: each step was only 18 ms
	if v2 := take(steady, 1500*time.Millisecond); v2 == v {
		t.Fatal("accumulated drift never woke the QML")
	} else {
		v = v2
	}
	if p := sh.peerDecks[2]; p.anchorPos != 2536 || p.anchorAt != t0.Add(1500*time.Millisecond) {
		t.Fatalf("re-anchored: %+v", p)
	}
	for name, edit := range map[string]func(*deckState){
		"pause":  func(d *deckState) { d.Playing = false },
		"pitch":  func(d *deckState) { d.Pitch = 0.5 },
		"title":  func(d *deckState) { d.Title = "U" },
		"cue":    func(d *deckState) { d.Cues = []shadowCue{{I: 1, P: 4801, N: "A", C: "#fff"}} },
		"cuePos": func(d *deckState) { x := 1.0; d.CuePos = &x },
		"jump":   func(d *deckState) { d.Jumps++ },
		"master": func(d *deckState) { d.Master = true },
		"track":  func(d *deckState) { d.Key = "k2" },
	} {
		d := steady
		d.PositionMS = 2536 + 500 // on the line at 2 s
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
		s.decksAlive(w, httptest.NewRequest("POST", "/v1/sc5000/decks/alive", nil))
		return w.Code
	}
	if c := ping(); c != http.StatusConflict {
		t.Fatalf("ping before any post: %d, want 409 (ask for a full post)", c)
	}
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(fmt.Sprintf(`[{"deck":1,"playing":true,"positionMs":5000,"pitch":10,"bpm":120,"beat":1,"master":true,"at":%d}]`, time.Now().UnixMilli()))))
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
	header := func() string {
		t.Helper()
		r, err := http.Get(srv.URL + "/v1/sc5000/shadow")
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ Decks []deckState }
		json.NewDecoder(r.Body).Decode(&v)
		r.Body.Close()
		return r.Header.Get("X-Shadow-State")
	}
	now := time.Now()
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Playing: true, PositionMS: 1000, Seq: 1}}, now)
	s.shadow.mu.Unlock()
	h1 := header()
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Playing: true, PositionMS: 1300, Seq: 2}}, now.Add(300*time.Millisecond))
	s.shadow.mu.Unlock()
	if h2 := header(); h2 != h1 || h1 == "" {
		t.Fatalf("heartbeat changed the header: %q -> %q", h1, h2)
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
		r, err := http.Get(srv.URL + "/v1/sc5000/shadow")
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
		r, err := http.Get(fmt.Sprintf("%s/v1/sc5000/shadow?ver=%d", srv.URL, ver))
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
