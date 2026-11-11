package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestModsUpload(t *testing.T) {
	for _, multipartUpload := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "multipart"}[multipartUpload], func(t *testing.T) {
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			for name, data := range map[string]string{"mod.json": `{"id":"upload-test","qmlFiles":{"view.qml":"Views/Test.qml"}}`, "view.qml": "Item {}"} {
				if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write([]byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			contentType := "application/gzip"
			if multipartUpload {
				form := multipart.NewWriter(&body)
				part, err := form.CreateFormFile("file", "mod.tar.gz")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write(archive.Bytes()); err != nil {
					t.Fatal(err)
				}
				contentType = form.FormDataContentType()
				if err := form.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				body.Write(archive.Bytes())
			}
			base := t.TempDir()
			s := New(Config{StateDir: filepath.Join(base, "state"), UIDir: filepath.Join(base, "ui")}, nil)
			req := httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods/upload", &body)
			req.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("upload %d: %s", w.Code, w.Body.String())
			}
			if _, err := os.Stat(filepath.Join(base, "ui", "mods-overlay", "Views", "Test.qml")); err == nil {
				t.Fatal("uploaded mod must stay disabled until enabled")
			}
			w = httptest.NewRecorder()
			s.handleMods(w, httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods", bytes.NewBufferString(`{"id":"upload-test","enabled":true}`)))
			if w.Code != 200 {
				t.Fatalf("enable %d: %s", w.Code, w.Body.String())
			}
			if _, err := os.Stat(filepath.Join(base, "ui", "mods-overlay", "Views", "Test.qml")); err != nil {
				t.Fatal(err)
			}
			w = httptest.NewRecorder()
			s.handleMods(w, httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods", bytes.NewBufferString(`{"id":"upload-test","action":"uninstall"}`)))
			if w.Code != 200 {
				t.Fatalf("uninstall %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestModsTailscaleLifecycle(t *testing.T) {
	fake := &tsFake{}
	SetSystemRunner(fake.run)
	defer SetSystemRunner(nil)
	tsRelease(t, []byte("fake binary"), "")
	base := t.TempDir()
	s := New(Config{StateDir: filepath.Join(base, "state"), UIDir: filepath.Join(base, "ui")}, nil)
	for _, body := range []string{`{"id":"tailscale-remote","enabled":true}`, `{"id":"tailscale-remote","enabled":false}`, `{"id":"tailscale-remote","action":"uninstall"}`} {
		w := httptest.NewRecorder()
		s.handleMods(w, httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods", bytes.NewBufferString(body)))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if s.tailscaleInstalled() || s.loadTailscaleConfig().Enabled || !fake.has("systemd-run --unit=sc5000-tailscaled") || !fake.has("systemctl stop sc5000-tailscaled") {
		t.Fatal("tailscale lifecycle did not complete")
	}
}

func createWebUITestMod(t *testing.T, mediaRoot, drive, id, qmlRel string) {
	t.Helper()
	modDir := filepath.Join(mediaRoot, drive, "mods", id)
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	srcFile := "test.qml"
	if err := os.WriteFile(filepath.Join(modDir, srcFile), []byte("Item { Text { text: \"Hello\" } }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"id":          id,
		"name":        "Test " + id,
		"version":     "1.0.0",
		"author":      "Tester",
		"description": "Mod for webui testing",
		"qmlFiles": map[string]string{
			srcFile: qmlRel,
		},
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(modDir, "manifest.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestMods_WebUI_Get(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()

	createWebUITestMod(t, mediaRoot, "USB_KEY", "dark-theme", "Skins/Dark.qml")

	srv := &Server{
		cfg: Config{
			MediaRoot: mediaRoot,
			UIDir:     uiDir,
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sc5000/mods", nil)
	rec := httptest.NewRecorder()
	srv.handleMods(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var res struct {
		Mods []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Version     string `json:"version"`
			Author      string `json:"author"`
			Description string `json:"description"`
			Drive       string `json:"drive"`
			Enabled     bool   `json:"enabled"`
			Valid       bool   `json:"valid"`
			Error       string `json:"error"`
			QMLCount    int    `json:"qmlCount"`
		} `json:"mods"`
		SafeModeQuarantined bool `json:"safeModeQuarantined"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if res.SafeModeQuarantined {
		t.Errorf("expected SafeModeQuarantined to be false")
	}
	if len(res.Mods) != 2 || res.Mods[0].ID != "tailscale-remote" {
		t.Fatalf("expected system mod and external mod, got %+v", res.Mods)
	}
	m := res.Mods[1]
	if m.ID != "dark-theme" || m.Drive != "USB_KEY" || !m.Valid || m.QMLCount != 1 || m.Enabled {
		t.Errorf("unexpected mod details: %+v", m)
	}
}

func TestMods_WebUI_PostEnableAndDisable(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()

	createWebUITestMod(t, mediaRoot, "USB_KEY", "wave-mod", "Skins/Wave.qml")

	srv := &Server{
		cfg: Config{
			MediaRoot: mediaRoot,
			UIDir:     uiDir,
		},
	}

	// 1. Enable mod
	body := bytes.NewBufferString(`{"id": "wave-mod", "enabled": true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods", body)
	rec := httptest.NewRecorder()
	srv.handleMods(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("enable failed: %d: %s", rec.Code, rec.Body.String())
	}

	// Check overlay file was generated
	overlayFile := filepath.Join(uiDir, "mods-overlay", "Skins", "Wave.qml")
	if _, err := os.Stat(overlayFile); err != nil {
		t.Fatalf("expected overlay file %s to exist: %v", overlayFile, err)
	}

	// Verify GET reports enabled: true
	getReq := httptest.NewRequest(http.MethodGet, "/v1/sc5000/mods", nil)
	getRec := httptest.NewRecorder()
	srv.handleMods(getRec, getReq)

	var getRes struct {
		Mods []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"mods"`
	}
	_ = json.NewDecoder(getRec.Body).Decode(&getRes)
	if len(getRes.Mods) != 2 || !getRes.Mods[1].Enabled {
		t.Fatalf("expected mod to be reported as enabled, got: %+v", getRes.Mods)
	}

	// 2. Disable mod
	bodyDisable := bytes.NewBufferString(`{"id": "wave-mod", "enabled": false}`)
	reqDisable := httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods", bodyDisable)
	recDisable := httptest.NewRecorder()
	srv.handleMods(recDisable, reqDisable)

	if recDisable.Code != http.StatusOK {
		t.Fatalf("disable failed: %d: %s", recDisable.Code, recDisable.Body.String())
	}

	// Check overlay file was removed
	if _, err := os.Stat(overlayFile); !os.IsNotExist(err) {
		t.Fatalf("expected overlay file to be removed after disable")
	}
}

func TestMods_WebUI_QuarantineReset(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()

	createWebUITestMod(t, mediaRoot, "USB_KEY", "mod-q", "Skins/ModQ.qml")

	srv := &Server{
		cfg: Config{
			MediaRoot: mediaRoot,
			UIDir:     uiDir,
		},
	}

	mgr := srv.modsManager()
	_ = mgr.SetModEnabled("mod-q", true)
	_ = mgr.QuarantineAll()

	// Verify currently quarantined
	if !mgr.IsSafeModeQuarantined() {
		t.Fatalf("expected safe mode quarantined")
	}

	// Call quarantine_reset via POST
	body := bytes.NewBufferString(`{"action": "quarantine_reset"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods", body)
	rec := httptest.NewRecorder()
	srv.handleMods(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("quarantine_reset failed: %d: %s", rec.Code, rec.Body.String())
	}

	var res map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&res)
	if res["ok"] != true || res["safeModeQuarantined"] != false {
		t.Fatalf("unexpected reset response: %+v", res)
	}

	if mgr.IsSafeModeQuarantined() {
		t.Fatalf("expected safe mode to be cleared")
	}
}

func TestMods_WebUI_InvalidRequests(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()

	srv := &Server{
		cfg: Config{
			MediaRoot: mediaRoot,
			UIDir:     uiDir,
		},
	}

	// Method not allowed
	req1 := httptest.NewRequest(http.MethodDelete, "/v1/sc5000/mods", nil)
	rec1 := httptest.NewRecorder()
	srv.handleMods(rec1, req1)
	if rec1.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 Method Not Allowed, got %d", rec1.Code)
	}

	// Bad JSON
	req2 := httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods", bytes.NewBufferString(`{invalid`))
	rec2 := httptest.NewRecorder()
	srv.handleMods(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for bad JSON, got %d", rec2.Code)
	}

	// Missing fields
	req3 := httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods", bytes.NewBufferString(`{}`))
	rec3 := httptest.NewRecorder()
	srv.handleMods(rec3, req3)
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for empty object, got %d", rec3.Code)
	}
}

func TestModsWebConsoleLANEndpoints(t *testing.T) {
	base := t.TempDir()
	token := "TEST-CODE"
	srv := New(Config{
		AccessToken: token,
		StateDir:    filepath.Join(base, "state"),
		UIDir:       filepath.Join(base, "ui"),
	}, nil)

	makeArchive := func(id string) []byte {
		var archive bytes.Buffer
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		manifest := []byte(fmt.Sprintf(`{"id":%q,"name":"LAN Test","version":"1.0.0","author":"Tester","description":"Testing","qmlFiles":{"view.qml":"Views/Test.qml"}}`, id))
		tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0644, Size: int64(len(manifest))})
		tw.Write(manifest)
		qml := []byte("import QtQuick 2.12\nItem {}\n")
		tw.WriteHeader(&tar.Header{Name: "view.qml", Mode: 0644, Size: int64(len(qml))})
		tw.Write(qml)
		tw.Close()
		gz.Close()
		return archive.Bytes()
	}

	arc1 := makeArchive("lan-test-1")
	arc2 := makeArchive("lan-test-2")
	lanAddr := "192.168.1.150:49210"

	// 1. Unauthenticated request to /api/mods from LAN returns 401
	{
		req := httptest.NewRequest(http.MethodGet, "/api/mods", nil)
		req.RemoteAddr = lanAddr
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthenticated /api/mods from LAN, got %d", rec.Code)
		}
	}

	// 2. Unauthenticated upload to /api/mods/upload from LAN returns 401
	{
		req := httptest.NewRequest(http.MethodPost, "/api/mods/upload", bytes.NewReader(arc1))
		req.Header.Set("Content-Type", "application/gzip")
		req.RemoteAddr = lanAddr
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthenticated /api/mods/upload from LAN, got %d", rec.Code)
		}
	}

	// 3. /v1/sc5000/mods and /v1/sc5000/mods/upload from LAN return 403 (streaming gateway loopback check)
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/sc5000/mods", nil)
		req.RemoteAddr = lanAddr
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for /v1/sc5000/mods from LAN, got %d", rec.Code)
		}

		reqUp := httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods/upload", bytes.NewReader(arc1))
		reqUp.Header.Set("Content-Type", "application/gzip")
		reqUp.RemoteAddr = lanAddr
		recUp := httptest.NewRecorder()
		srv.ServeHTTP(recUp, reqUp)
		if recUp.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for /v1/sc5000/mods/upload from LAN, got %d", recUp.Code)
		}
	}

	// 3b. /v1/sc5000/mods and /v1/sc5000/mods/upload from loopback succeed via srv.ServeHTTP without console login
	{
		reqUp := httptest.NewRequest(http.MethodPost, "/v1/sc5000/mods/upload", bytes.NewReader(arc1))
		reqUp.Header.Set("Content-Type", "application/gzip")
		reqUp.RemoteAddr = "127.0.0.1:45678"
		recUp := httptest.NewRecorder()
		srv.ServeHTTP(recUp, reqUp)
		if recUp.Code != http.StatusOK {
			t.Fatalf("expected 200 for loopback /v1/sc5000/mods/upload, got %d: %s", recUp.Code, recUp.Body.String())
		}
		var upRes map[string]any
		if err := json.Unmarshal(recUp.Body.Bytes(), &upRes); err != nil || upRes["id"] != "lan-test-1" {
			t.Fatalf("expected mod id lan-test-1 in loopback upload response, got: %s", recUp.Body.String())
		}

		reqList := httptest.NewRequest(http.MethodGet, "/v1/sc5000/mods", nil)
		reqList.RemoteAddr = "127.0.0.1:45678"
		recList := httptest.NewRecorder()
		srv.ServeHTTP(recList, reqList)
		if recList.Code != http.StatusOK {
			t.Fatalf("expected 200 for loopback /v1/sc5000/mods, got %d: %s", recList.Code, recList.Body.String())
		}
		var listRes modsListResponse
		if err := json.Unmarshal(recList.Body.Bytes(), &listRes); err != nil {
			t.Fatalf("failed to decode loopback /v1/sc5000/mods response: %v", err)
		}
		found := false
		for _, m := range listRes.Mods {
			if m.ID == "lan-test-1" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected lan-test-1 in loopback mods list, got: %s", recList.Body.String())
		}
	}

	// 4. Authenticated request from LAN via cookie stream_sync: GET /api/mods succeeds (200)
	{
		req := httptest.NewRequest(http.MethodGet, "/api/mods", nil)
		req.RemoteAddr = lanAddr
		req.AddCookie(&http.Cookie{Name: "stream_sync", Value: token})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for authenticated /api/mods, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 5. Cross-origin POST /api/mods/upload returns 403
	{
		req := httptest.NewRequest(http.MethodPost, "/api/mods/upload", bytes.NewReader(arc2))
		req.RemoteAddr = lanAddr
		req.Host = "192.168.1.1:8080"
		req.Header.Set("Origin", "http://evil.example.com")
		req.Header.Set("Content-Type", "application/gzip")
		req.AddCookie(&http.Cookie{Name: "stream_sync", Value: token})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for cross-origin upload, got %d", rec.Code)
		}
	}

	// 6. Authenticated same-origin POST /api/mods/upload succeeds (200) and returns mod metadata
	{
		req := httptest.NewRequest(http.MethodPost, "/api/mods/upload", bytes.NewReader(arc2))
		req.RemoteAddr = lanAddr
		req.Host = "192.168.1.1:8080"
		req.Header.Set("Origin", "http://192.168.1.1:8080")
		req.Header.Set("Content-Type", "application/gzip")
		req.AddCookie(&http.Cookie{Name: "stream_sync", Value: token})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for authenticated LAN upload, got %d: %s", rec.Code, rec.Body.String())
		}
		var upRes map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &upRes); err != nil || upRes["id"] != "lan-test-2" {
			t.Fatalf("expected mod id lan-test-2 in LAN upload response, got: %s", rec.Body.String())
		}
	}

	// 7. Authenticated POST /api/mods to enable succeeds (200) and updates state
	{
		req := httptest.NewRequest(http.MethodPost, "/api/mods", bytes.NewBufferString(`{"id":"lan-test-2","enabled":true}`))
		req.RemoteAddr = lanAddr
		req.Host = "192.168.1.1:8080"
		req.Header.Set("Origin", "http://192.168.1.1:8080")
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "stream_sync", Value: token})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for authenticated LAN mod enable, got %d: %s", rec.Code, rec.Body.String())
		}

		// Verify GET /api/mods lists lan-test-2 as enabled
		reqGet := httptest.NewRequest(http.MethodGet, "/api/mods", nil)
		reqGet.RemoteAddr = lanAddr
		reqGet.AddCookie(&http.Cookie{Name: "stream_sync", Value: token})
		recGet := httptest.NewRecorder()
		srv.ServeHTTP(recGet, reqGet)
		if recGet.Code != http.StatusOK {
			t.Fatalf("expected 200 for GET /api/mods, got %d: %s", recGet.Code, recGet.Body.String())
		}
		var apiList modsListResponse
		if err := json.Unmarshal(recGet.Body.Bytes(), &apiList); err != nil {
			t.Fatalf("failed to decode /api/mods response: %v", err)
		}
		foundLan2 := false
		for _, m := range apiList.Mods {
			if m.ID == "lan-test-2" && m.Enabled {
				foundLan2 = true
				break
			}
		}
		if !foundLan2 {
			t.Fatalf("expected enabled lan-test-2 in /api/mods list, got: %s", recGet.Body.String())
		}
	}
}
