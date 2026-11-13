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
	for file, data := range map[string][]byte{"idVendor": []byte("15e4"), "busnum": []byte("1"), "devnum": []byte(strings.TrimPrefix(name, "1-")), "bConfigurationValue": []byte(active), "descriptors": descriptors, "product": []byte(name)} {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLinuxEmptyConfigurationDoesNotHideConfiguredSibling(t *testing.T) {
	root := t.TempDir()
	linuxEnumerationFixture(t, root, "1-1", "1")
	path := linuxEnumerationFixture(t, root, "1-2", "")
	backend := &linuxBackend{sysroot: root, devroot: root}
	devices, err := backend.Enumerate(context.Background())
	if err != nil || len(devices) != 2 {
		t.Fatalf("devices=%v error=%v", devices, err)
	}
	_, err = backend.Open(context.Background(), devices[1].ID, 3, 0)
	var detail *Error
	if len(devices[1].Interfaces) != 0 || !errors.As(err, &detail) || detail.Kind != Unsupported || !strings.Contains(err.Error(), "device "+path+":") {
		t.Fatalf("unconfigured device: %v", err)
	}
}

func TestLinuxMalformedSiblingDoesNotAbortEnumeration(t *testing.T) {
	root := t.TempDir()
	target := linuxEnumerationFixture(t, root, "1-1", "1")
	bad := filepath.Join(linuxEnumerationFixture(t, root, "1-2", "1"), "descriptors")
	data, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	data[18+4] = 7
	if err := os.WriteFile(bad, data, 0600); err != nil {
		t.Fatal(err)
	}
	backend := &linuxBackend{sysroot: root, devroot: root}
	devices, err := backend.Enumerate(context.Background())
	if err != nil || len(devices) != 1 || devices[0].Product != "1-1" {
		t.Fatalf("devices=%v error=%v", devices, err)
	}
	// The selected device must still be validated if its descriptors change.
	if err := os.WriteFile(filepath.Join(target, "descriptors"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = backend.Open(context.Background(), devices[0].ID, 3, 0)
	if err == nil || !strings.Contains(err.Error(), "device "+target+":") {
		t.Fatalf("missing device context: %v", err)
	}
}

func TestLinuxDeviceReadFailureDoesNotHideSibling(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(linuxEnumerationFixture(t, root, "1-1", "1"), "descriptors")
	linuxEnumerationFixture(t, root, "1-2", "1")
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bad, 0700); err != nil {
		t.Fatal(err)
	}
	// A real read error exercises the generic skip path used by ENODEV too,
	// without a production test hook or a physical USB device.
	backend := &linuxBackend{sysroot: root, devroot: root}
	devices, err := backend.Enumerate(context.Background())
	if err != nil || len(devices) != 1 || devices[0].Product != "1-2" {
		t.Fatalf("devices=%v error=%v", devices, err)
	}
}
