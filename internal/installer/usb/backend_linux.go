//go:build linux && (amd64 || arm64)

package usb

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type linuxCalls struct {
	open  func(string, int, uint32) (int, error)
	ioctl func(int, uintptr, unsafe.Pointer) error
	close func(int) error
}

func nativeLinuxCalls() linuxCalls {
	return linuxCalls{unix.Open, func(fd int, req uintptr, arg unsafe.Pointer) error {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		runtime.KeepAlive(arg)
		if errno != 0 {
			return errno
		}
		return nil
	}, unix.Close}
}

type linuxAttachment struct {
	info                    DeviceInfo
	path, node, fingerprint string
}
type linuxBackend struct {
	mu               sync.Mutex
	sysroot, devroot string
	attachments      map[DeviceID]linuxAttachment
	calls            linuxCalls
}

func NewBackend() (Backend, error) {
	return &linuxBackend{sysroot: "/sys/bus/usb/devices", devroot: "/dev/bus/usb", calls: nativeLinuxCalls()}, nil
}

func linuxError(op string, err error, written bool) error {
	kind := System
	switch {
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		kind = Permission
	case errors.Is(err, unix.EBUSY):
		kind = Busy
	case errors.Is(err, unix.ENODEV), errors.Is(err, unix.ENOENT), errors.Is(err, unix.ESHUTDOWN):
		kind = Disconnected
	case errors.Is(err, unix.ETIMEDOUT):
		kind = Timeout
	case errors.Is(err, unix.EPIPE), errors.Is(err, unix.EPROTO), errors.Is(err, unix.EOVERFLOW):
		kind = Protocol
	}
	return fault(op, kind, written, err)
}

func readSmall(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, errors.New("sysfs property exceeds limit")
	}
	return b, nil
}

func sysNumber(path string, bits int) (uint64, error) {
	b, err := readSmall(path, 32)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, bits)
}

func (b *linuxBackend) attachment(path string) (linuxAttachment, error) {
	desc, err := readSmall(filepath.Join(path, "descriptors"), 65535)
	if err != nil {
		return linuxAttachment{}, err
	}
	if len(desc) < 18 || desc[0] != 18 || desc[1] != 1 {
		return linuxAttachment{}, errors.New("invalid device descriptor")
	}
	bus, err := sysNumber(filepath.Join(path, "busnum"), 16)
	if err != nil {
		return linuxAttachment{}, err
	}
	addr, err := sysNumber(filepath.Join(path, "devnum"), 16)
	if err != nil {
		return linuxAttachment{}, err
	}
	active, err := sysNumber(filepath.Join(path, "bConfigurationValue"), 8)
	if err != nil {
		return linuxAttachment{}, err
	}
	info := DeviceInfo{VID: binary.LittleEndian.Uint16(desc[8:10]), PID: binary.LittleEndian.Uint16(desc[10:12])}
	found := false
	for offset := 18; offset < len(desc); {
		if len(desc)-offset < 9 {
			return linuxAttachment{}, errors.New("truncated configuration")
		}
		n := int(binary.LittleEndian.Uint16(desc[offset+2 : offset+4]))
		if n < 9 || n > len(desc)-offset {
			return linuxAttachment{}, errors.New("invalid configuration size")
		}
		if uint64(desc[offset+5]) == active {
			if found {
				return linuxAttachment{}, errors.New("duplicate active configuration")
			}
			info.Interfaces, err = parseConfiguration(desc[offset : offset+n])
			if err != nil {
				return linuxAttachment{}, err
			}
			found = true
		}
		offset += n
	}
	// An unconfigured device can be diagnosed, but Open will not configure it.
	if !found && active != 0 {
		return linuxAttachment{}, errors.New("active configuration is absent")
	}
	for name, target := range map[string]*string{"product": &info.Product, "serial": &info.Serial} {
		if value, e := readSmall(filepath.Join(path, name), 1024); e == nil {
			*target = strings.TrimSpace(string(value))
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		return linuxAttachment{}, err
	}
	stat := st.Sys().(*syscall.Stat_t)
	stamp := fmt.Sprintf("%d:%d:%v:%x:%d:%d:%d", stat.Dev, stat.Ino, stat.Ctim, sha256.Sum256(desc), bus, addr, active)
	return linuxAttachment{info: info, path: path, node: filepath.Join(b.devroot, fmt.Sprintf("%03d/%03d", bus, addr)), fingerprint: stamp}, nil
}

func (b *linuxBackend) Enumerate(ctx context.Context) ([]DeviceInfo, error) {
	entries, err := os.ReadDir(b.sysroot)
	if err != nil {
		return nil, linuxError("enumerate", err, false)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	next := make(map[DeviceID]linuxAttachment)
	var result []DeviceInfo
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, fault("enumerate", Cancelled, false, err)
		}
		if strings.Contains(entry.Name(), ":") {
			continue
		}
		path := filepath.Join(b.sysroot, entry.Name())
		if _, err := os.Stat(filepath.Join(path, "idVendor")); errors.Is(err, os.ErrNotExist) {
			continue
		}
		a, err := b.attachment(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, linuxError("enumerate descriptors", err, false)
		}
		a.info.ID = nextID()
		next[a.info.ID] = a
		result = append(result, cloneInfo(a.info))
	}
	b.attachments = next
	return result, nil
}

func (b *linuxBackend) Open(ctx context.Context, id DeviceID, number, alt uint8) (Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, fault("open", Cancelled, false, err)
	}
	b.mu.Lock()
	a, ok := b.attachments[id]
	b.mu.Unlock()
	if !ok {
		return nil, fault("open", Disconnected, false, errors.New("unknown or stale enumeration identity"))
	}
	it, err := selected(a.info, number, alt)
	if err != nil {
		return nil, err
	}
	current, err := b.attachment(a.path)
	if err != nil {
		return nil, linuxError("open identity", err, false)
	}
	if current.fingerprint != a.fingerprint {
		return nil, fault("open", Disconnected, false, errors.New("attachment changed after enumeration"))
	}
	fd, err := b.calls.open(a.node, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, linuxError("open usbfs", err, false)
	}
	cleanup := func(e error) (Device, error) {
		return nil, errors.Join(e, linuxError("close usbfs", b.calls.close(fd), false))
	}
	current, err = b.attachment(a.path)
	if err != nil {
		return cleanup(linuxError("open identity", err, false))
	}
	if current.fingerprint != a.fingerprint {
		return cleanup(fault("open", Disconnected, false, errors.New("attachment changed while opening")))
	}
	claim := uint32(number)
	if err = b.calls.ioctl(fd, 0x8004550f, unsafe.Pointer(&claim)); err != nil {
		return cleanup(linuxError("claim interface", err, false))
	}
	settings := [2]uint32{uint32(number), uint32(alt)}
	if err = b.calls.ioctl(fd, 0x80085504, unsafe.Pointer(&settings)); err != nil {
		releaseErr := b.calls.ioctl(fd, 0x80045510, unsafe.Pointer(&claim))
		return cleanup(errors.Join(linuxError("set alternate", err, false), linuxError("release interface", releaseErr, false)))
	}
	a.info.Interfaces = []Interface{it}
	return &linuxDevice{s: newSession(), fd: fd, calls: b.calls, info: a.info, it: it}, nil
}

type linuxURB struct {
	Type, Endpoint                                  uint8
	Status                                          int32
	Flags                                           uint32
	Buffer                                          unsafe.Pointer
	Length, Actual, StartFrame, Packets, ErrorCount uint32
	Signal                                          uint32
	UserContext                                     uintptr
}

type linuxDevice struct {
	s     *session
	fd    int
	calls linuxCalls
	info  DeviceInfo
	it    Interface
}

func (d *linuxDevice) Info() DeviceInfo { return cloneInfo(d.info) }

func (d *linuxDevice) exchange(parent context.Context, ep uint8, data []byte, control, write bool) (int, error) {
	if err := transferSize(len(data)); err != nil {
		return 0, err
	}
	ctx, end, err := d.s.begin(parent)
	if err != nil {
		return 0, err
	}
	defer end()
	if err := ctx.Err(); err != nil {
		return 0, fault("transfer", Cancelled, false, err)
	}
	if d.fd < 0 {
		return 0, fault("transfer", Disconnected, false, errors.New("usbfs handle closed"))
	}
	urb := &linuxURB{Type: 3, Endpoint: ep, Length: uint32(len(data))}
	if control {
		urb.Type = 2
	}
	var pinned runtime.Pinner
	pinned.Pin(urb)
	defer pinned.Unpin()
	if len(data) > 0 {
		pinned.Pin(&data[0])
		urb.Buffer = unsafe.Pointer(&data[0])
	}
	if err := d.calls.ioctl(d.fd, 0x8038550a, unsafe.Pointer(urb)); err != nil {
		return 0, linuxError("submit transfer", err, false)
	}
	// usbfs retains both URB and data until reap or close. Never return early.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var cancelled error
	for {
		var completed *linuxURB
		err := d.calls.ioctl(d.fd, 0x4008550d, unsafe.Pointer(&completed))
		if err == nil {
			if completed != urb {
				d.abortHandle()
				return 0, fault("reap transfer", Protocol, write, errors.New("unexpected completion identity"))
			}
			n := int(urb.Actual)
			if n > len(data) {
				d.abortHandle()
				return 0, fault("reap transfer", Protocol, write, errors.New("invalid actual transfer length"))
			}
			if cancelled != nil {
				return n, fault("transfer", Cancelled, write, cancelled)
			}
			if urb.Status != 0 {
				return n, linuxError("transfer", unix.Errno(-urb.Status), write)
			}
			return n, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EAGAIN) {
			closeErr := d.abortHandle()
			return 0, errors.Join(linuxError("reap transfer", err, write), closeErr)
		}
		if cancelled != nil {
			<-ticker.C
			continue
		}
		select {
		case <-ctx.Done():
			cancelled = ctx.Err()
			if e := d.calls.ioctl(d.fd, 0x550b, unsafe.Pointer(urb)); e != nil && !errors.Is(e, unix.EINVAL) {
				closeErr := d.abortHandle()
				return 0, errors.Join(fault("cancel transfer", Cancelled, write, cancelled), linuxError("discard transfer", e, write), closeErr)
			}
		case <-ticker.C:
		}
	}
}

func (d *linuxDevice) abortHandle() error {
	fd := d.fd
	d.fd = -1
	if fd < 0 {
		return nil
	}
	return linuxError("close usbfs", d.calls.close(fd), false)
}
func (d *linuxDevice) ReadBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	if err := bulkEndpoint(d.it, ep, true); err != nil {
		return 0, err
	}
	return d.exchange(ctx, ep, data, false, false)
}
func (d *linuxDevice) WriteBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	if err := bulkEndpoint(d.it, ep, false); err != nil {
		return 0, err
	}
	return d.exchange(ctx, ep, data, false, true)
}
func (d *linuxDevice) Control(ctx context.Context, typ, request uint8, value, index uint16, data []byte) (int, error) {
	if err := controlRequest(d.it, typ, request, index, len(data)); err != nil {
		return 0, err
	}
	buffer := make([]byte, 8+len(data))
	buffer[0] = typ
	buffer[1] = request
	binary.LittleEndian.PutUint16(buffer[2:4], value)
	binary.LittleEndian.PutUint16(buffer[4:6], index)
	binary.LittleEndian.PutUint16(buffer[6:8], uint16(len(data)))
	if typ&0x80 == 0 {
		copy(buffer[8:], data)
	}
	ep := uint8(0)
	if typ&0x80 != 0 {
		ep = 0x80
	}
	n, err := d.exchange(ctx, ep, buffer, true, typ&0x80 == 0)
	// usbfs control actual_length excludes the eight-byte setup packet.
	if n > len(data) {
		return 0, fault("control", Protocol, typ&0x80 == 0, errors.New("invalid control response length"))
	}
	if typ&0x80 != 0 {
		copy(data[:n], buffer[8:8+n])
	}
	return n, err
}
func (d *linuxDevice) ClearHalt(parent context.Context, ep uint8) error {
	if err := bulkEndpoint(d.it, ep, ep&0x80 != 0); err != nil {
		return err
	}
	ctx, end, err := d.s.begin(parent)
	if err != nil {
		return err
	}
	defer end()
	if err := ctx.Err(); err != nil {
		return fault("clear halt", Cancelled, false, err)
	}
	if d.fd < 0 {
		return fault("clear halt", Disconnected, false, errors.New("device closed"))
	}
	endpoint := uint32(ep)
	return linuxError("clear halt", d.calls.ioctl(d.fd, 0x80045515, unsafe.Pointer(&endpoint)), false)
}
func (d *linuxDevice) Close() error {
	return d.s.close(func() error {
		if d.fd < 0 {
			return nil
		}
		number := uint32(d.it.Number)
		err := linuxError("release interface", d.calls.ioctl(d.fd, 0x80045510, unsafe.Pointer(&number)), false)
		return errors.Join(err, d.abortHandle())
	})
}
