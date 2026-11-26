package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func validTestNativeAnalysis(t testing.TB) []byte {
	t.Helper()
	p, err := enginedb.EncodePerformance(
		rekordbox.Track{Duration: 4, SampleRate: 44100, SampleCount: 4 * 44100, BPM: 126},
		&rekordbox.Analysis{
			Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 126, Number: 1}, {TimeMS: 726, BPM: 126, Number: 2}},
			Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{180, 120, 60}, 40)}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodeShadowAnalysis(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// BLOCK 1 (reviewer repro): a source 404 on /perf must never publish bare audio
// for Engine's OfflineAnalyzer.
func TestReviewShadowDoesNotPublishWithoutAnalysis(t *testing.T) {
	peer := &loadingPeer{audio: map[string][]loadingAnswer{
		"missing": {{body: "AUDIO"}},
	}}
	_, source, deck := loadingServer(t, peer)
	source("missing")
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if d := deck(); d.Path != "" {
			t.Fatal("audio was published without analysis after source /perf returned 404")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if peer.called("missing") != 0 {
		t.Fatalf("audio downloaded %d times when /perf returned 404, want 0", peer.called("missing"))
	}
	if d := deck(); d.Error != "no analysis" {
		t.Fatalf("deck error = %q, want \"no analysis\"", d.Error)
	}
}

// BLOCK 1 (extended): native 503 retry exhaustion blocks publication on players,
// first-request network error stays pending, EXPF DB query requires isAnalyzed=1,
// and older EXPF-only peers require explicit capability absence.
func TestReviewShadowBlock1FailClosedAndNegotiation(t *testing.T) {
	oldWait, oldTries, oldTimeout := perfRetryWait, perfRetries, perfRequestTimeout
	perfRetryWait, perfRetries, perfRequestTimeout = 2*time.Millisecond, 3, 100*time.Millisecond
	oldPlayer := shadowIsPlayerBoard
	defer func() {
		perfRetryWait, perfRetries, perfRequestTimeout = oldWait, oldTries, oldTimeout
		shadowIsPlayerBoard = oldPlayer
	}()

	expf := validTestEXPF(128)
	nat := validTestNativeAnalysis(t)

	t.Run("native-503-exhausted-blocks-player", func(t *testing.T) {
		shadowIsPlayerBoard = func() bool { return true }
		var audioCalls, nativeCalls atomic.Int32
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(shadowPerfFormatsHeader, "expf,native,rgb")
			switch {
			case r.URL.Path == "/peer/decks/1/audio":
				audioCalls.Add(1)
				_, _ = w.Write([]byte("AUDIO"))
			case r.URL.Query().Get("format") == "native":
				nativeCalls.Add(1)
				http.Error(w, "analysing", http.StatusServiceUnavailable)
			default:
				_, _ = w.Write(expf)
			}
		}))
		defer peer.Close()
		s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
		defer s.Close()
		u, _ := url.Parse(peer.URL)
		s.shadow.peerPort, _ = strconv.Atoi(u.Port())
		p := peerDeck{deckState: deckState{Deck: 1, Key: "k503", Ext: ".flac", Name: "Song.flac"}, ip: "127.0.0.1"}
		s.shadow.mu.Lock()
		s.shadowFetchPeer("peer:k503", p)
		s.shadow.mu.Unlock()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			s.shadow.mu.Lock()
			busy, f := s.shadow.busy["peer:k503"], s.shadow.files["peer:k503"]
			s.shadow.mu.Unlock()
			if !busy {
				if f != "" {
					t.Fatalf("copy published after native 503 retries ran out: %s", f)
				}
				break
			}
		}
		if audioCalls.Load() != 0 || int(nativeCalls.Load()) != perfRetries {
			t.Fatalf("audioCalls=%d nativeCalls=%d (want 0 and %d)", audioCalls.Load(), nativeCalls.Load(), perfRetries)
		}
	})

	t.Run("first-request-network-error-stays-pending", func(t *testing.T) {
		closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedURL := closed.URL
		closed.Close()
		if b, pending := fetchPerfPending(context.Background(), closedURL+"/peer/decks/1/perf?key=k"); b != nil || !pending {
			t.Fatalf("network error on first /perf: b=%d pending=%v, want nil and true", len(b), pending)
		}
	})

	t.Run("expf-db-requires-isAnalyzed", func(t *testing.T) {
		_, vol, db := testlibrary.Create(t)
		p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100, BPM: 128}, &rekordbox.Analysis{
			Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}, {TimeMS: 719, BPM: 128, Number: 2}},
			Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{100, 50, 25}, 20)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../early.wav','early.wav',128,0)`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=? WHERE trackId=(SELECT id FROM Track WHERE filename='early.wav')`,
			p.TrackData, p.Overview, p.Beats); err != nil {
			t.Fatal(err)
		}
		s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
		defer s.Close()
		s.shadow.mediaRoot = filepath.Dir(vol)
		s.shadow.own = []deckState{{Deck: 1, Key: "early", Path: filepath.Join(vol, "early.wav"), libraryPath: filepath.Join(vol, "early.wav")}}
		for _, q := range []string{"", "&format=native"} {
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/peer/decks/1/perf?key=early"+q, nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("query %q on isAnalyzed=0 row: status=%d, want 404", q, w.Code)
			}
			if got := w.Header().Get(shadowPerfFormatsHeader); got != "expf,native,rgb" {
				t.Fatalf("missing capability header: %q", got)
			}
		}
	})

	t.Run("older-expf-only-peer-vs-modern-peer-negotiation", func(t *testing.T) {
		shadowIsPlayerBoard = func() bool { return true }
		// Modern peer advertises native but returns non-RBXA1 (EXPF) for format=native -> must fail closed.
		modern := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(shadowPerfFormatsHeader, "expf,native,rgb")
			if r.URL.Path == "/peer/decks/1/audio" {
				_, _ = w.Write([]byte("AUDIO"))
				return
			}
			_, _ = w.Write(expf)
		}))
		defer modern.Close()
		s1 := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
		defer s1.Close()
		u1, _ := url.Parse(modern.URL)
		s1.shadow.peerPort, _ = strconv.Atoi(u1.Port())
		s1.shadow.mu.Lock()
		s1.shadowFetchPeer("peer:mod", peerDeck{deckState: deckState{Deck: 1, Key: "mod", Ext: ".flac", Name: "Song.flac"}, ip: "127.0.0.1"})
		s1.shadow.mu.Unlock()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			s1.shadow.mu.Lock()
			busy, f := s1.shadow.busy["peer:mod"], s1.shadow.files["peer:mod"]
			s1.shadow.mu.Unlock()
			if !busy {
				if f != "" {
					t.Fatalf("modern peer missing native published anyway: %s", f)
				}
				break
			}
		}

		// Older peer has no capability header and serves valid EXPF for both queries -> allowed with .perf.
		older := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/peer/decks/1/audio" {
				_, _ = w.Write([]byte("AUDIO"))
				return
			}
			_, _ = w.Write(expf)
		}))
		defer older.Close()
		s2 := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
		defer s2.Close()
		u2, _ := url.Parse(older.URL)
		s2.shadow.peerPort, _ = strconv.Atoi(u2.Port())
		s2.shadow.mu.Lock()
		s2.shadowFetchPeer("peer:old", peerDeck{deckState: deckState{Deck: 1, Key: "old", Ext: ".flac", Name: "Song.flac"}, ip: "127.0.0.1"})
		s2.shadow.mu.Unlock()
		var published string
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			s2.shadow.mu.Lock()
			busy, f := s2.shadow.busy["peer:old"], s2.shadow.files["peer:old"]
			s2.shadow.mu.Unlock()
			if !busy {
				published = f
				break
			}
		}
		if published == "" {
			t.Fatal("older EXPF-only peer did not publish with valid .perf")
		}
		if got, err := os.ReadFile(published + ".perf"); err != nil || !bytes.Equal(got, expf) {
			t.Fatalf("older peer missing valid .perf: %v", err)
		}
		// Modern peer with both EXPF and native publishes with both sidecars.
		full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(shadowPerfFormatsHeader, "expf,native,rgb")
			switch {
			case r.URL.Path == "/peer/decks/1/audio":
				_, _ = w.Write([]byte("AUDIO"))
			case r.URL.Query().Get("format") == "native":
				_, _ = w.Write(nat)
			default:
				_, _ = w.Write(expf)
			}
		}))
		defer full.Close()
		s3 := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
		defer s3.Close()
		u3, _ := url.Parse(full.URL)
		s3.shadow.peerPort, _ = strconv.Atoi(u3.Port())
		s3.shadow.mu.Lock()
		s3.shadowFetchPeer("peer:full", peerDeck{deckState: deckState{Deck: 1, Key: "full", Ext: ".flac", Name: "Song.flac"}, ip: "127.0.0.1"})
		s3.shadow.mu.Unlock()
		published = ""
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			s3.shadow.mu.Lock()
			busy, f := s3.shadow.busy["peer:full"], s3.shadow.files["peer:full"]
			s3.shadow.mu.Unlock()
			if !busy {
				published = f
				break
			}
		}
		if published == "" {
			t.Fatal("modern peer with valid EXPF+native did not publish")
		}
		if _, err := os.Stat(published + ".analysis"); err != nil {
			t.Fatalf("modern peer missing .analysis sidecar: %v", err)
		}
	})
}

// BLOCK 2 (reviewer repro): swapping a stick with the same label, path, and
// m.db size/mtime must never serve the old stick's cached payload.
func TestReviewShadowCacheRejectsReplacementWithSameFileStats(t *testing.T) {
	media := t.TempDir()
	first, rel, _, _ := makeOnDemandStickFixture(t, media, "ACTIVE")
	replacement, _, _, _ := makeOnDemandStickFixture(t, media, "REPLACEMENT")
	const db = "Engine Library/Database2/m.db"
	info, err := os.Stat(filepath.Join(first, db))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(replacement, db), info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(replacement, "Engine Library/Database2/.rb-bridge-snapshot")); err != nil {
		t.Fatal(err)
	}

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
	defer s.Close()
	req := bridge.OnDemandRequest{Mount: first, EnginePath: "../" + rel, EngineKey: -1}
	if _, err := s.buildOnDemandCached(context.Background(), req, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(first, filepath.Join(media, "REMOVED")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, first); err != nil {
		t.Fatal(err)
	}
	repInfo, err := os.Stat(filepath.Join(first, db))
	if err != nil {
		t.Fatal(err)
	}
	if repInfo.Size() != info.Size() || !repInfo.ModTime().Equal(info.ModTime()) {
		t.Fatalf("fixture requires identical m.db stats: got (%d,%v) want (%d,%v)", repInfo.Size(), repInfo.ModTime(), info.Size(), info.ModTime())
	}
	if res, err := s.buildOnDemandCached(context.Background(), req, true); err == nil {
		t.Fatalf("replacement has no bridge snapshot yet cache served %d old analysis bytes", len(res.RBXA1))
	}
}

// BLOCK 2 (extended): swapping to another valid stick with the same label and
// matching m.db size/mtime, or modifying export.pdb/exportLibrary.db, rebuilds.
func TestReviewShadowCacheRejectsSameLabelValidReplacementAndExportChange(t *testing.T) {
	media := t.TempDir()
	first, rel, _, _ := makeOnDemandStickFixture(t, media, "SWAP_LABEL")
	second, _, _, _ := makeOnDemandStickFixture(t, media, "SWAP_OTHER")
	const db = "Engine Library/Database2/m.db"
	info, err := os.Stat(filepath.Join(first, db))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(second, db), info.ModTime(), info.ModTime())

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
	defer s.Close()
	req := bridge.OnDemandRequest{Mount: first, EnginePath: "../" + rel, EngineKey: -1}
	if _, err := s.buildOnDemandCached(context.Background(), req, true); err != nil {
		t.Fatal(err)
	}
	if got := s.rbOnDemandBuildCount.Load(); got != 1 {
		t.Fatalf("initial build count = %d, want 1", got)
	}
	// Swap first -> second under identical mount path.
	if err := os.Rename(first, filepath.Join(media, "SWAP_OLD")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(second, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.buildOnDemandCached(context.Background(), req, true); err != nil {
		t.Fatal(err)
	}
	if got := s.rbOnDemandBuildCount.Load(); got != 2 {
		t.Fatalf("after same-label stick swap build count = %d, want 2", got)
	}
	// Touch Device Library export (export.pdb) -> must invalidate and rebuild.
	pdb := filepath.Join(first, "PIONEER", "rekordbox", "export.pdb")
	future := time.Now().Add(5 * time.Second)
	if err := os.Chtimes(pdb, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := s.buildOnDemandCached(context.Background(), req, true); err != nil {
		t.Fatal(err)
	}
	if got := s.rbOnDemandBuildCount.Load(); got != 3 {
		t.Fatalf("after export.pdb change build count = %d, want 3", got)
	}
}

// FIX 3 (reviewer repro): a peer-first build must be reused by the source deck's
// own on-demand call (1 build total, not 2).
func TestReviewShadowPeerFirstBuildIsSharedWithDeck(t *testing.T) {
	media := t.TempDir()
	stick, rel, uuid, tr := makeOnDemandStickFixture(t, media, "PEER_FIRST")
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
	defer s.Close()
	req := bridge.OnDemandRequest{Mount: stick, EnginePath: "../" + rel, EngineKey: -1}
	if _, err := s.buildOnDemandCached(context.Background(), req, true); err != nil {
		t.Fatal(err)
	}
	req.UUID, req.EngineID = uuid, tr.EngineID
	if _, err := s.buildOnDemandCached(context.Background(), req, false); err != nil {
		t.Fatal(err)
	}
	if n := s.rbOnDemandBuildCount.Load(); n != 1 {
		t.Fatalf("peer-first load built the same track %d times, want 1", n)
	}
}

// FIX 3 (extended): ordinary Engine m.db writes do not invalidate the cache, and
// a missing Track row in live m.db defers peer requests with ErrOnDemandUnavailable.
func TestReviewShadowFix3OrdinaryRowUpdateAndMissingRowDefer(t *testing.T) {
	media := t.TempDir()
	stick, rel, _, _ := makeOnDemandStickFixture(t, media, "ORDINARY_FILL")
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
	defer s.Close()
	req := bridge.OnDemandRequest{Mount: stick, EnginePath: "../" + rel, EngineKey: -1}
	if _, err := s.buildOnDemandCached(context.Background(), req, true); err != nil {
		t.Fatal(err)
	}
	// Ordinary in-place m.db update (e.g. Engine updating playOrder / mtime without
	// replacing m.db inode, UUID, or track key/bpm/length) keeps the cache hot.
	mdb := filepath.Join(stick, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", mdb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET playOrder = 7`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	future := time.Now().Add(4 * time.Second)
	_ = os.Chtimes(mdb, future, future)
	if _, err := s.buildOnDemandCached(context.Background(), req, true); err != nil {
		t.Fatal(err)
	}
	if got := s.rbOnDemandBuildCount.Load(); got != 1 {
		t.Fatalf("ordinary m.db update triggered rebuild: count=%d, want 1", got)
	}
	// If the Track row is missing from live m.db, peer-first defers with ErrOnDemandUnavailable.
	db, err = sql.Open("sqlite", mdb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM Track`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	_ = os.Chtimes(mdb, future.Add(2*time.Second), future.Add(2*time.Second))
	if _, err := s.buildOnDemandCached(context.Background(), req, true); !errors.Is(err, bridge.ErrOnDemandUnavailable) {
		t.Fatalf("missing Track row for peer: err=%v, want ErrOnDemandUnavailable", err)
	}
}

// FIX 4: inMusicTempFiles loads carry the fingerprint-verified library path and
// identity through copy read paths; when the original is removed or replaced and
// another same-name same-size track exists, /perf returns 503 instead of pairing
// the temp copy with the wrong track's analysis.
func TestReviewShadowFix4TempFileStaleValidationReturns503(t *testing.T) {
	dataRoot := t.TempDir()
	mediaRoot := t.TempDir()
	lib := filepath.Join(dataRoot, "AppDataUser", "AIR Music Technology", "Engine", "Engine Library")
	for _, d := range []string{filepath.Join(lib, "Database2"), filepath.Join(dataRoot, "inMusicTempFiles"), filepath.Join(dataRoot, "Music", "a"), filepath.Join(dataRoot, "Music", "b")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dbFile, _, db := testlibrary.Create(t)
	origA := filepath.Join(dataRoot, "Music", "a", "song.wav")
	origB := filepath.Join(dataRoot, "Music", "b", "song.wav")
	tempCopy := filepath.Join(dataRoot, "inMusicTempFiles", "1111-aaaa_song.wav")
	if err := os.WriteFile(origA, []byte("AAAAAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempCopy, []byte("AAAAAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeP := func(bpm float64) enginedb.Performance {
		p, err := enginedb.EncodePerformance(
			rekordbox.Track{Duration: 4, SampleRate: 44100, SampleCount: 4 * 44100, BPM: bpm},
			&rekordbox.Analysis{
				Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: bpm, Number: 1}, {TimeMS: 750, BPM: bpm, Number: 2}},
				Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{150, 90, 40}, 20)}},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pA, pB := makeP(122), makeP(140)
	if _, err := db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../../../../Music/a/song.wav','song.wav',122,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=? WHERE trackId=(SELECT max(id) FROM Track)`,
		pA.TrackData, pA.Overview, pA.Beats, pA.QuickCues, pA.Loops); err != nil {
		t.Fatal(err)
	}
	dbBytes, err := os.ReadFile(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	mdbPath := filepath.Join(lib, "Database2", "m.db")
	if err := os.WriteFile(mdbPath, dbBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	mdb, err := sql.Open("sqlite", mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = dataRoot, mediaRoot
	d := shadowReviewPost(t, s, "net://u/(Unknown)/inMusicTempFiles/1111-aaaa_song.wav")
	if d.libraryPath != origA || !d.isTempFile {
		t.Fatalf("temp deck state = %+v, want libraryPath=%q isTempFile=true", d, origA)
	}
	// Initial fetch succeeds with A's 122 BPM analysis.
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/peer/decks/1/perf?key="+url.QueryEscape(d.Key), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("initial /perf status=%d body=%q", w.Code, w.Body.String())
	}
	if bpm, _, _ := parseEXPFOverviewAndBPM(t, w.Body.Bytes()); bpm != 122 {
		t.Fatalf("initial BPM = %v, want 122", bpm)
	}
	// Now remove origA and place same-name same-size track B (140 BPM) in m.db.
	// Re-resolving by basename+size would wrongly serve B's 140 BPM analysis;
	// verified library identity must return 503 pending instead.
	if err := os.Remove(origA); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(origB, []byte("BBBBBBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../../../../Music/b/song.wav','song.wav',140,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=? WHERE trackId=(SELECT max(id) FROM Track)`,
		pB.TrackData, pB.Overview, pB.Beats, pB.QuickCues, pB.Loops); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "&format=native", "&format=rgb"} {
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/peer/decks/1/perf?key="+url.QueryEscape(d.Key)+q, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("stale temp /perf%s status=%d, want 503 (never wrong track B)", q, w.Code)
		}
	}
}

// FIX 5: maps remain bounded across many stock loads/unloads, and canonical path
// comparison prevents alternate path spellings from dropping an active deck's cache.
func TestReviewShadowFix5BoundedMapsAndCanonicalComparison(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	mediaRoot := t.TempDir()
	oldMedia := shadowMediaRoot
	shadowMediaRoot = mediaRoot
	defer func() { shadowMediaRoot = oldMedia }()

	stick, rel, _, _ := makeOnDemandStickFixture(t, mediaRoot, "CANON_VOL")
	stockVol := filepath.Join(mediaRoot, "STOCK_VOL")
	if err := os.MkdirAll(filepath.Join(stockVol, "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: mediaRoot, RekordboxOnDemandSock: "-"}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	post := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("decksPost: %v %v", resp, err)
		}
		resp.Body.Close()
	}

	// Cycle 40 distinct stock USB tracks: on-demand caches stay empty/bounded.
	for i := 0; i < 40; i++ {
		name := "stock_" + strconv.Itoa(i) + ".wav"
		_ = os.WriteFile(filepath.Join(stockVol, "Music", name), []byte("WAV"), 0o644)
		post(`[{"deck":1,"net":"net://u/STOCK_VOL/Music/` + name + `"}]`)
	}
	post(`[{"deck":1,"net":""}]`)
	s.rbOnDemandCacheMu.Lock()
	cLen, fLen, dLen := len(s.rbOnDemandCache), len(s.rbOnDemandFlights), len(s.rbOnDemandDBMeta)
	s.rbOnDemandCacheMu.Unlock()
	if cLen != 0 || fLen != 0 || dLen > rbOnDemandDBMetaMax {
		t.Fatalf("unbounded state after stock cycling: cache=%d flights=%d dbMeta=%d", cLen, fLen, dLen)
	}

	// Canonical path comparison: Deck 1 holds canonical path, Deck 2 holds an
	// alternate spelling (double slash) of the same file; unloading Deck 2 must
	// keep Deck 1's cached on-demand entry alive.
	req := bridge.OnDemandRequest{Mount: stick, EnginePath: "../" + rel, EngineKey: -1}
	if _, err := s.buildOnDemandCached(context.Background(), req, true); err != nil {
		t.Fatal(err)
	}
	altPath := filepath.Join(stick, filepath.FromSlash(rel))
	s.shadow.mu.Lock()
	s.shadow.own = []deckState{
		{Deck: 1, Key: "k1", Path: altPath, libraryPath: stick + "//" + filepath.FromSlash(rel)},
		{Deck: 2, Key: "k2", Path: altPath, libraryPath: altPath},
	}
	s.shadow.mu.Unlock()
	post(`[{"deck":1,"net":"net://u/CANON_VOL/` + rel + `"}]`)
	s.rbOnDemandCacheMu.Lock()
	kept := len(s.rbOnDemandCache)
	s.rbOnDemandCacheMu.Unlock()
	if kept != 1 {
		t.Fatalf("alternate spelling dropped active deck cache: len=%d, want 1", kept)
	}
}

// NOTE 6: shared encodings across EXPF/native/RGB and zero analysis/DB work on
// steady-state 1 s position posts (including Pro DJ Link perfGen).
func TestReviewShadowNote6SharedEncodingsAndZeroWorkPositionPost(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	mediaRoot := t.TempDir()
	oldMedia := shadowMediaRoot
	shadowMediaRoot = mediaRoot
	defer func() { shadowMediaRoot = oldMedia }()

	// 1. On-demand entry shares one build AND one decode/encode across EXPF, native, and 2 copies.
	stick, rel, _, _ := makeOnDemandStickFixture(t, mediaRoot, "NOTE6_OD")
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: mediaRoot, RekordboxOnDemandSock: "-"}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	post := func(body string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("decksPost: %v %v", resp, err)
		}
		resp.Body.Close()
	}
	post(`[{"deck":1,"net":"net://u/NOTE6_OD/` + rel + `","bpm":120}]`)
	var peer struct{ Decks []deckState }
	resp, err := http.Get(srv.URL + "/peer/decks")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&peer)
	resp.Body.Close()
	key := peer.Decks[0].Key
	for copyIdx := 0; copyIdx < 2; copyIdx++ {
		for _, q := range []string{"", "&format=native"} {
			r, err := http.Get(srv.URL + "/peer/decks/1/perf?key=" + url.QueryEscape(key) + q)
			if err != nil || r.StatusCode != http.StatusOK {
				t.Fatalf("copy %d query %q: %v %v", copyIdx, q, r, err)
			}
			_, _ = io.ReadAll(r.Body)
			r.Body.Close()
		}
	}
	if n := s.rbOnDemandBuildCount.Load(); n != 1 {
		t.Fatalf("on-demand build count = %d, want 1", n)
	}
	s.rbOnDemandCacheMu.Lock()
	encCached := 0
	for _, e := range s.rbOnDemandCache {
		if len(e.enc.native) > 0 && len(e.enc.expf) > 0 && e.enc.gen != "" {
			encCached++
		}
	}
	s.rbOnDemandCacheMu.Unlock()
	if encCached != 1 {
		t.Fatalf("expected 1 entry with shared cached EXPF+native encodings, got %d", encCached)
	}
	_ = stick

	// 2. Pro DJ Link 1 s position posts do NOT re-read or re-decode analysis when
	// the analysis file on disk is unchanged, even past perfGenEvery.
	oldEvery := perfGenEvery
	perfGenEvery = 0
	defer func() { perfGenEvery = oldEvery }()
	r := newRbxShadowSource(t)
	id := r.row(42)
	r.publish(t, id, 1000)
	// First post populates perfGen and shadowPerfCache.
	uri := "streaming://TIDAL/Track/" + strconv.FormatInt(id, 10)
	postRbx := func(pos float64) deckState {
		t.Helper()
		body := `[{"deck":1,"net":` + strconv.Quote(uri) + `,"playing":true,"positionMs":` + strconv.FormatFloat(pos, 'f', 1, 64) + `,"bpm":120}]`
		resp, err := http.Post(r.srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("rbx post: %v %v", resp, err)
		}
		resp.Body.Close()
		r.s.shadow.mu.Lock()
		defer r.s.shadow.mu.Unlock()
		return r.s.shadow.own[0]
	}
	d1 := postRbx(1000)
	if d1.PerfGen == "" {
		t.Fatal("initial Pro DJ Link post produced empty PerfGen")
	}
	// Clear deckPerf slot and confirm steady-state 1 s position posts still hit
	// stat-pinned perfGens without re-running ownPerfPair/deckEncodeCount.
	r.s.shadow.perfMu.Lock()
	r.s.shadow.deckPerf[1] = nil
	r.s.shadow.perfMu.Unlock()
	encBefore := r.s.shadow.deckEncodeCount.Load()
	for i := 0; i < 10; i++ {
		dNext := postRbx(float64(2000 + i*1000))
		if dNext.PerfGen != d1.PerfGen || dNext.Key != d1.Key {
			t.Fatalf("steady-state post %d changed key/gen: %+v vs %+v", i, dNext, d1)
		}
	}
	if r.s.shadow.deckEncodeCount.Load() != encBefore {
		t.Fatal("steady-state 1 s position posts re-ran ownPerf despite unchanged rbxAnalysis stat identity")
	}
	// Verify /peer/decks/1/perf returns X-EngineX-Perf-Gen matching d1.PerfGen so
	// a copy fetched at t=0ms (before PerfGen was advertised) records the exact
	// generation and does not spuriously run shadowRefreshPeerPerf when PerfGen appears.
	for _, q := range []string{"", "&format=native"} {
		resp, err := http.Get(r.srv.URL + "/peer/decks/1/perf?key=" + url.QueryEscape(d1.Key) + q)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("prolink /perf%s: %v %v", q, resp, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if gotGen := resp.Header.Get(shadowPerfGenHeader); gotGen != d1.PerfGen {
			t.Fatalf("prolink /perf%s %s = %q, want %q", q, shadowPerfGenHeader, gotGen, d1.PerfGen)
		}
	}

	// 3. Concurrent EXPF + native + RGB requests for the same loaded DB track
	// share one per-deck shadowDeckPerf slot cleanly under -race and evict on unload.
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := []string{"", "&format=native", "&format=rgb"}[i%3]
			resp, err := http.Get(srv.URL + "/peer/decks/1/perf?key=" + url.QueryEscape(key) + q)
			if err != nil {
				return
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}(g)
	}
	wg.Wait()
	post(`[{"deck":1,"net":""}]`)
	if s.getDeckPerf(1, key) != nil {
		t.Fatal("deckPerf was not evicted on deck unload")
	}
}

// BenchmarkDecksPostPosition measures steady-state 1 s position-post cost on
// stock USB, inMusicTempFiles, and Pro DJ Link decks.
func BenchmarkDecksPostPosition(b *testing.B) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()

	b.Run("StockUSB_1sPositionPost", func(b *testing.B) {
		media := b.TempDir()
		oldMedia := shadowMediaRoot
		shadowMediaRoot = media
		defer func() { shadowMediaRoot = oldMedia }()
		vol := filepath.Join(media, "BENCH_STOCK")
		_ = os.MkdirAll(filepath.Join(vol, "Music"), 0o755)
		_ = os.WriteFile(filepath.Join(vol, "Music", "track.flac"), []byte("AUDIO"), 0o644)
		s := New(Config{UIDir: b.TempDir(), StateDir: b.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
		defer s.Close()
		body := []byte(`[{"deck":1,"net":"net://u/BENCH_STOCK/Music/track.flac","playing":true,"positionMs":12345,"bpm":128,"pitch":0}]`)
		postOnce := func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/enginex/decks", bytes.NewReader(body))
			w := httptest.NewRecorder()
			s.decksPost(w, req)
			if w.Code != http.StatusOK {
				b.Fatalf("status=%d", w.Code)
			}
		}
		postOnce()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			postOnce()
		}
	})

	b.Run("InMusicTemp_1sPositionPost", func(b *testing.B) {
		data := b.TempDir()
		media := b.TempDir()
		lib := filepath.Join(data, "AppDataUser", "AIR Music Technology", "Engine", "Engine Library")
		_ = os.MkdirAll(filepath.Join(lib, "Database2"), 0o755)
		_ = os.MkdirAll(filepath.Join(data, "inMusicTempFiles"), 0o755)
		_ = os.MkdirAll(filepath.Join(data, "Music"), 0o755)
		mdbPath := filepath.Join(lib, "Database2", "m.db")
		db, err := sql.Open("sqlite", mdbPath)
		if err != nil {
			b.Fatal(err)
		}
		_, err = db.Exec(`CREATE TABLE Track(id INTEGER PRIMARY KEY, path TEXT, filename TEXT, bpmAnalyzed REAL, isAnalyzed INTEGER);
			INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../../../../Music/song.wav','song.wav',128,1)`)
		db.Close()
		if err != nil {
			b.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(data, "Music", "song.wav"), []byte("WAVDATA"), 0o644)
		_ = os.WriteFile(filepath.Join(data, "inMusicTempFiles", "1111-aaaa_song.wav"), []byte("WAVDATA"), 0o644)
		s := New(Config{UIDir: b.TempDir(), StateDir: b.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
		defer s.Close()
		s.shadow.dataRoot, s.shadow.mediaRoot = data, media
		body := []byte(`[{"deck":1,"net":"net://u/(Unknown)/inMusicTempFiles/1111-aaaa_song.wav","playing":true,"positionMs":12345,"bpm":128}]`)
		postOnce := func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/enginex/decks", bytes.NewReader(body))
			w := httptest.NewRecorder()
			s.decksPost(w, req)
			if w.Code != http.StatusOK {
				b.Fatalf("status=%d", w.Code)
			}
		}
		postOnce()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			postOnce()
		}
	})

	b.Run("ProLink_1sPositionPost", func(b *testing.B) {
		oldEvery := perfGenEvery
		perfGenEvery = 0 // simulate >= 1 s between position posts so perfGen executes every iteration
		defer func() { perfGenEvery = oldEvery }()
		oldDir := rbxAnalysisDir
		rbxAnalysisDir = b.TempDir()
		defer func() { rbxAnalysisDir = oldDir }()
		media := b.TempDir()
		oldMedia := shadowMediaRoot
		shadowMediaRoot = media
		defer func() { shadowMediaRoot = oldMedia }()
		drive := filepath.Join(media, "USB")
		audio := filepath.Join(drive, rbxCacheDir, "link.m4a")
		_ = os.MkdirAll(filepath.Dir(audio), 0o755)
		_ = os.WriteFile(audio, []byte("AUDIO"), 0o644)
		p, err := enginedb.EncodePerformance(
			rekordbox.Track{Duration: 4, SampleRate: 44100, SampleCount: 4 * 44100, BPM: 128},
			&rekordbox.Analysis{
				Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}, {TimeMS: 719, BPM: 128, Number: 2}},
				Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{180, 120, 60}, 60)}},
			},
		)
		if err != nil {
			b.Fatal(err)
		}
		s := New(Config{UIDir: b.TempDir(), StateDir: b.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
		defer s.Close()
		gid := s.tidal.addTrack(trackRef{track: providers.Track{ID: "x\x0010.0.0.2\x0099", Provider: rekordboxScope, Title: "Link"}, file: audio, drive: drive})
		if !rbxPublishAnalysis(rbxAnalysisPath(gid), p, 128, false) {
			b.Fatal("publish rbx analysis")
		}
		uri := "streaming://TIDAL/Track/" + strconv.FormatInt(gid, 10)
		body := []byte(`[{"deck":1,"net":` + strconv.Quote(uri) + `,"playing":true,"positionMs":12345,"bpm":128}]`)
		postOnce := func() {
			req := httptest.NewRequest(http.MethodPost, "/v1/enginex/decks", bytes.NewReader(body))
			w := httptest.NewRecorder()
			s.decksPost(w, req)
			if w.Code != http.StatusOK {
				b.Fatalf("status=%d", w.Code)
			}
		}
		postOnce()
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			postOnce()
		}
	})
}

var _ = sync.Mutex{}

// GPT R2 BLOCK (Finding 1): the receiver must never publish EXPF from one
// generation alongside native RBXA1 from another generation.
func TestReviewShadowR2DoesNotPublishMixedAnalysisGenerations(t *testing.T) {
	oldBoard := shadowIsPlayerBoard
	oldWait := shadowPairGenRetryWait
	shadowIsPlayerBoard = func() bool { return true }
	shadowPairGenRetryWait = time.Millisecond
	t.Cleanup(func() {
		shadowIsPlayerBoard = oldBoard
		shadowPairGenRetryWait = oldWait
	})
	expf, native := validTestEXPF(128), validTestNativeAnalysis(t)
	var mu sync.Mutex
	expfGen, natGen := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		eg, ng := expfGen, natGen
		mu.Unlock()
		w.Header().Set(shadowPerfFormatsHeader, "expf,native")
		switch {
		case r.URL.Path == "/peer/decks/1/audio":
			_, _ = w.Write([]byte("AUDIO"))
		case r.URL.Query().Get("format") == "native":
			w.Header().Set(shadowPerfGenHeader, ng)
			_, _ = w.Write(native)
		case r.URL.Query().Get("format") == "rgb":
			http.NotFound(w, r)
		default:
			w.Header().Set(shadowPerfGenHeader, eg)
			_, _ = w.Write(expf)
		}
	}))
	defer peer.Close()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.copyRetry = time.Millisecond
	u, _ := url.Parse(peer.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())
	const key = "peer:mixed"
	p := peerDeck{
		deckState: deckState{Deck: 1, Key: "mixed", Ext: ".flac", Name: "Song.flac"},
		ip:        "127.0.0.1",
	}
	s.shadow.mu.Lock()
	s.shadowFetchPeer(key, p)
	s.shadow.mu.Unlock()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.shadow.mu.Lock()
		busy, path, gen := s.shadow.busy[key], s.shadow.files[key], s.shadow.copyGens[key]
		s.shadow.mu.Unlock()
		if busy {
			continue
		}
		if path != "" {
			t.Fatalf("published EXPF generation A with native generation B and recorded %q", gen)
		}
		break
	}
	// Once both responses carry the same generation, retry succeeds and records it.
	mu.Lock()
	expfGen = "bbbbbbbbbbbbbbbb"
	mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	s.shadow.mu.Lock()
	s.shadowFetchPeer(key, p)
	s.shadow.mu.Unlock()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.shadow.mu.Lock()
		busy, path, gen := s.shadow.busy[key], s.shadow.files[key], s.shadow.copyGens[key]
		s.shadow.mu.Unlock()
		if busy {
			continue
		}
		if path == "" || gen != "bbbbbbbbbbbbbbbb" {
			t.Fatalf("matching generation did not publish cleanly: path=%q gen=%q", path, gen)
		}
		return
	}
	t.Fatal("matching copy job did not settle")
}

// GPT R2 FIX (Finding 2): persistent 503 answers transition after a bounded wait
// to a visible terminal "no analysis yet" state, stop polling, and retry only
// when a new source generation arrives or on explicit DJ retry.
func TestReviewShadowR2Persistent503TransitionsToVisibleTerminalStateAndRetries(t *testing.T) {
	oldWait, oldTries := perfRetryWait, perfRetries
	oldMaxFails, oldTimeout := shadowPendingMaxFails, shadowPendingTimeout
	oldBoard := shadowIsPlayerBoard
	perfRetryWait, perfRetries = time.Millisecond, 1
	shadowPendingMaxFails, shadowPendingTimeout = 2, 10*time.Second
	shadowIsPlayerBoard = func() bool { return true }
	t.Cleanup(func() {
		perfRetryWait, perfRetries = oldWait, oldTries
		shadowPendingMaxFails, shadowPendingTimeout = oldMaxFails, oldTimeout
		shadowIsPlayerBoard = oldBoard
	})
	expf, native := validTestEXPF(128), validTestNativeAnalysis(t)
	var ready atomic.Bool
	var perfReqs, audioReqs atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(shadowPerfFormatsHeader, "expf,native")
		if r.URL.Path == "/peer/decks/1/audio" {
			audioReqs.Add(1)
			_, _ = w.Write([]byte("AUDIO"))
			return
		}
		if r.URL.Query().Get("format") == "rgb" {
			http.NotFound(w, r)
			return
		}
		perfReqs.Add(1)
		if !ready.Load() {
			http.Error(w, "analysing", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(shadowPerfGenHeader, "2222222222222222")
		if r.URL.Query().Get("format") == "native" {
			_, _ = w.Write(native)
			return
		}
		_, _ = w.Write(expf)
	}))
	defer peer.Close()

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.copyRetry = 2 * time.Millisecond
	u, _ := url.Parse(peer.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())

	setPeer := func(k, gen string) {
		s.shadow.mu.Lock()
		s.shadow.peerDecks[1] = peerDeck{
			deckState: deckState{Deck: 1, Key: k, Ext: ".flac", Name: k + ".flac", PerfGen: gen},
			ip:        "127.0.0.1",
			at:        time.Now(),
		}
		s.shadow.mu.Unlock()
	}
	compose := func() deckState {
		out := s.shadowComposeLegacy("24", time.Now())
		if len(out) != 1 {
			t.Fatalf("expected 1 shadow deck, got %+v", out)
		}
		return out[0]
	}

	// Sub-case A: persistent 503 transitions to visible "no analysis yet", stops polling,
	// and retries automatically when a new PerfGen arrives.
	setPeer("k503a", "1111111111111111")
	var d deckState
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		d = compose()
		if d.Error == "no analysis yet" {
			break
		}
	}
	if d.Error != "no analysis yet" || d.Path != "" || audioReqs.Load() != 0 {
		t.Fatalf("persistent 503 did not reach visible terminal state: deck=%+v audioReqs=%d", d, audioReqs.Load())
	}
	settledReqs := perfReqs.Load()
	time.Sleep(25 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if d2 := compose(); d2.Error != "no analysis yet" || d2.Path != "" {
			t.Fatalf("terminal state lost on idle compose: %+v", d2)
		}
	}
	if perfReqs.Load() != settledReqs {
		t.Fatalf("terminal 503 kept polling: reqs %d -> %d", settledReqs, perfReqs.Load())
	}
	ready.Store(true)
	setPeer("k503a", "2222222222222222")
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		d = compose()
		if d.Path != "" {
			break
		}
	}
	if d.Path == "" || d.Error != "" {
		t.Fatalf("new PerfGen did not trigger retry and publish: %+v", d)
	}

	// Sub-case B: unused POST /v1/enginex/shadow/retry route is removed (not
	// registered on the gateway mux), and a source reload (new load Key) wakes
	// a terminal copy cleanly.
	retryReq := httptest.NewRequest(http.MethodPost, "/v1/enginex/shadow/retry", strings.NewReader(`{"key":"peer:k503a"}`))
	if _, pat := s.tidal.mux.Handler(retryReq); strings.Contains(pat, "/shadow/retry") {
		t.Fatalf("unused POST /v1/enginex/shadow/retry still registered: pattern=%q", pat)
	}
	ready.Store(false)
	setPeer("k503b-1", "")
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		d = compose()
		if d.Error == "no analysis yet" {
			break
		}
	}
	if d.Error != "no analysis yet" {
		t.Fatalf("k503b-1 did not enter terminal state: %+v", d)
	}
	ready.Store(true)
	setPeer("k503b-2", "")
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		d = compose()
		if d.Path != "" {
			break
		}
	}
	if d.Path == "" || d.Error != "" {
		t.Fatalf("source reload did not wake terminal copy: %+v", d)
	}
}

// GPT R2 FIX (Finding 3): concurrent first EXPF and native requests initialize
// the encoding + precomputed generation exactly once per entry (both on-demand
// and Pro DJ Link), with zero re-encoding or per-response full-blob hashing.
func TestReviewShadowR2ConcurrentEncodingsRunOnceAndReuseGen(t *testing.T) {
	// 1. On-demand concurrent first EXPF + native requests run sync.Once exactly once.
	mediaRoot := t.TempDir()
	oldMedia := shadowMediaRoot
	shadowMediaRoot = mediaRoot
	defer func() { shadowMediaRoot = oldMedia }()
	stick, rel, _, _ := makeOnDemandStickFixture(t, mediaRoot, "R2_CONC")
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: mediaRoot, RekordboxOnDemandSock: "-"}, nil)
	defer s.Close()
	entry, err := s.buildOnDemandEntry(context.Background(), bridge.OnDemandRequest{
		Mount:      stick,
		EnginePath: "../" + rel,
		EngineKey:  -1,
	}, true)
	if err != nil {
		t.Fatalf("buildOnDemandEntry: %v", err)
	}
	s.rbOnDemandEncodeCount.Store(0)
	const workers = 8
	start := make(chan struct{})
	gens := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			nat, expf, gen, gerr := s.onDemandEncodings(entry)
			if gerr != nil {
				errs[idx] = gerr
				return
			}
			if !validShadowAnalysis(nat) || !validPerf(expf) || gen == "" {
				errs[idx] = fmt.Errorf("invalid encoding/gen: nat=%d expf=%d gen=%q", len(nat), len(expf), gen)
				return
			}
			gens[idx] = gen
		}(i)
	}
	close(start)
	wg.Wait()
	for i, gerr := range errs {
		if gerr != nil {
			t.Fatalf("worker %d: %v", i, gerr)
		}
		if gens[i] != gens[0] {
			t.Fatalf("worker %d gen=%q != %q", i, gens[i], gens[0])
		}
	}
	if got := s.rbOnDemandEncodeCount.Load(); got != 1 {
		t.Fatalf("onDemandEncodings initialized %d times under concurrent requests, want 1", got)
	}

	// 2. Pro DJ Link concurrent first EXPF + native requests encode once and return identical X-EngineX-Perf-Gen.
	r := newRbxShadowSource(t)
	id := r.row(77)
	r.publish(t, id, 1000)
	uri := "streaming://TIDAL/Track/" + strconv.FormatInt(id, 10)
	body := `[{"deck":1,"net":` + strconv.Quote(uri) + `,"playing":true,"positionMs":1000,"bpm":120}]`
	resp, err := http.Post(r.srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("rbx decksPost: %v %v", resp, err)
	}
	resp.Body.Close()
	r.s.shadow.mu.Lock()
	d1 := r.s.shadow.own[0]
	r.s.shadow.mu.Unlock()
	// Reset deckPerf and counter to test cold concurrent first EXPF + native HTTP requests.
	r.s.shadow.perfMu.Lock()
	r.s.shadow.deckPerf[1] = nil
	r.s.shadow.perfMu.Unlock()
	r.s.shadow.deckEncodeCount.Store(0)

	start2 := make(chan struct{})
	gotGens := make([]string, workers)
	gotErrs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start2
			q := ""
			if idx%2 == 1 {
				q = "&format=native"
			}
			res, rerr := http.Get(r.srv.URL + "/peer/decks/1/perf?key=" + url.QueryEscape(d1.Key) + q)
			if rerr != nil {
				gotErrs[idx] = rerr
				return
			}
			_, _ = io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				gotErrs[idx] = fmt.Errorf("status %d", res.StatusCode)
				return
			}
			gotGens[idx] = res.Header.Get(shadowPerfGenHeader)
		}(i)
	}
	close(start2)
	wg.Wait()
	for i, rerr := range gotErrs {
		if rerr != nil {
			t.Fatalf("prolink worker %d: %v", i, rerr)
		}
		if gotGens[i] != d1.PerfGen || gotGens[i] == "" {
			t.Fatalf("prolink worker %d gen=%q, want %q", i, gotGens[i], d1.PerfGen)
		}
	}
	if got := r.s.shadow.deckEncodeCount.Load(); got != 1 {
		t.Fatalf("prolink deckPerf encoded %d times under concurrent requests, want 1", got)
	}
}

// GPT R2 FIX (Finding 5 on A): EXPF-only compatibility path validates the beat
// grid semantically and rejects a framed one-byte unreadable beat grid.
func TestReviewShadowR2RejectsEXPFWithUnreadableBeatGrid(t *testing.T) {
	b := validTestEXPF(128)
	n := int(binary.LittleEndian.Uint32(b[16:20]))
	beatsLen := 20 + n
	end := beatsLen + 4 + int(binary.LittleEndian.Uint32(b[beatsLen:]))
	bad := append([]byte(nil), b[:beatsLen]...)
	bad = append(bad, 1, 0, 0, 0, 0)
	bad = append(bad, b[end:]...)
	if validPerf(bad) {
		t.Fatal("validPerf accepted EXPF with one-byte unreadable beat grid")
	}
	oldBoard := shadowIsPlayerBoard
	shadowIsPlayerBoard = func() bool { return true }
	t.Cleanup(func() { shadowIsPlayerBoard = oldBoard })
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/peer/decks/1/audio" {
			_, _ = w.Write([]byte("AUDIO"))
			return
		}
		// Older EXPF-only peer (no X-EngineX-Perf-Formats header) serving bad beat grid.
		_, _ = w.Write(bad)
	}))
	defer peer.Close()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	u, _ := url.Parse(peer.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())
	const key = "peer:bad-expf-grid"
	s.shadow.mu.Lock()
	s.shadowFetchPeer(key, peerDeck{
		deckState: deckState{Deck: 1, Key: "bad-expf-grid", Ext: ".flac", Name: "Song.flac"},
		ip:        "127.0.0.1",
	})
	s.shadow.mu.Unlock()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.shadow.mu.Lock()
		busy, path, cp := s.shadow.busy[key], s.shadow.files[key], s.shadow.copies[key]
		s.shadow.mu.Unlock()
		if busy {
			continue
		}
		if path != "" {
			t.Fatalf("EXPF-only compatibility path published copy with unreadable beat grid: %s", path)
		}
		if cp == nil || cp.err != "no analysis" {
			t.Fatalf("expected \"no analysis\" error, got %+v", cp)
		}
		return
	}
	t.Fatal("copy job did not settle")
}

// GPT R3 FIX (Finding 1): stock USB (m.db), streaming (stm.db), and rekordbox
// on-demand sources all advertise analysis readiness via PerfGen in the existing
// deck state when analysis finishes, waking a terminal copy without reload or
// extra polling.
func TestReviewShadowR3OrdinarySourcesWakeTerminalCopyWhenAnalysisFinishes(t *testing.T) {
	oldBoard := shadowIsPlayerBoard
	oldWait, oldTries := perfRetryWait, perfRetries
	oldMaxFails, oldTimeout := shadowPendingMaxFails, shadowPendingTimeout
	oldEvery := perfGenEvery
	oldMedia, oldData, oldBeacon := shadowMediaRoot, shadowDataRoot, shadowBeaconOn
	shadowIsPlayerBoard = func() bool { return true }
	perfRetryWait, perfRetries = time.Millisecond, 1
	shadowPendingMaxFails, shadowPendingTimeout = 1, 2*time.Second
	perfGenEvery = 0
	shadowBeaconOn = false
	t.Cleanup(func() {
		shadowIsPlayerBoard = oldBoard
		perfRetryWait, perfRetries = oldWait, oldTries
		shadowPendingMaxFails, shadowPendingTimeout = oldMaxFails, oldTimeout
		perfGenEvery = oldEvery
		shadowMediaRoot, shadowDataRoot, shadowBeaconOn = oldMedia, oldData, oldBeacon
	})

	qc := func(b []byte) []byte { c, _ := enginedb.QCompress(b); return c }
	trackBlob, beatBlob, ovBlob := validTestPerformanceBlobs(126.0)

	postAndReadPeer := func(t *testing.T, srvURL, body string) deckState {
		t.Helper()
		resp, err := http.Post(srvURL+"/v1/enginex/decks", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("decksPost: %v %v", resp, err)
		}
		resp.Body.Close()
		get, err := http.Get(srvURL + "/peer/decks")
		if err != nil || get.StatusCode != http.StatusOK {
			t.Fatalf("peer/decks: %v %v", get, err)
		}
		defer get.Body.Close()
		var out struct{ Decks []deckState }
		if err := json.NewDecoder(get.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if len(out.Decks) != 1 {
			t.Fatalf("expected 1 deck, got %+v", out.Decks)
		}
		return out.Decks[0]
	}

	runCase := func(t *testing.T, src *Server, srcSrv *httptest.Server, postBody string, finishAnalysis func()) {
		t.Helper()
		first := postAndReadPeer(t, srcSrv.URL, postBody)
		if first.Key == "" || first.PerfGen != "" {
			t.Fatalf("unready source deck: key=%q perfGen=%q (want non-empty key, empty perfGen)", first.Key, first.PerfGen)
		}
		dst := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
		defer dst.Close()
		dst.shadow.dataRoot = t.TempDir()
		dst.shadow.mediaRoot = t.TempDir()
		dst.shadow.copyRetry = 2 * time.Millisecond
		u, _ := url.Parse(srcSrv.URL)
		dst.shadow.peerPort, _ = strconv.Atoi(u.Port())
		feedPeer := func(d deckState) deckState {
			dst.shadow.mu.Lock()
			dst.shadow.peerDecks[1] = peerDeck{deckState: d, ip: "127.0.0.1", at: time.Now()}
			dst.shadow.mu.Unlock()
			out := dst.shadowComposeLegacy("24", time.Now())
			if len(out) != 1 {
				t.Fatalf("expected 1 shadow deck, got %+v", out)
			}
			return out[0]
		}
		var shown deckState
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			shown = feedPeer(first)
			if shown.Error == "no analysis yet" || shown.Error == "no analysis" {
				break
			}
		}
		if (shown.Error != "no analysis yet" && shown.Error != "no analysis") || shown.Path != "" {
			t.Fatalf("copy did not enter terminal state while unready: %+v", shown)
		}
		// Now source finishes analysis for the exact same loaded track (no reload).
		finishAnalysis()
		ready := postAndReadPeer(t, srcSrv.URL, postBody)
		if ready.Key != first.Key {
			t.Fatalf("load key changed across analysis finish: %q -> %q", first.Key, ready.Key)
		}
		if ready.PerfGen == "" {
			t.Fatalf("source did not advertise non-empty PerfGen after analysis finished: %+v", ready)
		}
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			shown = feedPeer(ready)
			if shown.Path != "" {
				break
			}
		}
		if shown.Path == "" || shown.Error != "" {
			t.Fatalf("terminal copy did not wake and publish once source advertised PerfGen=%q: %+v", ready.PerfGen, shown)
		}
	}

	t.Run("stock-usb-mdb", func(t *testing.T) {
		media := t.TempDir()
		shadowMediaRoot = media
		stick := filepath.Join(media, "USB1")
		audio := filepath.Join(stick, "Track.flac")
		_ = os.MkdirAll(filepath.Join(stick, "Engine Library/Database2"), 0o755)
		_ = os.WriteFile(audio, []byte("AUDIO"), 0o644)
		dbPath, _, db := testlibrary.Create(t)
		if _, err := db.Exec(`INSERT INTO Track (path, filename, bpmAnalyzed, isAnalyzed) VALUES ('../Track.flac', 'Track.flac', 0, 0)`); err != nil {
			t.Fatal(err)
		}
		mdb := filepath.Join(stick, "Engine Library/Database2/m.db")
		copyDB := func() {
			raw, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mdb, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		copyDB()
		src := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
		defer src.Close()
		srcSrv := httptest.NewServer(src)
		defer srcSrv.Close()
		body := `[{"deck":1,"net":"net://u/USB1/Track.flac","songLoading":true,"playing":false,"bpm":126}]`
		runCase(t, src, srcSrv, body, func() {
			if _, err := db.Exec(`UPDATE Track SET bpmAnalyzed=126.0, isAnalyzed=1`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?`, qc(trackBlob), qc(ovBlob), qc(beatBlob)); err != nil {
				t.Fatal(err)
			}
			copyDB()
		})
	})

	t.Run("streaming-stm-db", func(t *testing.T) {
		data := t.TempDir()
		shadowDataRoot = data
		dbDir := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library/Database2")
		_ = os.MkdirAll(dbDir, 0o755)
		audio := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library/Stream/s1.flac")
		_ = os.MkdirAll(filepath.Dir(audio), 0o755)
		_ = os.WriteFile(audio, []byte("STREAM-AUDIO"), 0o644)
		const uri = "streaming://TIDAL/Track/9001"
		dbPath, _, db := testlibrary.Create(t)
		if _, err := db.Exec(`INSERT INTO Track (path, filename, uri, bpmAnalyzed, isAnalyzed) VALUES ('Stream/s1.flac', 's1.flac', ?, 0, 0)`, uri); err != nil {
			t.Fatal(err)
		}
		stmdb := filepath.Join(dbDir, "stm.db")
		copyDB := func() {
			raw, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stmdb, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		copyDB()
		src := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), RekordboxOnDemandSock: "-"}, nil)
		defer src.Close()
		srcSrv := httptest.NewServer(src)
		defer srcSrv.Close()
		body := `[{"deck":1,"net":` + strconv.Quote(uri) + `,"playing":false,"bpm":126}]`
		runCase(t, src, srcSrv, body, func() {
			if _, err := db.Exec(`UPDATE Track SET bpmAnalyzed=126.0, isAnalyzed=1 WHERE uri=?`, uri); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, overviewWaveFormData=?, beatData=?`, qc(trackBlob), qc(ovBlob), qc(beatBlob)); err != nil {
				t.Fatal(err)
			}
			copyDB()
		})
	})

	t.Run("rekordbox-ondemand", func(t *testing.T) {
		media := t.TempDir()
		shadowMediaRoot = media
		stick, rel, _, _ := makeOnDemandStickFixture(t, media, "RB_WAKE")
		anlzDir := filepath.Join(stick, "PIONEER/USBANLZ")
		anlzBak := anlzDir + ".bak"
		if err := os.Rename(anlzDir, anlzBak); err != nil {
			t.Fatalf("hide USBANLZ: %v", err)
		}
		src := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: media, RekordboxOnDemandSock: "-"}, nil)
		defer src.Close()
		srcSrv := httptest.NewServer(src)
		defer srcSrv.Close()
		body := `[{"deck":1,"net":"net://u/RB_WAKE/` + rel + `","songLoading":true,"playing":false,"bpm":128}]`
		runCase(t, src, srcSrv, body, func() {
			if err := os.Rename(anlzBak, anlzDir); err != nil {
				t.Fatalf("restore USBANLZ: %v", err)
			}
			if _, err := src.buildOnDemandEntry(context.Background(), bridge.OnDemandRequest{
				Mount:      stick,
				EnginePath: "../" + rel,
				EngineKey:  -1,
			}, false); err != nil {
				t.Fatalf("buildOnDemandEntry: %v", err)
			}
		})
	})
}

// GPT R3 FIX (Finding 2): shadowPendingTimeout starts when waiting begins (not
// after the first job fails) and is enforced as one shared deadline across both
// EXPF and native format fetches.
func TestReviewShadowR3PendingDeadlineStartsAtWaitBeginAndSpansBothFormats(t *testing.T) {
	oldBoard := shadowIsPlayerBoard
	oldWait, oldTries, oldReqTimeout := perfRetryWait, perfRetries, perfRequestTimeout
	oldMaxFails, oldTimeout := shadowPendingMaxFails, shadowPendingTimeout
	shadowIsPlayerBoard = func() bool { return true }
	perfRetryWait = 40 * time.Millisecond
	perfRetries = 10
	perfRequestTimeout = 5 * time.Second
	shadowPendingMaxFails = 5 // must trip on elapsed timeout, not fail count
	shadowPendingTimeout = 160 * time.Millisecond
	t.Cleanup(func() {
		shadowIsPlayerBoard = oldBoard
		perfRetryWait, perfRetries, perfRequestTimeout = oldWait, oldTries, oldReqTimeout
		shadowPendingMaxFails, shadowPendingTimeout = oldMaxFails, oldTimeout
	})

	expf := validTestEXPF(128)
	var expfHits, nativeHits atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(shadowPerfFormatsHeader, "expf,native")
		if r.URL.Query().Get("format") == "native" {
			nativeHits.Add(1)
			// Hang longer than the remaining shared deadline (or return 503).
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
				http.Error(w, "analysing", http.StatusServiceUnavailable)
			}
			return
		}
		expfHits.Add(1)
		// Spend ~60% of the total shared budget on EXPF before succeeding so native
		// must inherit only the remaining ~40% of the same deadline.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(90 * time.Millisecond):
		}
		w.Header().Set(shadowPerfGenHeader, "1111111111111111")
		_, _ = w.Write(expf)
	}))
	defer peer.Close()

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.copyRetry = 10 * time.Millisecond
	u, _ := url.Parse(peer.URL)
	s.shadow.peerPort, _ = strconv.Atoi(u.Port())

	const key = "peer:shared-deadline"
	start := time.Now()
	s.shadow.mu.Lock()
	s.shadowFetchPeer(key, peerDeck{
		deckState: deckState{Deck: 1, Key: "shared-deadline", Ext: ".flac", Name: "Song.flac"},
		ip:        "127.0.0.1",
	})
	s.shadow.mu.Unlock()

	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.shadow.mu.Lock()
		busy := s.shadow.busy[key]
		c := s.shadow.copies[key]
		terminal := c != nil && c.terminal
		cerr := ""
		if c != nil {
			cerr = c.err
		}
		s.shadow.mu.Unlock()
		if !busy {
			elapsed := time.Since(start)
			if !terminal || cerr != "no analysis yet" {
				t.Fatalf("first job did not reach terminal \"no analysis yet\" on elapsed shared deadline: terminal=%v err=%q elapsed=%v", terminal, cerr, elapsed)
			}
			if elapsed > 450*time.Millisecond {
				t.Fatalf("shared deadline exceeded: elapsed=%v (want near %v, not perfRetries*perfRequestTimeout)", elapsed, shadowPendingTimeout)
			}
			if expfHits.Load() != 1 || nativeHits.Load() != 1 {
				t.Fatalf("expected 1 EXPF + 1 native request before shared deadline fired, got expf=%d native=%d", expfHits.Load(), nativeHits.Load())
			}
			return
		}
	}
	t.Fatal("shared deadline test timed out waiting for copy job to settle")
}
