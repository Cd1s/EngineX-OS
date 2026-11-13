package broadcast

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

		// A card driven by snd-usb-audio is a USB DAC whatever its name says: a USB-C dock or
		// interface may call itself "HDMI" (BH-GF3-6). The keywords keep out the built-in cards.
		usbDriver := strings.HasPrefix(strings.ToLower(currRawName), "usb-audio")
		excludeKeywords := []string{"loopback", "uac2gadget", "uac2_gadget"}
		if !usbDriver {
			excludeKeywords = append(excludeKeywords, "rockchip", "az01", "wm8960", "sndhdmi", "hdmi")
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
		scriptPath = "/usr/sbin/enginex-usb-audio"
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

// ErrInvalidUSBAudioConfig marks a configuration Apply refuses (the caller's mistake, not the deck's).
var ErrInvalidUSBAudioConfig = errors.New("invalid USB audio configuration")

// Apply validates configuration, writes to disk, and invokes /usr/sbin/enginex-usb-audio. It reports a
// configuration that could not be saved or a helper run that failed (BH-GF3-6: both were dropped and
// the Web UI showed success); every step is still attempted.
func (m *USBAudioManager) Apply(cfg USBAudioConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg.HostDACMode = strings.ToLower(strings.TrimSpace(cfg.HostDACMode))
	if cfg.HostDACMode == "" {
		cfg.HostDACMode = "default"
	}
	if cfg.HostDACMode != "default" && cfg.HostDACMode != "external" {
		return fmt.Errorf("%w: hostDacMode %q (must be 'default' or 'external')", ErrInvalidUSBAudioConfig, cfg.HostDACMode)
	}

	if cfg.SampleRate == 0 {
		cfg.SampleRate = 48000
	}
	if cfg.SampleRate != 44100 && cfg.SampleRate != 48000 && cfg.SampleRate != 96000 {
		return fmt.Errorf("%w: sampleRate %d (must be 44100, 48000, or 96000)", ErrInvalidUSBAudioConfig, cfg.SampleRate)
	}

	cfg.HostDACCard = strings.TrimSpace(cfg.HostDACCard)
	if cfg.HostDACCard == "" {
		cfg.HostDACCard = "auto"
	}

	m.cfg = cfg

	var errs []error
	if m.configPath != "" {
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err == nil {
			err = os.WriteFile(m.configPath, data, 0600)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("saving the USB audio settings: %w", err))
		}
	}
	run := func(args ...string) {
		if out, err := exec.Command(m.scriptPath, args...).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w: %s", filepath.Base(m.scriptPath), strings.Join(args, " "), err, strings.TrimSpace(string(out))))
		}
	}

	// Invoke script if present and executable
	if st, err := os.Stat(m.scriptPath); err == nil && st.Mode()&0111 != 0 {
		if cfg.GadgetUAC2 {
			run("enable-uac2", strconv.Itoa(cfg.SampleRate)) // the rate the gadget offers the computer
		} else {
			run("disable-uac2")
		}

		if cfg.HostDACMode == "external" {
			target := cfg.HostDACCard
			if target == "auto" {
				if dacs, _ := DetectUSBDACs(m.cardsPath); len(dacs) > 0 {
					target = dacs[0].Device
				}
			}
			if target != "" && target != "auto" {
				run("route-dac", target)
			}
		} else {
			run("route-dac", "default")
		}
	}

	return errors.Join(errs...)
}
