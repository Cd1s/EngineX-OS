package webui

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
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

func TestShadowNetworkVolumeSuffix(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), t.TempDir()
	rel := "Contents/UnknownArtist/UnknownAlbum/Massa (7) - Reforma.flac"
	local := filepath.Join(s.shadow.mediaRoot, "KDATA KF328", rel)
	if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("audio"), 0644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.shadow.peerDecks[1] = peerDeck{deckState: deckState{Deck: 1, Net: "net://7022ea42-920b-4c66-b913-1892c3ee73af/KDATA KF328 (USB 3)/" + rel, Playing: true}, at: now}
	got := s.shadowCompose("24", now)
	if len(got) != 1 || got[0].Path != local || !got[0].Playing {
		t.Fatalf("shadow: %+v, want path %q and playing", got, local)
	}
	if got := resolveNetPathFile("net://uuid/other/"+rel, s.shadow.dataRoot, s.shadow.mediaRoot, false); got != local {
		t.Fatalf("other volume: %q, want %q", got, local)
	}
	if got := resolveNetPathFile("net://uuid/other/../secret.flac", s.shadow.dataRoot, s.shadow.mediaRoot, false); got != "" {
		t.Fatalf("unsafe path: %q", got)
	}
	cache := filepath.Join(s.shadow.dataRoot, "inMusicTempFiles", "ab12_only-cached.flac")
	if err := os.MkdirAll(filepath.Dir(cache), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("audio"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := resolveNetPathFile("net://uuid/USB/only-cached.flac", s.shadow.dataRoot, s.shadow.mediaRoot, false); got != "" {
		t.Fatalf("shadow deck took Engine's temp copy %q (it reloads forever)", got)
	}
	if got := resolveNetPathFile("net://uuid/(Unknown)/inMusicTempFiles/ab12_only-cached.flac", s.shadow.dataRoot, s.shadow.mediaRoot, false); got != "" {
		t.Fatalf("shadow deck took Engine's temp dir %q", got)
	}
	if got := resolveNetPathFile("net://uuid/USB/only-cached.flac", s.shadow.dataRoot, s.shadow.mediaRoot, true); got != cache {
		t.Fatalf("loading player: %q, want its copy %q", got, cache)
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
	bs.shadow.dataRoot = t.TempDir()
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

// #49: the shadow deck's copy gets the source's analysis as <copy>.perf, which
// the OfflineAnalyzer hook replays; nothing writes either deck's m.db.
func TestShadowPerfFromPeer(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	_, root, src := testlibrary.Create(t)
	os.MkdirAll(filepath.Join(root, "Contents"), 0o755)
	os.WriteFile(filepath.Join(root, "Contents", "it's.mp3"), []byte("AUDIO"), 0o644)
	os.WriteFile(filepath.Join(root, "Contents", "raw.mp3"), []byte("AUDIO"), 0o644)
	// A 5.0.4-style 44-byte trackData (zero peaks) and a one-entry overview.
	track := bytes.Repeat([]byte{0}, 44)
	binary.BigEndian.PutUint64(track, 0x40e5888000000000) // 44100.0
	overview := append(bytes.Repeat([]byte{0}, 24), 100, 50, 25, 7, 7, 7)
	beats := []byte("GRID")
	qc := func(b []byte) []byte { c, _ := enginedb.QCompress(b); return c }
	if _, err := src.Exec(`INSERT INTO Track (path, filename, bpmAnalyzed, isAnalyzed) VALUES ('../Contents/it''s.mp3', 'it''s.mp3', 130.5, 1), ('../Contents/raw.mp3', 'raw.mp3', 120, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?`, qc(track), qc(overview), qc(beats)); err != nil {
		t.Fatal(err)
	}
	as := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	as.shadow.mediaRoot = filepath.Dir(root)
	a := httptest.NewServer(as)
	defer a.Close()
	vol := filepath.Base(root)
	body := `[{"deck":2,"net":"net://u/` + vol + `/Contents/it's.mp3","playing":true},{"deck":4,"net":"net://u/` + vol + `/Contents/raw.mp3"}]`
	if resp, err := http.Post(a.URL+"/v1/sc5000/decks", "application/json", strings.NewReader(body)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("post: %v %v", resp, err)
	}
	as.shadow.mu.Lock()
	key, rawKey := as.shadow.own[0].Key, as.shadow.own[1].Key
	as.shadow.mu.Unlock()
	if r, _ := http.Get(a.URL + "/peer/decks/2/perf?key=wrong"); r.StatusCode != http.StatusConflict {
		t.Fatalf("wrong key served: %d", r.StatusCode)
	}
	if r, _ := http.Get(a.URL + "/peer/decks/4/perf?key=" + rawKey); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unanalysed track served: %d", r.StatusCode)
	}

	bs := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	u, _ := url.Parse(a.URL)
	bs.shadow.peerPort, _ = strconv.Atoi(u.Port())
	fetched := func(p peerDeck) string {
		bs.shadow.mu.Lock()
		bs.shadowFetchPeer("peer:"+p.Key, p)
		bs.shadow.mu.Unlock()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			bs.shadow.mu.Lock()
			local := bs.shadow.files["peer:"+p.Key]
			bs.shadow.mu.Unlock()
			if local != "" {
				return local
			}
		}
		t.Fatal("never copied")
		return ""
	}
	local := fetched(peerDeck{deckState: deckState{Deck: 2, Key: key, Ext: ".mp3", Name: "it's.mp3", Net: "net://u/" + vol + "/Contents/it's.mp3"}, ip: "127.0.0.1"})
	if filepath.Base(filepath.Dir(local)) != "Contents" { // so the UI stretches its overview as on the source
		t.Fatalf("copy of an export track outside Contents/: %s", local)
	}
	got, err := os.ReadFile(local + ".perf")
	if err != nil {
		t.Fatal(err)
	}
	peaks := enginedb.TrackDataPeaks(track, overview)
	var want bytes.Buffer
	want.WriteString("EXPF")
	binary.Write(&want, binary.LittleEndian, uint32(1))
	binary.Write(&want, binary.LittleEndian, 130.5)
	for _, part := range [][]byte{peaks, beats, overview} {
		binary.Write(&want, binary.LittleEndian, uint32(len(part)))
		want.Write(part)
	}
	if !bytes.Equal(got, want.Bytes()) || len(peaks) != 68 {
		t.Fatalf("perf %x\nwant %x", got, want.Bytes())
	}
	// An unanalysed source track: no .perf, so Engine analyses the copy.
	raw := fetched(peerDeck{deckState: deckState{Deck: 4, Key: rawKey, Ext: ".mp3", Name: "raw.mp3"}, ip: "127.0.0.1"})
	if filepath.Base(filepath.Dir(raw)) == "Contents" {
		t.Fatalf("copy of a track outside an export under Contents/: %s", raw)
	}
	if _, err := os.Stat(raw + ".perf"); !os.IsNotExist(err) {
		t.Fatalf("perf for an unanalysed track: %v", err)
	}
}

func TestShadowSharedUSBAndEngineCache(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	a := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	a.shadow.dataRoot, a.shadow.mediaRoot = t.TempDir(), t.TempDir()
	b := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	b.shadow.dataRoot, b.shadow.mediaRoot = t.TempDir(), t.TempDir()
	server := httptest.NewServer(b)
	defer server.Close()
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = "/run/sc5000-assignments/role" }()
	if err := os.WriteFile(fourDeckRolePath, []byte("24\n"), 0644); err != nil {
		t.Fatal(err)
	}
	uiPost(t, server.URL, "fourDeckWaveform=1")
	local := filepath.Join(b.shadow.mediaRoot, "USB", "track.flac")
	if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("audio"), 0644); err != nil {
		t.Fatal(err)
	}
	post := func() deckState {
		t.Helper()
		w := httptest.NewRecorder()
		a.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(`[{"deck":1,"net":"net://uuid/USB/track.flac"}]`)))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
		w = httptest.NewRecorder()
		a.peerDecksGet(w, httptest.NewRequest("GET", "/peer/decks", nil))
		var v struct{ Decks []deckState }
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || len(v.Decks) != 1 {
			t.Fatalf("peer: %s %v", w.Body, err)
		}
		return v.Decks[0]
	}
	d := post()
	if d.Net == "" || d.Key != "" {
		t.Fatalf("missing source: %+v", d)
	}
	b.shadow.peerDecks[1] = peerDeck{deckState: d, ip: "127.0.0.1", at: time.Now()}
	role, got := shadowGetJSON(t, server.URL+"/v1/sc5000/shadow")
	if role != "24" || len(got) != 1 || got[0].Path != local || got[0].Net != "" {
		t.Fatalf("local source: %s %+v", role, got)
	}
	cache := filepath.Join(a.shadow.dataRoot, "inMusicTempFiles", "uuid_track.flac")
	if err := os.MkdirAll(filepath.Dir(cache), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("audio"), 0644); err != nil {
		t.Fatal(err)
	}
	d = post()
	if d.Net == "" || d.Key == "" || d.Name != "uuid_track.flac" || a.shadow.own[0].Path != cache {
		t.Fatalf("cache: %+v", d)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cache), "other_track.flac"), []byte("other"), 0644); err != nil {
		t.Fatal(err)
	}
	if d = post(); d.Key != "" || d.Net == "" {
		t.Fatalf("ambiguous cache: %+v", d)
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

// Engine 5.3.0's TrackNetworkPath for a streaming track is only
// "net://<uuid>/(Unknown)"; the QML posts its TrackUri, and the source serves
// Engine's own download, with its analysis, from stm.db.
func TestShadowStreamingDownloadFromStmDB(t *testing.T) {
	shadowBeaconOn = false
	_, root, db := testlibrary.Create(t)
	qc := func(b []byte) []byte { c, _ := enginedb.QCompress(b); return c }
	if _, err := db.Exec(`INSERT INTO Track (path, filename, uri, title, artist, bpmAnalyzed, isAnalyzed) VALUES ('../../../../downloads/TIDAL/abc.mp3', 'Song.mp3', 'streaming://TIDAL/Track/37', 'Laor: The/Greater?', 'Some Artist', 140, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?`, qc(bytes.Repeat([]byte{0}, 68)), qc(bytes.Repeat([]byte{0}, 30)), qc([]byte("GRID"))); err != nil {
		t.Fatal(err)
	}
	db.Close()
	data := t.TempDir()
	lib := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	os.MkdirAll(filepath.Dir(lib), 0o755)
	os.Rename(filepath.Join(root, "Engine Library"), lib)
	os.Rename(filepath.Join(lib, "Database2", "m.db"), filepath.Join(lib, "Database2", "stm.db"))
	os.MkdirAll(filepath.Join(data, "downloads", "TIDAL"), 0o755)
	os.WriteFile(filepath.Join(data, "downloads", "TIDAL", "abc.mp3"), []byte("AUDIO"), 0o644)
	shadowDataRoot = data
	defer func() { shadowDataRoot, shadowBeaconOn = "/data", true }()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	body := `[{"deck":3,"net":"streaming://TIDAL/Track/37"},{"deck":1,"net":"streaming://TIDAL/Track/38"}]`
	if resp, err := http.Post(srv.URL+"/v1/sc5000/decks", "application/json", strings.NewReader(body)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("post: %v %v", resp, err)
	}
	s.shadow.mu.Lock()
	own := append([]deckState(nil), s.shadow.own...)
	s.shadow.mu.Unlock()
	if own[0].Path != filepath.Join(data, "downloads", "TIDAL", "abc.mp3") || own[0].Key == "" || own[1].Key != "" || own[0].Name != "Some Artist - Laor TheGreater.mp3" {
		t.Fatalf("own decks: %+v", own)
	}
	resp, err := http.Get(srv.URL + "/peer/decks/3/perf?key=" + own[0].Key)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("perf: %v %v", resp, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.HasPrefix(b, []byte("EXPF")) {
		t.Fatalf("perf %q", b)
	}
}

// The QML's sample time, not the post's arrival, anchors a playing deck's position.
func TestShadowPositionFromSampleTime(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	sampled := time.Now().UnixMilli() - 1000
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(fmt.Sprintf(`[{"deck":1,"playing":true,"positionMs":5000,"at":%d},{"deck":3,"playing":true,"positionMs":5000,"at":1}]`, sampled))))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	s.peerDecksGet(w, httptest.NewRequest("GET", "/peer/decks", nil))
	var v struct{ Decks []deckState }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || len(v.Decks) != 2 {
		t.Fatalf("peer: %s %v", w.Body, err)
	}
	if p := v.Decks[0].PositionMS; p < 5990 || p > 6100 || v.Decks[0].At != 0 {
		t.Fatalf("sampled 1 s ago: %+v, want ~6000 ms and no clock", v.Decks[0])
	}
	if p := v.Decks[1].PositionMS; p < 5000 || p > 5100 {
		t.Fatalf("untrusted clock: %+v, want ~5000 ms", v.Decks[1])
	}
	d := v.Decks[0]
	d.Deck = 2
	s.shadow.peerDecks[2] = peerDeck{deckState: d, at: time.Now()}
	if got := s.shadowCompose("13", time.Now()); len(got) != 1 || got[0].At == 0 {
		t.Fatalf("shadow lacks its clock: %+v", got)
	}
}

// A post on one player reaches the other over UDP and wakes its long poll at
// once; an older update (the HTTP fallback, or a reordered datagram) never
// overwrites a newer one.
func TestShadowUDPPushWakesLongPoll(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	recv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	send, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()

	a := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	a.shadow.conn, a.shadow.pushPort = send, recv.LocalAddr().(*net.UDPAddr).Port
	a.shadow.peers["127.0.0.1"] = time.Now()

	dirB := t.TempDir()
	bs := New(Config{UIDir: filepath.Join(dirB, "ui"), StateDir: t.TempDir()}, nil)
	b := httptest.NewServer(bs)
	defer b.Close()
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = "/run/sc5000-assignments/role" }()
	os.WriteFile(fourDeckRolePath, []byte("13\n"), 0o644)
	os.MkdirAll(filepath.Join(dirB, "ui"), 0o755)
	uiPost(t, b.URL, "fourDeckWaveform=1")

	var first struct{ Ver uint64 }
	resp, err := http.Get(b.URL + "/v1/sc5000/shadow")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&first)
	resp.Body.Close()

	// Nothing new: the long poll holds for its 120 ms.
	start := time.Now()
	shadowGetJSON(t, fmt.Sprintf("%s/v1/sc5000/shadow?ver=%d", b.URL, first.Ver))
	if d := time.Since(start); d < 100*time.Millisecond || d > time.Second {
		t.Fatalf("idle long poll returned after %v, want ~120 ms", d)
	}

	type result struct {
		decks []deckState
		took  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, decks := shadowGetJSON(t, fmt.Sprintf("%s/v1/sc5000/shadow?ver=%d", b.URL, first.Ver))
		done <- result{decks, time.Since(start)}
	}()
	time.Sleep(30 * time.Millisecond) // the poll is waiting
	w := httptest.NewRecorder()
	a.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(fmt.Sprintf(`[{"deck":2,"playing":true,"positionMs":4000,"jumps":3,"at":%d,"cuePos":3792.6,"cues":[{"i":2,"p":11910836.7,"n":"Drop","c":"#00ff00"}]}]`, time.Now().UnixMilli()))))
	buf := make([]byte, 64<<10)
	recv.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := recv.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	bs.shadow.receive(buf[:n], "192.0.2.7", time.Now())
	r := <-done
	if r.took > 100*time.Millisecond || len(r.decks) != 1 || r.decks[0].Deck != 2 || !r.decks[0].Playing || r.decks[0].Jumps != 3 || r.decks[0].PositionMS < 4000 ||
		r.decks[0].CuePos == nil || *r.decks[0].CuePos != 3792.6 || len(r.decks[0].Cues) != 1 || r.decks[0].Cues[0] != (shadowCue{I: 2, P: 11910836.7, N: "Drop", C: "#00ff00"}) {
		t.Fatalf("pushed update took %v: %+v", r.took, r.decks)
	}

	bs.shadow.mu.Lock()
	seq := bs.shadow.peerDecks[2].Seq
	bs.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Seq: seq - 1}}, time.Now()) // stale: still playing
	stale := bs.shadow.peerDecks[2].Playing
	bs.shadow.takePeer("192.0.2.7", []deckState{{Deck: 2, Seq: seq + 1}}, time.Now()) // CUE released
	stopped := !bs.shadow.peerDecks[2].Playing
	bs.shadow.mu.Unlock()
	if !stale || !stopped {
		t.Fatalf("order: kept stale update %v, took newer %v", !stale, stopped)
	}
	bs.shadow.receive([]byte("EngineX-decks 1\n{garbage"), "192.0.2.8", time.Now())
	bs.shadow.mu.Lock()
	_, bad := bs.shadow.peers["192.0.2.8"]
	bs.shadow.mu.Unlock()
	if bad {
		t.Fatal("a malformed datagram counted as a peer")
	}

	// An older post (the QML's XHRs race) neither replaces nor gets pushed.
	w = httptest.NewRecorder()
	a.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(fmt.Sprintf(`[{"deck":2,"playing":false,"positionMs":1,"at":%d}]`, time.Now().UnixMilli()-500))))
	a.shadow.mu.Lock()
	defer a.shadow.mu.Unlock()
	if a.shadow.own[0].PositionMS != 4000 {
		t.Fatalf("a stale post replaced %+v", a.shadow.own)
	}
}

// A CDJ's edges wake the long poll at once, and its position jumps are counted
// for the shadow deck (it seeks on a new count instead of after 1 s).
func TestCDJObserveEdges(t *testing.T) {
	sh := newShadowState()
	ver := func() uint64 { sh.mu.Lock(); defer sh.mu.Unlock(); return sh.ver }
	status := func(playing bool, p1 byte) {
		sh.observe(prolink.Device{Number: 3, PacketType: 0x0a, TrackID: 5, Playing: playing, PlayState1: p1})
	}
	pos := func(ms uint32) {
		sh.observe(prolink.Device{Number: 3, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: ms})
	}
	status(true, 3)
	if ver() != 1 {
		t.Fatalf("play edge: ver %d", ver())
	}
	status(true, 3)
	if ver() != 1 {
		t.Fatalf("no edge woke: ver %d", ver())
	}
	start := time.Now()
	pos(1000)
	time.Sleep(30 * time.Millisecond)
	pos(1000 + uint32(time.Since(start).Milliseconds()))
	time.Sleep(30 * time.Millisecond)
	pos(1000 + uint32(time.Since(start).Milliseconds()))
	if ver() != 1 || sh.cdj[3].jumps != 0 {
		t.Fatalf("playing counted as a jump: ver %d jumps %d", ver(), sh.cdj[3].jumps)
	}
	pos(60000) // hot cue
	if ver() != 2 || sh.cdj[3].jumps != 1 {
		t.Fatalf("jump: ver %d jumps %d", ver(), sh.cdj[3].jumps)
	}
	status(true, 3) // the deck was paused and then holds CUE: no edge between the two
	status(false, cdjCuePlay)
	if ver() != 2 {
		t.Fatalf("CUE held is playing: ver %d", ver())
	}
	status(false, 6) // CUE released
	pos(52)          // back on the cue point
	if ver() != 4 || sh.cdj[3].jumps != 2 || sh.cdj[3].playing {
		t.Fatalf("CUE release: ver %d %+v", ver(), sh.cdj[3])
	}
}

func TestDisplayFileName(t *testing.T) {
	for _, c := range []struct{ artist, title, ext, want string }{
		{"", "Laor The Greater", ".flac", "Laor The Greater.flac"},
		{"A", "B", ".mp3", "A - B.mp3"},
		{"A|B", "x\\y*z\n", ".flac", "AB - xyz.flac"},
		{"A", "", ".flac", ""},
		{"", "...", ".flac", ".flac"},
	} {
		if got := displayFileName(c.artist, c.title, c.ext); got != c.want {
			t.Errorf("displayFileName(%q, %q) = %q, want %q", c.artist, c.title, got, c.want)
		}
	}
}

// A peer that ejects its track, goes quiet, or restarts with a clock behind its
// old Seq must not leave a shadow deck loaded or its updates dropped.
func TestTakePeerClearsEjectedDecksAndResetsSeq(t *testing.T) {
	sh := newShadowState()
	now := time.Now()
	sh.takePeer("192.0.2.7", []deckState{{Deck: 1, Seq: 1000, Key: "a"}, {Deck: 2, Seq: 1000, Key: "b"}}, now)
	sh.takePeer("192.0.2.9", []deckState{{Deck: 3, Seq: 5, Key: "c"}}, now)
	sh.takePeer("192.0.2.7", []deckState{{Deck: 2, Seq: 1001, Key: "b"}}, now.Add(time.Second))
	if _, ok := sh.peerDecks[1]; ok || sh.peerDecks[2].Key != "b" || sh.peerDecks[3].Key != "c" {
		t.Fatalf("ejected deck kept or other peer touched: %+v", sh.peerDecks)
	}
	ver := sh.ver
	sh.takePeer("192.0.2.7", []deckState{}, now.Add(1500*time.Millisecond)) // goodbye
	if _, ok := sh.peerDecks[2]; ok || sh.peerDecks[3].Key != "c" || sh.ver == ver {
		t.Fatalf("goodbye: %+v ver %d->%d", sh.peerDecks, ver, sh.ver)
	}
	// Restart after a long silence with an older Seq (clock not yet synced).
	sh.takePeer("192.0.2.7", []deckState{{Deck: 2, Seq: 500, Key: "n"}}, now.Add(time.Minute))
	if sh.peerDecks[2].Key != "n" {
		t.Fatalf("restarted peer dropped: %+v", sh.peerDecks)
	}
}

func TestShadowComposeMarksLoadingAndUnloads(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	now := time.Now()
	s.shadow.mu.Lock()
	s.shadow.peerDecks[3] = peerDeck{deckState: deckState{Deck: 3, Key: "k", Ext: ".flac", Name: "x.flac"}, ip: "192.0.2.7", at: now}
	s.shadow.peerDecks[4] = peerDeck{deckState: deckState{Deck: 4}, ip: "192.0.2.7", at: now}
	s.shadow.mu.Unlock()
	out := s.shadowCompose("12", now)
	var got = map[int]deckState{}
	for _, d := range out {
		got[d.Deck] = d
	}
	if !got[3].Loading || got[3].Path != "" || got[4].Loading || got[4].Path != "" {
		t.Fatalf("compose: %+v", out)
	}
	// #160: an overlapping peer deck (e.g. an SC5000 on Deck 1 while a PRIME 4 owns
	// Decks 1+2) is placed onto the next free shadow deck (Deck 3).
	s.shadow.mu.Lock()
	delete(s.shadow.peerDecks, 3)
	delete(s.shadow.peerDecks, 4)
	s.shadow.peerDecks[1] = peerDeck{deckState: deckState{Deck: 1, Key: "k1", Ext: ".flac", Name: "sc5000.flac", Playing: true}, ip: "192.0.2.8", at: now}
	s.shadow.mu.Unlock()
	out = s.shadowCompose("12", now)
	if len(out) != 1 || out[0].Deck != 3 || !out[0].Loading || !out[0].Playing {
		t.Fatalf("overlapping peer deck on 12 role: %+v, want shadow deck 3", out)
	}
	// Two SC5000 players both playing on their own Deck 1 map to shadow Decks 3 and 4.
	s.shadow.mu.Lock()
	s.shadow.peerDecks = map[int]peerDeck{}
	s.shadow.takePeer("192.0.2.8", []deckState{{Deck: 1, Seq: 1, Key: "k1", Ext: ".flac", Name: "p1.flac", Playing: true}}, now)
	s.shadow.takePeer("192.0.2.9", []deckState{{Deck: 1, Seq: 1, Key: "k2", Ext: ".flac", Name: "p2.flac", Playing: true}}, now)
	s.shadow.mu.Unlock()
	out = s.shadowCompose("12", now)
	if len(out) != 2 || out[0].Deck != 3 || out[1].Deck != 4 {
		t.Fatalf("two peers on Deck 1 with 12 role: %+v, want shadow decks 3 and 4", out)
	}
}

// Debug telemetry: off writes nothing; on, posts, peer updates and the QML's
// records reach the ring and the log, which rotates at shadowDebugMax.
func TestShadowDebugTelemetry(t *testing.T) {
	dir := t.TempDir()
	oldLog, oldMax := shadowDebugLog, shadowDebugMax
	shadowDebugLog, shadowDebugMax = filepath.Join(dir, "debug.log"), 600
	defer func() { shadowDebugLog, shadowDebugMax = oldLog, oldMax }()
	s := New(Config{UIDir: filepath.Join(dir, "ui"), StateDir: t.TempDir()}, nil)
	post := `[{"deck":1,"playing":true,"positionMs":5000,"pitch":11.1111,"bpm":126,"currentBpm":140,"sync":true}]`
	s.decksPost(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(post)))
	if _, err := os.Stat(shadowDebugLog); err == nil || len(s.shadow.dbg.lines()) != 0 {
		t.Fatal("telemetry written while off")
	}
	if s.shadow.ownForPeer(time.Now())[0].CurrentBPM != 140 {
		t.Fatalf("currentBpm not kept: %+v", s.shadow.own)
	}

	os.MkdirAll(filepath.Join(dir, "ui"), 0o755)
	os.WriteFile(filepath.Join(dir, "ui", "settings.conf"), []byte("debug_telemetry=1\n"), 0o644)
	s.uiMu.Lock()
	on := s.loadUI().DebugTelemetry
	s.uiMu.Unlock()
	if !on || !s.shadow.dbg.set(on) {
		t.Fatal("debug_telemetry=1 not read")
	}
	for range 5 {
		s.decksPost(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(post)))
	}
	s.shadow.mu.Lock()
	s.shadow.takePeer("192.0.2.9", []deckState{{Deck: 2, Seq: 1, Playing: true}}, time.Now())
	s.shadow.mu.Unlock()
	w := httptest.NewRecorder()
	s.shadowDebugPost(w, httptest.NewRequest("POST", "/v1/sc5000/debug/shadow", strings.NewReader(`[{"deck":2,"act":"pll","drift":40,"pll":3.2,"t":1}]`)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	s.shadowDebugGet(w, httptest.NewRequest("GET", "/v1/sc5000/debug/shadow", nil))
	var got struct {
		Enabled   bool
		PeerDecks []struct {
			Deck int
			IP   string
		}
		Log []struct {
			Ev  string
			Act string
			Qt  float64
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !got.Enabled || len(got.PeerDecks) != 1 || got.PeerDecks[0].IP != "192.0.2.9" || len(got.Log) != 7 {
		t.Fatalf("debug state: %s %v", w.Body, err)
	}
	if l := got.Log[6]; l.Ev != "qml" || l.Act != "pll" || l.Qt != 1 {
		t.Fatalf("qml record: %+v", l)
	}
	cur, _ := os.Stat(shadowDebugLog)
	if _, err := os.Stat(shadowDebugLog + ".1"); err != nil || cur.Size() > shadowDebugMax {
		t.Fatalf("not rotated: %v, %d bytes", err, cur.Size())
	}
	if strings.Contains(string(must(os.ReadFile(shadowDebugLog))), `"cues"`+":[") {
		t.Fatal("cues logged")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// One player heard on two addresses (eth0 and wlan0) is one peer: the second
// copy of an update is stale and does not wake the long poll again.
func TestTakePeerDedupesByLinkAcrossAddresses(t *testing.T) {
	sh := newShadowState()
	now := time.Now()
	up := []deckState{{Deck: 1, Seq: 5, Link: "{u}", Playing: true}}
	sh.takePeer("192.0.2.1", up, now)
	sh.takePeer("192.0.2.2", up, now)
	if sh.ver != 1 {
		t.Fatalf("same update via the second address bumped: ver %d", sh.ver)
	}
	sh.takePeer("192.0.2.2", []deckState{{Deck: 3, Seq: 6, Link: "{u}"}}, now) // deck 1 ejected, seen via the other address
	if _, ok := sh.peerDecks[1]; ok || sh.ver != 2 {
		t.Fatalf("eject via the other address: %+v ver %d", sh.peerDecks, sh.ver)
	}
}

// #160's colliding deck slots must remain stable when a wip peer changes address.
func TestTakePeerCollisionAcrossAddresses(t *testing.T) {
	sh := newShadowState()
	now := time.Now()
	sh.takePeer("192.0.2.1", []deckState{{Deck: 1, Seq: 5, Link: "a"}}, now)
	sh.takePeer("192.0.2.2", []deckState{{Deck: 1, Seq: 5, Link: "b"}}, now)
	sh.takePeer("192.0.2.3", []deckState{{Deck: 1, Seq: 6, Link: "b"}}, now)
	if len(sh.peerDecks) != 2 || sh.peerDecks[1].Link != "a" || sh.peerDecks[2].Link != "b" || sh.peerDecks[2].ip != "192.0.2.3" {
		t.Fatalf("address change duplicated or displaced a peer: %+v", sh.peerDecks)
	}
	sh.takePeer("192.0.2.2", []deckState{{Deck: 1, Seq: 6, Link: "b"}}, now)
	if sh.ver != 3 {
		t.Fatalf("duplicate update bumped version: %d", sh.ver)
	}
	sh.takePeer("192.0.2.2", []deckState{{Deck: 3, Seq: 7, Link: "b"}}, now)
	if len(sh.peerDecks) != 2 || sh.peerDecks[1].Link != "a" || sh.peerDecks[3].Link != "b" {
		t.Fatalf("eject used slot instead of original deck: %+v", sh.peerDecks)
	}
}
