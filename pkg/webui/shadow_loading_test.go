package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// loadingPeer is the other player's deck 1. Each audio key answers as its
// entry says; an entry is used once per request, the last one from then on.
type loadingPeer struct {
	mu      sync.Mutex
	audio   map[string][]loadingAnswer
	calls   map[string]int
	display []string // the queries of /peer/decks/1/display
	asset   []byte
}

type loadingAnswer struct {
	status   int           // not 0: answer with it
	body     string        // else the audio
	noLength bool          // without Content-Length
	half     chan struct{} // send half, then wait for it
	cut      bool          // send half and drop the connection
	hang     bool          // never answer
}

func (p *loadingPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if r.URL.Path == "/peer/decks/1/display" {
		p.mu.Lock()
		p.display = append(p.display, r.URL.RawQuery)
		asset := p.asset
		p.mu.Unlock()
		if asset == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(asset)
		return
	}
	if r.URL.Path != "/peer/decks/1/audio" {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	n := p.calls[key]
	p.calls[key]++
	answers := p.audio[key]
	p.mu.Unlock()
	if len(answers) == 0 {
		http.Error(w, "not loaded", http.StatusConflict)
		return
	}
	a := answers[min(n, len(answers)-1)]
	switch {
	case a.hang:
		<-r.Context().Done()
	case a.status != 0:
		http.Error(w, "no", a.status)
	default:
		if !a.noLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(a.body)))
		}
		if a.half == nil && !a.cut {
			w.Write([]byte(a.body))
			return
		}
		w.Write([]byte(a.body[:len(a.body)/2]))
		w.(http.Flusher).Flush()
		if a.cut {
			panic(http.ErrAbortHandler) // the connection drops in the middle
		}
		<-a.half
		w.Write([]byte(a.body[len(a.body)/2:]))
	}
}

func (p *loadingPeer) called(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[key]
}

func loadingServer(t *testing.T, peer *loadingPeer) (*Server, func(key string), func() deckState) {
	t.Helper()
	peer.calls = map[string]int{}
	server := httptest.NewServer(peer)
	t.Cleanup(server.Close)
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	t.Cleanup(func() { s.Close() })
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), t.TempDir()
	u, _ := url.Parse(server.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())
	source := func(key string) {
		s.shadow.mu.Lock()
		defer s.shadow.mu.Unlock()
		// A peer cannot make this player show progress or an error of its choosing.
		s.shadow.peerDecks[1] = peerDeck{deckState: deckState{Deck: 1, Key: key, Ext: ".flac", Name: key + ".flac", Title: "Title " + key, Progress: 0.9, Error: "timeout"}, ip: "127.0.0.1", at: time.Now()}
	}
	deck := func() deckState {
		t.Helper()
		out := s.shadowComposeLegacy("24", time.Now())
		if len(out) != 1 || out[0].Deck != 1 {
			t.Fatalf("compose: %+v", out)
		}
		return out[0]
	}
	return s, source, deck
}

// until composes until ok, at most 3 s.
func until(t *testing.T, deck func() deckState, what string, ok func(deckState) bool) deckState {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		d := deck()
		if ok(d) {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: never, the deck is %+v", what, d)
		}
	}
}

func TestShadowCopyShown(t *testing.T) {
	sh := newShadowState()
	set := func(done, total int64, err string) {
		c := &shadowCopy{err: err}
		c.done.Store(done)
		c.total.Store(total)
		sh.copies["k"] = c
	}
	if p, e := sh.copyShown("k"); p != 0 || e != "" {
		t.Fatalf("no copy: %v %q", p, e)
	}
	for _, c := range []struct {
		done, total int64
		err         string
		progress    float64
		shown       string
	}{
		{0, 0, "", 0, ""},
		{0, 1000, "", 0, ""},
		{250, 1000, "", 0.25, ""},
		{1, 1000000, "", 0.01, ""}, // bytes arrived: a ring, not the turning one
		{333, 1000, "", 0.33, ""},
		{1000, 1000, "", 1, ""},
		{1200, 1000, "", 1, ""},            // a source that sends more than it said
		{500, 0, "", 0, ""},                // no Content-Length
		{0, 0, "timeout", 0, "timeout"},    // failed, waiting for the next attempt
		{0, 1000, "timeout", 0, "timeout"}, // the next attempt has no bytes yet
		{250, 1000, "timeout", 0.25, ""},   // it has: the failure is over
	} {
		set(c.done, c.total, c.err)
		if p, e := sh.copyShown("k"); p != c.progress || e != c.shown {
			t.Errorf("%d of %d after %q: %v %q, want %v %q", c.done, c.total, c.err, p, e, c.progress, c.shown)
		}
	}
}

// While the copy runs the shadow deck is loading, tells how far the copy is
// and names the source's display data. Another track starts again from nothing.
func TestShadowLegacyCopyProgress(t *testing.T) {
	half := map[string]chan struct{}{"A": make(chan struct{}), "B": make(chan struct{}), "N": make(chan struct{})}
	peer := &loadingPeer{audio: map[string][]loadingAnswer{
		"A": {{body: "AUDIO-AAAAAAAAAAAAAA", half: half["A"]}},
		"B": {{body: "B", half: half["B"]}}, // not a byte before it is released (an empty body would be a finished copy)
		"N": {{body: "AUDIO-NNNNNNNNNNNNNN", half: half["N"], noLength: true}},
	}}
	s, source, deck := loadingServer(t, peer)
	defer func() {
		for _, c := range half {
			close(c)
		}
	}()
	source("A")
	d := until(t, deck, "half of A", func(d deckState) bool { return d.Progress != 0 })
	s.shadow.mu.Lock()
	want := displayTrackKey(s.shadow.peerDecks[1])
	s.shadow.mu.Unlock()
	if d.Progress != 0.5 || !d.Loading || d.Path != "" || d.Error != "" || d.Key != want || d.Title != "Title A" || d.ID != "A" {
		t.Fatalf("half of A: %+v", d)
	}
	source("B")
	if d := deck(); d.Progress != 0 || d.Error != "" || !d.Loading || d.Path != "" || d.ID != "B" || d.Key == want {
		t.Fatalf("B keeps A's progress: %+v", d)
	}
	source("N")
	until(t, deck, "N's first bytes", func(deckState) bool {
		s.shadow.mu.Lock()
		defer s.shadow.mu.Unlock()
		return s.shadow.copies["peer:N"].done.Load() > 0
	})
	if d := deck(); d.Progress != 0 || d.Error != "" || !d.Loading {
		t.Fatalf("a copy of unknown length shows a share: %+v", d)
	}
	half["N"] <- struct{}{}
	d = until(t, deck, "N's file", func(d deckState) bool { return d.Path != "" })
	if b, _ := os.ReadFile(d.Path); string(b) != "AUDIO-NNNNNNNNNNNNNN" || d.Loading || d.Progress != 0 || d.Error != "" {
		t.Fatalf("N: %+v with %q", d, b)
	}
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	if s.shadow.copies["peer:N"] != nil {
		t.Fatal("a finished copy is still counted")
	}
}

// A copy that fails says why, is tried again later and later, never 8 times a
// second, and shows no failure once the next attempt delivers.
func TestShadowLegacyCopyFailsAndRetries(t *testing.T) {
	peer := &loadingPeer{audio: map[string][]loadingAnswer{
		"gone": {{status: 404}, {status: 404}, {status: 404}, {body: "AUDIO-GONE"}},
		"cut":  {{body: "AUDIO-CUT-AUDIO-CUT", cut: true}, {body: "AUDIO-CUT-AUDIO-CUT"}},
		"slow": {{hang: true}},
		"ok":   {{body: "AUDIO-OK"}},
	}}
	s, source, deck := loadingServer(t, peer)
	s.shadow.copyRetry, s.shadow.copyTimeout = 40*time.Millisecond, 150*time.Millisecond
	failing := func(d deckState) {
		t.Helper()
		if d.Path != "" || !d.Loading || d.Progress != 0 {
			t.Fatalf("a failed copy stays loading, with no file and no share: %+v", d)
		}
	}

	source("gone")
	start := time.Now()
	failing(until(t, deck, "404", func(d deckState) bool { return d.Error == "unavailable" }))
	until(t, deck, "the third attempt", func(d deckState) bool { failing(d); return peer.called("gone") >= 3 })
	// The second attempt waits 40 ms, the third 80 ms more.
	if wait := time.Since(start); wait < 120*time.Millisecond {
		t.Fatalf("three attempts in %v: no backoff", wait)
	}
	d := until(t, deck, "the track once the source has it", func(d deckState) bool { return d.Path != "" })
	if b, _ := os.ReadFile(d.Path); string(b) != "AUDIO-GONE" || d.Error != "" || d.Loading || peer.called("gone") != 4 {
		t.Fatalf("after the failures: %+v with %q, %d attempts", d, b, peer.called("gone"))
	}

	source("cut")
	if d := deck(); d.Error != "" {
		t.Fatalf("another track shows the last one's failure: %+v", d)
	}
	saw := false
	d = until(t, deck, "the copy after the dropped one", func(d deckState) bool {
		saw = saw || d.Error == "interrupted"
		return d.Path != ""
	})
	if b, _ := os.ReadFile(d.Path); !saw || string(b) != "AUDIO-CUT-AUDIO-CUT" || d.Error != "" || peer.called("cut") != 2 {
		t.Fatalf("dropped copy: seen %v, %+v with %q, %d attempts", saw, d, b, peer.called("cut"))
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(d.Path), ".fetch-*")); len(left) != 0 {
		t.Fatalf("half a copy left behind: %v", left)
	}

	source("slow")
	failing(until(t, deck, "timeout", func(d deckState) bool { return d.Error == "timeout" }))
	source("ok")
	if d := deck(); d.Error != "" || !d.Loading {
		t.Fatalf("another track shows the timeout: %+v", d)
	}
	until(t, deck, "ok", func(d deckState) bool { return d.Path != "" && d.Error == "" })
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	if c := s.shadow.copies["peer:slow"]; c == nil || c.fails == 0 || c.done.Load() != 0 {
		t.Fatalf("the slow copy's state: %+v", c)
	}
}

// Engine mode reads the cover and key of the source's track for the deck it
// covers while loading, and asks the source for no detail waveform.
func TestShadowLegacyCoverReadsDisplayHead(t *testing.T) {
	a := testDisplayAsset()
	a.Key, a.KeyName, a.Artist = "A", "8A", "Source artist"
	a.Artwork = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
	if !validDisplayArtwork(a.Artwork) {
		t.Fatal("fixture: artwork")
	}
	asset, _ := json.Marshal(a)
	hold := make(chan struct{})
	defer close(hold)
	peer := &loadingPeer{asset: asset, audio: map[string][]loadingAnswer{"A": {{body: "AUDIO-A", half: hold}}}}
	s, source, _ := loadingServer(t, peer)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("24"), 0600)
	if _, v := uiPost(t, srv.URL, "fourDeckWaveform=1&shadowMode=engine"); v.ShadowMode != "engine" {
		t.Fatal("Engine mode not set")
	}
	source("A")
	var state struct{ Decks []deckState }
	resp, err := http.Get(srv.URL + "/v1/sc5000/shadow")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&state)
	resp.Body.Close()
	if len(state.Decks) != 1 || !state.Decks[0].Loading || state.Decks[0].Key == "" || state.Decks[0].Title != "Title A" {
		t.Fatalf("the deck while its copy runs: %+v", state.Decks)
	}
	q := fmt.Sprintf("?deck=1&key=%s", state.Decks[0].Key)
	var meta struct {
		Title, Artist, KeyName, ArtworkURL string
		DetailPending                      bool
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		resp, err := http.Get(srv.URL + "/v1/sc5000/shadow/display" + q)
		if err != nil {
			t.Fatal(err)
		}
		ok := resp.StatusCode == 200
		json.NewDecoder(resp.Body).Decode(&meta)
		resp.Body.Close()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cover's data never arrived")
		}
	}
	if meta.Title != "Source title" || meta.Artist != "Source artist" || meta.KeyName != "8A" || meta.ArtworkURL == "" || meta.DetailPending {
		t.Fatalf("cover data: %+v", meta)
	}
	for _, path := range []string{"/v1/sc5000/shadow/artwork" + q, "/v1/sc5000/shadow/waveform.png" + q + "&overview=1"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := http.Get(srv.URL + "/v1/sc5000/shadow/display?deck=1&key=other"); resp.StatusCode != 404 {
		t.Fatal("another track's key accepted")
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.display) != 1 || !strings.HasSuffix(peer.display[0], "&detail=0") {
		t.Fatalf("asked the source: %v", peer.display)
	}
	s.shadow.display.mu.Lock()
	defer s.shadow.display.mu.Unlock()
	for id := range s.shadow.display.entries {
		if !strings.HasSuffix(id, ":head") {
			t.Fatalf("the head is kept as the full display data: %s", id)
		}
	}
}

// Display mode: while the source computes the detail waveform, the deck tells
// how far that is, and the QML hears it through the header digest.
func TestShadowDisplayDetailProgress(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&shadowMode=cdj")
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Key: "a", Seq: 1, Progress: 0.9}}, time.Now())
	id := displayPeerID(s.shadow.peerDecks[2])
	s.shadow.mu.Unlock()
	get := func(a *shadowDisplayAsset) (string, deckState) {
		t.Helper()
		a.Key = "a"
		s.shadow.display.mu.Lock()
		s.shadow.display.entries = map[string]*shadowDisplayEntry{id: {asset: a, at: time.Now(), retry: time.Now().Add(time.Hour)}}
		s.shadow.display.mu.Unlock()
		r, err := http.Get(srv.URL + "/v1/sc5000/shadow")
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ Decks []deckState }
		json.NewDecoder(r.Body).Decode(&v)
		r.Body.Close()
		if len(v.Decks) != 1 {
			t.Fatalf("decks: %+v", v.Decks)
		}
		return r.Header.Get("X-Shadow-State"), v.Decks[0]
	}
	h0, d0 := get(&shadowDisplayAsset{DetailPending: true})
	h1, d1 := get(&shadowDisplayAsset{DetailPending: true, DetailProgress: 0.4})
	h2, d2 := get(&shadowDisplayAsset{DetailPending: true, DetailProgress: 0.4})
	h3, d3 := get(&shadowDisplayAsset{Detail: true, DetailProgress: 0.4})
	if d0.Progress != 0 || d1.Progress != 0.4 || d2.Progress != 0.4 || d3.Progress != 0 || !d3.DisplayDetail {
		t.Fatalf("progress: %v %v %v %v", d0.Progress, d1.Progress, d2.Progress, d3.Progress)
	}
	if h0 == h1 || h1 != h2 || h2 == h3 {
		t.Fatalf("digest: %q %q %q %q", h0, h1, h2, h3)
	}
}

// The source: an asset read for Engine mode's cover queues no detail waveform
// and is not kept; a pending one tells how far the worker has read.
func TestShadowSourceHeadAndProgress(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	c := &s.shadow.sourceDisplay
	if _, err := s.sourceDisplayAsset(t.Context(), filepath.Join(t.TempDir(), "none.flac"), "k", false); err == nil {
		t.Fatal("a file in no library has display data")
	}
	if len(c.entries) != 0 || c.running {
		t.Fatalf("the head was queued: %+v", c.entries)
	}
	e := &shadowSourceEntry{asset: &shadowDisplayAsset{Key: "k", Title: "T", DetailPending: true}, path: "/p", key: "k", req: "k"}
	c.entries["k"] = e
	if a, err := s.sourceDisplayAsset(t.Context(), "/p", "k", false); err != nil || a != e.asset {
		t.Fatalf("before the worker starts: %+v %v", a, err)
	}
	(&shadowSourceTicker{job: e}).Progress(300)
	e.size.Store(1000)
	a, err := s.sourceDisplayAsset(t.Context(), "/p", "k", true)
	if err != nil || a.DetailProgress != 0.3 || a.Title != "T" || !a.DetailPending || e.asset.DetailProgress != 0 {
		t.Fatalf("pending: %+v %v, cached %v", a, err, e.asset.DetailProgress)
	}
	done := *e.asset
	done.DetailPending = false
	e.asset = &done
	if a, _ := s.sourceDisplayAsset(t.Context(), "/p", "k", true); a.DetailProgress != 0 {
		t.Fatalf("finished: %+v", a)
	}
}
