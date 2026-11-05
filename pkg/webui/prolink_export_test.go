package webui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestProlinkExportSettingLifecycle(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	dir := t.TempDir()
	cfg := Config{UIDir: dir, StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{Root: root, IP: net.IPv4(127, 64, 0, 1), MAC: net.HardwareAddr{2, 0, 0, 0, 0, 1}, DBAddr: "127.64.0.1:0", PortmapAddr: "127.64.0.1:0", DiscoveryAddr: "127.64.0.1:0", StatusAddr: "127.64.0.1:0", AnnounceAddr: "127.64.0.1:9", ProbeDuration: time.Millisecond}}
	ui := New(cfg, nil)
	defer ui.Close()
	srv := httptest.NewServer(ui)
	defer srv.Close()
	request := func(query string) (int, prolinkResponse) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/sc5000/prolink?"+query, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result prolinkResponse
		_ = json.NewDecoder(resp.Body).Decode(&result)
		return resp.StatusCode, result
	}
	if ui.loadUI().ProlinkExport || ui.prolinkExport.Status().Enabled {
		t.Fatal("export must default OFF")
	}
	if code, _ := uiPost(t, srv.URL, "prolinkExport=yes"); code != 400 {
		t.Fatal("invalid toggle accepted")
	}
	if ui.prolinkExport.Status().Enabled {
		t.Fatal("invalid toggle started exporter")
	}
	if code, v := request("export=1&share=0"); code != 200 || !v.Export || !v.ExportState.Enabled || v.Share {
		t.Fatalf("enable: %d %+v", code, v)
	}
	state := ui.prolinkExport.Status()
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(b), "prolink_export=1\n") {
		t.Fatalf("toggle not persisted: %s", b)
	}
	// Saving unrelated preferences must preserve the running export.
	if code, v := uiPost(t, srv.URL, "waveform=rgb"); code != 200 || !v.ProlinkExport || ui.prolinkExport.Status().DBAddress != state.DBAddress {
		t.Fatal("unrelated preference restarted/dropped export")
	}
	if code, _ := request("export=0&share=invalid"); code != 400 || !ui.prolinkExport.Status().Enabled {
		t.Fatal("partially applied invalid settings")
	}
	if code, v := uiPost(t, srv.URL, "prolinkExport=0"); code != 200 || v.ProlinkExport || ui.prolinkExport.Status().Enabled {
		t.Fatal("UI toggle did not stop export")
	}
	l, err := net.Listen("tcp4", state.DBAddress)
	if err != nil {
		t.Fatalf("OFF retained port: %v", err)
	}
	l.Close()
	if code, v := request("export=1"); code != 200 || !v.ExportState.Enabled {
		t.Fatal("prolink endpoint failed to reenable")
	}
	ui.Close()
	// settings.conf's explicit opt-in is restored on process startup.
	restored := New(cfg, nil)
	defer restored.Close()
	if !restored.prolinkExport.Status().Enabled || !restored.loadUI().ProlinkExport {
		t.Fatal("saved opt-in not restored")
	}
	if err := restored.saveUI(uiSettings{}); err != nil {
		t.Fatal(err)
	}
	if restored.prolinkExport.Status().Enabled {
		t.Fatal("OFF not applied")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); strings.Contains(string(b), "prolink_export=1") {
		t.Fatalf("ON survived OFF: %s", b)
	}
}

func TestProlinkExportStartFailureDoesNotPersistOptIn(t *testing.T) {
	dir := t.TempDir()
	ui := New(Config{UIDir: dir, StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{Root: filepath.Join(t.TempDir(), "missing")}}, nil)
	defer ui.Close()
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if code, _ := uiPost(t, srv.URL, "prolinkExport=1"); code != 500 {
		t.Fatalf("startup failure: %d", code)
	}
	if ui.loadUI().ProlinkExport || ui.prolinkExport.Status().Enabled {
		t.Fatal("failed start published/persisted ON")
	}
}

func TestProlinkSavedOnWithoutUSBDoesNotBlockOtherSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("prolink_export=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{UIDir: dir, StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{Root: filepath.Join(t.TempDir(), "missing")}}, nil)
	defer ui.Close()
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if code, v := uiPost(t, srv.URL, "waveform=rgb"); code != 200 || !v.ProlinkExport {
		t.Fatal("missing USB blocked unrelated preference")
	}
	if ui.prolinkExport.Status().Enabled {
		t.Fatal("missing USB started export")
	}
	if code, _ := uiPost(t, srv.URL, "prolinkExport=1"); code != 500 {
		t.Fatal("explicit ON must retry and report missing USB")
	}
	if code, _ := uiPost(t, srv.URL, "prolinkExport=0"); code != 200 {
		t.Fatal("OFF failed")
	}
}
