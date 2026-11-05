package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestNetPathFile(t *testing.T) {
	for in, want := range map[string]string{
		"net://7022/(Unknown)/sc5000-custom/Stream/1.mp3": "/data/sc5000-custom/Stream/1.mp3",
		"net://7022/KDATA KF328/Contents/a b.flac":        "/media/KDATA KF328/Contents/a b.flac",
		"net://7022/(Unknown)/../etc/passwd":              "",
		"net://7022/../x/y.mp3":                           "",
		"file:///data/x.mp3":                              "",
		"net://7022/(Unknown)":                            "",
	} {
		if got := netPathFile(in, "/data", "/media"); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func shadowGetJSON(t *testing.T, u string) (string, []deckState) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		Role  string
		Decks []deckState
	}
	json.NewDecoder(resp.Body).Decode(&v)
	return v.Role, v.Decks
}

// Two EngineX players: A plays deck 2, B (role 13) shows it as a shadow deck,
// with A's track copied over.
func TestShadowDeckFromPeer(t *testing.T) {
	shadowBeaconOn = false
	data := t.TempDir()
	shadowDataRoot = data
	defer func() { shadowDataRoot, shadowBeaconOn = "/data", true }()
	os.MkdirAll(filepath.Join(data, "sc5000-custom/Stream"), 0o755)
	os.WriteFile(filepath.Join(data, "sc5000-custom/Stream/a.mp3"), []byte("AUDIO"), 0o644)

	a := httptest.NewServer(New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer a.Close()
	body := `[{"deck":2,"net":"net://u/(Unknown)/sc5000-custom/Stream/a.mp3","playing":true,"positionMs":1000,"pitch":2},{"deck":4,"net":"net://u/(Unknown)/../etc/passwd"}]`
	if resp, err := http.Post(a.URL+"/v1/sc5000/decks", "application/json", strings.NewReader(body)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("post: %v %v", resp, err)
	}
	var peer struct{ Decks []deckState }
	resp, _ := http.Get(a.URL + "/peer/decks")
	json.NewDecoder(resp.Body).Decode(&peer)
	resp.Body.Close()
	if len(peer.Decks) != 2 || peer.Decks[0].Key == "" || peer.Decks[0].Path != "" || peer.Decks[1].Key != "" || peer.Decks[0].PositionMS < 1000 {
		t.Fatalf("peer decks: %+v", peer.Decks)
	}
	if r, _ := http.Get(a.URL + "/peer/decks/2/audio?key=wrong"); r.StatusCode != http.StatusConflict {
		t.Fatalf("wrong key served: %d", r.StatusCode)
	}

	dirB := t.TempDir()
	bs := New(Config{UIDir: filepath.Join(dirB, "ui"), StateDir: t.TempDir()}, nil)
	b := httptest.NewServer(bs)
	defer b.Close()
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = "/run/sc5000-assignments/role" }()
	os.WriteFile(fourDeckRolePath, []byte("13\n"), 0o644)
	if role, decks := shadowGetJSON(t, b.URL+"/v1/sc5000/shadow"); role != "" || len(decks) != 0 {
		t.Fatalf("shadow without the 4-deck layout: %q %+v", role, decks)
	}
	os.MkdirAll(filepath.Join(dirB, "ui"), 0o755)
	uiPost(t, b.URL, "fourDeckWaveform=1")
	u, _ := url.Parse(a.URL)
	bs.shadow.peerPort, _ = strconv.Atoi(u.Port())
	bs.shadow.peers["127.0.0.1"] = time.Now()
	bs.shadowPollPeers()
	deadline := time.Now().Add(3 * time.Second)
	for {
		bs.shadowPollPeers()
		// Deck 4 is the peer's too, with nothing loaded: an entry without a file.
		role, decks := shadowGetJSON(t, b.URL+"/v1/sc5000/shadow")
		if role != "13" || len(decks) != 2 || decks[0].Deck != 2 || !decks[0].Playing || decks[0].Pitch != 2 || decks[0].PositionMS < 1000 || decks[1].Deck != 4 || decks[1].Path != "" {
			t.Fatalf("shadow: %q %+v", role, decks)
		}
		if p := decks[0].Path; p != "" {
			if got, _ := os.ReadFile(p); string(got) != "AUDIO" || !strings.HasPrefix(p, filepath.Join(dirB, "shadow")) || filepath.Ext(p) != ".mp3" {
				t.Fatalf("copy %s: %q", p, got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer's track was never copied")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A CDJ on Pro DJ Link drives the shadow deck of its player number.
func TestShadowDeckFromCDJ(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = "/run/sc5000-assignments/role" }()
	os.WriteFile(fourDeckRolePath, []byte("24\n"), 0o644)
	uiPost(t, srv.URL, "fourDeckWaveform=1")
	dev := `{"address":"127.0.0.1","name":"CDJ-3000","number":3,"playing":true,"play_position_ms":5000,"pitch":-1.5}`
	if resp, err := http.Post(srv.URL+"/v1/sc5000/prolink", "application/json", strings.NewReader(dev)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("post device: %v %v", resp, err)
	}
	_, decks := shadowGetJSON(t, srv.URL+"/v1/sc5000/shadow")
	if len(decks) != 1 || decks[0].Deck != 3 || !decks[0].Playing || decks[0].Pitch != -1.5 || decks[0].PositionMS < 5000 || decks[0].Path != "" {
		t.Fatalf("shadow: %+v", decks)
	}
	// A CDJ-3000's absolute position (0x0b) wins over the beat-based one.
	s.shadow.observe(prolink.Device{Number: 3, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 9000})
	if _, decks = shadowGetJSON(t, srv.URL+"/v1/sc5000/shadow"); len(decks) != 1 || decks[0].PositionMS < 9000 || decks[0].PositionMS > 9500 {
		t.Fatalf("precise: %+v", decks)
	}
}
