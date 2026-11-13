package webui

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
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

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestShadowScratchBumpsPollAndStopsForwardCarry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	old := peerDeck{
		deckState: deckState{Deck: 1, Key: "k1", Playing: true, Scratch: true, PositionMS: 10000, Pitch: 0},
		anchorPos: 10000,
		anchorAt:  now,
	}
	// A 1 ms hold while scratching at +100 ms later must NOT be treated as a +100 ms forward line:
	// old.anchorPos is 10000, d.PositionMS is 10000 (within 5 ms), whereas 100 ms later +1x would expect 10100.
	if !samePeerPicture(old, deckState{Deck: 1, Key: "k1", Playing: true, Scratch: true, PositionMS: 10002}, now.Add(100*time.Millisecond)) {
		t.Fatal("stationary platter while scratching must stay on anchor")
	}
	// Moving >= 6 ms while scratching bumps immediately.
	if samePeerPicture(old, deckState{Deck: 1, Key: "k1", Playing: true, Scratch: true, PositionMS: 9990}, now.Add(25*time.Millisecond)) {
		t.Fatal("10 ms backward scratch must wake long poll")
	}
	// Scratch edge always wakes long poll.
	if samePeerPicture(old, deckState{Deck: 1, Key: "k1", Playing: true, Scratch: false, PositionMS: 10000}, now) {
		t.Fatal("scratch edge must wake long poll")
	}

	sh := newShadowState()
	sh.own = []deckState{{Deck: 1, Playing: true, Scratch: true, PositionMS: 10000, At: now.UnixMilli(), BPM: 128}}
	sh.ownAt = now
	own := sh.ownForPeer(now.Add(100 * time.Millisecond))
	if len(own) != 1 || own[0].PositionMS != 10000 {
		t.Fatalf("ownForPeer extrapolated while scratching: %+v", own)
	}

	sh.takePeer("192.0.2.10", []deckState{{Deck: 1, Key: "k1", Playing: true, Scratch: true, PositionMS: 10000, Seq: 1, Sent: now.UnixMilli()}}, now)
	sh.takePeer("192.0.2.10", []deckState{{Deck: 1, Key: "k1", Playing: true, Scratch: true, PositionMS: 9980, Seq: 2, Sent: now.UnixMilli()}}, now.Add(50*time.Millisecond))
	if got := sh.peerDecks[1].PositionMS; got != 9980 {
		t.Fatalf("takePeer added late forward carry while scratching: got %v, want 9980", got)
	}

	// CDJ backward scratch on absolute position packets bumps version and sets Scratch.
	v0 := sh.ver
	sh.observeAt(prolink.Device{Number: 1, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 20000, Pitch: 0}, now)
	sh.observeAt(prolink.Device{Number: 1, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 19980, Pitch: 0}, now.Add(30*time.Millisecond))
	if sh.ver == v0 || !sh.cdj[1].scratch {
		t.Fatalf("CDJ backward scratch did not bump or mark scratch: ver %d->%d scratch=%v", v0, sh.ver, sh.cdj[1].scratch)
	}
}

func TestShadowLegacyReusesTrackPathAcrossReloads(t *testing.T) {
	var mu sync.Mutex
	audioCalls := map[string]int{}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if r.URL.Path == "/peer/decks/1/audio" {
			mu.Lock()
			audioCalls[key]++
			mu.Unlock()
			w.Write([]byte("AUDIO-TRACK-X"))
			return
		}
		http.NotFound(w, r)
	}))
	defer peer.Close()

	dir := t.TempDir()
	s := New(Config{UIDir: filepath.Join(dir, "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), t.TempDir()
	u, _ := url.Parse(peer.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())

	load := func(key string) deckState {
		t.Helper()
		s.shadow.mu.Lock()
		s.shadow.peerDecks[1] = peerDeck{
			deckState: deckState{Deck: 1, Key: key, Ext: ".flac", Name: "track.flac", Title: "Track", DurationMS: 1000},
			ip:        "127.0.0.1",
			at:        time.Now(),
		}
		s.shadow.mu.Unlock()
		return until(t, func() deckState {
			out := s.shadowComposeLegacy("24", time.Now())
			if len(out) != 1 {
				t.Fatalf("compose: %+v", out)
			}
			return out[0]
		}, "load "+key, func(d deckState) bool { return d.Path != "" })
	}

	k1 := "665fb348dd6a106e-18e78ca266f65244"
	k2 := "665fb348dd6a106e-18e78cb900000001"
	d1 := load(k1)
	d2 := load(k2)
	if d1.ID != k1 || d2.ID != k2 {
		t.Fatalf("session IDs must distinguish reloads: %q vs %q", d1.ID, d2.ID)
	}
	if d1.Path != d2.Path {
		t.Fatalf("reloaded track got a new path %q instead of reusing %q", d2.Path, d1.Path)
	}
	mu.Lock()
	totalFetches := audioCalls[k1] + audioCalls[k2]
	mu.Unlock()
	if totalFetches != 1 {
		t.Fatalf("expected 1 audio download across reloads, got %d (%v)", totalFetches, audioCalls)
	}

	// Even after 2-minute cleanup removes the cached audio file, the next reload uses
	// the exact same shadow-v3 path so Engine hits its existing m.db entry.
	s.shadow.mu.Lock()
	s.shadow.used["peer:665fb348dd6a106e"] = time.Now().Add(-3 * time.Minute)
	s.shadowCleanCopies("")
	s.shadow.mu.Unlock()
	k3 := "665fb348dd6a106e-18e78cc000000002"
	d3 := load(k3)
	if d3.Path != d1.Path {
		t.Fatalf("post-cleanup reload changed path from %q to %q", d1.Path, d3.Path)
	}
}

func TestShadowInMusicTempFilesPreservesKeyAndPerf(t *testing.T) {
	dataRoot := t.TempDir()
	mediaRoot := t.TempDir()
	lib := filepath.Join(dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	if err := os.MkdirAll(filepath.Join(lib, "Database2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataRoot, "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataRoot, "inMusicTempFiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(dataRoot, "Music/song.wav")
	if err := os.WriteFile(orig, []byte("WAVDATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create m.db in lib with analyzed Track + PerformanceData for ../../../../Music/song.wav.
	tmpDBFile, _, tmpDB := testlibrary.Create(t)
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmpDB.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../../../../Music/song.wav','song.wav',128,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tmpDB.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
		t.Fatal(err)
	}
	mdbBytes, err := os.ReadFile(tmpDBFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "Database2", "m.db"), mdbBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	temp1 := filepath.Join(dataRoot, "inMusicTempFiles/1111-aaaa_song.wav")
	temp2 := filepath.Join(dataRoot, "inMusicTempFiles/2222-bbbb_song.wav")
	if err := os.WriteFile(temp1, []byte("WAVDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(temp2, []byte("WAVDATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = dataRoot, mediaRoot

	post := func(netPath string) deckState {
		body, _ := json.Marshal([]deckState{{Deck: 1, Net: netPath, Playing: false}})
		req := httptest.NewRequest("POST", "/v1/sc5000/decks", bytes.NewReader(body))
		w := httptest.NewRecorder()
		s.decksPost(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("decksPost %d", w.Code)
		}
		s.shadow.mu.Lock()
		defer s.shadow.mu.Unlock()
		return s.shadow.own[0]
	}

	d1 := post("net://u/(Unknown)/inMusicTempFiles/1111-aaaa_song.wav")
	d2 := post("net://u/(Unknown)/inMusicTempFiles/2222-bbbb_song.wav")
	if d1.baseKey == "" || d1.baseKey != d2.baseKey {
		t.Fatalf("inMusicTempFiles changed baseKey across loads: %q vs %q", d1.baseKey, d2.baseKey)
	}
	req := httptest.NewRequest("GET", "/peer/decks/1/perf?key="+url.QueryEscape(d2.Key), nil)
	req.SetPathValue("n", "1")
	w := httptest.NewRecorder()
	s.peerPerfGet(w, req)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "EXPF") {
		t.Fatalf("peerPerfGet failed for inMusicTempFiles copy: %d %q", w.Code, w.Body.String())
	}
}

func TestShadowDisplayReusesWaveformAcrossReloads(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	path := filepath.Join(root, "Contents/song.wav")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const rate = 8000
	const samples = 2 * rate
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, uint32(36+samples*2))
	wav.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		binary.Write(&wav, binary.LittleEndian, v)
	}
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, uint32(samples*2))
	for i := 0; i < samples; i++ {
		binary.Write(&wav, binary.LittleEndian, int16(16000*math.Sin(2*math.Pi*100*float64(i)/rate)))
	}
	if err := os.WriteFile(path, wav.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 2, SampleRate: rate}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 120, Number: 1}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../Contents/song.wav','song.wav',120,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
		t.Fatal(err)
	}

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.mediaRoot = filepath.Dir(root)
	k1 := "665fb348dd6a106e-18e78ca266f65244"
	k2 := "665fb348dd6a106e-18e78cb900000001"
	s.shadow.own = []deckState{{Deck: 1, Key: k1, Path: path}}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		a, err := s.sourceDisplayAsset(context.Background(), path, k1, true)
		if err != nil {
			t.Fatal(err)
		}
		if !a.DetailPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial source envelope never completed")
		}
	}

	// Reload on another session key (k2) and empty own-deck worker guard: must hit cache immediately without re-decoding.
	s.shadow.own = []deckState{{Deck: 1, Key: k2, Path: path}}
	a2, err := s.sourceDisplayAsset(context.Background(), path, k2, true)
	if err != nil || a2.DetailPending || !a2.Detail || a2.Key != k2 || a2.Waveform == nil {
		t.Fatalf("reload did not reuse cached waveform: err=%v a=%+v", err, a2)
	}

	// A new load refreshes receiver metadata; the source envelope above remains cached.
	p1 := peerDeck{deckState: deckState{Deck: 1, Key: k1, Link: "{peer}"}, ip: "192.0.2.1"}
	p2 := peerDeck{deckState: deckState{Deck: 1, Key: k2, Link: "{peer}"}, ip: "192.0.2.1"}
	if displayPeerID(p1) == displayPeerID(p2) || displayTrackKey(p1) == displayTrackKey(p2) {
		t.Fatalf("display metadata identity did not change on reload: %q vs %q", displayPeerID(p1), displayPeerID(p2))
	}
}
