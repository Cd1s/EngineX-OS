package webui

import (
	"bytes"
	"context"
	"encoding/json"
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
