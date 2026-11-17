package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
)

// Existing mount command tests model a deck with CIFS and its mount helpers.
func mockMountCapabilities(t *testing.T) {
	t.Helper()
	oldProc, oldLook := procFilesystems, mountLookPath
	t.Cleanup(func() { procFilesystems, mountLookPath = oldProc, oldLook })
	procFilesystems = filepath.Join(t.TempDir(), "filesystems")
	os.WriteFile(procFilesystems, []byte("nodev\tcifs\n"), 0600)
	mountLookPath = func(name string) (string, error) { return "/mock/" + name, nil }
}

func TestSMBMountWithoutCIFSNeverExecutesMount(t *testing.T) {
	smb := testsmb.Start(t)
	oldProc, oldLook := procFilesystems, mountLookPath
	t.Cleanup(func() { procFilesystems, mountLookPath = oldProc, oldLook })
	procFilesystems = filepath.Join(t.TempDir(), "filesystems")
	os.WriteFile(procFilesystems, []byte("nodev\tfuse\nfuseblk\n"), 0600)
	mountLookPath = func(string) (string, error) { return "", errors.New("not installed") }
	SetSystemRunner(func(context.Context, string, ...string) ([]byte, error) {
		t.Error("mount/helper must not be executed")
		return nil, errors.New("unexpected exec")
	})
	t.Cleanup(func() { SetSystemRunner(nil) })
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	m := RemoteMount{Name: "Music", Protocol: "smb", Host: smb.Host, Port: smb.Port, RemotePath: "music", Username: smb.User, Password: smb.Password, Enabled: true}
	post := func(m RemoteMount) *httptest.ResponseRecorder {
		b, _ := json.Marshal(m)
		w := httptest.NewRecorder()
		s.handleMounts(w, httptest.NewRequest("POST", "/api/mounts", bytes.NewReader(b)))
		return w
	}
	w := post(m)
	var view RemoteMountView
	json.Unmarshal(w.Body.Bytes(), &view)
	if w.Code != 200 || view.Mounted || !strings.Contains(view.Status, "userspace") || !view.HasPassword || strings.Contains(w.Body.String(), smb.Password) {
		t.Fatalf("mount view: %d %s", w.Code, w.Body.String())
	}
	m.Password = "wrong"
	w = post(m)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "SMB") {
		t.Fatalf("login error: %d %s", w.Code, w.Body.String())
	}
	saved := s.loadMounts()
	if len(saved) != 1 || saved[0].Mounted || !strings.Contains(saved[0].Status, "failed") {
		t.Fatalf("failed entry not retained: %+v", saved)
	}
	for _, proto := range []string{"sftp", "webdav"} {
		t.Run(proto, func(t *testing.T) {
			m.Protocol = proto
			w := post(m)
			if w.Code != 502 || !strings.Contains(w.Body.String(), "not installed") {
				t.Fatalf("missing helper: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
