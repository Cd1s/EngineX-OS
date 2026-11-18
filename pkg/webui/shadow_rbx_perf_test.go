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

// Maintainer, two linked SC5000s with shadow decks: a rekordbox Link load is instant
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
