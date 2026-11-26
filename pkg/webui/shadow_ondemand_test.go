package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"io"
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

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func assertOverviewBandMaxima(t *testing.T, label string, raw []byte) {
	t.Helper()
	if len(raw) < 27 || len(raw)%3 != 0 {
		t.Fatalf("%s: bad uncompressed overview len=%d", label, len(raw))
	}
	var want [3]byte
	for i, v := range raw[24 : len(raw)-3] {
		want[i%3] = max(want[i%3], v)
	}
	if want[0]|want[1]|want[2] == 0 {
		t.Fatalf("%s: expected non-silent overview bins", label)
	}
	got := raw[len(raw)-3:]
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("%s: overview band-maxima trailer = %v, want %v", label, got, want)
	}
}

func parseEXPFOverviewAndBPM(t *testing.T, expf []byte) (bpm float64, rawTrack, rawOverview []byte) {
	t.Helper()
	if len(expf) < 16 || string(expf[:4]) != "EXPF" {
		t.Fatalf("not EXPF (%d bytes)", len(expf))
	}
	bpm = float64FromBits(binary.LittleEndian.Uint64(expf[8:16]))
	off := 16
	parts := make([][]byte, 3)
	for i := range parts {
		if len(expf)-off < 4 {
			t.Fatalf("EXPF truncated at part %d", i)
		}
		n := int(binary.LittleEndian.Uint32(expf[off : off+4]))
		off += 4
		if len(expf)-off < n {
			t.Fatalf("EXPF part %d truncated", i)
		}
		parts[i] = expf[off : off+n]
		off += n
	}
	return bpm, parts[0], parts[2]
}

func parseRBXA1Blobs(t *testing.T, b []byte) (rawTrack, rawOverview []byte) {
	t.Helper()
	if !bytes.HasPrefix(b, []byte(shadowAnalysisMagic)) {
		t.Fatalf("not RBXA1 (%d bytes)", len(b))
	}
	rest := b[len(shadowAnalysisMagic):]
	var parts [5][]byte
	for i := range parts {
		n := int(binary.LittleEndian.Uint32(rest[:4]))
		rest = rest[4:]
		parts[i] = rest[:n]
		rest = rest[n:]
	}
	var err error
	if rawTrack, err = enginedb.QUncompress(parts[0], 4096); err != nil {
		t.Fatalf("uncompress trackData: %v", err)
	}
	if rawOverview, err = enginedb.QUncompress(parts[1], shadowAnalysisMax); err != nil {
		t.Fatalf("uncompress overview: %v", err)
	}
	return rawTrack, rawOverview
}

func float64FromBits(u uint64) float64 {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], u)
	var f float64
	_ = binary.Read(bytes.NewReader(b[:]), binary.LittleEndian, &f)
	return f
}

func makeOnDemandStickFixture(t *testing.T, mediaRoot, volName string) (stick, relPath, libUUID string, tr *rbstate.Track) {
	t.Helper()
	stick = filepath.Join(mediaRoot, volName)
	testfixture.LargeExport(t, stick, 1)
	const mp3Frame = 417
	longMP3 := make([]byte, 120*mp3Frame)
	for i := 0; i < 120; i++ {
		copy(longMP3[i*mp3Frame:], []byte{0xff, 0xfb, 0x90, 0})
	}
	copy(longMP3[36:], "Xing")
	binary.BigEndian.PutUint32(longMP3[40:], 3)
	binary.BigEndian.PutUint32(longMP3[44:], 120)
	binary.BigEndian.PutUint32(longMP3[48:], uint32(len(longMP3)))
	copy(longMP3[52:], "LAME3.100")
	longMP3[73], longMP3[74], longMP3[75] = 0x24, 0, 0
	relPath = testfixture.LargeTrackPath(1)
	testfixture.Write(t, stick, relPath, longMP3)
	if _, err := bridge.ConvertAtMount(context.Background(), stick, bridge.MountOptions{OnDemand: true}); err != nil {
		t.Fatalf("ConvertAtMount(OnDemand): %v", err)
	}
	snap, err := rbstate.Load(filepath.Join(stick, "Engine Library", "Database2"))
	if err != nil {
		t.Fatalf("rbstate.Load: %v", err)
	}
	tr, ok := snap.TrackAt("../" + relPath)
	if !ok || tr == nil {
		t.Fatalf("snapshot missing ../%s", relPath)
	}
	return stick, relPath, snap.LibraryUUID, tr
}

// Proves onDemandPeerPerf closes the 404 across 0 ms (including BPM=0), 100 ms,
// 1 s and 3 s, shares the single on-demand build across the source load and two
// asking shadow copies (EXPF + native), drops the cache when the deck unloads,
// and emits full band-maxima overview trailers on both EXPF and native RBXA1.
func TestShadowOnDemandReusesSingleBuildAndEvictsOnUnload(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	mediaRoot := t.TempDir()
	oldMedia := shadowMediaRoot
	shadowMediaRoot = mediaRoot
	defer func() { shadowMediaRoot = oldMedia }()

	stick, relPath, libUUID, tr := makeOnDemandStickFixture(t, mediaRoot, "ONDE_REUSE")

	s := New(Config{
		UIDir:                 t.TempDir(),
		StateDir:              t.TempDir(),
		MediaRoot:             mediaRoot,
		RekordboxOnDemandSock: "-",
	}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()

	// 1. Deck posts track at t=0ms with BPM=0 (before OriginalBPM settles) while
	//    the source deck's own on-demand hook call runs concurrently with two shadow copies.
	netPath := "net://u/ONDE_REUSE/" + relPath
	postDeck := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /v1/enginex/decks: %v %v", resp, err)
		}
		resp.Body.Close()
	}
	postDeck(`[{"deck":1,"net":` + strconv.Quote(netPath) + `,"bpm":0}]`)
	dresp, err := http.Get(srv.URL + "/peer/decks")
	if err != nil {
		t.Fatal(err)
	}
	var peer struct{ Decks []deckState }
	_ = json.NewDecoder(dresp.Body).Decode(&peer)
	dresp.Body.Close()
	if len(peer.Decks) != 1 || peer.Decks[0].Key == "" {
		t.Fatalf("peer decks: %+v", peer.Decks)
	}
	key := peer.Decks[0].Key

	// Source deck invokes on-demand endpoint first (same as Engine's sqlite3_bind hook),
	// immediately followed concurrently by two shadow copies requesting EXPF and native.
	odPayload, _ := json.Marshal(rbOnDemandRequest{
		Mount: stick,
		UUID:  libUUID,
		Path:  "../" + relPath,
		ID:    tr.EngineID,
		BPM:   120,
	})
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	// Source deck on-demand fill
	odReq := httptest.NewRequest(http.MethodPost, RekordboxOnDemandRoute, bytes.NewReader(odPayload))
	odReq.RemoteAddr = "127.0.0.1:4242"
	odRec := httptest.NewRecorder()
	s.ServeHTTP(odRec, odReq)
	if odRec.Code != http.StatusOK {
		t.Fatalf("source on-demand status=%d body=%s", odRec.Code, odRec.Body.String())
	}

	// Two shadow copies asking concurrently at t=0ms (EXPF + native each, with BPM=0 on deck).
	for copyIdx := 0; copyIdx < 2; copyIdx++ {
		for _, q := range []string{"", "&format=native"} {
			wg.Add(1)
			go func(copyIdx int, q string) {
				defer wg.Done()
				resp, err := http.Get(srv.URL + "/peer/decks/1/perf?key=" + url.QueryEscape(key) + q)
				if err != nil {
					errCh <- err
					return
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errCh <- context.DeadlineExceeded
					t.Errorf("copy %d query %q: status=%d body=%q", copyIdx, q, resp.StatusCode, body)
					return
				}
				if q == "" {
					bpm, rawTr, rawOv := parseEXPFOverviewAndBPM(t, body)
					if bpm <= 0 {
						t.Errorf("copy %d EXPF bpm=%v with deck BPM=0", copyIdx, bpm)
					}
					if len(rawTr) != 68 {
						t.Errorf("copy %d EXPF trackData len=%d, want 68", copyIdx, len(rawTr))
					}
					assertOverviewBandMaxima(t, "ondemand-expf", rawOv)
				} else {
					if !validShadowAnalysis(body) {
						t.Errorf("copy %d invalid native RBXA1", copyIdx)
					}
					rawTr, rawOv := parseRBXA1Blobs(t, body)
					if len(rawTr) != 68 {
						t.Errorf("copy %d native trackData len=%d, want 68", copyIdx, len(rawTr))
					}
					assertOverviewBandMaxima(t, "ondemand-native", rawOv)
				}
			}(copyIdx, q)
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if got := s.rbOnDemandBuildCount.Load(); got != 1 {
		t.Fatalf("on-demand build count = %d after source + 2 copies (EXPF+native), want 1", got)
	}

	// Unload Deck 1: cache entry must be dropped immediately.
	postDeck(`[{"deck":1,"net":""}]`)
	s.rbOnDemandCacheMu.Lock()
	remaining := len(s.rbOnDemandCache)
	s.rbOnDemandCacheMu.Unlock()
	if remaining != 0 {
		t.Fatalf("on-demand cache retained %d entries after deck unload, want 0", remaining)
	}
}

func TestShadowOnDemandEvictsInMusicTempTwoDecksAndRebuiltLibrary(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	dataRoot := t.TempDir()
	mediaRoot := t.TempDir()
	oldData, oldMedia := shadowDataRoot, shadowMediaRoot
	shadowDataRoot, shadowMediaRoot = dataRoot, mediaRoot
	defer func() { shadowDataRoot, shadowMediaRoot = oldData, oldMedia }()

	stick, relPath, _, _ := makeOnDemandStickFixture(t, mediaRoot, "ONDE_TEMP")
	stickAudio := filepath.Join(stick, filepath.FromSlash(relPath))
	rawAudio, err := os.ReadFile(stickAudio)
	if err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(dataRoot, "inMusicTempFiles")
	_ = os.MkdirAll(tempDir, 0o755)
	tempAudio := filepath.Join(tempDir, "1_"+filepath.Base(relPath))
	if err := os.WriteFile(tempAudio, rawAudio, 0o644); err != nil {
		t.Fatal(err)
	}
	dataLib := filepath.Join(dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	_ = os.MkdirAll(filepath.Join(dataLib, "Database2"), 0o755)
	relToStick, err := filepath.Rel(dataLib, stickAudio)
	if err != nil {
		t.Fatal(err)
	}
	tmpDBFile, _, tdb := testlibrary.Create(t)
	if _, err := tdb.Exec(`INSERT INTO Track(path,filename,isAnalyzed) VALUES(?,?,0)`, filepath.ToSlash(relToStick), filepath.Base(relPath)); err != nil {
		t.Fatal(err)
	}
	_ = tdb.Close()
	rawTDB, _ := os.ReadFile(tmpDBFile)
	if err := os.WriteFile(filepath.Join(dataLib, "Database2", "m.db"), rawTDB, 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{
		UIDir:                 t.TempDir(),
		StateDir:              t.TempDir(),
		MediaRoot:             mediaRoot,
		RekordboxOnDemandSock: "-",
	}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()

	postDeck := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /v1/enginex/decks: %v %v", resp, err)
		}
		resp.Body.Close()
	}
	getPeerKey := func(deck int) string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/peer/decks")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var peer struct{ Decks []deckState }
		_ = json.NewDecoder(resp.Body).Decode(&peer)
		for _, d := range peer.Decks {
			if d.Deck == deck && d.Key != "" {
				return d.Key
			}
		}
		t.Fatalf("missing key for deck %d: %+v", deck, peer.Decks)
		return ""
	}
	fetchNative := func(deck int, key string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/peer/decks/" + strconv.Itoa(deck) + "/perf?key=" + url.QueryEscape(key) + "&format=native")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("deck %d /perf status=%d body=%q", deck, resp.StatusCode, b)
		}
	}

	// Both Deck 1 (via inMusicTempFiles) and Deck 2 load the same on-demand track.
	netTemp := "net://u/(Unknown)/inMusicTempFiles/" + filepath.Base(tempAudio)
	netUSB := "net://u/ONDE_TEMP/" + relPath
	postDeck(`[{"deck":1,"net":` + strconv.Quote(netTemp) + `,"bpm":120},{"deck":2,"net":` + strconv.Quote(netUSB) + `,"bpm":120}]`)
	k1 := getPeerKey(1)
	k2 := getPeerKey(2)
	fetchNative(1, k1)
	fetchNative(2, k2)
	if got := s.rbOnDemandBuildCount.Load(); got != 1 {
		t.Fatalf("build count with inMusicTempFiles + deck 2 = %d, want 1", got)
	}

	// Unloading Deck 2 keeps the cache alive while Deck 1 still holds the track.
	postDeck(`[{"deck":1,"net":` + strconv.Quote(netTemp) + `,"bpm":120},{"deck":2,"net":""}]`)
	s.rbOnDemandCacheMu.Lock()
	kept := len(s.rbOnDemandCache)
	s.rbOnDemandCacheMu.Unlock()
	if kept != 1 {
		t.Fatalf("cache size after unloading 1 of 2 decks = %d, want 1", kept)
	}

	// Even if Engine deletes the inMusicTempFiles copy right before Deck 1 unloads,
	// evicting via d.libraryPath drops the cache entry.
	_ = os.Remove(tempAudio)
	postDeck(`[{"deck":1,"net":""}]`)
	s.rbOnDemandCacheMu.Lock()
	afterUnload := len(s.rbOnDemandCache)
	s.rbOnDemandCacheMu.Unlock()
	if afterUnload != 0 {
		t.Fatalf("inMusicTempFiles unload left %d cache entries, want 0", afterUnload)
	}

	// Reload on Deck 2, populate cache, then simulate an ordinary Engine m.db
	// update (same inode, same Information.uuid, same track row): must NOT
	// invalidate the cache (FIX 3).
	postDeck(`[{"deck":2,"net":` + strconv.Quote(netUSB) + `,"bpm":120}]`)
	k2 = getPeerKey(2)
	fetchNative(2, k2)
	if got := s.rbOnDemandBuildCount.Load(); got != 2 {
		t.Fatalf("build count after reload = %d, want 2", got)
	}
	mdb := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	future := time.Now().Add(3 * time.Second)
	_ = os.Chtimes(mdb, future, future)
	fetchNative(2, k2)
	if got := s.rbOnDemandBuildCount.Load(); got != 2 {
		t.Fatalf("build count after ordinary m.db mtime update = %d, want 2 (no invalidation)", got)
	}
	// A real library replacement (new m.db inode via atomic rewrite) MUST invalidate and rebuild.
	mdbBytes, err := os.ReadFile(mdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mdb+".rebuilt", mdbBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(mdb+".rebuilt", mdb); err != nil {
		t.Fatal(err)
	}
	fetchNative(2, k2)
	if got := s.rbOnDemandBuildCount.Load(); got != 3 {
		t.Fatalf("build count after replaced m.db inode = %d, want 3", got)
	}

	// Mid-flight unload cancels any active flight and marks it evicted so a
	// completing build cannot repopulate the cache after unload (without
	// growing any unbounded eviction-generation map).
	trackKey := onDemandCacheKey(stick, "../"+relPath)
	fCtx, fCancel := context.WithCancel(context.Background())
	defer fCancel()
	fakeID := onDemandVerifiedID{mount: stick, trackRel: relPath}
	flight := &rbOnDemandFlight{done: make(chan struct{}), cancel: fCancel, waiters: 1, trackKey: trackKey}
	s.rbOnDemandCacheMu.Lock()
	if s.rbOnDemandFlights == nil {
		s.rbOnDemandFlights = map[onDemandVerifiedID]*rbOnDemandFlight{}
	}
	s.rbOnDemandFlights[fakeID] = flight
	s.rbOnDemandCacheMu.Unlock()
	s.evictOnDemandCacheForPath(stickAudio)
	select {
	case <-fCtx.Done():
	default:
		t.Fatal("evictOnDemandCacheForPath did not cancel active in-flight build")
	}
	s.rbOnDemandCacheMu.Lock()
	if s.rbOnDemandFlights[fakeID] != nil || !flight.evicted {
		s.rbOnDemandCacheMu.Unlock()
		t.Fatalf("mid-flight eviction did not clear flight or mark evicted")
	}
	s.rbOnDemandCacheMu.Unlock()
}

// Covers all 5 load sources (stock Engine USB, rekordbox on-demand, rekordbox
// already in m.db with legacy zero-maxima overview, Pro DJ Link, streaming track
// via stm.db) and verifies zero 404s, proper 503 wait for in-progress analysis,
// full band-maxima trailers on BOTH EXPF and native RBXA1, and saveShadowAnalysis
// 503 retry.
func TestShadowNeverReanalysesAllSourcesAndTimings(t *testing.T) {
	shadowBeaconOn = false
	oldWait, oldTries := perfRetryWait, perfRetries
	perfRetryWait, perfRetries = 5*time.Millisecond, 5
	defer func() {
		shadowBeaconOn = true
		perfRetryWait, perfRetries = oldWait, oldTries
	}()

	dataRoot := t.TempDir()
	mediaRoot := t.TempDir()
	oldData, oldMedia := shadowDataRoot, shadowMediaRoot
	shadowDataRoot, shadowMediaRoot = dataRoot, mediaRoot
	defer func() { shadowDataRoot, shadowMediaRoot = oldData, oldMedia }()

	makePerf := func(zeroMaxima bool) enginedb.Performance {
		t.Helper()
		p, err := enginedb.EncodePerformance(
			rekordbox.Track{Duration: 4, SampleRate: 44100, SampleCount: 4 * 44100, BPM: 126},
			&rekordbox.Analysis{
				Beats: []rekordbox.Beat{
					{TimeMS: 250, BPM: 126, Number: 1},
					{TimeMS: 726, BPM: 126, Number: 2},
				},
				Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{180, 120, 60}, 60)}},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if zeroMaxima {
			rawOv, err := enginedb.QUncompress(p.Overview, shadowAnalysisMax)
			if err != nil {
				t.Fatal(err)
			}
			rawOv[len(rawOv)-3], rawOv[len(rawOv)-2], rawOv[len(rawOv)-1] = 0, 0, 0
			p.Overview, err = enginedb.QCompress(rawOv)
			if err != nil {
				t.Fatal(err)
			}
			rawTr, err := enginedb.QUncompress(p.TrackData, 4096)
			if err != nil {
				t.Fatal(err)
			}
			// 44-byte legacy trackData with zero peaks.
			legacy := make([]byte, 44)
			copy(legacy[:20], rawTr[:20])
			p.TrackData, err = enginedb.QCompress(legacy)
			if err != nil {
				t.Fatal(err)
			}
		}
		return p
	}

	s := New(Config{
		UIDir:                 t.TempDir(),
		StateDir:              t.TempDir(),
		MediaRoot:             mediaRoot,
		RekordboxOnDemandSock: "-",
	}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()

	postAndKey := func(body string) string {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("post: %v %v", resp, err)
		}
		resp.Body.Close()
		dresp, err := http.Get(srv.URL + "/peer/decks")
		if err != nil {
			t.Fatal(err)
		}
		var peer struct{ Decks []deckState }
		_ = json.NewDecoder(dresp.Body).Decode(&peer)
		dresp.Body.Close()
		if len(peer.Decks) != 1 || peer.Decks[0].Key == "" {
			t.Fatalf("peer decks: %+v", peer.Decks)
		}
		return peer.Decks[0].Key
	}

	fetch := func(key, q string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/peer/decks/1/perf?key=" + url.QueryEscape(key) + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	checkBothFormats := func(label, key string) {
		t.Helper()
		code, expf := fetch(key, "")
		if code != http.StatusOK {
			t.Fatalf("%s EXPF status=%d (%q)", label, code, expf)
		}
		bpm, rawTr, rawOv := parseEXPFOverviewAndBPM(t, expf)
		if bpm <= 0 || len(rawTr) != 68 {
			t.Fatalf("%s EXPF bpm=%v trackLen=%d", label, bpm, len(rawTr))
		}
		assertOverviewBandMaxima(t, label+"-expf", rawOv)

		code, nat := fetch(key, "&format=native")
		if code != http.StatusOK || !validShadowAnalysis(nat) {
			t.Fatalf("%s native status=%d valid=%v", label, code, validShadowAnalysis(nat))
		}
		rawTrN, rawOvN := parseRBXA1Blobs(t, nat)
		if len(rawTrN) != 68 || binary.BigEndian.Uint64(rawTrN[20:28]) == 0 {
			t.Fatalf("%s native trackData missing 5.3.0 peaks (len=%d)", label, len(rawTrN))
		}
		assertOverviewBandMaxima(t, label+"-native", rawOvN)
	}

	// Source 1: Stock Engine-analysed USB stick.
	t.Run("stock-usb", func(t *testing.T) {
		file, _, db := testlibrary.Create(t)
		p := makePerf(false)
		if _, err := db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../Music/stock.wav','stock.wav',126,1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
			t.Fatal(err)
		}
		db.Close()
		vol := filepath.Join(mediaRoot, "STOCK_USB")
		_ = os.MkdirAll(filepath.Join(vol, "Engine Library", "Database2"), 0o755)
		_ = os.MkdirAll(filepath.Join(vol, "Music"), 0o755)
		rawDB, _ := os.ReadFile(file)
		_ = os.WriteFile(filepath.Join(vol, "Engine Library", "Database2", "m.db"), rawDB, 0o644)
		_ = os.WriteFile(filepath.Join(vol, "Music", "stock.wav"), []byte("WAV"), 0o644)
		key := postAndKey(`[{"deck":1,"net":"net://u/STOCK_USB/Music/stock.wav","bpm":0}]`)
		checkBothFormats("stock-usb", key)
	})

	// Source 2: Rekordbox stick already in m.db with legacy [0,0,0] overview maxima.
	t.Run("rb-mdb-legacy-maxima", func(t *testing.T) {
		file, _, db := testlibrary.Create(t)
		p := makePerf(true)
		if _, err := db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../Contents/legacy.wav','legacy.wav',126,1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
			t.Fatal(err)
		}
		db.Close()
		vol := filepath.Join(mediaRoot, "RB_MDB")
		_ = os.MkdirAll(filepath.Join(vol, "Engine Library", "Database2"), 0o755)
		_ = os.MkdirAll(filepath.Join(vol, "Contents"), 0o755)
		rawDB, _ := os.ReadFile(file)
		_ = os.WriteFile(filepath.Join(vol, "Engine Library", "Database2", "m.db"), rawDB, 0o644)
		_ = os.WriteFile(filepath.Join(vol, "Contents", "legacy.wav"), []byte("WAV"), 0o644)
		key := postAndKey(`[{"deck":1,"net":"net://u/RB_MDB/Contents/legacy.wav","bpm":126}]`)
		checkBothFormats("rb-mdb-legacy-maxima", key)
	})

	// Source 3: Pro DJ Link (delayed analysis returns 503 while running, then 200 even with BPM=0).
	t.Run("prolink", func(t *testing.T) {
		oldDir := rbxAnalysisDir
		rbxAnalysisDir = t.TempDir()
		defer func() { rbxAnalysisDir = oldDir }()
		audio := filepath.Join(t.TempDir(), "prolink.mp3")
		_ = os.WriteFile(audio, []byte("MP3"), 0o644)
		gid := s.tidal.addTrack(trackRef{
			track: providers.Track{ID: "x\x00192.0.2.9\x0077", Provider: rekordboxScope, Title: "Link"},
			file:  audio,
			drive: filepath.Dir(audio),
		})
		rbxAnalysing.Store(gid, true)
		key := postAndKey(`[{"deck":1,"net":"streaming://TIDAL/Track/` + strconv.FormatInt(gid, 10) + `","bpm":0}]`)
		if code, _ := fetch(key, ""); code != http.StatusServiceUnavailable {
			t.Fatalf("prolink while analysing: status=%d, want 503", code)
		}
		p := makePerf(true) // even if Pro DJ Link cache had zero maxima, shadow output must fill them
		if !rbxPublishAnalysis(rbxAnalysisPath(gid), p, 126, false) {
			t.Fatal("rbxPublishAnalysis failed")
		}
		rbxAnalysing.Delete(gid)
		checkBothFormats("prolink", key)
	})

	// Source 4: Streaming track in stm.db (returns 503 while unanalysed on source, then 200;
	// also verifies URI lookup when d.Path came from gateway cache rather than Engine's download path).
	t.Run("streaming-stm-db", func(t *testing.T) {
		file, _, db := testlibrary.Create(t)
		uri := "streaming://TIDAL/Track/9001"
		if _, err := db.Exec(`INSERT INTO Track(path,filename,uri,title,artist,isAnalyzed) VALUES('../../../../downloads/TIDAL/s9001.wav','s9001.wav',?,'StreamSong','StreamArtist',0)`, uri); err != nil {
			t.Fatal(err)
		}
		db.Close()
		lib := filepath.Join(dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
		_ = os.MkdirAll(filepath.Join(lib, "Database2"), 0o755)
		_ = os.MkdirAll(filepath.Join(dataRoot, "downloads", "TIDAL"), 0o755)
		stmPath := filepath.Join(lib, "Database2", "stm.db")
		rawDB, _ := os.ReadFile(file)
		_ = os.WriteFile(stmPath, rawDB, 0o644)
		_ = os.WriteFile(filepath.Join(dataRoot, "downloads", "TIDAL", "s9001.wav"), []byte("WAV"), 0o644)

		key := postAndKey(`[{"deck":1,"net":"` + uri + `","bpm":0}]`)
		if code, _ := fetch(key, ""); code != http.StatusServiceUnavailable {
			t.Fatalf("streaming unanalysed EXPF: status=%d, want 503", code)
		}
		if code, _ := fetch(key, "&format=native"); code != http.StatusServiceUnavailable {
			t.Fatalf("streaming unanalysed native: status=%d, want 503", code)
		}

		// Source Engine finishes analysing into stm.db.
		p := makePerf(true)
		stmDB, err := sql.Open("sqlite", stmPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stmDB.Exec(`UPDATE Track SET isAnalyzed=1, bpmAnalyzed=126 WHERE uri=?`, uri); err != nil {
			t.Fatal(err)
		}
		if _, err := stmDB.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=? WHERE trackId=(SELECT id FROM Track WHERE uri=?)`,
			p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops, uri); err != nil {
			t.Fatal(err)
		}
		stmDB.Close()
		checkBothFormats("streaming-stm-db", key)

		// Also verify URI lookup when d.Path came from gateway cache (not Engine's
		// download path), so libraryPerfFormat misses and stmURIPerf is required.
		gwAudio := filepath.Join(t.TempDir(), "gateway-cached-9002.wav")
		_ = os.WriteFile(gwAudio, []byte("WAV"), 0o644)
		gid := s.tidal.addTrack(trackRef{
			track: providers.Track{ID: "gw-9002", Provider: "TIDAL", Title: "StreamGW"},
			file:  gwAudio,
			drive: filepath.Dir(gwAudio),
		})
		gwURI := "streaming://TIDAL/Track/" + strconv.FormatInt(gid, 10)
		stmDB2, err := sql.Open("sqlite", stmPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stmDB2.Exec(`INSERT INTO Track(path,filename,uri,title,artist,isAnalyzed,bpmAnalyzed) VALUES('../../../../downloads/TIDAL/s9002.wav','s9002.wav',?,'StreamGW','StreamArtist',1,126)`, gwURI); err != nil {
			t.Fatal(err)
		}
		if _, err := stmDB2.Exec(`INSERT OR REPLACE INTO PerformanceData(trackId,trackData,overviewWaveFormData,beatData,quickCues,loops) VALUES((SELECT id FROM Track WHERE uri=?),?,?,?,?,?)`,
			gwURI, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
			t.Fatal(err)
		}
		stmDB2.Close()
		gwKey := postAndKey(`[{"deck":1,"net":"` + gwURI + `","bpm":0}]`)
		checkBothFormats("streaming-gateway-uri", gwKey)
	})

	// Source 5: SongLoading=true at t=0ms returns 503 instead of 404, and saveShadowAnalysis retries 503.
	t.Run("songloading-and-native-retry", func(t *testing.T) {
		p := makePerf(false)
		natBody, err := encodeShadowAnalysis(p)
		if err != nil {
			t.Fatal(err)
		}
		var calls int
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls < 3 {
				http.Error(w, "analysing", http.StatusServiceUnavailable)
				return
			}
			w.Write(natBody)
		}))
		defer mock.Close()
		local := filepath.Join(t.TempDir(), "copy.wav")
		if why := saveShadowAnalysis(context.Background(), local, mock.URL); why != "" || calls != 3 {
			t.Fatalf("saveShadowAnalysis after 503 retry: why=%q calls=%d", why, calls)
		}
		if _, err := os.Stat(local + ".analysis"); err != nil {
			t.Fatalf("missing staged .analysis: %v", err)
		}

		// Also verify SongLoading=true yields 503 on /perf when analysis isn't in m.db yet.
		vol := filepath.Join(mediaRoot, "LOADING_USB")
		_ = os.MkdirAll(filepath.Join(vol, "Music"), 0o755)
		_ = os.WriteFile(filepath.Join(vol, "Music", "loading.wav"), []byte("WAV"), 0o644)
		key := postAndKey(`[{"deck":1,"net":"net://u/LOADING_USB/Music/loading.wav","songLoading":true}]`)
		if code, _ := fetch(key, ""); code != http.StatusServiceUnavailable {
			t.Fatalf("songLoading=true status=%d, want 503", code)
		}
	})
}

// BenchmarkShadowOnDemandPeerPerf measures the source's extra work per copy
// request on ARM hardware: cold on-demand build (once per track load, shared
// with the source deck), cached EXPF copy request, cached native RBXA1 copy
// request, and legacy zero-maxima overview repair in encodeShadowAnalysis.
func BenchmarkShadowOnDemandPeerPerf(b *testing.B) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	mediaRoot := b.TempDir()
	oldMedia := shadowMediaRoot
	shadowMediaRoot = mediaRoot
	defer func() { shadowMediaRoot = oldMedia }()

	stick := filepath.Join(mediaRoot, "BENCH_USB")
	testfixture.LargeExport(b, stick, 1)
	const mp3Frame = 417
	longMP3 := make([]byte, 120*mp3Frame)
	for i := 0; i < 120; i++ {
		copy(longMP3[i*mp3Frame:], []byte{0xff, 0xfb, 0x90, 0})
	}
	copy(longMP3[36:], "Xing")
	binary.BigEndian.PutUint32(longMP3[40:], 3)
	binary.BigEndian.PutUint32(longMP3[44:], 120)
	binary.BigEndian.PutUint32(longMP3[48:], uint32(len(longMP3)))
	copy(longMP3[52:], "LAME3.100")
	longMP3[73], longMP3[74], longMP3[75] = 0x24, 0, 0
	relPath := testfixture.LargeTrackPath(1)
	testfixture.Write(b, stick, relPath, longMP3)
	if _, err := bridge.ConvertAtMount(context.Background(), stick, bridge.MountOptions{OnDemand: true}); err != nil {
		b.Fatal(err)
	}

	s := New(Config{
		UIDir:                 b.TempDir(),
		StateDir:              b.TempDir(),
		MediaRoot:             mediaRoot,
		RekordboxOnDemandSock: "-",
	}, nil)
	defer s.Close()
	d := deckState{Deck: 1, Path: filepath.Join(stick, filepath.FromSlash(relPath)), BPM: 120}
	ctx := context.Background()

	b.Run("ColdOnDemandBuild_OncePerLoad", func(b *testing.B) {
		for b.Loop() {
			s.evictOnDemandCacheForPath(d.Path)
			if _, err := s.onDemandPeerPerf(ctx, d, true); err != nil {
				b.Fatal(err)
			}
		}
	})
	// Prime cache once (as the source deck's own on-demand fill does).
	if _, err := s.onDemandPeerPerf(ctx, d, true); err != nil {
		b.Fatal(err)
	}
	b.Run("CachedCopy_NativeRBXA1", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.onDemandPeerPerf(ctx, d, true); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("CachedCopy_EXPF", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.onDemandPeerPerf(ctx, d, false); err != nil {
				b.Fatal(err)
			}
		}
	})

	// Legacy [0,0,0] band-maxima repair cost in encodeShadowAnalysis.
	p, err := enginedb.EncodePerformance(
		rekordbox.Track{Duration: 4, SampleRate: 44100, SampleCount: 4 * 44100, BPM: 126},
		&rekordbox.Analysis{
			Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 126, Number: 1}, {TimeMS: 726, BPM: 126, Number: 2}},
			Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{180, 120, 60}, 60)}},
		},
	)
	if err != nil {
		b.Fatal(err)
	}
	rawOv, _ := enginedb.QUncompress(p.Overview, shadowAnalysisMax)
	rawOv[len(rawOv)-3], rawOv[len(rawOv)-2], rawOv[len(rawOv)-1] = 0, 0, 0
	p.Overview, _ = enginedb.QCompress(rawOv)
	b.Run("EncodeShadowAnalysis_RepairZeroMaxima", func(b *testing.B) {
		for b.Loop() {
			if _, err := encodeShadowAnalysis(p); err != nil {
				b.Fatal(err)
			}
		}
	})
}
