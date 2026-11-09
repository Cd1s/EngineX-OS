package webui

// Optional loopback integration with Cd1s/OpenProLink's independent simulator.
// ENGINEX_CDJ_SIM_BIN must name a cdj3000-sim built on the test host.
import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestOpenProLinkCDJDisplay(t *testing.T) {
	bin := os.Getenv("ENGINEX_CDJ_SIM_BIN")
	if bin == "" {
		t.Skip("set ENGINEX_CDJ_SIM_BIN to run OpenProLink loopback integration")
	}
	root := t.TempDir()
	f := newDisplayNFSFixture()
	for path, b := range f.files {
		p := filepath.Join(root, "usb", path)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct loopback addresses isolate all fixed Pro DJ Link ports from the
	// machine's other agents. No packet is addressed to a physical player.
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-http", addr, "-usb", filepath.Join(root, "usb"), "-sim-ip", "127.197.11.2", "-denon-ip", "127.197.11.1", "-denon-empty")
	cmd.Env = append(os.Environ(), "TMPDIR="+root)
	logFile, err := os.Create(filepath.Join(root, "sim.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	var state struct {
		CDJ struct {
			Number  byte   `json:"number"`
			TrackID uint32 `json:"track_id"`
			IP      string `json:"ip"`
			Portmap int    `json:"portmap"`
		} `json:"cdj"`
	}
	client := &http.Client{Timeout: time.Second}
	for state.CDJ.Portmap == 0 && ctx.Err() == nil {
		resp, e := client.Get("http://" + addr + "/api/state")
		if e == nil {
			_ = json.NewDecoder(resp.Body).Decode(&state)
			resp.Body.Close()
		}
		if state.CDJ.Portmap == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if state.CDJ.Portmap == 0 {
		b, _ := os.ReadFile(logFile.Name())
		t.Fatalf("simulator unavailable: %.1000s", b)
	}
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(root, "role")
	defer func() { fourDeckRolePath = oldRole }()
	if err = os.WriteFile(fourDeckRolePath, []byte("13"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: filepath.Join(root, "ui"), StateDir: root}, nil)
	// Exercise the HTTP state API with a snapshot from the independent simulator;
	// disable beacon startup so this test never binds the machine's LAN ports.
	s.shadow.running = true
	s.shadow.cdjReader = func(ip net.IP) prolink.AnalysisReader { return &prolink.NFSClient{IP: ip, Portmap: state.CDJ.Portmap} }
	// Fail with the actual NFS/conversion error before testing asynchronous HTTP.
	tk, analysis, err := s.shadow.cdjAnalysis.Read(ctx, s.shadow.cdjReader(net.ParseIP(state.CDJ.IP)), state.CDJ.IP, 3, state.CDJ.TrackID, filepath.Join(root, "shadow-display-cdj"))
	if err != nil {
		t.Fatalf("simulator NFS analysis: %v", err)
	}
	if _, err = cdjDisplayAsset(tk, analysis, "probe"); err != nil {
		t.Fatalf("simulator display conversion: %v", err)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	uiPost(t, srv.URL, "fourDeckWaveform=1")
	dev := prolink.Device{Name: "CDJ-3000", Address: state.CDJ.IP, Number: state.CDJ.Number, SourcePlayer: state.CDJ.Number, SourceSlot: 3, TrackType: 1, TrackID: state.CDJ.TrackID, LastSeen: time.Now()}
	b, _ := json.Marshal(prolink.Snapshot{Devices: []prolink.Device{dev}})
	if err = os.WriteFile(filepath.Join(root, "prolink.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	var shadow struct {
		DisplayOnly bool        `json:"displayOnly"`
		Decks       []deckState `json:"decks"`
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		resp, e := client.Get(srv.URL + "/v1/sc5000/shadow")
		if e != nil {
			t.Fatal(e)
		}
		shadow.Decks = nil // metadataOnly=false is omitted by JSON; do not retain the previous poll's true value
		_ = json.NewDecoder(resp.Body).Decode(&shadow)
		resp.Body.Close()
		if len(shadow.Decks) == 1 && !shadow.Decks[0].MetadataOnly {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !shadow.DisplayOnly || len(shadow.Decks) != 1 || shadow.Decks[0].MetadataOnly {
		t.Fatalf("no CDJ display asset: %+v", shadow)
	}
	d := shadow.Decks[0]
	q := "?deck=" + fmt.Sprint(d.Deck) + "&key=" + url.QueryEscape(d.Key)
	resp, err := client.Get(srv.URL + "/v1/sc5000/shadow/display" + q)
	if err != nil {
		t.Fatal(err)
	}
	var display struct {
		Beats    []any `json:"beats"`
		Cues     []any `json:"cues"`
		Loops    []any `json:"loops"`
		Detailed bool  `json:"detail"`
	}
	err = json.NewDecoder(resp.Body).Decode(&display)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || len(display.Beats) != 4 || len(display.Cues) == 0 || len(display.Loops) == 0 || !display.Detailed {
		t.Fatalf("display HTTP %d: %+v, %v", resp.StatusCode, display, err)
	}
	resp, err = client.Get(srv.URL + "/v1/sc5000/shadow/waveform.png" + q)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("waveform HTTP %d", resp.StatusCode)
	}
	t.Logf("OpenProLink CDJ-3000 NFS -> webui deck %d: detailed waveform, %d beats, %d cues, %d loops; no audio file supplied", d.Deck, len(display.Beats), len(display.Cues), len(display.Loops))
}
