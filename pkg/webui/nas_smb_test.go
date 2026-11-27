package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestSMBNASAPIAndRebootReload(t *testing.T) {
	smb := testsmb.Start(t)
	os.WriteFile(filepath.Join(smb.Root, "Été.mp3"), []byte("ID3 fixture"), 0600)
	s, dir := newTestServerWithUIDir(t)
	cfg := providers.NASConfig{Protocol: "smb", URL: smb.URL, Username: smb.User, Password: smb.Password, Enabled: true}
	b, _ := json.Marshal(cfg)
	w := httptest.NewRecorder()
	s.handleNASConfig(w, httptest.NewRequest("POST", "/api/nas", bytes.NewReader(b)))
	if w.Code != 200 || strings.Contains(w.Body.String(), smb.Password) || !strings.Contains(w.Body.String(), `"hasPassword":true`) {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	// A new server reloads the saved config on its first browse, with no mount/remount.
	s = New(Config{UIDir: dir}, nil)
	lists, p := s.nasPlaylists(context.Background())
	if p == nil || len(lists) != 1 || lists[0].Count != 1 {
		t.Fatalf("reboot browse: %+v", lists)
	}
	tracks, err := p.Tracks(context.Background(), "root")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("tracks: %+v %v", tracks, err)
	}
	file, err := p.DownloadTrack(context.Background(), tracks[0].ID, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "ID3 fixture" {
		t.Fatalf("download: %q", got)
	}
	w = httptest.NewRecorder()
	s.handleNASConfig(w, httptest.NewRequest("DELETE", "/api/nas", nil))
	if w.Code != 200 || s.ownerSelfHosted() != nil {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}
}

func TestNASRejectsPasswordsInURLs(t *testing.T) {
	s, _ := newTestServerWithUIDir(t)
	for _, protocol := range []string{"webdav", "smb"} {
		t.Run(protocol, func(t *testing.T) {
			scheme := "https"
			if protocol == "smb" {
				scheme = "smb"
			}
			cfg := providers.NASConfig{Protocol: protocol, URL: scheme + "://user:fixture-password@nas.example/music", Enabled: true}
			b, _ := json.Marshal(cfg)
			w := httptest.NewRecorder()
			s.handleNASConfig(w, httptest.NewRequest("POST", "/api/nas", bytes.NewReader(b)))
			if w.Code != 400 || strings.Contains(w.Body.String(), "fixture-password") {
				t.Fatalf("URL credentials accepted or echoed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNASLegacyURLLoginStaysPrivateAndWorking(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "fixture-user" || password != "fixture-password" {
			t.Error("legacy URL login was lost")
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `<multistatus xmlns="DAV:"><response><href>/music/a.mp3</href><propstat><prop><displayname>a.mp3</displayname></prop></propstat></response></multistatus>`)
	}))
	defer backend.Close()
	// The legacy file is there when the deck starts: start-up reads which server the
	// slot names (WebDAV round 5).
	dir := t.TempDir()
	cfg := providers.NASConfig{Protocol: "webdav", URL: strings.Replace(backend.URL, "://", "://fixture-user:fixture-password@", 1) + "/music", Enabled: true}
	if err := providers.SaveJSON(filepath.Join(dir, "nas.json"), cfg); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: dir, HTTP: http.DefaultClient}, nil)
	w := httptest.NewRecorder()
	s.handleNASConfig(w, httptest.NewRequest("GET", "/api/nas", nil))
	if strings.Contains(w.Body.String(), "fixture-password") || !strings.Contains(w.Body.String(), `"hasPassword":true`) {
		t.Fatalf("legacy credentials echoed or lost: %s", w.Body.String())
	}
	lists, provider := s.nasPlaylists(context.Background())
	if provider == nil || len(lists) != 1 || lists[0].Count != 1 {
		t.Fatalf("legacy source no longer works: %+v", lists)
	}
}
