package webui

import (
	"bytes"
	"encoding/json"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/bridgetest"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

func TestHandleRekordboxWriteback(t *testing.T) {
	mediaRoot := t.TempDir()
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), MediaRoot: mediaRoot}, nil)

	// Reject GET.
	w := httptest.NewRecorder()
	s.handleRekordboxWriteback(w, httptest.NewRequest(http.MethodGet, "/v1/enginex/rekordbox/writeback?drive=USB1", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405", w.Code)
	}

	// Reject traversal.
	w = httptest.NewRecorder()
	s.handleRekordboxWriteback(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/rekordbox/writeback?drive=../etc", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal status=%d, want 400", w.Code)
	}

	// Missing drive -> 404.
	w = httptest.NewRecorder()
	s.handleRekordboxWriteback(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/rekordbox/writeback?drive=MISSING", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing drive status=%d, want 404", w.Code)
	}

	// A library whose import is not complete (rows only, the job has not run) is skipped safely.
	stick := filepath.Join(mediaRoot, "USB1")
	testfixture.LargeExport(t, stick, 1)
	if _, err := bridge.ConvertAtMount(t.Context(), stick, bridge.MountOptions{}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleRekordboxWriteback(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/rekordbox/writeback?drive=USB1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("unstamped POST status=%d body=%s", w.Code, w.Body.String())
	}
	var skipped bridge.WriteBackResult
	if err := json.Unmarshal(w.Body.Bytes(), &skipped); err != nil {
		t.Fatal(err)
	}
	if skipped.TracksChecked != 0 || skipped.TracksChanged != 0 {
		t.Fatalf("expected unstamped stick to be skipped, got %+v", skipped)
	}

	// Properly mounted drive with v4 stamp -> 200 JSON and checks 1 track.
	bridgetest.Import(t, stick)

	w = httptest.NewRecorder()
	s.handleRekordboxWriteback(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/rekordbox/writeback?drive=USB1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", w.Code, w.Body.String())
	}
	var res bridge.WriteBackResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.TracksChecked != 1 || res.TracksChanged != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Verify /v1/enginex/rekordbox/writeback reaches the gateway through ServeHTTP.
	req := httptest.NewRequest(http.MethodPost, "/v1/enginex/rekordbox/writeback?drive=USB1", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ServeHTTP POST /v1/enginex/rekordbox/writeback status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// #321 review R2 (P3): final=1 (the Eject's pass) skips the check for a conversion at mount. Only
// the deck's own rb-bridge, on loopback, gets it; a request from the network, or relayed from
// outside, gets a plain pass, which keeps the check.
func TestRekordboxWritebackFinalOnlyFromTheDeck(t *testing.T) {
	mediaRoot := t.TempDir()
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), MediaRoot: mediaRoot}, nil)
	stick := filepath.Join(mediaRoot, "USB1")
	testfixture.LargeExport(t, stick, 1)
	bridgetest.Import(t, stick)
	var out bytes.Buffer
	log.SetOutput(&out)
	defer log.SetOutput(os.Stderr)
	for _, tc := range []struct {
		name, remote, relay, want string
	}{
		{"the deck", "127.0.0.1:43210", "", "pass: eject"},
		{"the network", "192.168.1.20:43210", "", "pass: manual"},
		{"relayed from outside", "127.0.0.1:43210", "Cf-Connecting-Ip", "pass: manual"},
	} {
		out.Reset()
		req := httptest.NewRequest(http.MethodPost, "/v1/enginex/rekordbox/writeback?final=1&drive=USB1", nil)
		req.RemoteAddr = tc.remote
		if tc.relay != "" {
			req.Header.Set(tc.relay, "203.0.113.9")
		}
		w := httptest.NewRecorder()
		s.handleRekordboxWriteback(w, req)
		if w.Code != http.StatusOK || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%s: status %d, log %q, want %q", tc.name, w.Code, out.String(), tc.want)
		}
	}
}
