package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBluetoothDefaultStatus(t *testing.T) {
	uiDir := t.TempDir()
	stateDir := t.TempDir()
	s := New(Config{UIDir: uiDir, StateDir: stateDir}, nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/sc5000/bluetooth", nil)
	s.handleBluetooth(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var st BluetoothStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if !st.Enabled {
		t.Errorf("expected Enabled: true, got %v", st.Enabled)
	}
	if st.Mode != BluetoothModeSink {
		t.Errorf("expected Mode: sink, got %s", st.Mode)
	}
	if st.ModeLabel != BluetoothModeLabelSink {
		t.Errorf("expected ModeLabel: %s, got %s", BluetoothModeLabelSink, st.ModeLabel)
	}
	if st.AssignLayer != "Master" {
		t.Errorf("expected AssignLayer: Master, got %s", st.AssignLayer)
	}
	if st.Devices == nil {
		t.Errorf("expected Devices to be non-nil empty slice, got nil")
	}
	if len(st.Devices) != 0 {
		t.Errorf("expected 0 devices, got %d", len(st.Devices))
	}
}

func TestBluetoothSwitchModeAndPersistence(t *testing.T) {
	uiDir := t.TempDir()
	stateDir := t.TempDir()
	s := New(Config{UIDir: uiDir, StateDir: stateDir}, nil)

	// 1. Switch to source mode
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/sc5000/bluetooth", strings.NewReader(`{"mode":"source"}`))
	r.Header.Set("Content-Type", "application/json")
	s.handleBluetooth(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var st BluetoothStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if st.Mode != BluetoothModeSource {
		t.Errorf("expected Mode: source, got %s", st.Mode)
	}
	if st.ModeLabel != BluetoothModeLabelSource {
		t.Errorf("expected ModeLabel: %s, got %s", BluetoothModeLabelSource, st.ModeLabel)
	}

	// Verify file persistence
	cfgBytes, err := os.ReadFile(filepath.Join(uiDir, bluetoothConfigFile))
	if err != nil {
		t.Fatalf("failed to read persisted bluetooth.json: %v", err)
	}
	if !strings.Contains(string(cfgBytes), `"mode": "source"`) {
		t.Errorf("persisted config missing source mode: %s", string(cfgBytes))
	}

	// 2. Switch back to sink mode and set assignLayer to Layer A
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/v1/sc5000/bluetooth", strings.NewReader(`{"mode":"sink","assignLayer":"Layer A"}`))
	r.Header.Set("Content-Type", "application/json")
	s.handleBluetooth(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if st.Mode != BluetoothModeSink {
		t.Errorf("expected Mode: sink, got %s", st.Mode)
	}
	if st.ModeLabel != BluetoothModeLabelSink {
		t.Errorf("expected ModeLabel: %s, got %s", BluetoothModeLabelSink, st.ModeLabel)
	}
	if st.AssignLayer != "Layer A" {
		t.Errorf("expected AssignLayer: Layer A, got %s", st.AssignLayer)
	}

	// 3. Restart simulation: create a new Server instance using the same UIDir
	s2 := New(Config{UIDir: uiDir, StateDir: stateDir}, nil)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/v1/sc5000/bluetooth", nil)
	s2.handleBluetooth(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on restarted server, got %d", w.Code)
	}
	var st2 BluetoothStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st2); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if st2.Mode != BluetoothModeSink {
		t.Errorf("expected persisted Mode: sink, got %s", st2.Mode)
	}
	if st2.AssignLayer != "Layer A" {
		t.Errorf("expected persisted AssignLayer: Layer A, got %s", st2.AssignLayer)
	}

	// 4. Test query parameters
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/v1/sc5000/bluetooth?mode=source&assignLayer=CH+2", nil)
	s2.handleBluetooth(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with query parameters, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st2); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if st2.Mode != BluetoothModeSource || st2.AssignLayer != "CH 2" {
		t.Errorf("query params update failed: mode=%s, assignLayer=%s", st2.Mode, st2.AssignLayer)
	}
}

func TestBluetoothDeviceParsing(t *testing.T) {
	devicesOutput := `
Device 11:22:33:44:55:66 iPhone 15 Pro
Device AA:BB:CC:DD:EE:FF Sony WH-1000XM4
Device DE:AD:BE:EF:00:01
`
	devs := ParseBluetoothctlDevices(devicesOutput)
	if len(devs) != 3 {
		t.Fatalf("expected 3 devices, got %d", len(devs))
	}
	if devs[0].MAC != "11:22:33:44:55:66" || devs[0].Name != "iPhone 15 Pro" {
		t.Errorf("device 0 mismatch: %+v", devs[0])
	}
	if devs[1].MAC != "AA:BB:CC:DD:EE:FF" || devs[1].Name != "Sony WH-1000XM4" {
		t.Errorf("device 1 mismatch: %+v", devs[1])
	}
	if devs[2].MAC != "DE:AD:BE:EF:00:01" || devs[2].Name != "DE:AD:BE:EF:00:01" {
		t.Errorf("device 2 fallback to MAC mismatch: %+v", devs[2])
	}

	// Test info parsing for Audio Source (Phone)
	phoneInfo := `
Device 11:22:33:44:55:66 (public)
	Name: iPhone 15 Pro
	Alias: iPhone 15 Pro
	Class: 0x007a020c
	Icon: phone
	Paired: yes
	Trusted: yes
	Blocked: no
	Connected: yes
	LegacyPairing: no
	UUID: Audio Source              (0000110a-0000-1000-8000-00805f9b34fb)
	UUID: A/V Remote Control Target (0000110c-0000-1000-8000-00805f9b34fb)
`
	devPhone := BluetoothDevice{MAC: "11:22:33:44:55:66"}
	ParseBluetoothctlInfo(phoneInfo, &devPhone)
	if !devPhone.Connected || !devPhone.Paired || !devPhone.Trusted {
		t.Errorf("expected phone connected/paired/trusted, got %+v", devPhone)
	}
	if devPhone.Profile != BluetoothProfileSink {
		t.Errorf("expected phone profile to be %s (deck as sink), got %s", BluetoothProfileSink, devPhone.Profile)
	}

	// Test info parsing for Audio Sink (Speaker/Headphones)
	speakerInfo := `
Device AA:BB:CC:DD:EE:FF (public)
	Name: Sony WH-1000XM4
	Alias: Sony WH-1000XM4
	Paired: yes
	Trusted: yes
	Blocked: no
	Connected: yes
	UUID: Audio Sink                (0000110b-0000-1000-8000-00805f9b34fb)
`
	devSpeaker := BluetoothDevice{MAC: "AA:BB:CC:DD:EE:FF"}
	ParseBluetoothctlInfo(speakerInfo, &devSpeaker)
	if !devSpeaker.Connected || !devSpeaker.Paired || !devSpeaker.Trusted {
		t.Errorf("expected speaker connected/paired/trusted, got %+v", devSpeaker)
	}
	if devSpeaker.Profile != BluetoothProfileSource {
		t.Errorf("expected speaker profile to be %s (deck as source), got %s", BluetoothProfileSource, devSpeaker.Profile)
	}
}

func TestBluetoothRunnerInjectionAndMocking(t *testing.T) {
	var mu sync.Mutex
	commandsRun := []string{}

	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		cmdStr := name + " " + strings.Join(args, " ")
		commandsRun = append(commandsRun, cmdStr)
		mu.Unlock()

		if name == "bluetoothctl" {
			if len(args) > 0 && args[0] == "devices" {
				return []byte("Device 11:22:33:44:55:66 Test Phone\n"), nil
			}
			if len(args) > 1 && args[0] == "info" && args[1] == "11:22:33:44:55:66" {
				return []byte("Name: Test Phone\nConnected: yes\nPaired: yes\nTrusted: yes\nUUID: Audio Source (0000110a-0000-1000-8000-00805f9b34fb)\n"), nil
			}
			if len(args) > 0 && args[0] == "show" {
				return []byte("Powered: yes\nDiscoverable: yes\nPairable: yes\n"), nil
			}
		}
		if name == "enginex-bt-helper" {
			return []byte("ok"), nil
		}
		return nil, nil
	}

	SetBluetoothRunner(mockRunner)
	t.Cleanup(func() { SetBluetoothRunner(nil) })

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)

	// GET status with mocked devices
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/sc5000/bluetooth", nil)
	s.handleBluetooth(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var st BluetoothStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(st.Devices) != 1 {
		t.Fatalf("expected 1 mocked device, got %d", len(st.Devices))
	}
	d := st.Devices[0]
	if d.MAC != "11:22:33:44:55:66" || d.Name != "Test Phone" || !d.Connected || d.Profile != BluetoothProfileSink {
		t.Errorf("mocked device details mismatch: %+v", d)
	}

	// POST with stabilize, mode change, and connect MAC
	w = httptest.NewRecorder()
	body := `{"stabilize":true,"mode":"source","mac":"11:22:33:44:55:66"}`
	r = httptest.NewRequest("POST", "/v1/sc5000/bluetooth", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	s.handleBluetooth(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	cmds := strings.Join(commandsRun, " | ")
	mu.Unlock()

	if !strings.Contains(cmds, "enginex-bt-helper stabilize") {
		t.Errorf("stabilize command was not called: %s", cmds)
	}
	if !strings.Contains(cmds, "enginex-bt-helper set-mode source") {
		t.Errorf("set-mode command was not called: %s", cmds)
	}
	if !strings.Contains(cmds, "enginex-bt-helper connect 11:22:33:44:55:66") {
		t.Errorf("connect command was not called: %s", cmds)
	}
}

func TestBluetoothInvalidParameters(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)

	// Invalid mode
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/sc5000/bluetooth", strings.NewReader(`{"mode":"invalid_mode"}`))
	s.handleBluetooth(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid mode, got %d", w.Code)
	}

	// Invalid assignLayer
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/v1/sc5000/bluetooth", strings.NewReader(`{"assignLayer":"CH 5"}`))
	s.handleBluetooth(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid assignLayer, got %d", w.Code)
	}

	// Invalid MAC
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/v1/sc5000/bluetooth", strings.NewReader(`{"mac":"not-a-valid-mac"}`))
	s.handleBluetooth(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid mac, got %d", w.Code)
	}

	// Invalid JSON
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/v1/sc5000/bluetooth", strings.NewReader(`{invalid json`))
	s.handleBluetooth(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad json, got %d", w.Code)
	}

	// Method not allowed
	w = httptest.NewRecorder()
	r = httptest.NewRequest("DELETE", "/v1/sc5000/bluetooth", nil)
	s.handleBluetooth(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for DELETE, got %d", w.Code)
	}
}

func TestBluetoothShowPoweredNo(t *testing.T) {
	mockRunner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "bluetoothctl" && len(args) > 0 && args[0] == "show" {
			return []byte("Powered: no\n"), nil
		}
		return nil, nil
	}

	SetBluetoothRunner(mockRunner)
	t.Cleanup(func() { SetBluetoothRunner(nil) })

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/sc5000/bluetooth", nil)
	s.handleBluetooth(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var st BluetoothStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if st.Enabled {
		t.Errorf("expected Enabled: false when Powered: no, got true")
	}
}
