package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestIWHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help", "help"} {
		var out, err bytes.Buffer
		code := IW([]string{flag}, &out, &err)
		if code != 0 {
			t.Fatalf("expected 0 for %s, got %d", flag, code)
		}
		if !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("expected usage in output for %s, got: %s", flag, out.String())
		}
	}
}

func TestIWVersion(t *testing.T) {
	for _, flag := range []string{"-v", "--version"} {
		var out, err bytes.Buffer
		code := IW([]string{flag}, &out, &err)
		if code != 0 {
			t.Fatalf("expected 0 for %s, got %d", flag, code)
		}
		if !strings.Contains(out.String(), "EngineX OS") {
			t.Fatalf("expected version in output for %s, got: %s", flag, out.String())
		}
	}
}

func TestIWInvalidArgs(t *testing.T) {
	cases := [][]string{
		{},
		{"dev"},
		{"wlan0"},
		{"dev", "wlan0"},
		{"dev", "wlan0", "unsupported"},
		{"dev", "wlan0", "get", "unknown"},
		{"dev", "wlan0", "set", "power_save"},
		{"dev", "wlan0", "set", "power_save", "invalid"},
	}
	for _, c := range cases {
		var out, err bytes.Buffer
		code := IW(c, &out, &err)
		if code != 2 {
			t.Errorf("expected exit code 2 for args %v, got %d (err: %s)", c, code, err.String())
		}
	}
}

func TestIWNonexistentInterface(t *testing.T) {
	var out, err bytes.Buffer
	code := IW([]string{"dev", "nonexistent_if99", "set", "power_save", "off"}, &out, &err)
	if code != 1 {
		t.Fatalf("expected exit code 1 for nonexistent interface, got %d", code)
	}
	if !strings.Contains(err.String(), "nonexistent_if99") {
		t.Fatalf("expected error mentioning nonexistent_if99, got: %s", err.String())
	}
}
