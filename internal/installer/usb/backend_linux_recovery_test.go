//go:build linux && (amd64 || arm64)

package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestLinuxUnplugDuringEnumerationDoesNotHideSibling(t *testing.T) {
	root := t.TempDir()
	linuxEnumerationFixture(t, root, "1-1", "1")
	linuxEnumerationFixture(t, root, "1-2", "1")
	backend := &linuxBackend{sysroot: root, devroot: root}
	backend.scanAttachment = func(path string) (linuxAttachment, error) {
		if filepath.Base(path) == "1-1" {
			return linuxAttachment{}, unix.ENODEV
		}
		return backend.attachment(path)
	}
	devices, err := backend.Enumerate(context.Background())
	if err != nil || len(devices) != 1 || devices[0].Product != "1-2" || devices[0].Location != "1-2" {
		t.Fatalf("hot-unplug blocked another port: devices=%v err=%v", devices, err)
	}
}

func TestLinuxOpenMalformedDeviceNamesItsPath(t *testing.T) {
	root := t.TempDir()
	path := linuxEnumerationFixture(t, root, "1-1", "1")
	backend := &linuxBackend{sysroot: root, devroot: root, calls: linuxCalls{open: func(string, int, uint32) (int, error) { t.Fatal("opened invalid descriptors"); return -1, nil }}}
	devices, err := backend.Enumerate(context.Background())
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices=%v err=%v", devices, err)
	}
	data, err := os.ReadFile(filepath.Join(path, "descriptors"))
	if err != nil {
		t.Fatal(err)
	}
	data[18+4] = 2
	if err := os.WriteFile(filepath.Join(path, "descriptors"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = backend.Open(context.Background(), devices[0].ID, 3, 0)
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != Protocol || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing descriptor/device context: %v", err)
	}
}

func TestLinuxAlternateSelection(t *testing.T) {
	for _, test := range []struct {
		name      string
		multiple  bool
		alternate uint8
		wantSets  int
	}{
		{"single default alternate", false, 0, 0}, {"multiple default alternate", true, 0, 1}, {"multiple nondefault alternate", true, 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := linuxEnumerationFixture(t, root, "1-1", "1")
			if test.multiple {
				file := filepath.Join(path, "descriptors")
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				alt := append([]byte(nil), data[18+9:]...)
				alt[3] = 1
				data = append(data, alt...)
				binary.LittleEndian.PutUint16(data[18+2:18+4], uint16(len(data)-18))
				if err := os.WriteFile(file, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			sets := 0
			backend := &linuxBackend{sysroot: root, devroot: root, calls: linuxCalls{open: func(string, int, uint32) (int, error) { return 10, nil }, close: func(int) error { return nil }, ioctl: func(_ int, request uintptr, arg unsafe.Pointer) error {
				if request == 0x80085504 {
					sets++
					if !test.multiple {
						return unix.EINVAL
					}
					if *(*[2]uint32)(arg) != [2]uint32{3, uint32(test.alternate)} {
						t.Fatal("wrong selected alternate")
					}
				}
				return nil
			}}}
			devices, err := backend.Enumerate(context.Background())
			if err != nil || len(devices) != 1 {
				t.Fatalf("devices=%v err=%v", devices, err)
			}
			device, err := backend.Open(context.Background(), devices[0].ID, 3, test.alternate)
			if err != nil {
				t.Fatal(err)
			}
			if err := device.Close(); err != nil {
				t.Fatal(err)
			}
			if sets != test.wantSets {
				t.Fatalf("SET_INTERFACE calls=%d want=%d", sets, test.wantSets)
			}
		})
	}
}

func TestLinuxPollCompletionAndInterruption(t *testing.T) {
	var active *linuxURB
	ready := false
	polls := 0
	device := linuxTestDevice(linuxCalls{ioctl: func(_ int, request uintptr, arg unsafe.Pointer) error {
		switch request {
		case 0x8038550a:
			active = (*linuxURB)(arg)
		case 0x4008550d:
			if !ready {
				return unix.EAGAIN
			}
			active.Actual = 4
			*(**linuxURB)(arg) = active
		}
		return nil
	}, poll: func(fds []unix.PollFd, wait int) (int, error) {
		polls++
		if len(fds) != 1 || fds[0].Fd != 10 || fds[0].Events&unix.POLLOUT == 0 || wait < 1 || wait > 50 {
			t.Fatal("invalid usbfs poll")
		}
		if polls == 1 {
			return 0, unix.EINTR
		}
		ready = true
		fds[0].Revents = unix.POLLOUT
		return 1, nil
	}})
	n, err := device.WriteBulk(context.Background(), 2, []byte{1, 2, 3, 4})
	if err != nil || n != 4 || polls != 2 {
		t.Fatalf("n=%d polls=%d err=%v", n, polls, err)
	}
}

func TestLinuxNativeTimeoutResetsAfterDrainWithoutReplay(t *testing.T) {
	var active *linuxURB
	drained := false
	submits, resets := 0, 0
	device := linuxTestDevice(linuxCalls{ioctl: func(_ int, request uintptr, arg unsafe.Pointer) error {
		switch request {
		case 0x8038550a:
			active = (*linuxURB)(arg)
			submits++
		case 0x4008550d:
			active.Status = -int32(unix.ETIMEDOUT)
			*(**linuxURB)(arg) = active
			drained = true
		case 0x80045515:
			if !drained || *(*uint32)(arg) != 2 {
				t.Fatal("reset preceded drain or used another endpoint")
			}
			resets++
			return unix.EPERM
		}
		return nil
	}})
	_, err := device.WriteBulk(context.Background(), 2, []byte{1})
	var detail *Error
	if !errors.Is(err, unix.ETIMEDOUT) || !errors.Is(err, unix.EPERM) || !errors.As(err, &detail) || detail.Kind != Timeout || !detail.MayHaveWritten || submits != 1 || resets != 1 {
		t.Fatalf("error=%v submits=%d resets=%d", err, submits, resets)
	}
}

func TestLinuxPollHangupClosesAndReportsDisconnected(t *testing.T) {
	closed := false
	device := linuxTestDevice(linuxCalls{ioctl: func(_ int, request uintptr, _ unsafe.Pointer) error {
		if request == 0x8038550a {
			return nil
		}
		return unix.EAGAIN
	}, close: func(int) error { closed = true; return nil }, poll: func(fds []unix.PollFd, _ int) (int, error) { fds[0].Revents = unix.POLLHUP; return 1, nil }})
	_, err := device.WriteBulk(context.Background(), 2, []byte{1})
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != Disconnected || !detail.MayHaveWritten || !closed || device.fd != -1 {
		t.Fatalf("error=%v closed=%v", err, closed)
	}
}
