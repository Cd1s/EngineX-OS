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
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	if err := os.WriteFile(filepath.Join(dir, "fx-selector.ready"), nil, 0600); err != nil {
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
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
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
