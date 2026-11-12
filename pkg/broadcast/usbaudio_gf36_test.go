package broadcast

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BH-GF3-6: a USB DAC whose name says HDMI (USB-C docks, HDMI audio extractors) was left out because
// of the word alone; the built-in HDMI card must still be.
func TestDetectUSBDACsKeepsAUSBDACNamedHDMI(t *testing.T) {
	cards := filepath.Join(t.TempDir(), "cards")
	os.WriteFile(cards, []byte(` 0 [rockchiphdmi   ]: rockchip-hdmi - rockchip,hdmi
                      rockchip,hdmi
 1 [sndhdmi        ]: dw-hdmi-i2s - sndhdmi
                      sndhdmi
 2 [USBHDMI        ]: USB-Audio - FiiO USB-C HDMI Audio DAC
                      FiiO USB-C HDMI Audio DAC at usb-ff540000.usb-1, high speed
 3 [Loopback       ]: Loopback - Loopback
                      Loopback 1
`), 0o644)
	dacs, err := DetectUSBDACs(cards)
	if err != nil || len(dacs) != 1 || dacs[0].Device != "hw:2,0" || dacs[0].Name != "FiiO USB-C HDMI Audio DAC" {
		t.Fatalf("DetectUSBDACs = %+v, %v; want only the FiiO on hw:2,0", dacs, err)
	}
}

// BH-GF3-6: Apply reported success when the settings could not be saved or the helper failed, and
// never told the helper the chosen sample rate.
func TestUSBAudioApplyReportsFailuresAndPassesTheRate(t *testing.T) {
	tmp := t.TempDir()
	log := filepath.Join(tmp, "script.log")
	script := filepath.Join(tmp, "helper")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> \""+log+"\"\n"), 0o755)
	mgr := NewUSBAudioManager(filepath.Join(tmp, "usb-audio.json"), script, filepath.Join(tmp, "no-cards"))
	if err := mgr.Apply(USBAudioConfig{HostDACMode: "default", SampleRate: 96000, GadgetUAC2: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(log); !strings.Contains(string(got), "enable-uac2 96000\n") {
		t.Fatalf("helper calls %q, want the chosen rate", got)
	}

	unsaved := NewUSBAudioManager(filepath.Join(tmp, "missing-dir", "usb-audio.json"), script, filepath.Join(tmp, "no-cards"))
	if err := unsaved.Apply(USBAudioConfig{HostDACMode: "default"}); err == nil {
		t.Fatal("Apply reported success although the settings could not be saved")
	}

	failing := filepath.Join(tmp, "failing-helper")
	os.WriteFile(failing, []byte("#!/bin/sh\necho no UDC >&2\nexit 1\n"), 0o755)
	broken := NewUSBAudioManager(filepath.Join(tmp, "b.json"), failing, filepath.Join(tmp, "no-cards"))
	err := broken.Apply(USBAudioConfig{HostDACMode: "default", GadgetUAC2: true})
	if err == nil || !strings.Contains(err.Error(), "enable-uac2") {
		t.Fatalf("Apply = %v, want the helper's failure", err)
	}
}
