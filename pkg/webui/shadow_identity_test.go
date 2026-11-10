package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// identityPeer is the other player's /peer/decks/1/audio: every key has its own
// audio, a key in hold answers only once released, a key in gone answers 409.
type identityPeer struct {
	mu    sync.Mutex
	audio map[string]string
	hold  map[string]chan struct{}
	gone  map[string]bool
	calls map[string]int
}

func (p *identityPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/peer/decks/1/audio" {
		http.NotFound(w, r) // no analysis: the copy is analysed here
		return
	}
	key := r.URL.Query().Get("key")
	p.mu.Lock()
	p.calls[key]++
	body, hold, gone := p.audio[key], p.hold[key], p.gone[key]
	p.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if gone || body == "" {
		http.Error(w, "not loaded", http.StatusConflict)
		return
	}
	w.Write([]byte(body))
}

func (p *identityPeer) called(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[key]
}

// The shadow deck's file follows the source's key: two tracks with one file
// name stay apart, a copy that arrives late does not replace the newer track,
// A-B-A ends on A, and a copy that fails is asked for again, never swapped for
// another track's file.
func TestShadowLegacyFileFollowsSourceKey(t *testing.T) {
	peer := &identityPeer{
		audio: map[string]string{"A": "AUDIO-A", "B": "AUDIO-B"},
		hold:  map[string]chan struct{}{"A": make(chan struct{})},
		gone:  map[string]bool{"C": true},
		calls: map[string]int{},
	}
	server := httptest.NewServer(peer)
	defer server.Close()
	dir := t.TempDir()
	s := New(Config{UIDir: filepath.Join(dir, "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), t.TempDir()
	u, _ := url.Parse(server.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())

	source := func(key string) {
		s.shadow.mu.Lock()
		defer s.shadow.mu.Unlock()
		s.shadow.peerDecks[1] = peerDeck{deckState: deckState{Deck: 1, Key: key, Ext: ".flac", Name: "same.flac", Title: "Same", DurationMS: 1000}, ip: "127.0.0.1", at: time.Now()}
	}
	deck := func() deckState {
		t.Helper()
		out := s.shadowComposeLegacy("24", time.Now())
		if len(out) != 1 || out[0].Deck != 1 {
			t.Fatalf("compose: %+v", out)
		}
		if out[0].Key != "" || out[0].Net != "" || out[0].Title != "Same" || out[0].DurationMS != 1000 {
			t.Fatalf("what the QML compares: %+v", out[0])
		}
		return out[0]
	}
	// ready waits for the copy of the track the source has now.
	ready := func(key, audio string) string {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			d := deck()
			if d.ID != key {
				t.Fatalf("source has %s, shadow deck told %q", key, d.ID)
			}
			if d.Path != "" {
				if got, _ := os.ReadFile(d.Path); string(got) != audio || filepath.Base(d.Path) != "same.flac" || d.Loading {
					t.Fatalf("key %s got %s with %q", key, d.Path, got)
				}
				return d.Path
			}
			if !d.Loading {
				t.Fatalf("no file and not loading: %+v", d)
			}
			if time.Now().After(deadline) {
				t.Fatalf("key %s was never copied", key)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	copied := func(key string) string {
		s.shadow.mu.Lock()
		defer s.shadow.mu.Unlock()
		return s.shadow.files["peer:"+key]
	}

	source("A")
	if d := deck(); d.Path != "" || !d.Loading || d.ID != "A" {
		t.Fatalf("A is still on its way: %+v", d)
	}
	source("B") // changed before A's copy arrived; same file name
	pathB := ready("B", "AUDIO-B")
	close(peer.hold["A"]) // A's copy arrives after B's
	for deadline := time.Now().Add(3 * time.Second); copied("A") == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("A's late copy never finished")
		}
	}
	if p := ready("B", "AUDIO-B"); p != pathB {
		t.Fatalf("A's late copy moved the deck from %s to %s", pathB, p)
	}
	source("A") // A-B-A
	pathA := ready("A", "AUDIO-A")
	if pathA == pathB {
		t.Fatalf("two tracks named same.flac share %s", pathA)
	}
	if peer.called("A") != 1 || peer.called("B") != 1 {
		t.Fatalf("copies fetched again: A %d, B %d", peer.called("A"), peer.called("B"))
	}

	source("C") // the peer refuses it
	for deadline := time.Now().Add(3 * time.Second); peer.called("C") < 2; time.Sleep(10 * time.Millisecond) {
		if d := deck(); d.Path != "" || !d.Loading || d.ID != "C" {
			t.Fatalf("a failed copy must stay loading, without a file: %+v", d)
		}
		if time.Now().After(deadline) {
			t.Fatal("a failed copy was not asked for again")
		}
	}
	if d := deck(); d.Path != "" || d.ID != "C" {
		t.Fatalf("after the failures: %+v", d)
	}
}

// A file of this player's own is named by its path. The peer's key shows up in
// the middle of the track (its Engine cached the file) and must not look like
// another track, or the shadow deck would load again while it plays.
func TestShadowLegacyOwnFileIdentityIsStable(t *testing.T) {
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), t.TempDir()
	local := filepath.Join(s.shadow.mediaRoot, "USB", "Contents", "track.flac")
	other := filepath.Join(s.shadow.mediaRoot, "USB", "Contents", "other.flac")
	for _, f := range []string{local, other} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	compose := func(d deckState) deckState {
		t.Helper()
		d.Deck = 1
		s.shadow.mu.Lock()
		s.shadow.peerDecks[1] = peerDeck{deckState: d, ip: "192.0.2.7", at: time.Now()}
		s.shadow.mu.Unlock()
		out := s.shadowComposeLegacy("24", time.Now())
		if len(out) != 1 {
			t.Fatalf("compose: %+v", out)
		}
		return out[0]
	}
	first := compose(deckState{Net: "net://uuid/USB (USB 1)/Contents/track.flac"})
	if first.Path != local || first.ID == "" || first.Loading {
		t.Fatalf("own file: %+v", first)
	}
	cached := compose(deckState{Net: "net://uuid/USB (USB 1)/Contents/track.flac", Key: "abc-1", Ext: ".flac", Name: "uuid_track.flac"})
	if cached.Path != local || cached.ID != first.ID {
		t.Fatalf("the peer's cache key changed the identity: %+v, was %q", cached, first.ID)
	}
	next := compose(deckState{Net: "net://uuid/USB (USB 1)/Contents/other.flac"})
	if next.Path != other || next.ID == first.ID {
		t.Fatalf("another track: %+v", next)
	}
	if empty := compose(deckState{}); empty.Path != "" || empty.ID != "" || empty.Loading {
		t.Fatalf("nothing loaded: %+v", empty)
	}
}

// The same path on both players' drives is two libraries' track, each with its
// own analysis (waveform peaks, grid): the shadow deck takes this player's file
// when the peer loaded the track from this player's drive, not from its own.
func TestShadowLegacyOwnFileOnlyFromOwnDrive(t *testing.T) {
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), t.TempDir()
	local := filepath.Join(s.shadow.mediaRoot, "USB", "Contents", "track.flac")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.shadow.mu.Lock()
	s.shadow.files["peer:k1"] = "/copy/track.flac"
	s.shadow.mu.Unlock()
	compose := func(d deckState) deckState {
		t.Helper()
		d.Deck = 1
		s.shadow.mu.Lock()
		s.shadow.peerDecks[1] = peerDeck{deckState: d, ip: "192.0.2.7", at: time.Now()}
		s.shadow.mu.Unlock()
		out := s.shadowComposeLegacy("24", time.Now())
		if len(out) != 1 {
			t.Fatalf("compose: %+v", out)
		}
		return out[0]
	}
	if d := compose(deckState{Net: "net://bbbb-peer/SD (SD)/Contents/track.flac", Key: "k1", Ext: ".flac", Link: "{BBBB-peer}"}); d.Path != "/copy/track.flac" || d.ID != "k1" {
		t.Fatalf("the peer's own drive: %+v", d)
	}
	if d := compose(deckState{Net: "net://aaaa-own/USB (USB 1)/Contents/track.flac", Key: "k1", Ext: ".flac", Link: "{bbbb-peer}"}); d.Path != local || d.ID == "k1" {
		t.Fatalf("this player's drive: %+v", d)
	}
}
