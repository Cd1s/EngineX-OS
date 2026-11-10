package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Use the same wire-format rekordbox disk and NFS server as prolink's
// CDJ-3000 simulator. Only discovery may fetch metadata; display is offline.
func cachedDisplayCDJ(t *testing.T) (*prolink.MediaCache, prolink.Device) {
	t.Helper()
	return cachedDisplayCDJWith(t, nil)
}

func cachedDisplayCDJWith(t *testing.T, change func(string)) (*prolink.MediaCache, prolink.Device) {
	t.Helper()
	usb := t.TempDir()
	testfixture.Export(t, usb)
	// A real 150 Hz detail stream covering the fixture's 1.5 second beat grid.
	wave := make([]byte, 12+225*3)
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], 225)
	for i := 12; i < len(wave); i += 3 {
		copy(wave[i:], []byte{10, 20, 30})
	}
	testfixture.Write(t, usb, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, wave)))
	if change != nil {
		change(usb)
	}
	nfs := &prolink.NFSServer{Root: usb}
	if err := nfs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer nfs.Close()
	cache := &prolink.MediaCache{Dir: t.TempDir(), Portmap: nfs.PortmapPort()}
	discovery := prolink.NewDiscovery()
	discovery.SetMediaCache(cache)
	dev := discovery.Observe(prolink.Device{Address: "127.0.0.1", Name: "CDJ-3000", Number: 1, SourcePlayer: 1, SourceSlot: 3, TrackID: 1, PacketType: 10, Playing: true, PlayPositionMS: 500, BPM: 120})
	if len(dev.Waveform) == 0 {
		t.Fatal("simulated CDJ analysis was not cached")
	}
	return cache, dev
}

func TestCDJDisplayCachedANLZWithoutAudio(t *testing.T) {
	cache, dev := cachedDisplayCDJ(t)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{MediaCache: cache}}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	os.WriteFile(fourDeckRolePath, []byte("24"), 0600)
	uiPost(t, srv.URL, "fourDeckWaveform=1&prolinkShare=1")
	prolinkMu.Lock()
	postedDevices[s] = map[string]prolink.Device{deviceKey(dev): dev}
	prolinkMu.Unlock()
	get := func(path string, code int) []byte {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.HasSuffix(path, "/shadow") && w.Header().Get("X-Shadow-State") != "" {
			t.Fatal("CDJ positions hidden behind EngineX state version")
		}
		return w.Body.Bytes()
	}
	_, decks := shadowGetJSON(t, srv.URL+"/v1/sc5000/shadow")
	if len(decks) != 1 || decks[0].MetadataOnly || decks[0].Path != "" || decks[0].Net != "" || decks[0].Deck != 1 {
		t.Fatalf("CDJ display state: %+v", decks)
	}
	get("/v1/sc5000/shadow", 200)
	if wait := s.shadow.pollWait(false, time.Now()); wait != 120*time.Millisecond {
		t.Fatalf("cached CDJ positions must retain 120 ms polls, got %v", wait)
	}
	q := "?deck=1&key=" + url.QueryEscape(decks[0].Key)
	var meta struct {
		Title         string
		Detail        bool
		DurationMS    float64
		Beats         []enginedb.DisplayBeat
		Cues          []enginedb.DisplayCue
		WaveformTiles []string
	}
	if err := json.Unmarshal(get("/v1/sc5000/shadow/display"+q, 200), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Title != dev.Title || !meta.Detail || meta.DurationMS != 1500 || len(meta.Beats) != 4 || len(meta.Cues) < 4 || len(meta.WaveformTiles) != 1 {
		t.Fatalf("ANLZ display asset: %+v", meta)
	}
	for _, suffix := range []string{"", "&overview=1", "&tile=0", "&tile=0&low=ff0000&mid=00ff00&high=0000ff&rgb=1&saturation=1.612&brightAmp=0.473&brightGain=2.405&brightFloor=0.197"} {
		if _, err := png.Decode(bytes.NewReader(get("/v1/sc5000/shadow/waveform.png"+q+suffix, 200))); err != nil {
			t.Fatal(err)
		}
	}
	// Exact 0x0b position wins over the slower status packet.
	s.shadow.observe(prolink.Device{Number: 1, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 900})
	got := s.shadowComposeDisplay("24", time.Now())
	if got[0].PositionMS < 900 || got[0].PositionMS > 1000 {
		t.Fatalf("precise CDJ position lost: %+v", got[0])
	}
	assertNoShadowAudioFetch(t, s)
	if _, err := os.Stat(s.shadowDir()); !os.IsNotExist(err) {
		t.Fatalf("audio directory created: %v", err)
	}
	dev.TrackID++
	prolinkMu.Lock()
	postedDevices[s][deviceKey(dev)] = dev
	prolinkMu.Unlock()
	get("/v1/sc5000/shadow/display"+q, 404)
}

func TestCDJDisplayWithoutANLZIsMetadataOnly(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	dev := prolink.Device{Address: "192.0.2.3", Name: "CDJ-3000", Number: 3, SourcePlayer: 3, SourceSlot: 3, TrackID: 4, Title: "Unanalysed", LastSeen: time.Now()}
	prolinkMu.Lock()
	postedDevices[s] = map[string]prolink.Device{deviceKey(dev): dev}
	prolinkMu.Unlock()
	decks := s.shadowComposeDisplay("24", time.Now())
	if len(decks) != 1 || !decks[0].MetadataOnly || decks[0].Title != dev.Title || decks[0].Path != "" {
		t.Fatalf("metadata fallback: %+v", decks)
	}
	assertNoShadowAudioFetch(t, s)
}
func TestJC11DisplayRoleWithSC5000AndCDJ(t *testing.T) {
	cache, dev := cachedDisplayCDJ(t)
	oldRole, oldCompatible := fourDeckRolePath, deviceCompatiblePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "missing-role")
	deviceCompatiblePath = filepath.Join(t.TempDir(), "compatible")
	defer func() { fourDeckRolePath, deviceCompatiblePath = oldRole, oldCompatible }()
	for _, board := range []string{"jc11", "jc11s", "jp21", "jp24"} {
		t.Run(board, func(t *testing.T) {
			os.WriteFile(deviceCompatiblePath, []byte("inmusic,"+board+"\x00"), 0600)
			s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{MediaCache: cache}}, nil)
			defer s.Close()
			os.WriteFile(filepath.Join(s.uiDir(), "settings.conf"), []byte("prolink_share=1\n"), 0600)
			if s.activeRole(uiSettings{}) != "" || s.activeRole(uiSettings{FourDeckWaveform: true}) != "12" {
				t.Fatal("console must opt into role 12")
			}
			dev.LastSeen = time.Now()
			prolinkMu.Lock()
			postedDevices[s] = map[string]prolink.Device{deviceKey(dev): dev}
			prolinkMu.Unlock()
			// SC5000's source deck may belong to any role. Only the two free
			// console slots may be used, even when its source deck is 1 or 2.
			for src := 1; src <= 4; src++ {
				now := time.Now()
				p := peerDeck{deckState: deckState{Deck: src, Key: "track-a"}, ip: "192.0.2.1", at: now}
				s.shadow.peerDecks = map[int]peerDeck{src: p}
				s.shadow.display.entries = map[string]*shadowDisplayEntry{displayPeerID(p): {asset: testDisplayAsset()}}
				decks := s.shadowComposeDisplay(s.activeRole(uiSettings{FourDeckWaveform: true}), now)
				if len(decks) != 2 || decks[0].Deck != 3 || decks[1].Deck != 4 || decks[0].MetadataOnly || decks[1].MetadataOnly {
					t.Fatalf("source deck %d: %+v", src, decks)
				}
				if !strings.HasPrefix(decks[0].Key, "cdj:") && decks[0].Key != displayTrackKey(p) || !strings.HasPrefix(decks[1].Key, "cdj:") && decks[1].Key != displayTrackKey(p) {
					t.Fatalf("source identity lost: %+v", decks)
				}
			}
			prolinkMu.Lock()
			delete(postedDevices, s)
			prolinkMu.Unlock()
			s.shadow.peerDecks = map[int]peerDeck{}
			if decks := s.shadowComposeDisplay("12", time.Now()); len(decks) != 0 {
				t.Fatalf("empty console slots must be local: %+v", decks)
			}
		})
	}
}

func TestConsoleHostCDJLineInDisplay(t *testing.T) {
	oldRole, oldCompatible := fourDeckRolePath, deviceCompatiblePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "missing-role")
	deviceCompatiblePath = filepath.Join(t.TempDir(), "compatible")
	defer func() { fourDeckRolePath, deviceCompatiblePath = oldRole, oldCompatible }()
	os.WriteFile(deviceCompatiblePath, []byte("inmusic,jc11\x00rockchip,rk3288\x00"), 0600)

	cache, dev := cachedDisplayCDJ(t)
	dev.Number = 4 // CDJ set to Player 4 as in Reddit user report
	dev.SourcePlayer = 4
	if err := os.Symlink(filepath.Join(cache.Dir, "127.0.0.1", "1-3"), filepath.Join(cache.Dir, "127.0.0.1", "4-3")); err != nil {
		t.Fatal(err)
	}
	s := displayCDJServer(t, cache, dev)
	now := time.Now()

	// 1 CDJ (Player 4) with Channel 3 set to Line In: maps to Deck 3.
	decks := s.shadowComposeDisplay(s.activeRole(uiSettings{FourDeckWaveform: true}), now, []int{3})
	if len(decks) != 1 || decks[0].Deck != 3 || decks[0].MetadataOnly {
		t.Fatalf("CDJ on Channel 3 Line: %+v", decks)
	}
	if !strings.HasPrefix(decks[0].Key, "cdj:") {
		t.Fatalf("expected CDJ key prefix, got %s", decks[0].Key)
	}
	e := s.shadow.display.entries[decks[0].Key]
	if e == nil || e.asset == nil || len(e.wave) == 0 {
		t.Fatalf("display asset missing or incomplete: %+v", e)
	}

	// 2 CDJs (Player 2 and Player 4) with Channels 3 and 4 Line In: map in order (P2->3, P4->4).
	dev2 := dev
	dev2.Number = 2
	dev2.Address = "127.0.0.2"
	prolinkMu.Lock()
	postedDevices[s][deviceKey(dev2)] = dev2
	prolinkMu.Unlock()

	decks2 := s.shadowComposeDisplay(s.activeRole(uiSettings{FourDeckWaveform: true}), now, []int{3, 4})
	if len(decks2) != 2 || decks2[0].Deck != 3 || decks2[1].Deck != 4 {
		t.Fatalf("2 CDJs on Channels 3 and 4 Line: %+v", decks2)
	}

	// Without Line channels on console: retains status quo (decks 3 and 4).
	noLine := s.shadowComposeDisplay(s.activeRole(uiSettings{FourDeckWaveform: true}), now, nil)
	if len(noLine) != 2 || noLine[0].Deck != 3 || noLine[1].Deck != 4 {
		t.Fatalf("expected status quo without Line channels on console, got %+v", noLine)
	}
}

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
	if resp.Header.Get("X-Shadow-State") == "" {
		t.Fatal("EngineX-only display response lost version caching")
	}
	s.shadow.mu.Lock()
	p.MetadataOnly = true
	s.shadow.peerDecks[2] = p
	s.shadow.mu.Unlock()
	resp = get("/v1/sc5000/shadow")
	resp.Body.Close()
	if resp.Header.Get("X-Shadow-State") != "" {
		t.Fatal("CDJ continuous state must not be skipped by peer version")
	}
	s.shadow.mu.Lock()
	p.MetadataOnly = false
	s.shadow.peerDecks[2] = p
	s.shadow.mu.Unlock()
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
	// Combining overview and tile must not poison the detailed tile cache.
	palette := "&low=ff1428&mid=00e632&high=0078ff&rgb=1&saturation=1.612&brightAmp=0.473&brightGain=2.405&brightFloor=0.197"
	for _, extra := range []string{"&overview=1", ""} {
		r, err := http.Get(srv.URL + "/v1/sc5000/shadow/waveform.png" + q + "&tile=2" + palette + extra)
		if err != nil {
			t.Fatal(err)
		}
		im, err := png.Decode(r.Body)
		r.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if im.Bounds().Dx() != 3 || im.Bounds().Dy() != 128 {
			t.Fatalf("styled tile cache dimensions: %v", im.Bounds())
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

func TestShadowJitterFreezesThenExpiresOrConfirmsEject(t *testing.T) {
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
	if len(d) != 0 {
		t.Fatalf("expired display still occupies a slot: %+v", d)
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

// The same L/M/H fixture must change with the local palette and mix bands in RGB.
func TestShadowWaveLocalPalette(t *testing.T) {
	wave := enginedb.DisplayWave{Bands: []byte{180, 0, 0, 100, 80, 0, 0, 180, 0, 0, 100, 80, 0, 0, 180}}
	style := defaultShadowWaveStyle()
	style.Colors = [3]color.NRGBA{{255, 20, 40, 255}, {0, 230, 50, 255}, {0, 120, 255, 255}}
	style.RGB, style.Saturation, style.BrightAmp, style.BrightGain, style.BrightFloor = true, 1.612, 0.473, 2.405, 0.197
	b, err := renderShadowWaveStyle(wave, 5, 32, style)
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	// WaveFormFBORGB.ps with the measured local parameters above.
	golden := []color.NRGBA{{255, 0, 14, 255}, {255, 215, 4, 255}, {0, 255, 0, 255}, {0, 255, 255, 255}, {0, 202, 255, 255}}
	for x, want := range golden {
		if got := color.NRGBAModel.Convert(im.At(x, 16)); got != want {
			t.Fatalf("native RGB column %d: got %v want %v", x, got, want)
		}
	}
	if _, _, _, a := im.At(0, 4).RGBA(); a != 0 {
		t.Fatal("detail RGB edge must use native 0.9 amplitude")
	}
	style.Dim = 0.6
	b, err = renderShadowWaveStyle(wave, 5, 32, style)
	if err != nil {
		t.Fatal(err)
	}
	im, err = png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.NRGBAModel.Convert(im.At(0, 16)).(color.NRGBA); got.R != 153 || got.A != 255 {
		t.Fatalf("native elapsed RGB: %v", got)
	}
	if _, _, _, a := im.At(0, 4).RGBA(); a != 0 {
		t.Fatal("overview RGB must also use native 0.9 amplitude")
	}
	style.Dim = 1
	style.RGB = false
	b, err = renderShadowWaveStyle(wave, 5, 32, style)
	if err != nil {
		t.Fatal(err)
	}
	im, err = png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.NRGBAModel.Convert(im.At(0, 16)); got != style.Colors[0] {
		t.Fatalf("local low color: %v", got)
	}
	q := url.Values{"low": {"ff1428"}, "mid": {"00e632"}, "high": {"0078ff"}, "rgb": {"1"}, "saturation": {"1"}, "brightAmp": {"1"}, "brightGain": {"1"}, "brightFloor": {"1"}}
	if _, err := parseShadowWaveStyle(q); err != nil {
		t.Fatal(err)
	}
	q.Set("low", "80ff1428")
	parsed, err := parseShadowWaveStyle(q)
	if err != nil || parsed.Colors[0] != (color.NRGBA{255, 20, 40, 128}) {
		t.Fatalf("QColor alpha: %+v %v", parsed, err)
	}
	q.Set("brightGain", "NaN")
	if _, err := parseShadowWaveStyle(q); err == nil {
		t.Fatal("accepted nonfinite brightness")
	}
}

func assertNoShadowAudioFetch(t *testing.T, s *Server) {
	t.Helper()
	if n, c := s.shadow.fetchCalls.Load(), s.shadow.fetchCDJCalls.Load(); n != 0 || c != 0 {
		t.Fatalf("audio fetch calls=%d CDJ=%d", n, c)
	}
}

func displayCDJServer(t *testing.T, cache *prolink.MediaCache, dev prolink.Device) *Server {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("prolink_share=1\n"), 0600)
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{MediaCache: cache}}, nil)
	t.Cleanup(s.Close)
	prolinkMu.Lock()
	postedDevices[s] = map[string]prolink.Device{deviceKey(dev): dev}
	prolinkMu.Unlock()
	return s
}

func TestCDJDisplayFiltersCueAndLoopLimits(t *testing.T) {
	cache, dev := cachedDisplayCDJWith(t, func(root string) {
		os.Remove(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT"))
		hot := make([]byte, 8)
		binary.BigEndian.PutUint32(hot, 1)
		binary.BigEndian.PutUint16(hot[4:], 9)
		for i := uint32(1); i <= 8; i++ {
			hot = append(hot, testfixture.CueExtended(i, i*100, 0, fmt.Sprint(i), [3]byte{})...)
		}
		hot = append(hot, testfixture.CueExtended(9, 1600, 0, "invalid", [3]byte{})...)
		memory := make([]byte, 8)
		binary.BigEndian.PutUint16(memory[4:], 4)
		for _, ms := range []uint32{200, 490, 1200} {
			memory = append(memory, testfixture.CueExtended(0, ms, 0, "memory", [3]byte{})...)
		}
		memory = append(memory, testfixture.CueExtended(0, 700, 1000, "loop", [3]byte{})...)
		// No old cues; the fixture beat grid and detail still cover 1500 ms.
		dat, err := rekordbox.ParseAnalysis(testfixture.DAT())
		if err != nil {
			t.Fatal(err)
		}
		beats := make([]byte, 12+len(dat.Beats)*8)
		binary.BigEndian.PutUint32(beats[8:], uint32(len(dat.Beats)))
		for i, b := range dat.Beats {
			off := 12 + i*8
			binary.BigEndian.PutUint16(beats[off:], b.Number)
			binary.BigEndian.PutUint16(beats[off+2:], uint16(b.BPM*100))
			binary.BigEndian.PutUint32(beats[off+4:], b.TimeMS)
		}
		testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT", testfixture.PMAI(testfixture.Section("PQTZ", 24, beats), testfixture.Section("PCO2", 20, hot), testfixture.Section("PCO2", 20, memory)))
	})
	s := displayCDJServer(t, cache, dev)
	decks := s.shadowComposeDisplay("24", time.Now())
	if len(decks) != 1 || decks[0].MetadataOnly {
		t.Fatalf("waveform lost: %+v", decks)
	}
	e := s.shadow.display.entries[decks[0].Key]
	if e == nil || e.asset.Waveform == nil || len(e.wave) == 0 || len(e.asset.Cues) != 9 || len(e.asset.Loops) != 1 {
		t.Fatalf("filtered asset: %+v", e)
	}
	for i := 0; i < 8; i++ {
		if e.asset.Cues[i].I != i+1 {
			t.Fatal(e.asset.Cues)
		}
	}
	if c := e.asset.Cues[8]; c.I != 9 || c.MS != 490 {
		t.Fatalf("nearest memory / main CUE collision: %+v", c)
	}
	if err := validateDisplayAsset(e.asset, decks[0].Key); err != nil {
		t.Fatal(err)
	}
	assertNoShadowAudioFetch(t, s)
}

func TestCDJDisplayNegativeCacheAndConcurrentCompose(t *testing.T) {
	cache, dev := cachedDisplayCDJ(t)
	root := filepath.Join(cache.Dir, dev.Address, "1-3", "PIONEER/USBANLZ/P001")
	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		if err := os.Remove(filepath.Join(root, "ANLZ0000"+ext)); err != nil {
			t.Fatal(err)
		}
	}
	cache = &prolink.MediaCache{Dir: cache.Dir}
	s := displayCDJServer(t, cache, dev)
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 50; i++ {
		s.shadowComposeDisplay("24", now)
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.shadowComposeDisplay("24", now) }()
	}
	wg.Wait()
	if n := cache.PDBParseCount(); n != 1 {
		t.Fatalf("50 failed composes parsed PDB %d times", n)
	}
	for _, e := range s.shadow.display.entries {
		if e.busy || e.asset != nil || !e.retry.Equal(now.Add(3*time.Second)) {
			t.Fatalf("negative cache: %+v", e)
		}
	}
	// ANLZ appearing during backoff is retried afterwards without reparsing PDB.
	testfixture.Write(t, filepath.Join(cache.Dir, dev.Address, "1-3"), "PIONEER/USBANLZ/P001/ANLZ0000.EXT", testfixture.EXT())
	if got := s.shadowComposeDisplay("24", now.Add(time.Second)); !got[0].MetadataOnly {
		t.Fatal("negative cache ignored")
	}
	dev.LastSeen = now.Add(4 * time.Second)
	prolinkMu.Lock()
	postedDevices[s][deviceKey(dev)] = dev
	prolinkMu.Unlock()
	if got := s.shadowComposeDisplay("24", now.Add(4*time.Second)); got[0].MetadataOnly {
		t.Fatal("negative cache did not retry")
	}
	assertNoShadowAudioFetch(t, s)
}

func TestCDJDisplayMediumSwapAndShareOff(t *testing.T) {
	cache, dev := cachedDisplayCDJ(t)
	s := displayCDJServer(t, cache, dev)
	before := s.shadowComposeDisplay("24", time.Now())
	root := filepath.Join(cache.Dir, dev.Address, "1-3")
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb.stamp", []byte("disc-two"))
	// Change the same track ID's waveform, with no audio service available.
	os.Remove(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX"))
	after := s.shadowComposeDisplay("24", time.Now())
	if before[0].Key == after[0].Key || after[0].MetadataOnly {
		t.Fatalf("medium swap: %+v -> %+v", before, after)
	}
	if s.shadow.display.entries[after[0].Key].asset.Waveform != nil {
		t.Fatal("old detail waveform survived disc swap")
	}
	os.WriteFile(filepath.Join(s.uiDir(), "settings.conf"), []byte("prolink_share=0\n"), 0600)
	off := s.shadowComposeDisplay("24", time.Now())
	if !off[0].MetadataOnly || off[0].Key == after[0].Key {
		t.Fatalf("share off used old cache: %+v", off)
	}
	assertNoShadowAudioFetch(t, s)
}

func TestDisplayPeerExpiresAndReleasesConsoleSlots(t *testing.T) {
	for _, role := range []string{"12", "24", "13"} {
		t.Run(role, func(t *testing.T) {
			s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
			defer s.Close()
			now := time.Now()
			for i := 1; i <= 2; i++ {
				p := peerDeck{deckState: deckState{Deck: i, Key: fmt.Sprint(i)}, ip: "192.0.2.1", at: now}
				s.shadow.peerDecks[i] = p
				s.shadow.display.entries = map[string]*shadowDisplayEntry{}
			}
			for _, p := range s.shadow.peerDecks {
				s.shadow.display.entries[displayPeerID(p)] = &shadowDisplayEntry{asset: testDisplayAsset()}
			}
			if decks := s.shadowComposeDisplay(role, now.Add(15*time.Second)); len(decks) != 2 {
				t.Fatalf("early expiry: %+v", decks)
			}
			if decks := s.shadowComposeDisplay(role, now.Add(15*time.Second+time.Millisecond)); len(decks) != 0 {
				t.Fatalf("shadow slots still locked: %+v", decks)
			}
		})
	}
}

func TestShadowAudioFetchCountersDetectCalls(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	assertNoShadowAudioFetch(t, s)
	done := make(chan struct{})
	s.shadow.mu.Lock()
	s.shadowFetch("counter", func(context.Context) (string, error) { close(done); return "", errors.New("counter test") })
	// Mark busy to exercise the CDJ entry without starting an NFS request.
	s.shadow.busy["cdj-counter"] = true
	s.shadowFetchCDJ("cdj-counter", nil, 3, 1)
	s.shadow.mu.Unlock()
	<-done
	if s.shadow.fetchCalls.Load() != 2 || s.shadow.fetchCDJCalls.Load() != 1 {
		t.Fatal("counters missed a download entry")
	}
}

func TestCDJDisplayFiltersMalformedAndExcessLoops(t *testing.T) {
	a := testDisplayAsset()
	cues := []rekordbox.Cue{{HotCue: 1, TimeMS: 9000}, {HotCue: 99, TimeMS: 100}, {TimeMS: 500, EndMS: 400, Loop: true}, {TimeMS: 500, EndMS: 9000, Loop: true}}
	for i := 0; i < 12; i++ {
		cues = append(cues, rekordbox.Cue{TimeMS: uint32(i * 100), EndMS: uint32(i*100 + 50), Loop: true})
	}
	filterCDJDisplayCues(a, cues, 100)
	if len(a.Loops) != 8 || len(a.Cues) != 9 {
		t.Fatalf("loop/cue caps: %d/%d", len(a.Loops), len(a.Cues))
	}
	if err := validateDisplayAsset(a, a.Key); err != nil {
		t.Fatal(err)
	}
	for _, c := range a.Cues {
		if c.I == 0 {
			t.Fatal("memory cue became main CUE")
		}
	}
}

func TestCDJOverviewUsesPDBDuration(t *testing.T) {
	cache, dev := cachedDisplayCDJWith(t, func(root string) {
		os.Remove(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX"))
		// Only a PWAV overview: last beat is 1500ms, PDB duration is 2000ms.
		dat := testfixture.DAT()
		overview := make([]byte, 8+4)
		binary.BigEndian.PutUint32(overview, 4)
		copy(overview[8:], []byte{1, 2, 3, 4})
		dat = append(dat, testfixture.Section("PWAV", 20, overview)...)
		binary.BigEndian.PutUint32(dat[8:], uint32(len(dat)))
		testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT", dat)
		os.Remove(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT"))
	})
	s := displayCDJServer(t, cache, dev)
	decks := s.shadowComposeDisplay("24", time.Now())
	if len(decks) != 1 || decks[0].MetadataOnly {
		t.Fatal(decks)
	}
	a := s.shadow.display.entries[decks[0].Key].asset
	if a.DurationMS != 2000 || a.Waveform != nil {
		t.Fatalf("overview duration: %v detail=%v", a.DurationMS, a.Waveform != nil)
	}
}

func TestCDJSourceHostDeterministicForDuplicatePlayerNumbers(t *testing.T) {
	devices := []prolink.Device{{Number: 2, Address: "192.0.2.9"}, {Number: 2, Address: "192.0.2.1"}}
	if host := sourceHost(prolink.ExportStatus{}, devices, 2); host.String() != "192.0.2.1" {
		t.Fatal(host)
	}
	slices.Reverse(devices)
	if host := sourceHost(prolink.ExportStatus{}, devices, 2); host.String() != "192.0.2.1" {
		t.Fatal(host)
	}
}

func TestCDJOverviewFiltersBeatBeyondPDBDuration(t *testing.T) {
	cache, dev := cachedDisplayCDJWith(t, func(root string) {
		for _, ext := range []string{".EXT", ".2EX"} {
			os.Remove(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000"+ext))
		}
		beats := make([]byte, 28)
		binary.BigEndian.PutUint32(beats[8:], 2)
		for i, ms := range []uint32{0, 2500} {
			off := 12 + i*8
			binary.BigEndian.PutUint16(beats[off:], uint16(i+1))
			binary.BigEndian.PutUint16(beats[off+2:], 12000)
			binary.BigEndian.PutUint32(beats[off+4:], ms)
		}
		overview := make([]byte, 12)
		binary.BigEndian.PutUint32(overview, 4)
		copy(overview[8:], []byte{1, 2, 3, 4})
		testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT", testfixture.PMAI(testfixture.Section("PQTZ", 24, beats), testfixture.Section("PWAV", 20, overview)))
	})
	s := displayCDJServer(t, cache, dev)
	decks := s.shadowComposeDisplay("24", time.Now())
	if len(decks) != 1 || decks[0].MetadataOnly {
		t.Fatal("rounded PDB duration rejected overview", decks)
	}
	a := s.shadow.display.entries[decks[0].Key].asset
	if a.DurationMS != 2000 || len(a.Beats) != 1 {
		t.Fatalf("duration=%v beats=%+v", a.DurationMS, a.Beats)
	}
}

// Exercise the UI's real poll/asset routes with the console's default role 12.
func consoleLineHTTPServer(t *testing.T) (*Server, string) {
	t.Helper()
	oldRole, oldCompatible := fourDeckRolePath, deviceCompatiblePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "missing-role")
	deviceCompatiblePath = filepath.Join(t.TempDir(), "compatible")
	t.Cleanup(func() { fourDeckRolePath, deviceCompatiblePath = oldRole, oldCompatible })
	if err := os.WriteFile(deviceCompatiblePath, []byte("inmusic,jc11\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	cache, dev := cachedDisplayCDJ(t)
	s := displayCDJServer(t, cache, dev)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	if code, _ := uiPost(t, srv.URL, "fourDeckWaveform=1&prolinkShare=1"); code != http.StatusOK {
		t.Fatalf("enable shadow: %d", code)
	}
	return s, srv.URL
}

func TestConsoleLineStateClearedWhenParameterMissing(t *testing.T) {
	_, base := consoleLineHTTPServer(t)
	_, decks := shadowGetJSON(t, base+"/v1/sc5000/shadow?line=1")
	if len(decks) != 1 || decks[0].Deck != 1 || !decks[0].LineIn {
		t.Fatalf("Line channel 1: %+v", decks)
	}
	_, decks = shadowGetJSON(t, base+"/v1/sc5000/shadow")
	if len(decks) != 1 || decks[0].Deck != 4 || decks[0].LineIn {
		t.Fatalf("switch back to Engine must restore default slot: %+v", decks)
	}
}

func TestConsoleLineCDJAssetsAllChannels(t *testing.T) {
	_, base := consoleLineHTTPServer(t)
	for channel := 1; channel <= 4; channel++ {
		t.Run(strconv.Itoa(channel), func(t *testing.T) {
			_, decks := shadowGetJSON(t, base+"/v1/sc5000/shadow?line="+strconv.Itoa(channel))
			if len(decks) != 1 || decks[0].Deck != channel || !decks[0].LineIn || decks[0].MetadataOnly {
				t.Fatalf("Line channel %d: %+v", channel, decks)
			}
			q := "?deck=" + strconv.Itoa(channel) + "&key=" + url.QueryEscape(decks[0].Key)
			for _, route := range []string{"display", "waveform.png"} {
				resp, err := http.Get(base + "/v1/sc5000/shadow/" + route + q)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("channel %d %s: %d", channel, route, resp.StatusCode)
				}
			}
		})
	}
}

func TestConsoleLineKeepsSCPlayerSlots(t *testing.T) {
	s, _ := consoleLineHTTPServer(t)
	now := time.Now()
	p := peerDeck{deckState: deckState{Deck: 3, Key: "sc-track", Title: "SC player"}, at: now}
	s.shadow.peerDecks = map[int]peerDeck{3: p}
	s.shadow.busy["peer:sc-track"] = true
	s.shadow.display.entries = map[string]*shadowDisplayEntry{displayPeerID(p): {asset: testDisplayAsset()}}
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			compose := s.shadowComposeDisplay
			if legacy {
				compose = s.shadowComposeLegacy
			}
			decks := compose("12", now, []int{1})
			if len(decks) != 2 || decks[0].Deck != 1 || !decks[0].LineIn || decks[0].Title == p.Title || decks[1].Deck != 3 || decks[1].LineIn || decks[1].Title != p.Title {
				t.Fatalf("CDJ must use Line 1 and SC must stay on 3: %+v", decks)
			}
			// A Line slot already occupied by the original peer mapping cannot
			// redirect CDJ to a non-Line slot or mark the peer as a Line source.
			decks = compose("12", now, []int{3})
			if len(decks) != 1 || decks[0].Deck != 3 || decks[0].LineIn || decks[0].Title != p.Title {
				t.Fatalf("occupied Line slot: %+v", decks)
			}
		})
	}
	prolinkMu.Lock()
	delete(postedDevices, s)
	prolinkMu.Unlock()
	for _, compose := range []func(string, time.Time, ...[]int) []deckState{s.shadowComposeDisplay, s.shadowComposeLegacy} {
		decks := compose("12", now, []int{1})
		if len(decks) != 1 || decks[0].Deck != 3 || decks[0].LineIn || decks[0].Title != p.Title {
			t.Fatalf("without CDJ, Line must keep stock behavior and SC stays on 3: %+v", decks)
		}
	}
}

func TestOverviewHalfWaveGrowsFromTheBottomEdge(t *testing.T) {
	q := url.Values{"low": {"0000ff"}, "mid": {"00ff00"}, "high": {"ff0000"}, "saturation": {"1"}, "brightAmp": {"1"}, "brightGain": {"1"}, "brightFloor": {"0"}}
	wave := enginedb.DisplayWave{Bands: []byte{64, 32, 16}} // a quiet column: 112/255 of the height
	for _, half := range []bool{false, true} {
		if half {
			q.Set("half", "1")
			wave.Bands = []byte{255, 114, 51} // layer tops at 19.2, 31.1 and 39.8 rows
		}
		style, err := parseShadowWaveStyle(q)
		if err != nil || style.Half != half {
			t.Fatalf("style: %+v %v", style, err)
		}
		b, err := renderShadowWaveStyle(wave, 1, 64, style)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		drawn := func(y int) bool { _, _, _, a := img.At(0, y).RGBA(); return a != 0 }
		if half { // the local Praline overview: low, mid, high stacked from the bottom edge
			at := func(row int) color.NRGBA { return color.NRGBAModel.Convert(img.At(0, 63-row)).(color.NRGBA) }
			low, mid, high := color.NRGBA{0, 0, 255, 255}, color.NRGBA{0, 255, 0, 255}, color.NRGBA{255, 0, 0, 255}
			if at(0) != low || at(18) != low || at(20) != mid || at(30) != mid || at(32) != high || at(38) != high {
				t.Fatalf("stock stacking: %v %v %v %v %v %v", at(0), at(18), at(20), at(30), at(32), at(38))
			}
			if edge := at(39); edge.R != 255 || edge.A < 150 || edge.A > 250 || drawn(23) || drawn(0) {
				t.Fatalf("top edge must be blended by coverage and stop at 39.8 rows: %v", edge)
			}
		} else if !drawn(32) || !drawn(31) || drawn(63) || drawn(0) || drawn(16) != drawn(47) {
			t.Fatal("default stays mirrored around the centre")
		}
	}
}

func TestDisplayPeerTrackOnOwnDrive(t *testing.T) {
	// The peer has no drive: it plays our file over Engine's network, cannot
	// read its analysis and sends no Key. This deck's own library has it.
	_, root, db := testlibrary.Create(t)
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
	os.MkdirAll(filepath.Join(root, "Contents"), 0700)
	if err = os.WriteFile(filepath.Join(root, "Contents/song.flac"), []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), filepath.Dir(root)
	now := time.Now()
	peer := peerDeck{deckState: deckState{Deck: 1, Net: "net://uuid/" + filepath.Base(root) + " (USB 3)/Contents/song.flac", Title: "From the peer"}, ip: "127.0.0.1", at: now}
	s.shadow.peerDecks[1] = peer
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		d := s.shadowCompose("24", now)
		if len(d) != 1 || d[0].Key == "" || d[0].Path != "" || d[0].Net != "" {
			t.Fatalf("display state: %+v", d)
		}
		if !d[0].Loading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("display never ready")
		}
	}
	local := s.localPeerTrack(peer, now)
	s.shadow.display.mu.Lock()
	e := s.shadow.display.entries[displayPeerID(local)]
	s.shadow.display.mu.Unlock()
	if e == nil || e.asset == nil || e.asset.Title != "Title" || len(e.asset.Beats) != 2 || len(e.overview) == 0 {
		t.Fatalf("own library not used: %+v", e)
	}
	if !s.shadowSourceLoaded(local.Key) {
		t.Fatal("the envelope worker would drop a peer track on our drive")
	}
	peer.Net = "net://uuid/USB/elsewhere.flac"
	if got := s.localPeerTrack(peer, now); got.Key != "" || got.local != "" {
		t.Fatalf("a track that is not on this deck got a key: %+v", got)
	}
}
