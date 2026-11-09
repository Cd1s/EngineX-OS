package webui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFXSelectorRequiresMatchingAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), FXSelectorReadyFile: filepath.Join(dir, "fx-selector.ready")}, nil)
	if err := os.WriteFile(filepath.Join(dir, "fx-selector.ready"), []byte("armhf-px-v2 JC11\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fx-selector.ack"), []byte("1 2 old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			b, err := os.ReadFile(filepath.Join(dir, "fx-selector.request"))
			if err == nil && strings.HasPrefix(string(b), "1 2 ") {
				done <- os.WriteFile(filepath.Join(dir, "fx-selector.ack"), []byte(strings.TrimSpace(string(b))+" 0\n"), 0600)
				return
			}
			time.Sleep(time.Millisecond)
		}
		done <- os.ErrDeadlineExceeded
	}()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/sc5000/ui?fxUnit=1&fxMode=2", nil)
	s.uiUpdate(w, r)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fxSelectorReady") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.conf")); !os.IsNotExist(err) {
		t.Fatal("FX routes must not persist across Engine restarts")
	}
}

func TestFXSelectorRejectsMissingHookAndInvalidUnits(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), FXSelectorReadyFile: filepath.Join(dir, "fx-selector.ready")}, nil)
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"fxUnit=0&fxMode=1", 400}, {"fxUnit=7&fxMode=1", 400},
		{"fxUnit=1&fxMode=6", 400}, {"fxUnit=1&fxMode=1", 503},
	} {
		w := httptest.NewRecorder()
		s.uiUpdate(w, httptest.NewRequest("POST", "/v1/sc5000/ui?"+tc.query, nil))
		if w.Code != tc.code {
			t.Fatalf("%s: %d", tc.query, w.Code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "fx-selector.request")); !os.IsNotExist(err) {
		t.Fatal("rejected request wrote a route")
	}
}

func TestFXSelectorMigratesLegacySettings(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), FXSelectorReadyFile: filepath.Join(dir, "ready")}, nil)
	path := filepath.Join(dir, "settings.conf")
	legacy := "lang=en\npioneer_fx=spiral\nunknown_setting=preserved\npioneer_fx=space\n"
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	// Unsupported boards (including AZ04) keep their legacy master preference.
	w := httptest.NewRecorder()
	s.uiStatus(w, httptest.NewRequest("GET", "/api/ui", nil))
	b, _ := os.ReadFile(path)
	if string(b) != legacy {
		t.Fatal("legacy settings changed without PX hook")
	}
	if err := os.WriteFile(s.fxSelectorReadyFile(), []byte("armhf-px-v2 JC11\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.uiStatus(w, httptest.NewRequest("GET", "/api/ui", nil))
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "pioneer_fx=") || !strings.Contains(string(b), "unknown_setting=preserved") {
		t.Fatalf("migration: %s", b)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("migration permissions: %v %v", info, err)
	}
	if !strings.Contains(w.Body.String(), `"pioneerFx":"off"`) {
		t.Fatal(w.Body.String())
	}
	v := s.loadUI()
	v.PioneerFx = "space"
	if err := s.saveUI(v); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "pioneer_fx=") {
		t.Fatal("PX board resurrected master FX")
	}
}

func TestFXSelectorTimeoutDoesNotBlockSettings(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), FXSelectorReadyFile: filepath.Join(dir, "ready")}, nil)
	if err := os.WriteFile(s.fxSelectorReadyFile(), []byte("armhf-px-v2 JC11\n"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s.uiUpdate(w, httptest.NewRequest("POST", "/api/ui?fxUnit=1&fxMode=2", nil))
		done <- w.Code
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "fx-selector.request")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no request")
		}
		time.Sleep(time.Millisecond)
	}
	if !s.uiMu.TryLock() {
		t.Fatal("FX ACK wait holds settings lock")
	}
	s.uiMu.Unlock()
	w := httptest.NewRecorder()
	s.uiUpdate(w, httptest.NewRequest("POST", "/api/ui?fxUnit=2&fxMode=1", nil))
	if w.Code != 409 {
		t.Fatalf("concurrent FX request: %d", w.Code)
	}
	if code := <-done; code != 504 {
		t.Fatalf("timeout: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "fx-selector.request")); !os.IsNotExist(err) {
		t.Fatal("timed out request survives")
	}
}

func TestFXSelectorJP07PreservesLegacy(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), FXSelectorReadyFile: filepath.Join(dir, "ready")}, nil)
	path := filepath.Join(dir, "settings.conf")
	legacy := "lang=en\npioneer_fx=spiral\nunknown_setting=preserved\n"
	if err := os.WriteFile(path, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}
	for _, heartbeat := range []string{"armhf-px-v1\n", "armhf-px-v2 JP07\n"} {
		if err := os.WriteFile(s.fxSelectorReadyFile(), []byte(heartbeat), 0600); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.uiStatus(w, httptest.NewRequest("GET", "/api/ui", nil))
		b, _ := os.ReadFile(path)
		if string(b) != legacy {
			t.Fatal("JP07 preference was migrated")
		}
		if !strings.Contains(w.Body.String(), `"fxSelectorAvailable":false`) || !strings.Contains(w.Body.String(), `"pioneerFx":"spiral"`) {
			t.Fatal(w.Body.String())
		}
	}
	v := s.loadUI()
	v.PioneerFx = "space"
	if err := s.saveUI(v); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "pioneer_fx=space\n") {
		t.Fatal("JP07 cannot save legacy master FX")
	}
}

func TestFXSelectorQueuesOverlappingUnits(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), FXSelectorReadyFile: filepath.Join(dir, "ready")}, nil)
	if err := os.WriteFile(s.fxSelectorReadyFile(), []byte("armhf-px-v2 JC11\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seen := make(chan struct{})
	worker := make(chan error, 1)
	go func() {
		last := ""
		for count := 0; count < 2; count++ {
			deadline := time.Now().Add(2 * time.Second)
			for {
				b, err := os.ReadFile(filepath.Join(dir, "fx-selector.request"))
				if err == nil && string(b) != last {
					last = string(b)
					if count == 0 {
						close(seen)
						time.Sleep(100 * time.Millisecond)
					}
					if err := os.WriteFile(filepath.Join(dir, "fx-selector.ack"), []byte(strings.TrimSpace(last)+" 0\n"), 0600); err != nil {
						worker <- err
						return
					}
					break
				}
				if time.Now().After(deadline) {
					worker <- os.ErrDeadlineExceeded
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
		worker <- nil
	}()
	first := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s.uiUpdate(w, httptest.NewRequest("POST", "/api/ui?fxUnit=1&fxMode=1", nil))
		first <- w.Code
	}()
	select {
	case <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("first request not observed")
	}
	w := httptest.NewRecorder()
	s.uiUpdate(w, httptest.NewRequest("POST", "/api/ui?fxUnit=2&fxMode=2", nil))
	if code := <-first; code != 200 {
		t.Fatalf("unit 1: %d", code)
	}
	if w.Code != 200 {
		t.Fatalf("unit 2 overlap lost: %d", w.Code)
	}
	if err := <-worker; err != nil {
		t.Fatal(err)
	}
}
