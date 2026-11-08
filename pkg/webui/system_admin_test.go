package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type mockSystemRunner struct {
	mu       sync.Mutex
	commands [][]string
	outputs  map[string][]byte
}

func (m *mockSystemRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := append([]string{name}, args...)
	m.commands = append(m.commands, cmd)
	key := strings.Join(cmd, " ")
	if out, ok := m.outputs[key]; ok {
		return out, nil
	}
	// Default outputs
	if name == "systemctl" && len(args) >= 2 && args[0] == "is-active" {
		return []byte("active\n"), nil
	}
	return []byte("ok\n"), nil
}

func (m *mockSystemRunner) hasCommand(prefix ...string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cmd := range m.commands {
		if len(cmd) < len(prefix) {
			continue
		}
		match := true
		for i := range prefix {
			if cmd[i] != prefix[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestSSHManagement(t *testing.T) {
	mockRunner := &mockSystemRunner{outputs: make(map[string][]byte)}
	SetSystemRunner(mockRunner.Run)
	defer SetSystemRunner(nil)

	oldDropIn := sshPortDropInPath
	sshPortDropInPath = filepath.Join(t.TempDir(), "51-port.conf")
	defer func() { sshPortDropInPath = oldDropIn }()

	stateDir := t.TempDir()
	uiDir := t.TempDir()
	s := New(Config{StateDir: stateDir, UIDir: uiDir}, nil)

	// 1. Initial GET
	req := httptest.NewRequest(http.MethodGet, "/api/ssh", nil)
	w := httptest.NewRecorder()
	s.handleSSH(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var view SSHStatusView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !view.Enabled || view.Port != 22 || view.HasPassword {
		t.Fatalf("unexpected initial view: %+v", view)
	}

	// 2. Invalid port check
	badPortBody := `{"port": 70000}`
	req = httptest.NewRequest(http.MethodPost, "/api/ssh", bytes.NewReader([]byte(badPortBody)))
	w = httptest.NewRecorder()
	s.handleSSH(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for port 70000, got %d", w.Code)
	}

	badPortZero := `{"port": 0}`
	req = httptest.NewRequest(http.MethodPost, "/api/ssh", bytes.NewReader([]byte(badPortZero)))
	w = httptest.NewRecorder()
	s.handleSSH(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for port 0, got %d", w.Code)
	}

	// Reserved EngineX ports and multi-line passwords are refused before anything runs.
	for _, bad := range []string{`{"port": 8080}`, `{"port": 6595}`, `{"port": 13579}`, `{"password": "a\nroot:b"}`, `{"password": "a\u0000b"}`} {
		req = httptest.NewRequest(http.MethodPost, "/api/ssh", strings.NewReader(bad))
		w = httptest.NewRecorder()
		s.handleSSH(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d", bad, w.Code)
		}
	}
	if mockRunner.hasCommand("sh") || mockRunner.hasCommand("systemctl", "daemon-reload") {
		t.Fatal("a rejected request must not run commands")
	}

	// 3. Update port and password
	postBody := `{"port": 2222, "password": "$(touch /tmp/pwn)` + "`id`" + `\"x"}`
	req = httptest.NewRequest(http.MethodPost, "/api/ssh", bytes.NewReader([]byte(postBody)))
	w = httptest.NewRecorder()
	s.handleSSH(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if view.Port != 2222 || !view.HasPassword {
		t.Fatalf("expected port 2222 and hasPassword true, got: %+v", view)
	}
	if !mockRunner.hasCommand("systemctl", "restart", "sshd") || !mockRunner.hasCommand("systemctl", "daemon-reload") {
		t.Fatalf("expected daemon-reload and restart sshd")
	}
	// The password reaches chpasswd as $1, never inside the shell script text.
	found := false
	for _, c := range mockRunner.commands {
		if c[0] == "sh" {
			found = true
			if len(c) != 5 || strings.Contains(c[2], "touch") || c[4] != "$(touch /tmp/pwn)`id`\"x" {
				t.Fatalf("unsafe chpasswd command: %q", c)
			}
		}
	}
	if !found {
		t.Fatal("chpasswd was not run")
	}
	if b, _ := os.ReadFile(filepath.Join(stateDir, "ssh-config.json")); strings.Contains(string(b), "touch") || !strings.Contains(string(b), `"hasPassword": true`) && !strings.Contains(string(b), `"hasPassword":true`) {
		t.Fatalf("password persisted or hasPassword lost: %s", b)
	}

	// 4. Verify password is saved to disk but not in GET
	req = httptest.NewRequest(http.MethodGet, "/api/ssh", nil)
	w = httptest.NewRecorder()
	s.handleSSH(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "enginex_secret") {
		t.Fatalf("plaintext password leaked in GET response: %s", w.Body.String())
	}

	// 5. Disable SSH
	disableBody := `{"enabled": false}`
	req = httptest.NewRequest(http.MethodPost, "/api/ssh", bytes.NewReader([]byte(disableBody)))
	w = httptest.NewRecorder()
	s.handleSSH(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !mockRunner.hasCommand("systemctl", "stop", "sshd") {
		t.Fatalf("expected systemctl stop sshd")
	}

	// 6. Persistence across restart
	s2 := New(Config{StateDir: stateDir, UIDir: uiDir}, nil)
	savedCfg := s2.loadSSHConfig()
	if savedCfg.Enabled || savedCfg.Port != 2222 || !savedCfg.HasPassword || savedCfg.Password != "" {
		t.Fatalf("persisted config mismatch: %+v", savedCfg)
	}
}

func TestRemoteMountsManagementAndOmniSourceSync(t *testing.T) {
	mockRunner := &mockSystemRunner{outputs: make(map[string][]byte)}
	SetSystemRunner(mockRunner.Run)
	defer SetSystemRunner(nil)

	stateDir := t.TempDir()
	uiDir := t.TempDir()
	mediaDir := t.TempDir()
	s := New(Config{StateDir: stateDir, UIDir: uiDir, MediaRoot: mediaDir}, nil)

	// 1. Initial mounts list is empty
	req := httptest.NewRequest(http.MethodGet, "/api/mounts", nil)
	w := httptest.NewRecorder()
	s.handleMounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var list []RemoteMountView
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 mounts, got %d", len(list))
	}

	// 2. Validation errors
	badProto := `{"protocol": "ftp", "host": "192.168.1.50"}`
	req = httptest.NewRequest(http.MethodPost, "/api/mounts", bytes.NewReader([]byte(badProto)))
	w = httptest.NewRecorder()
	s.handleMounts(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad protocol, got %d", w.Code)
	}

	emptyHost := `{"protocol": "sftp", "host": ""}`
	req = httptest.NewRequest(http.MethodPost, "/api/mounts", bytes.NewReader([]byte(emptyHost)))
	w = httptest.NewRecorder()
	s.handleMounts(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty host, got %d", w.Code)
	}

	// 3. Add SFTP mount with custom non-standard port 2222
	sftpBody := `{
		"name": "Studio-SFTP",
		"protocol": "sftp",
		"host": "192.168.1.100",
		"port": 2222,
		"remotePath": "/media/music",
		"username": "dj",
		"password": "sftp_password"
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/mounts", bytes.NewReader([]byte(sftpBody)))
	w = httptest.NewRecorder()
	s.handleMounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var sftpMount RemoteMountView
	if err := json.Unmarshal(w.Body.Bytes(), &sftpMount); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sftpMount.Port != 2222 || !sftpMount.Mounted || !sftpMount.HasPassword {
		t.Fatalf("unexpected sftp mount view: %+v", sftpMount)
	}
	if !mockRunner.hasCommand("sshfs", "-p", "2222") {
		t.Fatalf("expected sshfs -p 2222 command to be executed")
	}

	// 4. Add SMB mount with custom port
	smbBody := `{
		"name": "Club-SMB",
		"protocol": "smb",
		"host": "nas.club.internal",
		"port": 4455,
		"remotePath": "djshare",
		"username": "clubdj",
		"password": "smb_password"
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/mounts", bytes.NewReader([]byte(smbBody)))
	w = httptest.NewRecorder()
	s.handleMounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !mockRunner.hasCommand("mount", "-t", "cifs") {
		t.Fatalf("expected mount -t cifs command")
	}

	// 5. Add WebDAV mount and verify automatic synchronization to OmniSource NAS config (nas.json)
	webdavBody := `{
		"name": "Cloud-WebDAV",
		"protocol": "webdav",
		"host": "dav.example.com",
		"port": 8443,
		"remotePath": "/music",
		"username": "davuser",
		"password": "davpassword"
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/mounts", bytes.NewReader([]byte(webdavBody)))
	w = httptest.NewRecorder()
	s.handleMounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Check that nas.json was automatically synchronized
	nasCfg := s.loadNASConfig()
	if nasCfg.Protocol != "webdav" || !nasCfg.Enabled || nasCfg.Username != "davuser" || nasCfg.Password != "davpassword" {
		t.Fatalf("expected auto-synced webdav nas config, got: %+v", nasCfg)
	}
	if !strings.Contains(nasCfg.URL, "8443") {
		t.Fatalf("expected non-standard port 8443 in nas url, got: %s", nasCfg.URL)
	}

	// 6. Test OmniSource track discovery in mounted share
	// Create mock files in the mounted share directory
	shareDir := filepath.Join(mediaDir, "Studio-SFTP")
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		t.Fatalf("mkdir error: %v", err)
	}
	_ = os.WriteFile(filepath.Join(shareDir, "Song1.mp3"), []byte("dummy mp3 audio"), 0o644)
	_ = os.WriteFile(filepath.Join(shareDir, "Song2.flac"), []byte("dummy flac audio"), 0o644)

	tracks, err := s.driveListTracks(context.Background(), "mount\x00"+sftpMount.ID)
	if err != nil {
		t.Fatalf("driveListTracks error: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}

	// Test omniTracks search query
	found, err := s.omniTracks(context.Background(), "Song1")
	if err != nil || len(found) != 1 {
		t.Fatalf("expected 1 track in omniTracks for 'Song1', got %d (err: %v)", len(found), err)
	}

	// 7. Delete mount
	delReq := httptest.NewRequest(http.MethodDelete, "/api/mounts?id="+sftpMount.ID, nil)
	delW := httptest.NewRecorder()
	s.handleMounts(delW, delReq)
	if delW.Code != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d: %s", delW.Code, delW.Body.String())
	}
	if !mockRunner.hasCommand("umount") {
		t.Fatalf("expected umount to be executed")
	}

	// Verify sftp mount is removed
	remList := s.loadMounts()
	var davID string
	for _, m := range remList {
		if m.ID == sftpMount.ID {
			t.Fatalf("deleted mount still present in list")
		}
		if m.Protocol == "webdav" {
			davID = m.ID
		}
	}

	// Deleting the last WebDAV mount must not leave its password in nas.json
	delReq = httptest.NewRequest(http.MethodDelete, "/api/mounts?id="+davID, nil)
	delW = httptest.NewRecorder()
	s.handleMounts(delW, delReq)
	if delW.Code != http.StatusOK {
		t.Fatalf("expected 200 deleting webdav mount, got %d: %s", delW.Code, delW.Body.String())
	}
	if b, err := os.ReadFile(s.nasConfigPath()); err == nil && strings.Contains(string(b), "davpassword") {
		t.Fatalf("nas.json still holds the deleted mount's password: %s", b)
	}
	if cfg := s.loadNASConfig(); cfg.Password != "" || cfg.Enabled {
		t.Fatalf("expected cleared nas config, got %+v", cfg)
	}
}

func TestAutomixStateMachine(t *testing.T) {
	stateDir := t.TempDir()
	uiDir := t.TempDir()
	s := New(Config{StateDir: stateDir, UIDir: uiDir}, nil)

	// 1. Initial GET
	req := httptest.NewRequest(http.MethodGet, "/api/automix", nil)
	w := httptest.NewRecorder()
	s.handleAutomix(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var st AutomixStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if st.Enabled || st.FadeSeconds != 10 || st.State != "idle" {
		t.Fatalf("unexpected initial automix status: %+v", st)
	}

	// 2. Validate fadeSeconds bounds (4..30)
	for _, badFade := range []int{2, 3, 31, 60, -1} {
		body := fmt.Sprintf(`{"fadeSeconds": %d}`, badFade)
		r := httptest.NewRequest(http.MethodPost, "/api/automix", bytes.NewReader([]byte(body)))
		rec := httptest.NewRecorder()
		s.handleAutomix(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for fadeSeconds=%d, got %d", badFade, rec.Code)
		}
	}

	// 3. Configure valid fadeSeconds and playlist tracks
	setupBody := `{
		"enabled": true,
		"fadeSeconds": 8,
		"tracks": [
			{"id": "tr1", "title": "Warmup House 1", "artist": "DJ A", "durationSec": 100},
			{"id": "tr2", "title": "Wedding Disco 2", "artist": "DJ B", "durationSec": 120},
			{"id": "tr3", "title": "Cocktail Hour 3", "artist": "DJ C", "durationSec": 150}
		]
	}`
	req = httptest.NewRequest(http.MethodPost, "/api/automix", bytes.NewReader([]byte(setupBody)))
	w = httptest.NewRecorder()
	s.handleAutomix(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !st.Enabled || st.FadeSeconds != 8 || len(st.Tracks) != 3 || st.State != "playing" {
		t.Fatalf("unexpected state after setup: %+v", st)
	}
	if st.CurrentDeck != 1 || st.CurrentTrack == nil || st.CurrentTrack.ID != "tr1" {
		t.Fatalf("expected CurrentDeck 1 and tr1, got: %+v", st)
	}
	if st.NextTrack == nil || st.NextTrack.ID != "tr2" {
		t.Fatalf("expected NextTrack tr2, got: %+v", st)
	}

	// 4. Manually trigger Next
	nextBody := `{"action": "next"}`
	req = httptest.NewRequest(http.MethodPost, "/api/automix", bytes.NewReader([]byte(nextBody)))
	w = httptest.NewRecorder()
	s.handleAutomix(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.CurrentDeck != 2 || st.CurrentIndex != 1 || st.CurrentTrack.ID != "tr2" {
		t.Fatalf("expected CurrentDeck 2 and tr2 after Next, got: %+v", st)
	}
	if st.NextTrack == nil || st.NextTrack.ID != "tr3" {
		t.Fatalf("expected NextTrack tr3, got: %+v", st)
	}

	// 5. Test playback clock simulation and automated crossfade
	mgr := s.automix
	mgr.Tick(110) // remaining = 120 - 110 = 10s (> 8s fadeSec)
	status := mgr.Status()
	if status.State != "playing" || status.CurrentDeck != 2 {
		t.Fatalf("expected playing on Deck 2, got: %+v", status)
	}

	// Advance into crossfade window (fadeSeconds = 8)
	mgr.Tick(6) // total 116s, remaining = 4s (<= 8s)
	status = mgr.Status()
	if status.State != "fading" {
		t.Fatalf("expected fading state, got: %s", status.State)
	}
	if status.Crossfade <= 0.0 || status.Crossfade >= 1.0 {
		t.Fatalf("expected crossfade between 0 and 1, got %f", status.Crossfade)
	}

	// Finish track -> automatic transition to Deck 1 (tr3)
	mgr.Tick(5)
	status = mgr.Status()
	if status.CurrentDeck != 1 || status.CurrentIndex != 2 || status.CurrentTrack.ID != "tr3" {
		t.Fatalf("expected transition to Deck 1 and tr3, got: %+v", status)
	}
}

func TestSSHAuthorizedKeyAndReadOnlyEtc(t *testing.T) {
	mockRunner := &mockSystemRunner{outputs: make(map[string][]byte)}
	SetSystemRunner(mockRunner.Run)
	defer SetSystemRunner(nil)

	dir := t.TempDir()
	oldA, oldB, oldC := sshPortDropInPath, sshRunDropInPath, sshPersistKeyDir
	sshPortDropInPath = filepath.Join(dir, "missing-file", "x", "51-port.conf") // parent is a file -> write fails
	_ = os.WriteFile(filepath.Join(dir, "missing-file"), nil, 0o644)
	sshRunDropInPath = filepath.Join(dir, "run", "51-port.conf")
	sshPersistKeyDir = filepath.Join(dir, "no-data")
	defer func() { sshPortDropInPath, sshRunDropInPath, sshPersistKeyDir = oldA, oldB, oldC }()

	stateDir := t.TempDir()
	s := New(Config{StateDir: stateDir, UIDir: t.TempDir()}, nil)

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleSSH(w, httptest.NewRequest(http.MethodPost, "/api/ssh", strings.NewReader(body)))
		return w
	}
	if w := post(`{"authorizedKey":"ssh-ed25519 AAAA\u0000x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("NUL key: expected 400, got %d", w.Code)
	}
	if w := post(`{"authorizedKey":"ssh-ed25519 AAAA test","port":2200}`); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	st, err := os.Stat(filepath.Join(stateDir, "authorized_keys"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file missing or wrong mode: %v %v", st, err)
	}
	if _, err := os.Stat(sshRunDropInPath); err != nil {
		t.Fatalf("expected /run drop-in fallback: %v", err)
	}
	w := httptest.NewRecorder()
	s.handleSSH(w, httptest.NewRequest(http.MethodGet, "/api/ssh", nil))
	if !strings.Contains(w.Body.String(), `"hasAuthorizedKey":true`) {
		t.Fatalf("hasAuthorizedKey not reported: %s", w.Body.String())
	}
}

func TestUninstallGen2Endpoint(t *testing.T) {
	mockRunner := &mockSystemRunner{outputs: make(map[string][]byte)}
	SetSystemRunner(mockRunner.Run)
	defer SetSystemRunner(nil)

	stateDir := t.TempDir()
	uiDir := t.TempDir()
	compatDir := t.TempDir()
	compatible := filepath.Join(compatDir, "compatible")
	defer func(old string) { deviceCompatiblePath = old }(deviceCompatiblePath)
	deviceCompatiblePath = compatible

	version := filepath.Join(stateDir, "version.txt")
	_ = os.WriteFile(version, []byte("5.3.0.3\n"), 0o644)
	s := New(Config{StateDir: stateDir, UIDir: uiDir, VersionFile: version}, nil)

	// 1. Generation 1 deck (e.g. SC5000 JP07) -> 400 Bad Request
	_ = os.WriteFile(compatible, []byte("inmusic,jp07\x00rockchip,rk3288\x00"), 0o644)
	for _, path := range []string{"/v1/sc5000/uninstall-gen2", "/api/uninstall-gen2"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		s.handleUninstallGen2(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400 for 1st-gen deck, got %d: %s", path, w.Code, w.Body.String())
		}
	}

	// 2. Generation 2 deck (e.g. SC LIVE 4 JP21) without uninstall.sh -> 404 Not Found
	_ = os.WriteFile(compatible, []byte("inmusic,jp21\x00rockchip,rk3288\x00"), 0o644)
	for _, path := range []string{"/v1/sc5000/uninstall-gen2", "/api/uninstall-gen2"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		s.handleUninstallGen2(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: expected 404 when uninstall.sh missing, got %d: %s", path, w.Code, w.Body.String())
		}
	}

	// 3. Generation 2 deck with uninstall.sh -> 200 OK and runs sh <customDir>/bundle/uninstall.sh
	bundleDir := filepath.Join(stateDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	uninstallPath := filepath.Join(bundleDir, "uninstall.sh")
	if err := os.WriteFile(uninstallPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write uninstall.sh: %v", err)
	}

	for _, path := range []string{"/v1/sc5000/uninstall-gen2", "/api/uninstall-gen2"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		s.handleUninstallGen2(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, w.Code, w.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("%s: unmarshal error: %v", path, err)
		}
		if res["ok"] != true || res["prefix"] != "SCLIVE4" {
			t.Fatalf("%s: unexpected response: %+v", path, res)
		}
	}

	if !mockRunner.hasCommand("sh", uninstallPath) {
		t.Fatalf("expected sh %s to be executed", uninstallPath)
	}

	// 4. Method not allowed for GET
	req := httptest.NewRequest(http.MethodGet, "/api/uninstall-gen2", nil)
	w := httptest.NewRecorder()
	s.handleUninstallGen2(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", w.Code)
	}
}
