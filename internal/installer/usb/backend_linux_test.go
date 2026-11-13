//go:build linux && (amd64 || arm64)

package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestLinuxURBLayout(t *testing.T) {
	u := linuxURB{}
	if unsafe.Sizeof(u) != 56 || unsafe.Offsetof(u.Buffer) != 16 || unsafe.Offsetof(u.Length) != 24 || unsafe.Offsetof(u.UserContext) != 48 {
		t.Fatalf("invalid usbfs ABI: %+v size=%d", u, unsafe.Sizeof(u))
	}
}

func linuxTestDevice(calls linuxCalls) *linuxDevice {
	it, _ := parseConfiguration(configurationFixture())
	return &linuxDevice{s: newSession(), fd: 10, calls: calls, it: it[0]}
}
func TestLinuxOneTransferIncludingZLP(t *testing.T) {
	for _, size := range []int{0, 512, 513} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			var active *linuxURB
			submitted := 0
			d := linuxTestDevice(linuxCalls{ioctl: func(_ int, req uintptr, arg unsafe.Pointer) error {
				switch req {
				case 0x8038550a:
					active = (*linuxURB)(arg)
					submitted++
					if int(active.Length) != size {
						t.Fatal("wrong length")
					}
				case 0x4008550d:
					active.Actual = uint32(size)
					*(**linuxURB)(arg) = active
				default:
					t.Fatalf("unexpected ioctl %x", req)
				}
				return nil
			}})
			n, err := d.WriteBulk(context.Background(), 2, make([]byte, size))
			if err != nil || n != size || submitted != 1 {
				t.Fatalf("n=%d err=%v submissions=%d", n, err, submitted)
			}
		})
	}
}

func TestLinuxIdenticalDevicesOpenCloseAndBusy(t *testing.T) {
	root := t.TempDir()
	sysroot := filepath.Join(root, "sys")
	devroot := filepath.Join(root, "dev")
	if err := os.MkdirAll(filepath.Join(devroot, "001"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		node := filepath.Join(sysroot, "1-"+strconv.Itoa(i))
		if err := os.MkdirAll(node, 0700); err != nil {
			t.Fatal(err)
		}
		desc := append([]byte{18, 1, 0, 2, 0, 0, 0, 64, 0xe4, 0x15, 7, 0xd0, 0, 1, 0, 0, 0, 1}, configurationFixture()...)
		for name, data := range map[string][]byte{"idVendor": []byte("15e4"), "busnum": []byte("1"), "devnum": []byte(strconv.Itoa(i)), "bConfigurationValue": []byte("1"), "descriptors": desc, "serial": []byte("identical")} {
			if err := os.WriteFile(filepath.Join(node, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(devroot, "001", "00"+strconv.Itoa(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	claims, releases, closes := 0, 0, 0
	busy := false
	openedPath := ""
	calls := nativeLinuxCalls()
	nativeOpen, nativeClose := calls.open, calls.close
	calls.open = func(path string, flags int, mode uint32) (int, error) {
		openedPath = path
		return nativeOpen(path, flags, mode)
	}
	calls.close = func(fd int) error { closes++; return nativeClose(fd) }
	calls.ioctl = func(_ int, request uintptr, arg unsafe.Pointer) error {
		switch request {
		case 0x8004550f:
			if *(*uint32)(arg) != 3 {
				t.Fatal("claimed unrelated interface")
			}
			if busy {
				return unix.EBUSY
			}
			claims++
		case 0x80045510:
			releases++
		case 0x80085504:
			if *(*[2]uint32)(arg) != [2]uint32{3, 0} {
				t.Fatal("wrong alternate")
			}
		default:
			t.Fatalf("unexpected request %x", request)
		}
		return nil
	}
	b := &linuxBackend{sysroot: sysroot, devroot: devroot, calls: calls}
	list, err := b.Enumerate(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if list[0].ID == list[1].ID {
		t.Fatal("identical attachments share an ID")
	}
	for i := 0; i < 25; i++ {
		d, err := b.Open(context.Background(), list[1].ID, 3, 0)
		if err != nil {
			t.Fatal(err)
		}
		if openedPath != filepath.Join(devroot, "001", "002") {
			t.Fatal("opened another identical attachment")
		}
		if len(d.Info().Interfaces) != 1 {
			t.Fatal("open returned unclaimed interfaces")
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if claims != 25 || releases != 25 || closes != 25 {
		t.Fatalf("claims=%d releases=%d closes=%d", claims, releases, closes)
	}
	busy = true
	_, err = b.Open(context.Background(), list[0].ID, 3, 0)
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != Busy || detail.MayHaveWritten || closes != 26 {
		t.Fatalf("err=%v closes=%d", err, closes)
	}
}
func TestLinuxShortReadAndControl(t *testing.T) {
	var active *linuxURB
	d := linuxTestDevice(linuxCalls{ioctl: func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == 0x8038550a {
			active = (*linuxURB)(arg)
			return nil
		}
		if req != 0x4008550d {
			t.Fatalf("unexpected request %x", req)
		}
		b := unsafe.Slice((*byte)(active.Buffer), int(active.Length))
		offset := 0
		if active.Type == 2 {
			offset = 8
			if binary.LittleEndian.Uint16(b[6:8]) != 8 {
				t.Fatal("bad control setup")
			}
		}
		copy(b[offset:], []byte{7, 8, 9})
		active.Actual = 3
		*(**linuxURB)(arg) = active
		return nil
	}})
	b := make([]byte, 8)
	n, err := d.ReadBulk(context.Background(), 0x81, b)
	if n != 3 || err != nil || b[0] != 7 {
		t.Fatalf("n=%d err=%v b=%v", n, err, b)
	}
	n, err = d.Control(context.Background(), 0xc1, 1, 2, 3, b)
	if n != 3 || err != nil || b[0] != 7 {
		t.Fatalf("control n=%d err=%v", n, err)
	}
}
func TestLinuxCancelDrainsBeforeReturning(t *testing.T) {
	var active *linuxURB
	discarded := false
	reaped := false
	ctx, cancel := context.WithCancel(context.Background())
	d := linuxTestDevice(linuxCalls{ioctl: func(_ int, req uintptr, arg unsafe.Pointer) error {
		switch req {
		case 0x8038550a:
			active = (*linuxURB)(arg)
			cancel()
			return nil
		case 0x550b:
			discarded = true
			return nil
		case 0x4008550d:
			if !discarded {
				return unix.EAGAIN
			}
			reaped = true
			active.Status = -int32(unix.ENOENT)
			*(**linuxURB)(arg) = active
			return nil
		}
		t.Fatal("unexpected ioctl")
		return nil
	}})
	_, err := d.WriteBulk(ctx, 2, []byte("not safe to replay"))
	var detail *Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &detail) || !detail.MayHaveWritten || !reaped {
		t.Fatalf("err=%v reaped=%v", err, reaped)
	}
}
func TestLinuxReapFailureClosesBeforeUnpin(t *testing.T) {
	closed := false
	d := linuxTestDevice(linuxCalls{ioctl: func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req == 0x8038550a {
			return nil
		}
		return unix.ENODEV
	}, close: func(int) error { closed = true; return nil }})
	_, err := d.WriteBulk(context.Background(), 2, []byte{1})
	if err == nil || !closed || d.fd != -1 {
		t.Fatalf("err=%v closed=%v fd=%d", err, closed, d.fd)
	}
}
func TestLinuxSubmitPermissionDoesNotClaimWrite(t *testing.T) {
	d := linuxTestDevice(linuxCalls{ioctl: func(int, uintptr, unsafe.Pointer) error { return unix.EACCES }})
	_, err := d.WriteBulk(context.Background(), 2, []byte{1})
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != Permission || detail.MayHaveWritten {
		t.Fatal(err)
	}
}
func TestLinuxDescriptorEnumerationAndStaleIdentity(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "1-1")
	if err := os.Mkdir(node, 0700); err != nil {
		t.Fatal(err)
	}
	desc := append([]byte{18, 1, 0, 2, 0, 0, 0, 64, 0xe4, 0x15, 7, 0xd0, 0, 1, 0, 0, 0, 1}, configurationFixture()...)
	files := map[string][]byte{"idVendor": []byte("15e4"), "busnum": []byte("1"), "devnum": []byte("2"), "bConfigurationValue": []byte("1"), "descriptors": desc, "product": []byte("Unicode 设备"), "serial": []byte("same")}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(node, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := &linuxBackend{sysroot: root, devroot: root, calls: linuxCalls{open: func(string, int, uint32) (int, error) { t.Fatal("opened stale identity"); return -1, nil }}}
	list, err := b.Enumerate(context.Background())
	if err != nil || len(list) != 1 || list[0].Product != "Unicode 设备" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	id := list[0].ID
	if err := os.WriteFile(filepath.Join(node, "devnum"), []byte("3"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(context.Background(), id, 3, 0); err == nil {
		t.Fatal("opened replaced attachment")
	}
	list, err = b.Enumerate(context.Background())
	if err != nil || list[0].ID == id {
		t.Fatal("reused enumeration identity")
	}
	if _, err := b.Open(context.Background(), id, 3, 0); err == nil {
		t.Fatal("opened old snapshot")
	}
}
func TestLinuxTimeoutIsBounded(t *testing.T) {
	var active *linuxURB
	discarded := false
	d := linuxTestDevice(linuxCalls{ioctl: func(_ int, req uintptr, arg unsafe.Pointer) error {
		switch req {
		case 0x8038550a:
			active = (*linuxURB)(arg)
			return nil
		case 0x550b:
			discarded = true
			return nil
		case 0x4008550d:
			if !discarded {
				return unix.EAGAIN
			}
			active.Status = -int32(unix.ENOENT)
			*(**linuxURB)(arg) = active
			return nil
		}
		return nil
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := d.ReadBulk(ctx, 0x81, make([]byte, 64))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
