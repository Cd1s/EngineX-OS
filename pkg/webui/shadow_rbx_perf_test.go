package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Maintainer, two linked SC5000 players with shadow decks: a rekordbox Link load is instant
// on its deck, but the other player's shadow copy analysed the overview and grid
// again. The deck's file is the gateway's copy on a drive (or Engine's download
// before its row has it): no library lists it, so /perf answered 404 and the copy
// had no analysis. It must be the analysis this deck's load left for Engine.
func TestShadowPerfOfARekordboxLinkDeck(t *testing.T) {
	shadowBeaconOn = false
	defer func() { shadowBeaconOn = true }()
	media := t.TempDir()
	shadowMediaRoot = media
	defer func() { shadowMediaRoot = "/media" }()
	drive := filepath.Join(media, "USB")
	file := filepath.Join(drive, rbxCacheDir, "0123abcd.flac")
	os.MkdirAll(filepath.Dir(file), 0o755)
	os.WriteFile(file, []byte("AUDIO"), 0o644)
	oldDir := rbxAnalysisDir
	rbxAnalysisDir = t.TempDir()
	defer func() { rbxAnalysisDir = oldDir }()

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	id := s.tidal.addTrack(trackRef{track: providers.Track{ID: "x\x00192.0.2.5\x007", Provider: rekordboxScope, Title: "Song"}, file: file, drive: drive})
	tr := rekordbox.Track{ID: 7, SampleRate: 48000, SampleCount: 3 * 48000, Duration: 3, BPM: 120}
	a := rbxAnalysis(analysedRbxLink{}, 7)
	a.Waveforms = []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{100, 50, 25}, 300)}}
	p, err := enginedb.EncodePerformance(tr, a)
	if err != nil || !p.Complete {
		t.Fatalf("fixture: %v %v", err, p.Complete)
	}
	if !rbxPublishAnalysis(rbxAnalysisPath(id), p, tr.BPM, false) {
		t.Fatal("no analysis file")
	}

	body := fmt.Sprintf(`[{"deck":1,"net":"streaming://TIDAL/Track/%d","bpm":120}]`, id)
	if resp, err := http.Post(srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("post: %v %v", resp, err)
	}
	var peer struct{ Decks []deckState }
	resp, _ := http.Get(srv.URL + "/peer/decks")
	json.NewDecoder(resp.Body).Decode(&peer)
	resp.Body.Close()
	if len(peer.Decks) != 1 || peer.Decks[0].Key == "" {
		t.Fatalf("peer decks: %+v", peer.Decks)
	}
	get := func(q string) []byte {
		resp, err := http.Get(srv.URL + "/peer/decks/1/perf?key=" + peer.Decks[0].Key + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("perf%s: %s %q: the shadow copy analyses the track again", q, resp.Status, b)
		}
		return b
	}
	// The native record the shadow hook fills Engine's row with: the same blobs.
	native := get("&format=native")
	want, _ := encodeShadowAnalysis(p)
	if !validShadowAnalysis(native) || !bytes.Equal(native, want) {
		t.Fatalf("native analysis differs from the rekordbox load's")
	}
	// The EXPF OfflineAnalyzer replay, for a peer that cannot fill the row.
	if b := get(""); !bytes.HasPrefix(b, []byte("EXPF")) {
		t.Fatalf("perf %q", b)
	}
}

// rbxShadowSource is a source player with a rekordbox Link deck on deck 1 of the
// gateway row id, whose audio is the gateway's copy on a drive.
type rbxShadowSource struct {
	s    *Server
	srv  *httptest.Server
	file string
}

func newRbxShadowSource(t *testing.T) *rbxShadowSource {
	t.Helper()
	shadowBeaconOn = false
	media := t.TempDir()
	shadowMediaRoot = media
	oldDir := rbxAnalysisDir
	oldEvery := perfGenEvery
	rbxAnalysisDir = t.TempDir()
	perfGenEvery = 0
	t.Cleanup(func() {
		shadowBeaconOn, shadowMediaRoot, rbxAnalysisDir, perfGenEvery = true, "/media", oldDir, oldEvery
	})
	drive := filepath.Join(media, "USB")
	file := filepath.Join(drive, rbxCacheDir, "0123abcd.flac")
	os.MkdirAll(filepath.Dir(file), 0o755)
	os.WriteFile(file, []byte("AUDIO"), 0o644)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(s)
	t.Cleanup(func() { srv.Close(); s.Close() })
	return &rbxShadowSource{s: s, srv: srv, file: file}
}

func (r *rbxShadowSource) row(tid uint32) int64 {
	return r.s.tidal.addTrack(trackRef{track: providers.Track{ID: fmt.Sprintf("x\x00192.0.2.5\x00%d", tid), Provider: rekordboxScope, Title: "Song"}, file: r.file, drive: filepath.Dir(filepath.Dir(r.file))})
}

// publish leaves row id's analysis with its first beat at firstMS, as a load or a refresh does.
func (r *rbxShadowSource) publish(t *testing.T, id int64, firstMS uint32) enginedb.Performance {
	t.Helper()
	a := &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{Number: 1, BPM: 120, TimeMS: firstMS}, {Number: 2, BPM: 120, TimeMS: firstMS + 500}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: bytes.Repeat([]byte{100, 50, 25}, 300)}},
	}
	p, err := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 48000, SampleCount: 3 * 48000, Duration: 3, BPM: 120}, a)
	if err != nil || !p.Complete || !rbxPublishAnalysis(rbxAnalysisPath(id), p, 120, false) {
		t.Fatalf("publish: %v", err)
	}
	return p
}

// post shows row id on deck 1 and returns the key a peer copies it by.
func (r *rbxShadowSource) post(t *testing.T, id int64) string { return r.status(t, id).Key }

// status shows row id on deck 1 and returns what a peer sees of it.
func (r *rbxShadowSource) status(t *testing.T, id int64) deckState {
	t.Helper()
	body := fmt.Sprintf(`[{"deck":1,"net":"streaming://TIDAL/Track/%d","bpm":120}]`, id)
	if resp, err := http.Post(r.srv.URL+"/v1/enginex/decks", "application/json", strings.NewReader(body)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("post: %v %v", resp, err)
	}
	var peer struct{ Decks []deckState }
	resp, _ := http.Get(r.srv.URL + "/peer/decks")
	json.NewDecoder(resp.Body).Decode(&peer)
	resp.Body.Close()
	if len(peer.Decks) != 1 || peer.Decks[0].Key == "" {
		t.Fatalf("peer decks: %+v", peer.Decks)
	}
	return peer.Decks[0]
}

func (r *rbxShadowSource) native(t *testing.T, key string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(r.srv.URL + "/peer/decks/1/perf?format=native&key=" + key)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// Two rekordbox rows can share one audio file (the same file in two places of the
// library, or the gateway's cache): switching between them must not keep the copy
// of the first, with its grid.
func TestShadowRekordboxRowSwitchIsANewCopy(t *testing.T) {
	r := newRbxShadowSource(t)
	a, b := r.row(7), r.row(8)
	r.publish(t, a, 500)
	pb := r.publish(t, b, 250)
	ka := r.post(t, a)
	kb := r.post(t, b)
	if ka == kb {
		t.Fatal("the other row's copy key is the first row's: the receiver keeps the first grid")
	}
	want, _ := encodeShadowAnalysis(pb)
	if code, got := r.native(t, kb); code != 200 || !bytes.Equal(got, want) {
		t.Fatalf("second row's analysis: %d", code)
	}
}

// A rekordbox edit changes the analysis served to a shadow copy without changing
// the audio copy key. The source must publish that generation so the receiver can
// refresh its already-cached sidecars on the next load.
func TestShadowRekordboxEditChangesPerfGenerationWithoutNewAudioKey(t *testing.T) {
	r := newRbxShadowSource(t)
	id := r.row(7)
	r.publish(t, id, 500)
	first := r.status(t, id)
	if first.PerfGen == "" {
		t.Fatal("analysed rekordbox deck has no PerfGen")
	}
	if again := r.status(t, id); again.Key != first.Key || again.PerfGen != first.PerfGen {
		t.Fatalf("unchanged analysis moved key/generation: %+v -> %+v", first, again)
	}
	r.publish(t, id, 250)
	second := r.status(t, id)
	if second.Key != first.Key {
		t.Fatalf("analysis edit changed audio copy key: %q -> %q", first.Key, second.Key)
	}
	if second.PerfGen == first.PerfGen {
		t.Fatalf("analysis edit did not publish a new PerfGen: %q", second.PerfGen)
	}
}

// The deck's file can be there before its analysis is written: while the load is
// still reading it from rekordbox the source says it is coming (503) under the same
// key, then serves it; a load that ended without one is a 404 (analysed there).
func TestShadowRekordboxDelayedAnalysis(t *testing.T) {
	r := newRbxShadowSource(t)
	id := r.row(7)
	rbxAnalysing.Store(id, true)
	k1 := r.post(t, id)
	if code, _ := r.native(t, k1); code != http.StatusServiceUnavailable {
		t.Fatalf("analysis being read: %d, want 503", code)
	}
	p := r.publish(t, id, 500)
	rbxAnalysing.Delete(id)
	if k2 := r.post(t, id); k2 != k1 {
		t.Fatalf("analysis written between two reports moved the key %q -> %q: the copy gets 409", k1, k2)
	}
	want, _ := encodeShadowAnalysis(p)
	if code, got := r.native(t, k1); code != 200 || !bytes.Equal(got, want) {
		t.Fatalf("the first report's key: %d", code)
	}
	other := r.row(8) // its load ended without analysis
	if code, _ := r.native(t, r.post(t, other)); code != http.StatusNotFound {
		t.Fatalf("no analysis and no load reading one: %d, want 404", code)
	}
}

// The load can finish between /perf's read of the analysis and its check of the
// running load: the read missed a file that is now there, so it is 503 (ask again),
// not 404 (which makes the copy analyse the track again).
func TestShadowRekordboxAnalysisFinishesDuringRead(t *testing.T) {
	r := newRbxShadowSource(t)
	id := r.row(7)
	key := r.post(t, id)
	rbxAnalysing.Store(id, true)
	defer rbxAnalysing.Delete(id)
	old := afterPerfRead
	afterPerfRead = func() { r.publish(t, id, 500); rbxAnalysing.Delete(id) }
	defer func() { afterPerfRead = old }()
	if code, _ := r.native(t, key); code != http.StatusServiceUnavailable {
		t.Fatalf("analysis written during the read: %d, want 503", code)
	}
	afterPerfRead = old
	if code, _ := r.native(t, key); code != 200 {
		t.Fatalf("the next ask: %d", code)
	}
}
