package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// A Spotify, Beatport-less or unknown track never borrows another service's audio.
func TestAcquireDoesNotCrossServices(t *testing.T) {
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	defer j.Close()
	for provider, want := range map[string]string{"spotify": "spotify:", "applemusic": "applemusic:"} {
		_, _, err := j.acquire(context.Background(), "s", providers.Track{ID: "1", Provider: provider, Title: "T", Artist: "A", ISRC: "X"}, false, nil)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("%s: %v", provider, err)
		}
	}
}

func TestFromSpotifyDownloadsAndCaches(t *testing.T) {
	dlDir := t.TempDir()
	spAudio := []byte("OggS-downloaded-spotify-stream")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(spAudio)
	}))
	defer srv.Close()

	sp := &providers.Spotify{
		BaseURL: srv.URL,
		Token:   providers.Token{AccessToken: "test-token"},
	}

	deemix := &deemixclient.Client{DownloadDir: dlDir}
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: deemix})
	defer j.Close()
	j.spotify = func() *providers.Spotify { return sp }

	ctx := context.Background()
	track := providers.Track{ID: "sp-track-1", Provider: "spotify"}

	// 1. First fetch: should call DownloadAudio and return OGG_320
	dl, err := j.fromSpotify(ctx, "session1", track, false)
	if err != nil {
		t.Fatalf("fromSpotify download: %v", err)
	}
	if dl.Quality.Format != "OGG_320" {
		t.Fatalf("expected format OGG_320, got %q", dl.Quality.Format)
	}
	content, err := os.ReadFile(dl.Path)
	if err != nil || string(content) != string(spAudio) {
		t.Fatalf("unexpected downloaded content: %s %v", content, err)
	}

	// 2. Cached verification: Cached should find .ogg
	cached := deemix.Cached("spotify-" + track.ID)
	if cached == nil || cached.Path != dl.Path || cached.Quality.Format != "OGG_320" {
		t.Fatalf("deemix.Cached failed on ogg: %+v", cached)
	}

	// 3. Second fetch: shut down server, should hit cache without error
	srv.Close()
	dl2, err := j.fromSpotify(ctx, "session1", track, false)
	if err != nil || dl2.Path != dl.Path {
		t.Fatalf("fromSpotify cache hit failed: %v, %+v", err, dl2)
	}
}
