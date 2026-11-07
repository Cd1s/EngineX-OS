package webui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
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
