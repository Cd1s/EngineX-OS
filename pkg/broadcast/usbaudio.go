package broadcast

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// USBDAC represents an external USB audio device.
type USBDAC struct {
	CardIndex int    `json:"cardIndex"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Device    string `json:"device"` // e.g. "hw:2,0"
}

// USBAudioConfig specifies USB-A host DAC and USB-B gadget parameters.
type USBAudioConfig struct {
	HostDACMode string `json:"hostDacMode"` // "default" | "external"
	HostDACCard string `json:"hostDacCard"` // e.g. "hw:2,0" or "auto"
	SampleRate  int    `json:"sampleRate"`  // 44100 | 48000 | 96000 (default 48000)
	GadgetUAC2  bool   `json:"gadgetUac2"`  // USB-B UAC2 "EngineX Master Out" gadget enabled
}

// DefaultUSBAudioConfig returns recommended defaults for USB audio.
func DefaultUSBAudioConfig() USBAudioConfig {
	return USBAudioConfig{
		HostDACMode: "default",
		HostDACCard: "auto",
		SampleRate:  48000,
		GadgetUAC2:  false,
	}
}

var cardHeaderRE = regexp.MustCompile(`^\s*(\d+)\s+\[([^\]]+)\]:\s*(.*)$`)

// DetectUSBDACs scans an ALSA cards file (default /proc/asound/cards) and discovers external USB DACs.
// Built-in soundcards (rockchip, az01, wm8960, sndhdmi, loopback, uac2gadget) are ignored.
func DetectUSBDACs(asoundCardsPath string) ([]USBDAC, error) {
	if asoundCardsPath == "" {
		asoundCardsPath = "/proc/asound/cards"
	}

	f, err := os.Open(asoundCardsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []USBDAC{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var (
		dacs        []USBDAC
		scanner     = bufio.NewScanner(f)
		currIndex   = -1
		currID      = ""
		currRawName = ""
		currDesc    = ""
	)

	flushCard := func() {
		if currIndex < 0 {
			return
		}
		combined := strings.ToLower(currID + " " + currRawName + " " + currDesc)

		// Exclude built-in soundcards
		excludeKeywords := []string{
			"rockchip", "az01", "wm8960", "sndhdmi", "hdmi", "loopback", "uac2gadget", "uac2_gadget",
		}
		for _, kw := range excludeKeywords {
			if strings.Contains(combined, kw) {
				currIndex = -1
				return
			}
		}

		// Must match USB audio criteria
		if strings.Contains(combined, "usb-audio") || strings.Contains(combined, "usb-") || strings.Contains(combined, "usb ") {
			cleanName := currRawName
			if idx := strings.Index(cleanName, " - "); idx != -1 {
				cleanName = cleanName[idx+3:]
			}
			cleanName = strings.TrimPrefix(cleanName, "USB-Audio - ")
			cleanName = strings.TrimSpace(cleanName)
			if cleanName == "" {
				cleanName = currID
			}

			dacs = append(dacs, USBDAC{
				CardIndex: currIndex,
				ID:        currID,
				Name:      cleanName,
				Device:    fmt.Sprintf("hw:%d,0", currIndex),
			})
		}
		currIndex = -1
	}

	for scanner.Scan() {
		line := scanner.Text()
		m := cardHeaderRE.FindStringSubmatch(line)
		if len(m) == 4 {
			flushCard()
			idx, _ := strconv.Atoi(m[1])
			currIndex = idx
			currID = strings.TrimSpace(m[2])
			currRawName = strings.TrimSpace(m[3])
			currDesc = ""
		} else if currIndex >= 0 {
			currDesc += " " + strings.TrimSpace(line)
		}
	}
	flushCard()

	if dacs == nil {
		dacs = []USBDAC{}
	}
	return dacs, scanner.Err()
}

// USBAudioManager manages settings and OS interaction for USB-A DACs and USB-B UAC2 gadget.
type USBAudioManager struct {
	mu         sync.Mutex
	configPath string
	scriptPath string
	cardsPath  string
	cfg        USBAudioConfig
}

// NewUSBAudioManager creates a new USBAudioManager.
func NewUSBAudioManager(configPath, scriptPath, cardsPath string) *USBAudioManager {
	if scriptPath == "" {
		scriptPath = "/usr/sbin/sc5000-usb-audio"
	}
	if cardsPath == "" {
		cardsPath = "/proc/asound/cards"
	}
	mgr := &USBAudioManager{
		configPath: configPath,
		scriptPath: scriptPath,
		cardsPath:  cardsPath,
		cfg:        DefaultUSBAudioConfig(),
	}
	mgr.load()
	return mgr
}

func (m *USBAudioManager) load() {
	if m.configPath == "" {
		return
	}
	b, err := os.ReadFile(m.configPath)
	if err != nil {
		return
	}
	var cfg USBAudioConfig
	if err := json.Unmarshal(b, &cfg); err == nil {
		if cfg.SampleRate == 0 {
			cfg.SampleRate = 48000
		}
		if cfg.HostDACMode == "" {
			cfg.HostDACMode = "default"
		}
		if cfg.HostDACCard == "" {
			cfg.HostDACCard = "auto"
		}
		m.cfg = cfg
	}
}

// GetConfig returns a copy of current USB Audio configuration.
func (m *USBAudioManager) GetConfig() USBAudioConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// DetectDACs lists external USB DACs currently detected by the system.
func (m *USBAudioManager) DetectDACs() ([]USBDAC, error) {
	return DetectUSBDACs(m.cardsPath)
}

// Apply validates configuration, writes to disk, and invokes /usr/sbin/sc5000-usb-audio.
func (m *USBAudioManager) Apply(cfg USBAudioConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg.HostDACMode = strings.ToLower(strings.TrimSpace(cfg.HostDACMode))
	if cfg.HostDACMode == "" {
		cfg.HostDACMode = "default"
	}
	if cfg.HostDACMode != "default" && cfg.HostDACMode != "external" {
		return fmt.Errorf("invalid hostDacMode: %q (must be 'default' or 'external')", cfg.HostDACMode)
	}

	if cfg.SampleRate == 0 {
		cfg.SampleRate = 48000
	}
	if cfg.SampleRate != 44100 && cfg.SampleRate != 48000 && cfg.SampleRate != 96000 {
		return fmt.Errorf("invalid sampleRate: %d (must be 44100, 48000, or 96000)", cfg.SampleRate)
	}

	cfg.HostDACCard = strings.TrimSpace(cfg.HostDACCard)
	if cfg.HostDACCard == "" {
		cfg.HostDACCard = "auto"
	}

	m.cfg = cfg

	if m.configPath != "" {
		if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			_ = os.WriteFile(m.configPath, data, 0600)
		}
	}

	// Invoke script if present and executable
	if st, err := os.Stat(m.scriptPath); err == nil && st.Mode()&0111 != 0 {
		if cfg.GadgetUAC2 {
			_ = exec.Command(m.scriptPath, "enable-uac2").Run()
		} else {
			_ = exec.Command(m.scriptPath, "disable-uac2").Run()
		}

		if cfg.HostDACMode == "external" {
			target := cfg.HostDACCard
			if target == "auto" {
				if dacs, _ := DetectUSBDACs(m.cardsPath); len(dacs) > 0 {
					target = dacs[0].Device
				}
			}
			if target != "" && target != "auto" {
				_ = exec.Command(m.scriptPath, "route-dac", target).Run()
			}
		} else {
			_ = exec.Command(m.scriptPath, "route-dac", "default").Run()
		}
	}

	return nil
}
