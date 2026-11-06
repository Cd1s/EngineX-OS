package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	BluetoothModeSink   = "sink"
	BluetoothModeSource = "source"

	BluetoothModeLabelSink   = "Audio In (Phone → Deck)"
	BluetoothModeLabelSource = "Audio Out (Deck → Speaker/Headphones)"

	BluetoothProfileSink   = "a2dp-sink"
	BluetoothProfileSource = "a2dp-source"

	bluetoothConfigFile = "bluetooth.json"
)

type BluetoothDevice struct {
	MAC       string `json:"mac"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Paired    bool   `json:"paired"`
	Trusted   bool   `json:"trusted"`
	Profile   string `json:"profile"` // "a2dp-sink" (Phone->Deck In) | "a2dp-source" (Deck->Speaker Out)
}

type BluetoothStatus struct {
	Enabled     bool              `json:"enabled"`
	Mode        string            `json:"mode"`        // "sink" (Audio In: Phone -> Deck) | "source" (Audio Out: Deck -> Speaker)
	ModeLabel   string            `json:"modeLabel"`   // Clear human-readable label for UI
	AssignLayer string            `json:"assignLayer"` // "Master", "Layer A", "Layer B", "CH 1..4"
	Devices     []BluetoothDevice `json:"devices"`
}

type bluetoothConfig struct {
	Mode          string `json:"mode"`
	AssignLayer   string `json:"assignLayer"`
	AutoReconnect bool   `json:"autoReconnect"`
}

// ModeLabelFor returns the user-facing label for a Bluetooth mode.
func ModeLabelFor(mode string) string {
	if mode == BluetoothModeSource {
		return BluetoothModeLabelSource
	}
	return BluetoothModeLabelSink
}

func isValidAssignLayer(layer string) bool {
	switch layer {
	case "Master", "Layer A", "Layer B", "CH 1", "CH 2", "CH 3", "CH 4":
		return true
	default:
		return false
	}
}

// BluetoothRunner executes commands on the host or inside test mocks.
type BluetoothRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

var defaultBluetoothRunner BluetoothRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmdName := name
	if cmdName == "sc5000-bt-helper" {
		if _, err := exec.LookPath("sc5000-bt-helper"); err != nil {
			if _, err := os.Stat("/usr/sbin/sc5000-bt-helper"); err == nil {
				cmdName = "/usr/sbin/sc5000-bt-helper"
			}
		}
	}
	cmd := exec.CommandContext(ctx, cmdName, args...)
	return cmd.CombinedOutput()
}

var (
	btRunnerMu sync.RWMutex
	btRunner   = defaultBluetoothRunner
)

// SetBluetoothRunner allows tests or callers to mock command execution.
func SetBluetoothRunner(r BluetoothRunner) {
	btRunnerMu.Lock()
	defer btRunnerMu.Unlock()
	if r == nil {
		btRunner = defaultBluetoothRunner
	} else {
		btRunner = r
	}
}

func getBluetoothRunner() BluetoothRunner {
	btRunnerMu.RLock()
	defer btRunnerMu.RUnlock()
	return btRunner
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func cleanCLIOutput(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// ParseBluetoothctlDevices parses `bluetoothctl devices` into a slice of BluetoothDevice.
func ParseBluetoothctlDevices(out string) []BluetoothDevice {
	cleaned := cleanCLIOutput(out)
	devs := []BluetoothDevice{}
	lines := strings.Split(cleaned, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Device ") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 2 {
			continue
		}
		mac := strings.TrimSpace(parts[1])
		name := mac
		if len(parts) >= 3 && strings.TrimSpace(parts[2]) != "" {
			name = strings.TrimSpace(parts[2])
		}
		devs = append(devs, BluetoothDevice{
			MAC:       mac,
			Name:      name,
			Paired:    true,
			Trusted:   true,
			Connected: false,
		})
	}
	return devs
}

// ParseBluetoothctlInfo updates a BluetoothDevice from `bluetoothctl info <mac>` output.
func ParseBluetoothctlInfo(out string, dev *BluetoothDevice) {
	cleaned := cleanCLIOutput(out)
	lines := strings.Split(cleaned, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Name:"):
			name := strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			if name != "" {
				dev.Name = name
			}
		case strings.HasPrefix(line, "Connected:"):
			val := strings.TrimSpace(strings.TrimPrefix(line, "Connected:"))
			dev.Connected = strings.EqualFold(val, "yes")
		case strings.HasPrefix(line, "Paired:"):
			val := strings.TrimSpace(strings.TrimPrefix(line, "Paired:"))
			dev.Paired = strings.EqualFold(val, "yes")
		case strings.HasPrefix(line, "Trusted:"):
			val := strings.TrimSpace(strings.TrimPrefix(line, "Trusted:"))
			dev.Trusted = strings.EqualFold(val, "yes")
		case strings.HasPrefix(line, "UUID:"):
			lower := strings.ToLower(line)
			if strings.Contains(lower, "0000110b") || strings.Contains(lower, "audio sink") {
				dev.Profile = BluetoothProfileSource
			} else if strings.Contains(lower, "0000110a") || strings.Contains(lower, "audio source") {
				dev.Profile = BluetoothProfileSink
			}
		}
	}
}

func (s *Server) loadBluetoothConfig() bluetoothConfig {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()

	cfg := bluetoothConfig{
		Mode:          BluetoothModeSink,
		AssignLayer:   "Master",
		AutoReconnect: true,
	}

	b, err := os.ReadFile(filepath.Join(s.uiDir(), bluetoothConfigFile))
	if err == nil {
		_ = json.Unmarshal(b, &cfg)
	}

	if cfg.Mode != BluetoothModeSink && cfg.Mode != BluetoothModeSource {
		cfg.Mode = BluetoothModeSink
	}
	if cfg.AssignLayer == "" || !isValidAssignLayer(cfg.AssignLayer) {
		cfg.AssignLayer = "Master"
	}
	return cfg
}

func (s *Server) saveBluetoothConfig(cfg bluetoothConfig) error {
	if cfg.Mode != BluetoothModeSink && cfg.Mode != BluetoothModeSource {
		return errors.New("invalid mode: must be sink or source")
	}
	if cfg.AssignLayer == "" {
		cfg.AssignLayer = "Master"
	} else if !isValidAssignLayer(cfg.AssignLayer) {
		return errors.New("invalid assignLayer: must be Master, Layer A, Layer B, or CH 1..4")
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	s.uiMu.Lock()
	err = writeAtomic(s.uiDir(), bluetoothConfigFile, b)
	s.uiMu.Unlock()
	if err != nil {
		return err
	}

	_ = os.MkdirAll("/data/enginex/ui", 0o755)
	if err := os.WriteFile("/data/enginex/ui/bt-mode", []byte(cfg.Mode+"\n"), 0o644); err != nil {
		_ = os.WriteFile("/tmp/bt-mode", []byte(cfg.Mode+"\n"), 0o644)
	}

	return nil
}

func (s *Server) getBluetoothStatus(ctx context.Context) BluetoothStatus {
	cfg := s.loadBluetoothConfig()

	st := BluetoothStatus{
		Enabled:     true,
		Mode:        cfg.Mode,
		ModeLabel:   ModeLabelFor(cfg.Mode),
		AssignLayer: cfg.AssignLayer,
		Devices:     []BluetoothDevice{},
	}

	runner := getBluetoothRunner()

	// Check if controller is powered
	if showOut, err := runner(ctx, "bluetoothctl", "show"); err == nil && len(showOut) > 0 {
		cleaned := cleanCLIOutput(string(showOut))
		if strings.Contains(cleaned, "Powered: no") {
			st.Enabled = false
		}
	}

	// Query devices
	if devOut, err := runner(ctx, "bluetoothctl", "devices"); err == nil && len(devOut) > 0 {
		st.Devices = ParseBluetoothctlDevices(string(devOut))
		for i := range st.Devices {
			if st.Mode == BluetoothModeSource {
				st.Devices[i].Profile = BluetoothProfileSource
			} else {
				st.Devices[i].Profile = BluetoothProfileSink
			}
			if infoOut, err := runner(ctx, "bluetoothctl", "info", st.Devices[i].MAC); err == nil && len(infoOut) > 0 {
				ParseBluetoothctlInfo(string(infoOut), &st.Devices[i])
			}
			if st.Devices[i].Profile == "" {
				if st.Mode == BluetoothModeSource {
					st.Devices[i].Profile = BluetoothProfileSource
				} else {
					st.Devices[i].Profile = BluetoothProfileSink
				}
			}
		}
	}

	return st
}

type bluetoothPostRequest struct {
	Mode          *string `json:"mode"`
	AssignLayer   *string `json:"assignLayer"`
	Stabilize     *bool   `json:"stabilize"`
	MAC           *string `json:"mac"`
	AutoReconnect *bool   `json:"autoReconnect"`
}

// handleBluetooth dispatches GET and POST requests for /v1/sc5000/bluetooth.
func (s *Server) handleBluetooth(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.bluetoothGet(w, r)
	case http.MethodPost:
		s.bluetoothPost(w, r)
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) bluetoothGet(w http.ResponseWriter, r *http.Request) {
	st := s.getBluetoothStatus(r.Context())
	respond(w, st)
}

func (s *Server) bluetoothPost(w http.ResponseWriter, r *http.Request) {
	cfg := s.loadBluetoothConfig()

	var req bluetoothPostRequest

	// Support query parameters
	q := r.URL.Query()
	if m := q.Get("mode"); m != "" {
		req.Mode = &m
	}
	if a := q.Get("assignLayer"); a != "" {
		req.AssignLayer = &a
	}
	if q.Has("stabilize") {
		stab := q.Get("stabilize") == "1" || strings.EqualFold(q.Get("stabilize"), "true")
		req.Stabilize = &stab
	}
	if mac := q.Get("mac"); mac != "" {
		req.MAC = &mac
	}
	if q.Has("autoReconnect") {
		ar := q.Get("autoReconnect") == "1" || strings.EqualFold(q.Get("autoReconnect"), "true")
		req.AutoReconnect = &ar
	}

	// Support JSON body if present
	if r.Body != nil {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
		if err != nil {
			fail(w, http.StatusBadRequest, errors.New("request body too large"))
			return
		}
		if len(bytes.TrimSpace(b)) > 0 {
			d := json.NewDecoder(bytes.NewReader(b))
			d.DisallowUnknownFields()
			if err := d.Decode(&req); err != nil {
				fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
				return
			}
		}
	}

	// Validation
	if req.Mode != nil {
		m := strings.TrimSpace(*req.Mode)
		if m != BluetoothModeSink && m != BluetoothModeSource {
			fail(w, http.StatusBadRequest, errors.New("invalid mode: must be sink or source"))
			return
		}
		cfg.Mode = m
	}

	if req.AssignLayer != nil {
		a := strings.TrimSpace(*req.AssignLayer)
		if !isValidAssignLayer(a) {
			fail(w, http.StatusBadRequest, errors.New("invalid assignLayer: must be Master, Layer A, Layer B, or CH 1..4"))
			return
		}
		cfg.AssignLayer = a
	}

	if req.AutoReconnect != nil {
		cfg.AutoReconnect = *req.AutoReconnect
	}

	if req.MAC != nil {
		mac := strings.TrimSpace(*req.MAC)
		if _, err := net.ParseMAC(mac); err != nil {
			fail(w, http.StatusBadRequest, errors.New("invalid MAC address: "+mac))
			return
		}
	}

	runner := getBluetoothRunner()

	// Trigger stabilize if requested
	if req.Stabilize != nil && *req.Stabilize {
		_, _ = runner(r.Context(), "sc5000-bt-helper", "stabilize")
	}

	// If mode changed or provided, run helper
	if req.Mode != nil {
		_, _ = runner(r.Context(), "sc5000-bt-helper", "set-mode", cfg.Mode)
	}

	// Connect to MAC if specified
	if req.MAC != nil {
		mac := strings.TrimSpace(*req.MAC)
		_, _ = runner(r.Context(), "sc5000-bt-helper", "connect", mac)
	}

	// Persist configuration
	if err := s.saveBluetoothConfig(cfg); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	// Return current status
	st := s.getBluetoothStatus(r.Context())
	respond(w, st)
}
