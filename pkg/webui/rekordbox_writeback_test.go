package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
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

	// Valid drive -> 200 JSON.
	stick := filepath.Join(mediaRoot, "USB1")
	testfixture.LargeExport(t, stick, 1)
	if _, err := bridge.Convert(t.Context(), stick, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}

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
