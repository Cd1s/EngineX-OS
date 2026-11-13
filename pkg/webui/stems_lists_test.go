package webui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestStemmedPathStatsOnlyMatchingTrack(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: Config{StateDir: dir}}

	hitPath := filepath.Join(dir, "hit.flac")
	if err := os.WriteFile(hitPath, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.addStemmed("tidal", stemmedTrack{
		Track: providers.Track{Provider: "tidal", ID: "hit"},
		Path:  hitPath,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := s.addStemmed("tidal", stemmedTrack{
			Track: providers.Track{Provider: "tidal", ID: fmt.Sprintf("other-%d", i)},
			Path:  filepath.Join(dir, fmt.Sprintf("other-%d.flac", i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var stats int
	orig := statStemmedPath
	statStemmedPath = func(name string) (os.FileInfo, error) {
		stats++
		return orig(name)
	}
	t.Cleanup(func() { statStemmedPath = orig })

	if got := s.stemmedPath(providers.Track{Provider: "tidal", ID: "missing"}); got != "" {
		t.Fatalf("missing track returned %q", got)
	}
	if stats != 0 {
		t.Fatalf("stemmedPath on unstemmed track performed %d disk stat calls, want 0", stats)
	}

	if got := s.stemmedPath(providers.Track{Provider: "tidal", ID: "hit"}); got != hitPath {
		t.Fatalf("matching track returned %q, want %q", got, hitPath)
	}
	if stats != 1 {
		t.Fatalf("stemmedPath on matching track performed %d disk stat calls, want 1", stats)
	}
}

func TestStemsSourceRecordsUnderTrackProviderNotCurrentBrowserScope(t *testing.T) {
	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	// Browser scope is currently "deezer", while Deck 1 stems a track from "spotify".
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("scope=deezer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oggPath := filepath.Join(dlDir, "spotify-sp123.ogg")
	if err := os.WriteFile(oggPath, []byte("OggS-fake-vorbis"), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{
		Catalog: noCatalog{},
		Deemix:  &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true},
	})
	t.Cleanup(jobs.Close)

	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)

	spTrack := providers.Track{ID: "sp123", Title: "Spotify Song", Artist: "DJ", Provider: "spotify"}
	tid := s.tidal.addTrack(trackRef{track: spTrack, session: "sp-sess"})

	req := httptest.NewRequest(http.MethodGet, "/v1/enginex/stems/source?id="+strconv.FormatInt(tid, 10), nil)
	req.RemoteAddr = "127.0.0.1:41000"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stemsSource status=%d body=%s", w.Code, w.Body.String())
	}
	if got := s.stemmed("spotify"); len(got) != 1 || got[0].Track.ID != "sp123" {
		t.Fatalf("expected track in spotify Stems list, got %+v (deezer=%+v)", got, s.stemmed("deezer"))
	}
	if bad := s.stemmed("deezer"); len(bad) != 0 {
		t.Fatalf("deezer Stems list polluted by spotify track: %+v", bad)
	}

	// Session provider takes precedence when an authenticated session owns the trackRef.
	nePath := filepath.Join(dlDir, "spotify-sp456.ogg")
	if err := os.WriteFile(nePath, []byte("OggS-fake-vorbis"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.sessions["ne-sess"] = &Session{ID: "ne-sess", Provider: "netease", Ready: true}
	s.mu.Unlock()
	neTid := s.tidal.addTrack(trackRef{
		track:   providers.Track{ID: "sp456", Title: "NetEase Session Song", Artist: "DJ", Provider: "spotify"},
		session: "ne-sess",
	})
	req2 := httptest.NewRequest(http.MethodGet, "/v1/enginex/stems/source?id="+strconv.FormatInt(neTid, 10), nil)
	req2.RemoteAddr = "127.0.0.1:41000"
	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("session stemsSource status=%d body=%s", w2.Code, w2.Body.String())
	}
	if got := s.stemmed("netease"); len(got) != 1 || got[0].Track.ID != "sp456" {
		t.Fatalf("expected session track in netease Stems list, got %+v (deezer=%+v)", got, s.stemmed("deezer"))
	}
	if bad := s.stemmed("deezer"); len(bad) != 0 {
		t.Fatalf("deezer Stems list polluted by netease session track: %+v", bad)
	}
}
