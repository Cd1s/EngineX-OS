package broadcast

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleAsoundCards = ` 0 [wm8960         ]: wm8960-sound - wm8960-sound
                      wm8960-sound
 1 [rockchiprk3399 ]: rockchip_rk339 - rockchip,rk3399-hdmi
                      rockchip,rk3399-hdmi
 2 [D10s           ]: USB-Audio - Topping D10s
                      Topping D10s at usb-fe380000.usb-1.2, high speed
 3 [Scarlett       ]: USB-Audio - Focusrite Scarlett 2i2 USB
                      Focusrite Focusrite Scarlett 2i2 USB at usb-fe3a0000.usb-1.1, high speed
 4 [Loopback       ]: Loopback - Loopback
                      Loopback 1
 5 [UAC2Gadget     ]: UAC2_Gadget - UAC2_Gadget
                      UAC2_Gadget
`

func TestDetectUSBDACs(t *testing.T) {
	tmp := t.TempDir()
	cardsPath := filepath.Join(tmp, "cards")
	if err := os.WriteFile(cardsPath, []byte(sampleAsoundCards), 0644); err != nil {
		t.Fatalf("failed to write mock cards file: %v", err)
	}

	dacs, err := DetectUSBDACs(cardsPath)
	if err != nil {
		t.Fatalf("DetectUSBDACs failed: %v", err)
	}

	if len(dacs) != 2 {
		t.Fatalf("expected 2 detected USB DACs, got %d: %+v", len(dacs), dacs)
	}

	// Verify Card 2 (Topping D10s)
	if dacs[0].CardIndex != 2 {
		t.Errorf("expected CardIndex 2, got %d", dacs[0].CardIndex)
	}
	if dacs[0].ID != "D10s" {
		t.Errorf("expected ID 'D10s', got %q", dacs[0].ID)
	}
	if dacs[0].Name != "Topping D10s" {
		t.Errorf("expected Name 'Topping D10s', got %q", dacs[0].Name)
	}
	if dacs[0].Device != "hw:2,0" {
		t.Errorf("expected Device 'hw:2,0', got %q", dacs[0].Device)
	}

	// Verify Card 3 (Focusrite Scarlett)
	if dacs[1].CardIndex != 3 {
		t.Errorf("expected CardIndex 3, got %d", dacs[1].CardIndex)
	}
	if dacs[1].ID != "Scarlett" {
		t.Errorf("expected ID 'Scarlett', got %q", dacs[1].ID)
	}
	if dacs[1].Name != "Focusrite Scarlett 2i2 USB" {
		t.Errorf("expected Name 'Focusrite Scarlett 2i2 USB', got %q", dacs[1].Name)
	}
	if dacs[1].Device != "hw:3,0" {
		t.Errorf("expected Device 'hw:3,0', got %q", dacs[1].Device)
	}
}

func TestDetectUSBDACsNonExistent(t *testing.T) {
	dacs, err := DetectUSBDACs("/non/existent/path/to/cards")
	if err != nil {
		t.Fatalf("expected no error for non-existent path, got %v", err)
	}
	if len(dacs) != 0 {
		t.Errorf("expected 0 DACs, got %d", len(dacs))
	}
}

func TestUSBAudioManagerConfigAndApply(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "usb-audio.json")
	cardsPath := filepath.Join(tmp, "cards")
	_ = os.WriteFile(cardsPath, []byte(sampleAsoundCards), 0644)

	logPath := filepath.Join(tmp, "script.log")
	scriptPath := filepath.Join(tmp, "mock-script")
	mockScript := "#!/bin/sh\necho \"$@\" >> \"" + logPath + "\"\n"
	if err := os.WriteFile(scriptPath, []byte(mockScript), 0755); err != nil {
		t.Fatalf("failed to write mock script: %v", err)
	}

	mgr := NewUSBAudioManager(configPath, scriptPath, cardsPath)
	cfg := mgr.GetConfig()
	if cfg.HostDACMode != "default" || cfg.SampleRate != 48000 || cfg.GadgetUAC2 != false {
		t.Errorf("unexpected default config: %+v", cfg)
	}

	// Apply enabled configuration
	newCfg := USBAudioConfig{
		HostDACMode: "external",
		HostDACCard: "hw:2,0",
		SampleRate:  96000,
		GadgetUAC2:  true,
	}
	if err := mgr.Apply(newCfg); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Check updated state
	cur := mgr.GetConfig()
	if cur.HostDACMode != "external" || cur.HostDACCard != "hw:2,0" || cur.SampleRate != 96000 || !cur.GadgetUAC2 {
		t.Errorf("config not updated: %+v", cur)
	}

	// Check script log
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read script log: %v", err)
	}
	logStr := string(logBytes)
	if !strings.Contains(logStr, "enable-uac2") {
		t.Errorf("expected 'enable-uac2' in log, got: %s", logStr)
	}
	if !strings.Contains(logStr, "route-dac hw:2,0") {
		t.Errorf("expected 'route-dac hw:2,0' in log, got: %s", logStr)
	}

	// Test persistence by reloading
	mgr2 := NewUSBAudioManager(configPath, scriptPath, cardsPath)
	if mgr2.GetConfig().SampleRate != 96000 {
		t.Errorf("persisted sampleRate mismatch, got %d", mgr2.GetConfig().SampleRate)
	}

	// Test invalid validation
	if err := mgr.Apply(USBAudioConfig{HostDACMode: "bad"}); err == nil {
		t.Errorf("expected error on invalid hostDacMode")
	}
	if err := mgr.Apply(USBAudioConfig{SampleRate: 12345}); err == nil {
		t.Errorf("expected error on invalid sampleRate")
	}
}
