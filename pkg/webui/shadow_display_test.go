package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func testDisplayAsset() *shadowDisplayAsset {
	return &shadowDisplayAsset{Version: 1, Key: "track-a", Title: "Source title", BPM: 120,
		DisplayPerformance: enginedb.DisplayPerformance{SampleRate: 44100, DurationMS: 4000,
			Overview: enginedb.DisplayWave{Bands: []byte{255, 0, 0, 0, 255, 0, 0, 0, 255, 0, 0, 0}, SamplesPerBin: 44100},
			Beats:    []enginedb.DisplayBeat{{MS: 250, Number: 0}, {MS: 750, Number: 1}}, Cues: []enginedb.DisplayCue{}, Loops: []enginedb.DisplayLoop{}},
	}
}

func TestShadowWaveformPixels(t *testing.T) {
	b, err := renderShadowWave(testDisplayAsset().Overview, 4, 32)
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if im.Bounds().Dx() != 4 || im.Bounds().Dy() != 32 {
		t.Fatal(im.Bounds())
	}
	for x, want := range [][4]uint32{{0x4a4a, 0x7878, 0xffff, 0xffff}, {0xf0f0, 0xb4b4, 0, 0xffff}, {0xffff, 0xffff, 0xffff, 0xffff}, {0, 0, 0, 0}} {
		r, g, b, a := im.At(x, 16).RGBA()
		if [4]uint32{r, g, b, a} != want {
			t.Errorf("column %d: %x,%x,%x,%x", x, r, g, b, a)
		}
	}
	if _, err := renderShadowWave(testDisplayAsset().Overview, 4096, 128); err == nil {
		t.Fatal("GPU texture bound ignored")
	}
}

func TestDisplayOnlyDoesNotFetchOrExposeAudio(t *testing.T) {
	var assetRequests, audioRequests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/display") {
			assetRequests.Add(1)
			json.NewEncoder(w).Encode(testDisplayAsset())
			return
		}
		audioRequests.Add(1)
		t.Errorf("display-only requested legacy endpoint: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer peer.Close()
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	u, _ := url.Parse(peer.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())
	now := time.Now()
	p := peerDeck{deckState: deckState{Deck: 2, Key: "track-a", Path: "/data/song.flac", Net: "net://uuid/USB/song.flac", Playing: true, PositionMS: 500, Pitch: 2, Master: true, Sync: true}, ip: "127.0.0.1", at: now}
	s.shadow.peerDecks[2] = p
	for deadline := time.Now().Add(3 * time.Second); ; {
		d := s.shadowCompose("13", now)
		if len(d) != 1 || d[0].Path != "" || d[0].Net != "" || d[0].Key != displayTrackKey(p) || !d[0].Master || !d[0].Sync {
			t.Fatalf("display state: %+v", d)
		}
		if !d[0].Loading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("display never ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for range 10 {
		s.shadowCompose("13", now)
	}
	if audioRequests.Load() != 0 || assetRequests.Load() != 1 {
		t.Fatalf("asset=%d audio=%d", assetRequests.Load(), audioRequests.Load())
	}
	if _, err := os.Stat(s.shadowDir()); !os.IsNotExist(err) {
		t.Fatalf("legacy audio directory created: %v", err)
	}
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	if len(s.shadow.files) != 0 || len(s.shadow.busy) != 0 {
		t.Fatal("legacy copy pipeline was triggered")
	}
}

func TestDisplayEndpointsCheckRoleKeyAndExpiry(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("13"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1")
	p := peerDeck{deckState: deckState{Deck: 2, Key: "track-a", Cues: []shadowCue{{I: 1, P: 22050, N: "live", C: "#ff0000"}}}, ip: "127.0.0.1", at: time.Now()}
	s.shadow.peerDecks[2] = p
	a := testDisplayAsset()
	wave, _ := renderShadowWave(a.Overview, 8, 32)
	s.shadow.display.entries = map[string]*shadowDisplayEntry{displayPeerID(p): {asset: a, wave: wave, overview: wave}}
	get := func(path string) *http.Response {
		t.Helper()
		r, e := http.Get(srv.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	resp := get("/v1/sc5000/shadow")
	var state struct{ DisplayOnly bool }
	json.NewDecoder(resp.Body).Decode(&state)
	resp.Body.Close()
	if !state.DisplayOnly {
		t.Fatal("new mode not default")
	}
	resp = get("/v1/sc5000/shadow/display?deck=2&key=" + displayTrackKey(p))
	var meta struct{ Cues []enginedb.DisplayCue }
	json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(meta.Cues) != 1 || meta.Cues[0].N != "live" || meta.Cues[0].MS != 500 {
		t.Fatalf("live cues: %d %+v", resp.StatusCode, meta.Cues)
	}
	for _, query := range []string{"deck=1&key=track-a", "deck=2&key=wrong", "deck=9&key=track-a"} {
		resp = get("/v1/sc5000/shadow/waveform.png?" + query)
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("wrong identity accepted: %s", query)
		}
	}
	s.shadow.mu.Lock()
	p.at = time.Now().Add(-3 * time.Second)
	s.shadow.peerDecks[2] = p
	s.shadow.mu.Unlock()
	resp = get("/v1/sc5000/shadow/waveform.png?deck=2&key=" + displayTrackKey(p))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("jitter discarded cached display")
	}
	s.shadow.mu.Lock()
	s.shadow.takePeer(p.ip, nil, time.Now())
	s.shadow.mu.Unlock()
	resp = get("/v1/sc5000/shadow/waveform.png?deck=2&key=" + displayTrackKey(p))
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("explicit goodbye kept the display")
	}
	if _, v := uiPost(t, srv.URL, "shadowLegacyEngine=1"); !v.ShadowLegacyEngine {
		t.Fatal("rollback setting lost")
	}
	resp = get("/v1/sc5000/shadow")
	if resp.Header.Get("X-Shadow-State") != "" {
		t.Fatal("legacy responses must always be applied")
	}
	json.NewDecoder(resp.Body).Decode(&state)
	resp.Body.Close()
	if state.DisplayOnly {
		t.Fatal("legacy rollback flag ignored")
	}
}

func TestDisplayLibraryReadOnlyFilesAndStreaming(t *testing.T) {
	path, root, db := testlibrary.Create(t)
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{Beats: []rekordbox.Beat{{TimeMS: 0, BPM: 120, Number: 1}, {TimeMS: 500, BPM: 120, Number: 2}}, Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO Track(path,filename,title,artist,bpmAnalyzed,key,isAnalyzed) VALUES('../Contents/song.flac','song.flac','Title','Artist',120,1,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := libraryDisplay(context.Background(), filepath.Join(root, "Contents/song.flac"), t.TempDir(), filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if a.KeyName != "8A" || a.Title != "Title" || a.Detail || a.DurationMS != 4000 {
		t.Fatalf("metadata: %+v", a)
	}
	after, _ := os.ReadFile(path)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("reader changed Engine database")
	}
	// Streaming uses the same measured PerformanceData format, in stm.db.
	streamDB := filepath.Join(filepath.Dir(path), "stm.db")
	if err = os.WriteFile(streamDB, before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=NULL`); err != nil {
		t.Fatal(err)
	}
	a, err = libraryDisplay(context.Background(), filepath.Join(root, "Contents/song.flac"), t.TempDir(), filepath.Dir(root))
	if err != nil || a.Title != "Title" {
		t.Fatalf("stm.db fallback: %v", err)
	}
	streamAfter, _ := os.ReadFile(streamDB)
	if !bytes.Equal(before, streamAfter) {
		t.Fatal("reader changed streaming database")
	}
}

func TestDisplayAssetRejectsInvalidData(t *testing.T) {
	for _, change := range []func(*shadowDisplayAsset){
		func(a *shadowDisplayAsset) { a.Key = "wrong" },
		func(a *shadowDisplayAsset) { a.Beats[1].MS = -1 },
		func(a *shadowDisplayAsset) { a.Overview.Bands = []byte{1, 2} },
		func(a *shadowDisplayAsset) { a.Overview.SamplesPerBin = 1e300 },
		func(a *shadowDisplayAsset) { a.Artwork = []byte("not an image") },
	} {
		a := testDisplayAsset()
		change(a)
		if validateDisplayAsset(a, "track-a") == nil {
			t.Fatal("bad display asset accepted")
		}
	}
}

func TestDisplayTilesAndSourceIdentity(t *testing.T) {
	a := testDisplayAsset()
	a.Waveform = &enginedb.DisplayWave{Bands: bytes.Repeat([]byte{90, 40, 10}, 4099), SamplesPerBin: 176400.0 / 4099}
	a.Detail = true
	e := &shadowDisplayEntry{asset: a, tiles: make([][]byte, 3)}
	// Keep Engine/source deck 1 distinct from receiver's display slot 3.
	p := peerDeck{deckState: deckState{Deck: 1, Key: "track-a", Link: "source-a"}, ip: "127.0.0.1", at: time.Now()}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	s.shadow.peerDecks[1] = p
	s.shadow.display.entries = map[string]*shadowDisplayEntry{displayPeerID(p): e}
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("12"), 0600)
	srv := httptest.NewServer(s)
	defer srv.Close()
	uiPost(t, srv.URL, "fourDeckWaveform=1")
	key := displayTrackKey(p)
	q := "?deck=3&key=" + key
	for tile, wantWidth := range []int{2048, 2048, 3} {
		r, err := http.Get(srv.URL + "/v1/sc5000/shadow/waveform.png" + q + "&tile=" + strconv.Itoa(tile))
		if err != nil {
			t.Fatal(err)
		}
		im, err := png.Decode(r.Body)
		r.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if im.Bounds().Dx() != wantWidth || im.Bounds().Dy() != 128 {
			t.Fatal(im.Bounds())
		}
	}
	r, err := http.Get(srv.URL + "/v1/sc5000/shadow/display" + q)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Key           string
		WaveformTiles []string
		WaveformWidth int
	}
	json.NewDecoder(r.Body).Decode(&meta)
	r.Body.Close()
	if meta.Key != key || len(meta.WaveformTiles) != 3 || meta.WaveformWidth != 4099 {
		t.Fatalf("tile metadata: %+v", meta)
	}
	p.Link = "source-b"
	if displayTrackKey(p) == key {
		t.Fatal("same song on a different source reused an old grid/asset identity")
	}
	w := downsampleDisplayWave(enginedb.DisplayWave{Bands: bytes.Repeat([]byte{1, 2, 255}, 100000), SamplesPerBin: 1}, 65536)
	if len(w.Bands) != 65536*3 || w.Bands[2] != 255 || w.SamplesPerBin != 100000.0/65536 {
		t.Fatal("bounded downsample lost peaks or duration")
	}
}

func TestPeerTwoDeckConflictKeepsEachSourceDeck(t *testing.T) {
	sh := newShadowState()
	now := time.Now()
	sh.takePeer("192.0.2.1", []deckState{{Deck: 1, Key: "A1", Link: "A", Seq: 1}}, now)
	sh.takePeer("192.0.2.2", []deckState{{Deck: 1, Key: "B1", Link: "B", Seq: 1}, {Deck: 2, Key: "B2", Link: "B", Seq: 1}}, now)
	assert := func() {
		t.Helper()
		seen := map[string]int{}
		for _, p := range sh.peerDecks {
			seen[p.Key] = p.Deck
		}
		if len(seen) != 3 || seen["A1"] != 1 || seen["B1"] != 1 || seen["B2"] != 2 {
			t.Fatalf("sibling overwritten: %+v", seen)
		}
	}
	assert()
	// The same Engine heard on another NIC updates its existing two slots.
	sh.takePeer("192.0.2.3", []deckState{{Deck: 1, Key: "B1", Link: "B", Seq: 2}, {Deck: 2, Key: "B2", Link: "B", Seq: 2}}, now.Add(time.Second))
	assert()
	sh.takePeer("192.0.2.3", nil, now.Add(2*time.Second))
	if len(sh.peerDecks) != 1 {
		t.Fatalf("goodbye: %+v", sh.peerDecks)
	}
}

func TestShadowJitterFreezesUntilConfirmedEject(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	now := time.Now()
	s.shadow.peerDecks[2] = peerDeck{deckState: deckState{Deck: 2, Key: "track-a", Playing: true, PositionMS: 1000}, at: now}
	// Preseed an asset so this unit test performs no network operation.
	p := s.shadow.peerDecks[2]
	s.shadow.display.entries = map[string]*shadowDisplayEntry{displayPeerID(p): {asset: testDisplayAsset()}}
	d := s.shadowCompose("13", now.Add(10*time.Second))
	if len(d) != 1 || !d[0].Stale || d[0].PositionMS != 3000 || d[0].Key == "" {
		t.Fatalf("jitter: %+v", d)
	}
	d = s.shadowCompose("13", now.Add(time.Minute))
	if len(d) != 1 || d[0].PositionMS != 3000 {
		t.Fatalf("stale display kept running: %+v", d)
	}
	s.shadow.own = []deckState{{Deck: 1, Key: "own", Playing: true, PositionMS: 1000, At: now.UnixMilli()}}
	s.shadow.ownAt = now
	own := s.shadow.ownForPeer(now.Add(10 * time.Second))
	if len(own) != 1 || !own[0].Stale || own[0].Key != "own" {
		t.Fatalf("sender turned a GUI stall into an eject: %+v", own)
	}
	s.shadow.takePeer("", []deckState{{Deck: 2}}, now.Add(time.Minute))
	d = s.shadowCompose("13", now.Add(time.Minute))
	if len(d) != 1 || d[0].Key != "" {
		t.Fatalf("confirmed empty track not cleared: %+v", d)
	}
}

func TestBridgeDetailsBelongToLibraryPathAndDuration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, ".rb-bridge-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE BridgeAnalysis(trackId INTEGER PRIMARY KEY, sourceJSON BLOB); CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER)`); err != nil {
		t.Fatal(err)
	}
	source := struct{ Analysis *rekordbox.Analysis }{&rekordbox.Analysis{Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{100, 50, 25}, 600)}}}}
	b, _ := json.Marshal(source)
	if _, err = db.Exec(`INSERT INTO BridgeAnalysis VALUES(1,?);`, b); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO BridgeState VALUES('../Contents/a.flac',1)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w := bridgeDisplayWave(ctx, filepath.Join(dir, "m.db"), "../Contents/a.flac", 1, 44100, 4000)
	if w == nil || len(w.Bands) != 1800 {
		t.Fatal("persisted PWV7 detail not read")
	}
	for _, tc := range []struct {
		file, path string
		duration   float64
	}{
		{"stm.db", "../Contents/a.flac", 4000},
		{"m.db", "../Contents/other.flac", 4000},
		{"m.db", "../Contents/a.flac", 4500},
	} {
		if bridgeDisplayWave(ctx, filepath.Join(dir, tc.file), tc.path, 1, 44100, tc.duration) != nil {
			t.Fatalf("wrong bridge source accepted: %+v", tc)
		}
	}
}

func TestLegacyExcludesStaleSourceWithoutDisplayFlapping(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	now := time.Now()
	p := peerDeck{deckState: deckState{Deck: 2, Key: "track-a", Stale: true, Playing: true, PositionMS: 1234}, at: now}
	s.shadow.peerDecks[2] = p
	s.shadow.display.entries = map[string]*shadowDisplayEntry{displayPeerID(p): {asset: testDisplayAsset()}}
	if d := s.shadowComposeLegacy("13", now); len(d) != 0 {
		t.Fatalf("legacy took stale source: %+v", d)
	}
	if d := s.shadowComposeDisplay("13", now); len(d) != 1 || !d[0].Stale || d[0].PositionMS != 1234 {
		t.Fatalf("display discarded/followed stale source: %+v", d)
	}
}

func TestDisplayKeyStaysStableUntilConfirmedTrackReload(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot = t.TempDir()
	if err := os.WriteFile(filepath.Join(s.shadow.dataRoot, "song.mp3"), []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	post := func(net string) string {
		t.Helper()
		body, _ := json.Marshal([]deckState{{Deck: 1, Net: net}})
		w := httptest.NewRecorder()
		s.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", bytes.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		s.shadow.mu.Lock()
		defer s.shadow.mu.Unlock()
		return s.shadow.own[0].Key
	}
	path := "net://uuid/(Unknown)/song.mp3"
	first := post(path)
	if first == "" || post(path) != first {
		t.Fatal("steady source posts changed the asset key")
	}
	if post("") != "" {
		t.Fatal("eject did not clear the loaded key")
	}
	if post(path) == first {
		t.Fatal("reloading the same file reused stale grid/assets")
	}
}
