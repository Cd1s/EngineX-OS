//go:build windows && amd64

package usb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsNativeSystemAPIs(t *testing.T) {
	calls, err := nativeWinCalls()
	if err != nil {
		t.Fatal(err)
	}
	handle, err := calls.initialize(windows.InvalidHandle)
	if err == nil {
		if e := calls.free(handle); e != nil {
			t.Error(e)
		}
		t.Fatal("WinUSB accepted an invalid file handle")
	}
	t.Log("system WinUSB entry points loaded; invalid handle rejected")
}
func TestWindowsHardwareIDs(t *testing.T) {
	for _, id := range []string{"USB\\VID_15E4&PID_D007&REV_0100", "usb\\vid_15e4&pid_d007&mi_03"} {
		vid, pid, ok := usbHardwareID([]string{id})
		if !ok || vid != 0x15e4 || pid != 0xd007 {
			t.Fatalf("rejected %s", id)
		}
	}
	for _, id := range []string{"Denon USB deck", "USB\\VID_15E4&PID_ZZZZ", "HID\\VID_15E4&PID_D007", "USB\\VID_15E4&PID_D00"} {
		if _, _, ok := usbHardwareID([]string{id}); ok {
			t.Fatalf("accepted %s", id)
		}
	}
}
func winTestDevice(calls winCalls) *windowsDevice {
	it, _ := parseConfiguration(configurationFixture())
	return &windowsDevice{s: newSession(), file: 10, handle: 20, calls: calls, it: it[0]}
}
func TestWindowsInjectedZLPAndShortRead(t *testing.T) {
	writes := 0
	policies := 0
	d := winTestDevice(winCalls{policy: func(uintptr, uint8, uint32) error { policies++; return nil }, transfer: func(_ uintptr, ep uint8, _ bool, data *byte, size uint32, _ uint64, done *uint32, overlap *windows.Overlapped) error {
		if overlap.HEvent == 0 {
			t.Fatal("no overlapped event")
		}
		if ep == 2 {
			if size != 0 || data == nil {
				t.Fatal("incorrect ZLP")
			}
			writes++
			*done = 0
		} else {
			*data = 7
			*done = 1
		}
		return nil
	}})
	n, err := d.WriteBulk(context.Background(), 2, nil)
	if n != 0 || err != nil || writes != 1 {
		t.Fatalf("n=%d err=%v writes=%d", n, err, writes)
	}
	b := make([]byte, 64)
	n, err = d.ReadBulk(context.Background(), 0x81, b)
	if n != 1 || err != nil || b[0] != 7 || policies != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
func TestWindowsInjectedCancelDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	aborted := make(chan struct{})
	drained := false
	d := winTestDevice(winCalls{policy: func(uintptr, uint8, uint32) error { return nil }, transfer: func(uintptr, uint8, bool, *byte, uint32, uint64, *uint32, *windows.Overlapped) error {
		cancel()
		return windows.ERROR_IO_PENDING
	},
		cancel: func(windows.Handle, *windows.Overlapped) error { close(aborted); return nil },
		result: func(_ uintptr, _ *windows.Overlapped, done *uint32) error {
			select {
			case <-aborted:
			case <-time.After(time.Second):
				t.Error("not cancelled")
			}
			*done = 2
			drained = true
			return windows.ERROR_OPERATION_ABORTED
		},
	})
	n, err := d.WriteBulk(ctx, 2, []byte("payload"))
	var detail *Error
	if !drained || n != 2 || !errors.Is(err, context.Canceled) || !errors.As(err, &detail) || !detail.MayHaveWritten {
		t.Fatalf("n=%d drained=%v err=%v", n, drained, err)
	}
}
func TestWindowsInjectedControlSetup(t *testing.T) {
	d := winTestDevice(winCalls{policy: func(uintptr, uint8, uint32) error { return nil }, transfer: func(_ uintptr, ep uint8, control bool, _ *byte, size uint32, setup uint64, done *uint32, _ *windows.Overlapped) error {
		want := uint64(0xc1) | uint64(7)<<8 | uint64(0x1234)<<16 | uint64(3)<<32 | uint64(4)<<48
		if !control || ep != 0 || size != 4 || setup != want {
			t.Errorf("bad setup %016x", setup)
		}
		*done = 4
		return nil
	}})
	if _, err := d.Control(context.Background(), 0xc1, 7, 0x1234, 3, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Control(context.Background(), 0xc1, 7, 0x1234, 4, nil); err == nil {
		t.Fatal("control escaped interface")
	}
}
func TestWindowsOpenUnicodeAndResourceCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usb-caf\u00e9-\U0001f3b5")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	it, _ := parseConfiguration(configurationFixture())
	frees, closes := 0, 0
	a := winAttachment{info: DeviceInfo{ID: "one", VID: 0x15e4, PID: 0xd007, Driver: "WinUSB"}, instance: "fixture-a", devinst: 1, arrival: "one-arrival", paths: []string{path}}
	calls := winCalls{
		initialize: func(windows.Handle) (uintptr, error) { return 20, nil }, free: func(uintptr) error { frees++; return nil },
		query: func(uintptr, uint8) (Interface, error) { return it[0], nil }, descriptor: func(uintptr) (uint16, uint16, error) { return a.info.VID, a.info.PID, nil }, alternate: func(uintptr, uint8) error { return nil },
		close: func(handle windows.Handle) error { closes++; return windows.CloseHandle(handle) },
	}
	b := &windowsBackend{calls: calls, attachments: map[DeviceID]winAttachment{"one": a}, scan: func(context.Context) ([]winAttachment, error) {
		other := a
		other.instance = "fixture-b"
		other.devinst = 2
		return []winAttachment{other, a}, nil
	}}
	for i := 0; i < 25; i++ {
		d, err := b.Open(context.Background(), "one", 3, 0)
		if err != nil {
			t.Fatal(err)
		}
		if d.Info().ID != "one" {
			t.Fatal("selected another identical device")
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if frees != 25 || closes != 25 {
		t.Fatalf("free=%d close=%d", frees, closes)
	}
	if _, err := b.Open(context.Background(), "one", 4, 0); err == nil {
		t.Fatal("opened wrong interface")
	}
	if frees != 26 || closes != 26 {
		t.Fatalf("failed open leaked handles: free=%d close=%d", frees, closes)
	}
	b.scan = func(context.Context) ([]winAttachment, error) {
		replacement := a
		replacement.arrival = "another-arrival"
		return []winAttachment{replacement}, nil
	}
	if _, err := b.Open(context.Background(), "one", 3, 0); err == nil {
		t.Fatal("opened replaced attachment")
	}
}
func TestWindowsBindingDiagnostics(t *testing.T) {
	a := winAttachment{info: DeviceInfo{Driver: "HidUsb"}, paths: []string{"not opened"}}
	b := &windowsBackend{attachments: map[DeviceID]winAttachment{"hid": a}}
	_, err := b.Open(context.Background(), "hid", 0, 0)
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != Binding {
		t.Fatal(err)
	}
}
