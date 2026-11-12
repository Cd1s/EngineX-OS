package webui

import (
	"encoding/json"
	"fmt"
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

// ON without a Rekordbox USB stays ON and reports that the export waits (#63).
func TestProlinkExportOnWithoutUSBStaysOnAndWaits(t *testing.T) {
	dir := t.TempDir()
	ui := New(Config{UIDir: dir, StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{Root: filepath.Join(t.TempDir(), "missing")}}, nil)
	defer ui.Close()
	srv := httptest.NewServer(ui)
	defer srv.Close()
	code, v := uiPost(t, srv.URL, "prolinkExport=1")
	if code != 200 || !v.ProlinkExport || v.ProlinkExportActive {
		t.Fatalf("ON without USB: %d %+v", code, v)
	}
	if !ui.loadUI().ProlinkExport || ui.prolinkExport.Status().Enabled {
		t.Fatal("ON was not kept, or the export started without a USB")
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
	if code, v := uiPost(t, srv.URL, "prolinkExport=1"); code != 200 || !v.ProlinkExport || v.ProlinkExportActive {
		t.Fatal("explicit ON must stay ON and report that it waits for a USB")
	}
	if code, _ := uiPost(t, srv.URL, "prolinkExport=0"); code != 200 {
		t.Fatal("OFF failed")
	}
}

// A saved ON starts by itself once the Rekordbox USB appears (#63).
func TestProlinkExportStartsWhenUSBArrives(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usb")
	cfg := Config{UIDir: t.TempDir(), StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{Root: root, IP: net.IPv4(127, 64, 0, 2), MAC: net.HardwareAddr{2, 0, 0, 0, 0, 2}, DBAddr: "127.64.0.2:0", PortmapAddr: "127.64.0.2:0", DiscoveryAddr: "127.64.0.2:0", StatusAddr: "127.64.0.2:0", AnnounceAddr: "127.64.0.2:9", ProbeDuration: time.Millisecond}}
	ui := New(cfg, nil)
	defer ui.Close()
	srv := httptest.NewServer(ui)
	defer srv.Close()
	if code, v := uiPost(t, srv.URL, "prolinkExport=1"); code != 200 || v.ProlinkExportActive {
		t.Fatalf("ON without USB: %d %+v", code, v)
	}
	testfixture.Export(t, root)
	ui.tryExport()
	if !ui.prolinkExport.Status().Enabled {
		t.Fatal("export did not start once the USB was mounted")
	}
}

// #217: a console's Line In CDJ has no Engine deck that takes master from ours,
// so Engine always has one of our decks as master. MASTER pressed on another of
// our decks asks the CDJ for the link's master again.
func TestLinkMasterAskedAgainFromAnotherDeck(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	cfg := Config{UIDir: t.TempDir(), StateDir: t.TempDir(), ProlinkExport: prolink.ExportConfig{Root: root, IP: net.IPv4(127, 64, 0, 3), MAC: net.HardwareAddr{2, 0, 0, 0, 0, 3}, DBAddr: "127.64.0.3:0", PortmapAddr: "127.64.0.3:0", DiscoveryAddr: "127.64.0.3:0", StatusAddr: "127.64.0.3:0", AnnounceAddr: "127.64.0.3:9", ProbeDuration: 20 * time.Millisecond}}
	s := New(cfg, nil)
	defer s.Close()
	cdj, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 64, 0, 4), Port: 50001})
	if err != nil {
		t.Skip("beat port:", err)
	}
	defer cdj.Close()
	if err := s.prolinkExport.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	for until := time.Now().Add(2 * time.Second); s.prolinkExport.Status().Number == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(until) {
			t.Fatal("no player number")
		}
	}
	prolinkMu.Lock()
	postedDevices[s] = map[string]prolink.Device{"2": {Number: 2, Name: "CDJ-3000", Address: "127.64.0.4", Master: true, LastSeen: time.Now()}}
	prolinkMu.Unlock()
	asked := func(masterDeck int, want bool) {
		t.Helper()
		body := fmt.Sprintf(`[{"deck":1,"playing":true,"bpm":120,"master":%v},{"deck":2,"playing":true,"bpm":120,"master":%v}]`, masterDeck == 1, masterDeck == 2)
		w := httptest.NewRecorder()
		s.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("decks: %d", w.Code)
		}
		cdj.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		b := make([]byte, 64)
		n, _, err := cdj.ReadFromUDP(b)
		if got := err == nil && n == 0x28 && b[10] == 0x26; got != want {
			t.Fatalf("master on deck %d: asked %v (%v), want %v", masterDeck, got, err, want)
		}
	}
	asked(0, false)
	asked(1, true)  // none of ours to one of ours
	asked(1, false) // nothing changed
	asked(2, true)  // MASTER on our other deck
	asked(2, false)
}
