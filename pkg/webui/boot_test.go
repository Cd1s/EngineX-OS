package webui

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func isolateBootHostControl(t *testing.T) {
	t.Helper()
	// A regression in path selection must be caught before any named host
	// process can be signalled. Never delegate these stubs to the real tools.
	commands := t.TempDir()
	calls := filepath.Join(commands, "host-control-attempts")
	t.Setenv("ENGINEX_BOOT_CONTROL_LOG", calls)
	t.Setenv("PATH", commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"pkill", "killall", "kill", "systemctl", "systemd-run", "reboot", "az01-update", "planck-remote-screen"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"" + name + "\" >> \"$ENGINEX_BOOT_CONTROL_LOG\"\nexit 99\n"
		if err := os.WriteFile(filepath.Join(commands, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if body, err := os.ReadFile(calls); !os.IsNotExist(err) {
			t.Errorf("boot test attempted host process control: %q %v", body, err)
		}
	})
}

func bootTestServer(t *testing.T) (*Server, string, *int) {
	t.Helper()
	isolateBootHostControl(t)
	root := t.TempDir()
	dir := filepath.Join(root, "ui")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	restarts := new(int)
	s := New(Config{UIDir: dir, StateDir: filepath.Join(root, "state"), VersionFile: filepath.Join(root, "version.txt"), MediaRoot: filepath.Join(root, "media"), AccessToken: "TEST-CODE", RestartEngine: func() error { *restarts++; return nil }}, nil)
	if s.customDir() != root || isDeckDataDir(s.customDir()) {
		t.Fatal("boot tests must never use a host/deck data directory")
	}
	t.Cleanup(s.Close)
	return s, dir, restarts
}
func putBootTestFile(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBootUpdateFailureKeepsFallback(t *testing.T) {
	s, dir, _ := bootTestServer(t)
	putBootTestFile(t, dir, "safe-mode", "ui-ready-timeout")
	putBootTestFile(t, dir, "boot-count", "4")
	s.applyOnlineUpdate("://invalid")
	if !bootMarker(dir, "safe-mode") || !bootMarker(dir, "boot-count") {
		t.Fatal("starting a failed update cleared persistent fallback")
	}
}
func bootRequest(s *Server, method, path, body, remote, code string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remote
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	if code != "" {
		r.Header.Set("Authorization", "Bearer "+code)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestBootReadinessCurrentAttemptAndLoopback(t *testing.T) {
	s, dir, _ := bootTestServer(t)
	putBootTestFile(t, dir, "boot-token", "attempt-one\n")
	for _, tc := range []struct {
		name, method, path, body, remote string
		status                           int
	}{
		{"local token", "GET", "/v1/enginex/boot", "", "127.0.0.1:7", 200},
		{"remote token", "GET", "/v1/enginex/boot", "", "192.0.2.9:7", 403},
		{"remote ready", "POST", "/v1/enginex/boot/ready", `{"token":"attempt-one"}`, "192.0.2.9:7", 403},
		{"stale ready", "POST", "/v1/enginex/boot/ready", `{"token":"older"}`, "127.0.0.1:7", 409},
		{"unknown fields", "POST", "/v1/enginex/boot/ready", `{"token":"attempt-one","ready":true}`, "127.0.0.1:7", 400},
		{"current ready", "POST", "/v1/enginex/boot/ready", `{"token":"attempt-one"}`, "[::1]:7", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := bootRequest(s, tc.method, tc.path, tc.body, tc.remote, "")
			if w.Code != tc.status {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
		})
	}
	if b, err := os.ReadFile(filepath.Join(dir, "boot-ready")); err != nil || string(b) != "attempt-one\n" {
		t.Fatalf("ready %q: %v", b, err)
	}
	putBootTestFile(t, dir, "boot-token", "attempt-two\n")
	if w := bootRequest(s, "POST", "/v1/enginex/boot/ready", `{"token":"attempt-one"}`, "127.0.0.1:7", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	putBootTestFile(t, dir, "safe-mode", "")
	if w := bootRequest(s, "POST", "/v1/enginex/boot/ready", `{"token":"attempt-two"}`, "127.0.0.1:7", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w := bootRequest(s, "GET", "/v1/enginex/boot", "", "127.0.0.1:7", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
}

func TestBootPublicStatusHasOnlyFixedState(t *testing.T) {
	s, dir, _ := bootTestServer(t)
	putBootTestFile(t, dir, "boot-token", "private-attempt-value\n")
	putBootTestFile(t, dir, "boot-stage", "secret.invalid private log value")
	putBootTestFile(t, dir, "boot-progress", "private-attempt-value preparing 42\n")
	w := bootRequest(s, "GET", "/boot-status", "", "192.0.2.9:7", "")
	var status map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || len(status) != 5 || status["preparing"] != true {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, secret := range []string{"private", "secret", "token", "log", "path", "stage", "42"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("public response contains %s", secret)
		}
	}
	putBootTestFile(t, dir, "safe-mode", "")
	w = bootRequest(s, "GET", "/boot-status", "", "192.0.2.9:7", "")
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || status["fallback"] != true || status["preparing"] != false {
		t.Fatal(w.Body)
	}
	if w := bootRequest(s, "POST", "/boot-status", "", "192.0.2.9:7", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
	if w := bootRequest(s, "GET", "/", "", "192.0.2.9:7", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "/boot-status") || !strings.Contains(w.Body.String(), "EngineX fell back to stock Engine") {
		t.Fatalf("login notice missing: %d", w.Code)
	}
}

func TestBootResetExplicitAuthenticatedAndFailClosed(t *testing.T) {
	s, dir, restarts := bootTestServer(t)
	names := []string{"boot-token", "boot-ready", "boot-count", "boot-window", "boot-failures", "last-crash", "boot-progress", "boot-stage", "boot-report-latest", "boot-report-usb", "safe-mode"}
	for _, name := range names {
		putBootTestFile(t, dir, name, "old")
	}
	putBootTestFile(t, dir, "mods-quarantined", "keep")
	if w := bootRequest(s, "POST", "/api/boot-reset", "", "192.0.2.9:7", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/api/boot-reset", nil)
	r.Header.Set("Authorization", "Bearer TEST-CODE")
	r.Header.Set("Origin", "http://cross-site.invalid")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 || *restarts != 0 {
		t.Fatalf("CSRF %d", w.Code)
	}
	if w := bootRequest(s, "GET", "/api/boot-reset", "", "192.0.2.9:7", "TEST-CODE"); w.Code != 405 {
		t.Fatal(w.Code)
	}
	// A failed clearing operation cannot restart or remove fallback.
	if err := os.Remove(filepath.Join(dir, "boot-progress")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "boot-progress"), 0o700); err != nil {
		t.Fatal(err)
	}
	putBootTestFile(t, filepath.Join(dir, "boot-progress"), "occupied", "keep")
	if w := bootRequest(s, "POST", "/api/boot-reset", "", "192.0.2.9:7", "TEST-CODE"); w.Code != 500 || *restarts != 0 || !bootMarker(dir, "safe-mode") {
		t.Fatalf("failed reset %d restarts %d", w.Code, *restarts)
	}
	if err := os.RemoveAll(filepath.Join(dir, "boot-progress")); err != nil {
		t.Fatal(err)
	}
	if w := bootRequest(s, "POST", "/api/boot-reset", "", "192.0.2.9:7", "TEST-CODE"); w.Code != 200 || *restarts != 1 {
		t.Fatalf("reset %d restarts %d", w.Code, *restarts)
	}
	for _, name := range names {
		if bootMarker(dir, name) {
			t.Errorf("%s survived reset", name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "mods-quarantined")); err != nil || string(b) != "keep" {
		t.Fatal("quarantine changed")
	}
	s.cfg.RestartEngine = func() error { return errors.New("test failure") }
	if w := bootRequest(s, "POST", "/api/boot-reset", "", "192.0.2.9:7", "TEST-CODE"); w.Code != 500 || !bootMarker(dir, "safe-mode") {
		t.Fatalf("failed restart %d", w.Code)
	}
}

func TestBootResetCannotRaceReportPublication(t *testing.T) {
	s, dir, restarts := bootTestServer(t)
	putBootTestFile(t, dir, "safe-mode", "")
	putBootTestFile(t, dir, "boot-report-latest", "retained")
	lockFD, err := lockBootReports(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(lockFD)
	w := bootRequest(s, "POST", "/api/boot-reset", "", "192.0.2.9:7", "TEST-CODE")
	if w.Code != 500 || *restarts != 0 || !bootMarker(dir, "safe-mode") || !bootMarker(dir, "boot-report-latest") {
		t.Fatalf("reset raced report: %d", w.Code)
	}
}

// The console polls /boot-status once per page; showUI runs after every setting
// change and must not add another poller. The public login notice and the console
// table show the same three-language wording.
func TestBootNoticePollerOnceAndWordingShared(t *testing.T) {
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	if n := strings.Count(page, "setInterval(showBootStatus"); n != 1 {
		t.Fatalf("boot status poller registered %d times", n)
	}
	start := strings.Index(page, "function showUI(")
	end := strings.Index(page[start+1:], "\nfunction ")
	if start < 0 || end < 0 {
		t.Fatal("showUI not found")
	}
	if body := page[start : start+1+end]; strings.Contains(body, "showBootStatus") || strings.Contains(body, "bootRetry').onclick") {
		t.Fatal("showUI re-registers the boot notice handlers")
	}
	for _, key := range []string{"bootFallbackSaved", "bootFallbackNoDrive", "bootFallbackNoReport"} {
		m := regexp.MustCompile(key + `:(\['(?:[^'\\]|\\.)*','(?:[^'\\]|\\.)*','(?:[^'\\]|\\.)*'\])`).FindStringSubmatch(page)
		if m == nil || !strings.Contains(bootLoginNoticeScript, m[1]) {
			t.Errorf("%s: login notice wording differs from the console table", key)
		}
	}
}
