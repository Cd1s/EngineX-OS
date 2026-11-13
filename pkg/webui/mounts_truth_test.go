package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// BH-GC2-14: Remote Mounts answered 200 with mounted: true when the mount command failed (sshfs is
// not on the stock rootfs), reported a saved mounted: true after a reboot that remounted nothing,
// and left a share mounted when it was switched off.
func TestRemoteMountsReportTheTruth(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	SetSystemRunner(func(_ context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "sshfs" {
			return nil, errors.New(`exec: "sshfs": executable file not found in $PATH`)
		}
		return nil, nil
	})
	defer SetSystemRunner(nil)
	mounted := map[string]bool{}
	saved := mountedAt
	mountedAt = func(path string) bool { mu.Lock(); defer mu.Unlock(); return mounted[path] }
	defer func() { mountedAt = saved }()

	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleMounts(w, httptest.NewRequest(http.MethodPost, "/api/mounts", bytes.NewReader([]byte(body))))
		return w
	}
	get := func() []RemoteMountView {
		w := httptest.NewRecorder()
		s.handleMounts(w, httptest.NewRequest(http.MethodGet, "/api/mounts", nil))
		var list []RemoteMountView
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		return list
	}

	// A failing mount is an error, and the share is kept in the settings, not mounted.
	if w := post(`{"name":"Studio","protocol":"sftp","host":"10.0.0.2","remotePath":"/music"}`); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "sshfs") {
		t.Fatalf("failed sshfs: %d %s", w.Code, w.Body.String())
	}
	if l := get(); len(l) != 1 || l[0].Mounted {
		t.Fatalf("after a failed mount: %+v", l)
	}

	// A flag saved before a reboot is not the truth.
	if w := post(`{"name":"NAS","protocol":"smb","host":"nas","remotePath":"share"}`); w.Code != http.StatusOK {
		t.Fatalf("smb: %d %s", w.Code, w.Body.String())
	}
	var nas RemoteMountView
	for _, m := range get() {
		if m.Name == "NAS" {
			nas = m
		}
	}
	if nas.Mounted {
		t.Fatal("GET reports a share mounted that is not in the mount table (e.g. after a reboot)")
	}
	b, _ := os.ReadFile(s.mountsConfigPath())
	if !strings.Contains(string(b), `"mounted": true`) && !strings.Contains(string(b), `"mounted":true`) {
		t.Fatalf("expected the stale saved flag in the fixture: %s", b)
	}

	// Switching a mounted share off unmounts it.
	mu.Lock()
	mounted[nas.MountPoint] = true
	calls = nil
	mu.Unlock()
	if w := post(`{"name":"NAS","protocol":"smb","host":"nas","remotePath":"share","enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "umount "+nas.MountPoint {
		t.Fatalf("switching off ran %q, want one umount of %s", calls, nas.MountPoint)
	}
}
