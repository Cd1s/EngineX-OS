package webui

// Regression checks for the shadow scratch and identity review findings.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// A CDJ that is merely paused must not be reported as scratching (#217).
func TestShadowCDJPauseIsNotScratch(t *testing.T) {
	sh := newShadowState()
	now := time.Unix(1_700_000_000, 0)
	ms := uint32(20000)
	pk := func() {
		sh.observeAt(prolink.Device{Number: 1, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: ms}, now)
	}
	for i := 0; i < 20; i++ {
		now = now.Add(30 * time.Millisecond)
		ms += 30
		pk()
		if sh.cdj[1].scratch {
			t.Fatalf("steady forward play flagged as scratch at packet %d", i)
		}
	}
	v0 := sh.ver
	n := 0
	for i := 0; i < 10; i++ {
		now = now.Add(30 * time.Millisecond)
		pk() // paused: same position
		if sh.cdj[1].scratch {
			n++
		}
	}
	t.Logf("after pause: %d scratch-flagged packets, %d version bumps", n, sh.ver-v0)
	if n != 0 {
		t.Fatalf("a plain pause produced %d scratch-flagged packets (about %d ms)", n, n*30)
	}
}

// CDJ jog mode repeats a 13 ms frame while paused, even before its delayed
// status packet admits the pause (#217). That jitter cannot hold Touch either.
func TestShadowCDJPausedFrameRepeatIsNotScratch(t *testing.T) {
	sh := newShadowState()
	now := time.Unix(1_700_000_000, 0)
	sh.observeAt(prolink.Device{Number: 1, PacketType: 0x0a, Playing: true, TrackID: 1}, now)
	for i := 0; i < 12; i++ {
		now = now.Add(30 * time.Millisecond)
		sh.observeAt(prolink.Device{Number: 1, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: uint32(20000 + (i%2)*13)}, now)
		if sh.cdj[1].scratch {
			t.Fatalf("paused frame repeat became scratch at packet %d", i)
		}
	}
	sh.observeAt(prolink.Device{Number: 1, PacketType: 0x0a, Playing: false, TrackID: 1}, now)
	now = now.Add(30 * time.Millisecond)
	sh.observeAt(prolink.Device{Number: 1, PacketType: 0x0b, PrecisePosition: true, PlayPositionMS: 19980}, now)
	if sh.cdj[1].scratch {
		t.Fatal("known paused CDJ engaged scratch")
	}
}

type shadowReviewFile struct{ rel, name, data string }

// shadowReviewLibrary builds an internal library whose m.db lists the given files (analysed).
func shadowReviewLibrary(t *testing.T, files ...shadowReviewFile) (dataRoot, mediaRoot string) {
	t.Helper()
	dataRoot, mediaRoot = t.TempDir(), t.TempDir()
	lib := filepath.Join(dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	for _, d := range []string{filepath.Join(lib, "Database2"), filepath.Join(dataRoot, "inMusicTempFiles")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dbFile, _, db := testlibrary.Create(t)
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range files {
		abs := filepath.Join(dataRoot, f.rel)
		os.MkdirAll(filepath.Dir(abs), 0o755)
		os.WriteFile(abs, []byte(f.data), 0o600)
		if _, err := db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES(?,?,128,1)`, "../../../../"+f.rel, f.name); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, _ := os.ReadFile(dbFile)
	os.WriteFile(filepath.Join(lib, "Database2", "m.db"), b, 0o600)
	return
}

func shadowReviewPost(t *testing.T, s *Server, np string) deckState {
	t.Helper()
	body, _ := json.Marshal([]deckState{{Deck: 1, Net: np}})
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("decksPost %d", w.Code)
	}
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	return s.shadow.own[0]
}

// Two different library tracks with the same file name and the same size, in two
// folders (the m.db lookup is ambiguous): their loads must not share a track key.
func TestShadowTempCopyKeysSeparateSameNameAndSize(t *testing.T) {
	dataRoot, mediaRoot := shadowReviewLibrary(t,
		shadowReviewFile{"Music/a/song.wav", "song.wav", "AAAAAAA"},
		shadowReviewFile{"Music/b/song.wav", "song.wav", "BBBBBBB"})
	os.WriteFile(filepath.Join(dataRoot, "inMusicTempFiles/1111-aaaa_song.wav"), []byte("AAAAAAA"), 0o600)
	os.WriteFile(filepath.Join(dataRoot, "inMusicTempFiles/2222-bbbb_song.wav"), []byte("BBBBBBB"), 0o600)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = dataRoot, mediaRoot
	d1 := shadowReviewPost(t, s, "net://u/(Unknown)/inMusicTempFiles/1111-aaaa_song.wav")
	d2 := shadowReviewPost(t, s, "net://u/(Unknown)/inMusicTempFiles/2222-bbbb_song.wav")
	t.Logf("baseKey A=%s B=%s", d1.baseKey, d2.baseKey)
	if d1.baseKey == "" || d1.baseKey == d2.baseKey {
		t.Fatalf("two different tracks share baseKey %q", d1.baseKey)
	}
}

// A temp copy whose file name matches exactly one library row but whose size does
// not (another file with the same name) must not be served that row's analysis.
func TestShadowTempPerfRejectsDifferentSize(t *testing.T) {
	dataRoot, mediaRoot := shadowReviewLibrary(t, shadowReviewFile{"Music/song.wav", "song.wav", "WAVDATA"})
	other := filepath.Join(dataRoot, "inMusicTempFiles/9999-zzzz_song.wav")
	os.WriteFile(other, []byte("A-COMPLETELY-DIFFERENT-LONGER-FILE"), 0o600)
	perf, err := libraryPerf(context.Background(), other, dataRoot, mediaRoot)
	if err == nil && len(perf) > 0 {
		t.Fatalf("served %d bytes of another file's analysis for a different-sized temp copy", len(perf))
	}
}

func TestShadowTempDisplayRejectsDifferentSizeAndAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []shadowReviewFile
		data  string
	}{
		{"size", []shadowReviewFile{{"Music/song.wav", "song.wav", "WAVDATA"}}, "different-size"},
		{"ambiguous", []shadowReviewFile{{"Music/a/song.wav", "song.wav", "AAAAAAA"}, {"Music/b/song.wav", "song.wav", "BBBBBBB"}}, "AAAAAAA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, media := shadowReviewLibrary(t, tc.files...)
			path := filepath.Join(data, "inMusicTempFiles/9999-zzzz_song.wav")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := libraryDisplay(context.Background(), path, data, media); err == nil {
				t.Fatal("display accepted another file's metadata")
			}
			if _, err := libraryPerf(context.Background(), path, data, media); err == nil {
				t.Fatal("perf accepted another file's analysis")
			}
		})
	}
}

func TestShadowTempResolutionIsCachedAndInvalidatesOnWrite(t *testing.T) {
	data, media := shadowReviewLibrary(t, shadowReviewFile{"Music/song.wav", "song.wav", "WAVDATA"})
	path := filepath.Join(data, "inMusicTempFiles/1111-aaaa_song.wav")
	if err := os.WriteFile(path, []byte("WAVDATA"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = data, media
	np := "net://u/(Unknown)/inMusicTempFiles/1111-aaaa_song.wav"
	first := shadowReviewPost(t, s, np)
	db := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db")
	if err := os.Rename(db, db+".hidden"); err != nil {
		t.Fatal(err)
	}
	// Removing the DB proves hot posts use the resolved identity without reopening
	// it. A timing threshold would be unreliable on shared ARM builders.
	for i := 0; i < 40; i++ {
		if next := shadowReviewPost(t, s, np); next.baseKey != first.baseKey {
			t.Fatal("hot post reopened the library instead of reusing resolution")
		}
	}
	if err := os.WriteFile(path, []byte("CHANGED"), 0600); err != nil {
		t.Fatal(err)
	}
	if next := shadowReviewPost(t, s, np); next.baseKey == first.baseKey {
		t.Fatal("edited temp file reused stale identity")
	}
}

func TestShadowUnresolvedTempFingerprintChangesAfterEdit(t *testing.T) {
	data, media := shadowReviewLibrary(t)
	path := filepath.Join(data, "inMusicTempFiles/1111-aaaa_song.wav")
	if err := os.WriteFile(path, []byte("AAAAAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = data, media
	np := "net://u/(Unknown)/inMusicTempFiles/1111-aaaa_song.wav"
	first := shadowReviewPost(t, s, np)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("BBBBBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime().Add(time.Second), st.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if next := shadowReviewPost(t, s, np); next.baseKey == first.baseKey {
		t.Fatal("same-sized edit reused stale analysis")
	}
}

func TestShadowDisplayCacheKeepsFourEntries(t *testing.T) {
	var c shadowDisplayCache
	for i := 0; i < 5; i++ {
		c.claim(strconv.Itoa(i), time.Unix(int64(i), 0))
	}
	if len(c.entries) != 4 || c.entries["0"] != nil || c.entries["4"] == nil {
		t.Fatal("receiver cache must retain only the four newest entries")
	}
}

func TestShadowSourceCacheHasByteBudget(t *testing.T) {
	data, media := shadowReviewLibrary(t, shadowReviewFile{"Music/song.wav", "song.wav", "WAVDATA"})
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = data, media
	c := &s.shadow.sourceDisplay
	c.entries = make(map[string]*shadowSourceEntry)
	for i := 0; i < 8; i++ {
		a := testDisplayAsset()
		a.Waveform = &enginedb.DisplayWave{Bands: make([]byte, 5<<20)}
		c.entries[strconv.Itoa(i)] = &shadowSourceEntry{asset: a, at: time.Unix(int64(i), 0)}
	}
	if _, err := s.sourceDisplayAsset(context.Background(), filepath.Join(data, "Music/song.wav"), "new", true); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, e := range c.entries {
		if e.asset.Waveform != nil {
			total += cap(e.asset.Waveform.Bands)
		}
	}
	if total > 32<<20 {
		t.Fatalf("source cache retained %d waveform bytes", total)
	}
}

func TestShadowDisplayReloadRefreshesMetadata(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := testDisplayAsset()
		a.Cues = []enginedb.DisplayCue{{I: 1, MS: 500, C: "#ffffff"}}
		a.Key = r.URL.Query().Get("key")
		if calls.Add(1) > 1 {
			a.Cues[0].MS += 100
			a.Beats[0].MS += 50
		}
		if err := json.NewEncoder(w).Encode(a); err != nil {
			t.Error(err)
		}
	}))
	defer peer.Close()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	u, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.shadow.peerPort, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	p := peerDeck{deckState: deckState{Deck: 1, Link: "{peer}", Key: "665fb348dd6a106e-18e78ca266f65244"}, ip: "127.0.0.1"}
	await := func() *shadowDisplayEntry {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if e := s.ensureShadowDisplay(p, time.Now()); e != nil && e.asset != nil {
				return e
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("display fetch did not finish")
		return nil
	}
	first := await()
	for i := 0; i < 40; i++ {
		s.ensureShadowDisplay(p, time.Now())
	}
	if calls.Load() != 1 {
		t.Fatal("same-session scratch updates refetched display")
	}
	p.Key = "665fb348dd6a106e-18e78cb900000001"
	second := await()
	if second.asset.Cues[0].MS == first.asset.Cues[0].MS || second.asset.Beats[0].MS == first.asset.Beats[0].MS {
		t.Fatal("reload reused stale grid or cues")
	}
	if first.asset.Key == second.asset.Key {
		t.Fatal("metadata revalidation did not bind to the new session")
	}
}
