package webui

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type playlistOpenFailureProvider struct{}

func (playlistOpenFailureProvider) Playlists(context.Context) ([]providers.Playlist, error) {
	return nil, nil
}

func (playlistOpenFailureProvider) Tracks(context.Context, string) ([]providers.Track, error) {
	return nil, errors.New("folder tracks unavailable")
}

func TestPlaylistOpenFailureDiagnostic(t *testing.T) {
	uiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("scope=spotify\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: uiDir, StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.sessions["bad"] = &Session{ID: "bad", Provider: "spotify", Ready: true, p: playlistOpenFailureProvider{}, expires: time.Now().Add(time.Hour)}
	s.sessions["good"] = &Session{ID: "good", Provider: "spotify", Ready: true, p: fixtureProvider{}, expires: time.Now().Add(time.Hour)}
	badID := s.tidal.addPlaylist(playlistRef{"bad", "spotify", providers.Playlist{ID: "folder-7", Title: "Folder"}, false})
	goodID := s.tidal.addPlaylist(playlistRef{"good", "spotify", providers.Playlist{ID: "p1", Title: "Good"}, false})

	var output bytes.Buffer
	log.SetOutput(writerFunc(output.Write))
	defer log.SetOutput(os.Stderr)

	request := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/playlists/"+id+"/tracks", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.tidal.ServeHTTP(w, req)
		return w
	}
	failed := request(badID)
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("failed open status=%d body=%s", failed.Code, failed.Body.String())
	}
	line := output.String()
	if strings.Count(strings.TrimSpace(line), "\n") != 0 || !strings.Contains(line, "stream-sync: playlist open failed") {
		t.Fatalf("unexpected failure log: %q", line)
	}
	for _, want := range []string{"route=/v1/playlists/", "status=502", "scope=spotify", "provider=spotify", "session=owner", "source_id=folder-7", "error=folder tracks unavailable"} {
		if !strings.Contains(line, want) {
			t.Fatalf("failure log lacks %q: %q", want, line)
		}
	}

	output.Reset()
	good := request(goodID)
	if good.Code != http.StatusOK || output.Len() != 0 {
		t.Fatalf("good open status=%d body=%s log=%q", good.Code, good.Body.String(), output.String())
	}
}
