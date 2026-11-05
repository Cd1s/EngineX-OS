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
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
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
			if got, _ := os.ReadFile(p); string(got) != "AUDIO" || !strings.HasPrefix(p, filepath.Join(dirB, "shadow")) || filepath.Base(p) != "a.mp3" {
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
	dev := `{"address":"127.0.0.1","name":"CDJ-3000","number":3,"playing":true,"play_position_ms":5000,"pitch":-1.5,"master":true,"sync":true}`
	if resp, err := http.Post(srv.URL+"/v1/sc5000/prolink", "application/json", strings.NewReader(dev)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("post device: %v %v", resp, err)
	}
	_, decks := shadowGetJSON(t, srv.URL+"/v1/sc5000/shadow")
	if len(decks) != 1 || decks[0].Deck != 3 || !decks[0].Playing || decks[0].Pitch != -1.5 || decks[0].PositionMS < 5000 || decks[0].Path != "" || !decks[0].Master || !decks[0].Sync {
		t.Fatalf("shadow: %+v", decks)
	}
	// A CDJ-3000's absolute position (0x0b) wins over the beat-based one.
	s.shadow.observe(prolink.Device{Number: 3, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 9000})
	if _, decks = shadowGetJSON(t, srv.URL+"/v1/sc5000/shadow"); len(decks) != 1 || decks[0].PositionMS < 9000 || decks[0].PositionMS > 9500 {
		t.Fatalf("precise: %+v", decks)
	}
	// CUE: the status packet says paused; the position packets that keep coming
	// (no play flag) must not make it play again.
	for _, d := range []string{
		`{"address":"127.0.0.1","name":"CDJ-3000","number":3,"packet_type":10,"playing":false,"play_position_ms":52}`,
		`{"address":"127.0.0.1","name":"CDJ-3000","number":3,"packet_type":11,"play_position_ms":52}`,
	} {
		http.Post(srv.URL+"/v1/sc5000/prolink", "application/json", strings.NewReader(d))
	}
	s.shadow.observe(prolink.Device{Number: 3, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 52})
	if _, decks = shadowGetJSON(t, srv.URL+"/v1/sc5000/shadow"); len(decks) != 1 || decks[0].Playing || decks[0].PositionMS != 52 {
		t.Fatalf("cue: %+v", decks)
	}
}

func TestCDJDecks(t *testing.T) {
	now := time.Now()
	dev := func(n byte) prolink.Device { return prolink.Device{Number: n, Name: "CDJ-3000", LastSeen: now} }
	for _, c := range []struct {
		cdjs []byte
		free []int
		want map[int]byte
	}{
		{[]byte{3}, []int{2, 4}, map[int]byte{2: 3}},          // a lone CDJ 3 next to a 1+3 player: Deck 2
		{[]byte{2}, []int{2, 4}, map[int]byte{2: 2}},          // its own number
		{[]byte{4}, []int{2, 4}, map[int]byte{4: 4}},          // its own number, even if Deck 2 is free
		{[]byte{1, 3}, []int{2, 4}, map[int]byte{2: 1, 4: 3}}, // both moved, in player order
		{[]byte{3}, []int{1, 3}, map[int]byte{3: 3}},          // a 2+4 player: Deck 3
		{[]byte{1, 2, 3}, []int{4}, map[int]byte{4: 1}},       // more CDJs than shadow decks
	} {
		var devices []prolink.Device
		for i, n := range c.cdjs {
			d := dev(n)
			d.Address = "10.0.0." + strconv.Itoa(i+1)
			devices = append(devices, d)
		}
		devices = append(devices, prolink.Device{Number: 4, Name: "EngineX Export Beta", LastSeen: now}, prolink.Device{Number: 1, Name: "CDJ-2000", LastSeen: now.Add(-time.Minute)})
		got := cdjDecks(devices, c.free, now)
		// The same CDJ with its old number, silent for 2 s, is not another CDJ.
		if len(c.cdjs) == 1 {
			old := dev(c.cdjs[0]%4 + 1)
			old.Address, old.LastSeen = "10.0.0.1", now.Add(-2*time.Second)
			got = cdjDecks(append(devices, old), c.free, now)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%v on %v: %v", c.cdjs, c.free, got)
		}
		for deck, n := range c.want {
			if got[deck].Number != n {
				t.Fatalf("%v on %v: deck %d has %d, want %d", c.cdjs, c.free, deck, got[deck].Number, n)
			}
		}
	}
}

// An XDJ-XZ / XDJ-AZ is several players on one address: all of them are kept.
func TestCDJDecksAllInOne(t *testing.T) {
	now := time.Now()
	xz := func(n byte, ago time.Duration) prolink.Device {
		return prolink.Device{Number: n, Name: "XDJ-XZ", Address: "10.0.0.9", LastSeen: now.Add(-ago)}
	}
	got := cdjDecks([]prolink.Device{xz(1, 0), xz(2, 150*time.Millisecond)}, []int{2, 4}, now)
	if len(got) != 2 || got[2].Number != 2 || got[4].Number != 1 { // player 2 on its own deck, player 1 on the free one
		t.Fatalf("both decks of one address: %v", got)
	}
	// Its deck 2 went quiet 3 s ago (gone, or renumbered): only deck 1 stays.
	got = cdjDecks([]prolink.Device{xz(1, 0), xz(2, 3*time.Second)}, []int{2, 4}, now)
	if len(got) != 1 || got[2].Number != 1 {
		t.Fatalf("quiet number kept: %v", got)
	}
}

// A track loaded from the streaming browser (here an OmniSource drive row) is a
// file the other player can copy, under its own name.
func TestShadowDeckFromStreamedRow(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	drive := t.TempDir()
	file := filepath.Join(drive, "Song.flac")
	os.WriteFile(file, []byte("AUDIO"), 0o644)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	id := s.tidal.addTrack(trackRef{track: providers.Track{ID: "1", Provider: localProvider}, file: file, drive: drive})
	body := fmt.Sprintf(`[{"deck":1,"net":"streaming://TIDAL/Track/%d"},{"deck":3,"net":"streaming://TIDAL/Track/999999"}]`, id)
	if resp, err := http.Post(srv.URL+"/v1/sc5000/decks", "application/json", strings.NewReader(body)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("post: %v %v", resp, err)
	}
	var peer struct{ Decks []deckState }
	resp, _ := http.Get(srv.URL + "/peer/decks")
	json.NewDecoder(resp.Body).Decode(&peer)
	resp.Body.Close()
	if len(peer.Decks) != 2 || peer.Decks[0].Key == "" || peer.Decks[0].Name != "Song.flac" || peer.Decks[0].Path != "" || peer.Decks[1].Key != "" {
		t.Fatalf("peer decks: %+v", peer.Decks)
	}
}
