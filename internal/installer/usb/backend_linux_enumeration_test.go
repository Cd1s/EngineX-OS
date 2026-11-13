//go:build linux && (amd64 || arm64)

package usb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linuxEnumerationFixture(t *testing.T, root, name, active string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	descriptors := append([]byte{18, 1, 0, 2, 0, 0, 0, 64, 0xe4, 0x15, 7, 0xd0, 0, 1, 0, 0, 0, 1}, configurationFixture()...)
	for file, data := range map[string][]byte{
		"idVendor": []byte("15e4"), "busnum": []byte("1"), "devnum": []byte(strings.TrimPrefix(name, "1-")),
		"bConfigurationValue": []byte(active), "descriptors": descriptors, "product": []byte(name),
	} {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLinuxEmptyConfigurationDoesNotHideConfiguredSibling(t *testing.T) {
	for _, test := range []struct{ name, value string }{{"empty", ""}, {"newline", "\n"}, {"whitespace", " \t\n"}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			linuxEnumerationFixture(t, root, "1-1", "1")
			unconfigured := linuxEnumerationFixture(t, root, "1-2", test.value)
			backend := &linuxBackend{sysroot: root, devroot: root, calls: linuxCalls{open: func(string, int, uint32) (int, error) { t.Fatal("opened an unconfigured device"); return -1, nil }}}
			devices, err := backend.Enumerate(context.Background())
			if err != nil || len(devices) != 2 {
				t.Fatalf("unconfigured sibling blocked discovery: devices=%d err=%v", len(devices), err)
			}
			for _, device := range devices {
				if device.Product == "1-2" {
					if len(device.Interfaces) != 0 {
						t.Fatal("unconfigured device advertises active interfaces")
					}
					_, err := backend.Open(context.Background(), device.ID, 3, 0)
					var detail *Error
					if !errors.As(err, &detail) || detail.Kind != Unsupported || !strings.Contains(err.Error(), "unconfigured") || !strings.Contains(err.Error(), unconfigured) {
						t.Fatalf("unclear unconfigured-device error: %v", err)
					}
				}
			}
		})
	}
}

func TestLinuxMalformedSiblingDoesNotAbortEnumeration(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"interface count", func(b []byte) []byte { b[18+4] = 2; return b }},
		{"endpoint count", func(b []byte) []byte { b[18+13] = 3; return b }},
		{"truncated device", func(b []byte) []byte { return b[:8] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			linuxEnumerationFixture(t, root, "1-1", "1")
			badPath := linuxEnumerationFixture(t, root, "1-2", "1")
			path := filepath.Join(badPath, "descriptors")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, test.mutate(data), 0600); err != nil {
				t.Fatal(err)
			}
			backend := &linuxBackend{sysroot: root, devroot: root}
			devices, err := backend.Enumerate(context.Background())
			if err != nil || len(devices) != 1 || devices[0].Product != "1-1" {
				t.Fatalf("malformed unrelated device blocked discovery: devices=%v err=%v", devices, err)
			}
			_, err = backend.attachment(badPath)
			if err == nil || !strings.Contains(err.Error(), badPath) {
				t.Fatalf("descriptor error omits device path: %v", err)
			}
		})
	}
}
