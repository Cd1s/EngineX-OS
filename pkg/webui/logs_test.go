package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerboseLogSettings(t *testing.T) {
	old := verboseLogPath
	verboseLogPath = filepath.Join(t.TempDir(), "logging.ini")
	t.Cleanup(func() { verboseLogPath = old })
	dir := t.TempDir()
	restarts := 0
	cfg := Config{AccessToken: "console-code", UIDir: dir, StateDir: t.TempDir(), RestartEngine: func() error { restarts++; return nil }}
	s := New(cfg, nil)
	defer s.Close()
	if s.loadUI().VerboseLog {
		t.Fatal("verbose logging defaults on")
	}
	update := func(query string, status int) {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/ui?"+query, nil)
		req.Header.Set("Authorization", "Bearer console-code")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
		}
	}
	update("verboseLog=1&restart=1", 200)
	b, err := os.ReadFile(verboseLogPath)
	if err != nil || string(b) != verboseLogRules || restarts != 1 {
		t.Fatalf("config=%q err=%v restarts=%d", b, err, restarts)
	}
	b, err = os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil || !strings.Contains(string(b), "verbose_log=1\n") {
		t.Fatalf("settings=%q %v", b, err)
	}
	again := New(cfg, nil)
	defer again.Close()
	if !again.loadUI().VerboseLog {
		t.Fatal("preference lost after restart")
	}
	update("verboseLog=bad&restart=1", 400)
	if restarts != 1 {
		t.Fatal("invalid request restarted Engine")
	}
	update("verboseLog=0&restart=1", 200)
	if _, err := os.Stat(verboseLogPath); !os.IsNotExist(err) || s.loadUI().VerboseLog || restarts != 2 {
		t.Fatalf("off: %v restarts=%d", err, restarts)
	}
	update("verboseLog=0", 200) // removing an absent file is safe
	// A settings-file failure rolls back the already written logging config.
	settingsPath := filepath.Join(dir, "settings.conf")
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(settingsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	update("verboseLog=1&restart=1", 500)
	if _, err := os.Stat(verboseLogPath); !os.IsNotExist(err) || restarts != 2 {
		t.Fatalf("failed save left config: %v restarts=%d", err, restarts)
	}
	if err := os.Remove(settingsPath); err != nil {
		t.Fatal(err)
	}
	// Config write failure must neither persist the preference nor restart Engine.
	verboseLogPath = filepath.Join(t.TempDir(), "absent", "logging.ini")
	update("verboseLog=1&restart=1", 500)
	if s.loadUI().VerboseLog || restarts != 2 {
		t.Fatal("failed write changed preference or restarted")
	}
}

func TestRedactLog(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{"token", `accessToken="TOKEN_VALUE"`},
		{"arl", `{"deezerArl":"ARL_VALUE"}`},
		{"password", `password: "PASSWORD WITH SPACES"`},
		{"cookie", "Cookie: session=COOKIE_VALUE; other=OTHER_VALUE"},
		{"authorization", "Authorization: Bearer AUTH_VALUE"},
		{"bare bearer", "Bearer AUTH_VALUE"},
		{"signature header", "X-Amz-Signature: SIGNATURE_VALUE"},
		{"cookie field", "MUSIC_U=MUSIC_VALUE; sid=SID_VALUE"},
		{"stdout continuation", "  CONTINUATION_VALUE"},
		{"encoded mixed with percent", "CPU 45% https%3A%2F%2Fcdn.example%2Ff%3Fs%3DENCODED_VALUE"},
		{"encoded URL password", "fetch https://user:HUNTER%20TWO@cdn.example/f"},
		{"multiline", "HTTP response\nAuthorization: Basic AUTH_VALUE\n CONTINUATION_VALUE"},
		{"escaped json", `payload="{\"refresh_token\":\"ESCAPED_VALUE\"}"`},
		{"console code", "url /login CONSOLE_VALUE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactLog(tc.message, "CONSOLE_VALUE"); got != "[REDACTED: credential-related message]" {
				t.Fatalf("not redacted: %s", got)
			}
		})
	}
	if got := redactLog("console abcd efgh", "ABCD-EFGH"); got != "[REDACTED: credential-related message]" {
		t.Fatal("normalized console code survived")
	}
	got := redactLog("fetch https://user:URL_PASSWORD@cdn.example/music?Policy=URL_POLICY&Signature=URL_SIGNATURE&X-Amz-Credential=URL_CREDENTIAL#URL_FRAGMENT", "")
	for _, secret := range []string{"user", "URL_PASSWORD", "URL_POLICY", "URL_SIGNATURE", "URL_CREDENTIAL", "URL_FRAGMENT"} {
		if strings.Contains(got, secret) {
			t.Errorf("URL secret survived: %s", secret)
		}
	}
	for _, message := range []string{`GET /audio?Signature=PRIVATE_SIG&Expires=PRIVATE_EXPIRY`, `fetch https:\/\/cdn.example/file?Signature=PRIVATE_SIG`, `fetch https%3A%2F%2Fcdn.example%2Ffile%3FSignature%3DPRIVATE_SIG`, `fetch sftp://user:PRIVATE_PASS@nas.example/music`} {
		if got := redactLog(message, ""); strings.Contains(got, "PRIVATE_") {
			t.Fatal(got)
		}
	}
	got = redactLog("fetch https://alice:hunter2@cdn.example/f?x=PASTELVALUE#PARTVALUE", "")
	for _, value := range []string{"alice", "hunter2", "PASTELVALUE", "PARTVALUE"} {
		if strings.Contains(got, value) {
			t.Fatal("URL value survived")
		}
	}
	got = redactLog("GET /f?sig=ZXYABC&expires=12345", "")
	if got != "GET /f?sig=REDACTED&expires=REDACTED" {
		t.Fatal(got)
	}
	if got := redactLog("track loaded [air.audio] CPU 45%", ""); got != "track loaded [air.audio] CPU 45%" {
		t.Fatal(got)
	}
}

func logEntry(message string) []byte {
	b, _ := json.Marshal(map[string]string{"MESSAGE": message, "_SYSTEMD_UNIT": "engine.service", "__REALTIME_TIMESTAMP": "1700000000000000"})
	return append(b, '\n')
}

func TestJournalTextBounds(t *testing.T) {
	input := bytes.Repeat(logEntry(strings.Repeat("x", 1000)), 6000)
	out, limited, err := journalText(bytes.NewReader(input), "")
	if err != nil || !limited || len(out) == 0 || len(out) > logDownloadLimit {
		t.Fatalf("size=%d err=%v", len(out), err)
	}
	// A single giant or truncated entry must never be exported in fragments.
	out, limited, err = journalText(bytes.NewReader(logEntry(strings.Repeat("x", logReadLimit+100))), "")
	if err != nil || !limited || len(out) != 0 {
		t.Fatalf("oversized entry: %d %v", len(out), err)
	}
	input = append(logEntry("Authorization: secret\nprivate continuation"), logEntry("ordinary log")...)
	out, limited, err = journalText(bytes.NewReader(input), "")
	if err != nil || strings.Contains(string(out), "private continuation") || !strings.Contains(string(out), "ordinary log") {
		t.Fatalf("multiline output: %q %v", out, err)
	}
}

func TestLogDownloadRequiresAccessCode(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "journal.json")
	if err := os.WriteFile(fixture, append(logEntry("normal [air.audio]"), logEntry("ARL=private-credential")...), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "args")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > '%s'\ncat '%s'\n", marker, fixture)
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	s := New(Config{AccessToken: "console-code", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	for _, tc := range []struct {
		name, code string
		want       int
	}{
		{"missing", "", http.StatusUnauthorized}, {"wrong", "wrong", http.StatusUnauthorized}, {"correct", "console-code", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/logs?since=-1year&unit=sshd", nil)
			if tc.code != "" {
				req.Header.Set("Authorization", "Bearer "+tc.code)
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if tc.want != 200 {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("unauthorized request ran journalctl")
				}
				return
			}
			if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatal(w.Header())
			}
			if strings.Contains(w.Body.String(), "private-credential") || !strings.Contains(w.Body.String(), "normal [air.audio]") {
				t.Fatal(w.Body.String())
			}
		})
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	args := string(b)
	for _, arg := range []string{"--since=-30min", "--until=now", "--lines=5000", "--unit=engine.service", "--unit=stream-sync.service", "--unit=enginex-stream-sync.service", "--reverse"} {
		if !strings.Contains(args, arg+"\n") {
			t.Errorf("missing bound %s", arg)
		}
	}
	if strings.Contains(args, "sshd") || strings.Contains(args, "1year") {
		t.Fatal("request overrode journal bounds")
	}
}

func TestLogDownloadSizeAndBackendFailure(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "journal.json")
	input := bytes.Repeat(logEntry(strings.Repeat("x", 1000)), 6000)
	if err := os.WriteFile(fixture, input, 0o600); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	s := New(Config{AccessToken: "console-code", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	download := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/logs", nil)
		req.AddCookie(&http.Cookie{Name: "stream_sync", Value: "console-code"})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	w := download()
	if w.Code != 200 || w.Body.Len() == 0 || w.Body.Len() > logDownloadLimit || !strings.HasSuffix(w.Body.String(), logTruncated) {
		t.Fatalf("status=%d bytes=%d", w.Code, w.Body.Len())
	}
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf 'Authorization: BACKEND_SECRET' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w = download()
	if w.Code != 500 || strings.Contains(w.Body.String(), "BACKEND_SECRET") {
		t.Fatalf("backend error leaked: %d %s", w.Code, w.Body.String())
	}
}
