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
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func newTestServerWithUIDir(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		UIDir: dir,
		HTTP:  http.DefaultClient,
	}
	s := New(cfg, nil)
	return s, dir
}

func TestNASConfigCRUD(t *testing.T) {
	s, dir := newTestServerWithUIDir(t)

	// 1. Initial GET - empty config
	req := httptest.NewRequest(http.MethodGet, "/v1/sc5000/nas", nil)
	w := httptest.NewRecorder()
	s.handleNASConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var view NASConfigView
	if err := json.NewDecoder(w.Body).Decode(&view); err != nil {
		t.Fatalf("decode view failed: %v", err)
	}
	if view.Enabled || view.HasPassword || view.HasToken {
		t.Fatalf("expected empty config, got %+v", view)
	}

	// 2. POST invalid protocol
	badBody := []byte(`{"protocol":"ftp","url":"http://192.168.1.10:4533"}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/sc5000/nas", bytes.NewReader(badBody))
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad protocol, got %d", w.Code)
	}

	// 3. POST invalid URL
	badURLBody := []byte(`{"protocol":"subsonic","url":"not-a-url"}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/sc5000/nas", bytes.NewReader(badURLBody))
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad URL, got %d", w.Code)
	}

	// 4. POST valid config
	validBody := []byte(`{
		"protocol": "subsonic",
		"url": "http://192.168.1.10:4533",
		"username": "admin",
		"password": "supersecretpassword",
		"token": "secrettoken",
		"enabled": true
	}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/sc5000/nas", bytes.NewReader(validBody))
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.NewDecoder(w.Body).Decode(&view); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if view.Password != "***" || !view.HasPassword {
		t.Fatalf("expected masked password, got %+v", view)
	}
	if view.Token != "***" || !view.HasToken {
		t.Fatalf("expected masked token, got %+v", view)
	}

	// Check file on disk has 0600 permissions and contains plaintext secrets
	filePath := filepath.Join(dir, "nas.json")
	st, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("nas.json not created: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected 0600 permissions, got %o", perm)
	}
	rawDisk, _ := os.ReadFile(filePath)
	var onDisk providers.NASConfig
	_ = json.Unmarshal(rawDisk, &onDisk)
	if onDisk.Password != "supersecretpassword" || onDisk.Token != "secrettoken" {
		t.Fatalf("secrets not persisted accurately: %+v", onDisk)
	}

	// 5. POST update with empty / "***" password and token -> should keep existing secrets
	updateBody := []byte(`{
		"protocol": "subsonic",
		"url": "http://192.168.1.10:4533",
		"username": "admin2",
		"password": "***",
		"token": "",
		"enabled": true
	}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/sc5000/nas", bytes.NewReader(updateBody))
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify secrets were retained
	rawDisk, _ = os.ReadFile(filePath)
	_ = json.Unmarshal(rawDisk, &onDisk)
	if onDisk.Username != "admin2" || onDisk.Password != "supersecretpassword" || onDisk.Token != "secrettoken" {
		t.Fatalf("secrets were unexpectedly overwritten: %+v", onDisk)
	}

	// 6. Method not allowed
	req = httptest.NewRequest(http.MethodDelete, "/v1/sc5000/nas", nil)
	w = httptest.NewRecorder()
	s.handleNASConfig(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestNASTestAndPlaylists(t *testing.T) {
	mockBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/rest/getPlaylists.view" {
			fmt.Fprint(w, `{
				"subsonic-response": {
					"status": "ok",
					"version": "1.16.1",
					"playlists": {
						"playlist": [
							{"id": "p1", "name": "Favorites", "songCount": 10}
						]
					}
				}
			}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer mockBackend.Close()

	s, _ := newTestServerWithUIDir(t)

	// 1. handleNASTest with body
	testBody := fmt.Sprintf(`{
		"protocol": "subsonic",
		"url": %q,
		"username": "test",
		"password": "pwd",
		"enabled": true
	}`, mockBackend.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/sc5000/nas/test", bytes.NewReader([]byte(testBody)))
	w := httptest.NewRecorder()
	s.handleNASTest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handleNASTest failed (%d): %s", w.Code, w.Body.String())
	}
	var res map[string]any
	json.NewDecoder(w.Body).Decode(&res)
	if res["ok"] != true || res["playlists"] != float64(1) {
		t.Fatalf("unexpected test response: %+v", res)
	}

	// 2. Save config and test using saved config
	reqSave := httptest.NewRequest(http.MethodPost, "/v1/sc5000/nas", bytes.NewReader([]byte(testBody)))
	wSave := httptest.NewRecorder()
	s.handleNASConfig(wSave, reqSave)
	if wSave.Code != http.StatusOK {
		t.Fatalf("save failed: %s", wSave.Body.String())
	}

	reqSavedTest := httptest.NewRequest(http.MethodPost, "/v1/sc5000/nas/test", nil)
	wSavedTest := httptest.NewRecorder()
	s.handleNASTest(wSavedTest, reqSavedTest)
	if wSavedTest.Code != http.StatusOK {
		t.Fatalf("saved test failed: %s", wSavedTest.Body.String())
	}

	// 3. nasPlaylists should return the playlists and provider
	pls, prov := s.nasPlaylists(context.Background())
	if prov == nil {
		t.Fatalf("expected non-nil provider")
	}
	if len(pls) != 1 || pls[0].ID != "p1" {
		t.Fatalf("unexpected playlists: %+v", pls)
	}

	// 4. If disabled, nasPlaylists returns nil, nil
	disabledBody := fmt.Sprintf(`{
		"protocol": "subsonic",
		"url": %q,
		"enabled": false
	}`, mockBackend.URL)
	reqDisable := httptest.NewRequest(http.MethodPost, "/v1/sc5000/nas", bytes.NewReader([]byte(disabledBody)))
	wDisable := httptest.NewRecorder()
	s.handleNASConfig(wDisable, reqDisable)

	dPls, dProv := s.nasPlaylists(context.Background())
	if dPls != nil || dProv != nil {
		t.Fatalf("expected nil, nil when disabled, got pls=%+v, prov=%+v", dPls, dProv)
	}
}
