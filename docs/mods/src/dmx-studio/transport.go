package dmxstudio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ArtNet builds an ArtDmx packet (530 bytes).
func ArtNet(universe int, sequence byte, frame []byte) []byte {
	p := make([]byte, 18, 530)
	copy(p, "Art-Net\x00")
	binary.LittleEndian.PutUint16(p[8:], 0x5000)
	binary.BigEndian.PutUint16(p[10:], 14)
	p[12], p[13] = sequence, 0
	binary.LittleEndian.PutUint16(p[14:], uint16(universe-1))
	binary.BigEndian.PutUint16(p[16:], 512)
	return append(p, frame[:512]...)
}

// SACN builds an E1.31 data packet (638 bytes).
func SACN(universe int, sequence byte, frame, cid []byte, terminated bool) []byte {
	p := make([]byte, 638)
	copy(p, "\x00\x10\x00\x00ASC-E1.17\x00\x00\x00")
	binary.BigEndian.PutUint16(p[16:], 0x7000|622)
	binary.BigEndian.PutUint32(p[18:], 4)
	copy(p[22:38], cid)
	binary.BigEndian.PutUint16(p[38:], 0x7000|600)
	binary.BigEndian.PutUint32(p[40:], 2)
	copy(p[44:108], "EngineX DMX Studio")
	p[108] = 100
	p[111] = sequence
	if terminated {
		p[112] = 0x40
	}
	binary.BigEndian.PutUint16(p[113:], uint16(universe))
	binary.BigEndian.PutUint16(p[115:], 0x7000|523)
	p[117], p[118] = 2, 0xA1
	binary.BigEndian.PutUint16(p[119:], 0)
	binary.BigEndian.PutUint16(p[121:], 1)
	binary.BigEndian.PutUint16(p[123:], 513)
	p[125] = 0
	copy(p[126:], frame[:512])
	return p
}

// adapter is one output route.
type adapter interface {
	send(frame []byte) error
	close()
}

// ioctl retries EINTR: Go's preemption signals interrupt raw syscalls.
func ioctl(fd int, req uintptr, arg uintptr) error {
	for {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

// serialPort is FTDI/OpenDMX on /dev/ttyUSB* or /dev/ttyACM*: termios2 BOTHER
// 250000 baud 8N2, break 120 µs, mark-after-break 12 µs, then start code + 512.
// Validated on a deck (NH08S, FT232R, #208): ~38 Hz, no audio impact.
type serialPort struct{ fd int }

const (
	tcgets2  = 0x802C542A
	tcsets2  = 0x402C542B
	tcsbrk   = 0x5409
	tiocsbrk = 0x5427
	tioccbrk = 0x5428
)

func openSerial(device string) (*serialPort, error) {
	path, err := filepath.EvalSymlinks(device)
	if err == nil {
		path, err = filepath.Abs(path)
	}
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(path, "/dev/ttyUSB") && !strings.HasPrefix(path, "/dev/ttyACM") {
		return nil, errors.New("FTDI device must be /dev/ttyUSB* or /dev/ttyACM*")
	}
	var fd int
	for {
		fd, err = syscall.Open(path, syscall.O_WRONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var t [44]byte
	if err := ioctl(fd, tcgets2, uintptr(unsafe.Pointer(&t[0]))); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("TCGETS2: %w", err)
	}
	binary.NativeEndian.PutUint32(t[0:], 0)                           // c_iflag
	binary.NativeEndian.PutUint32(t[4:], 0)                           // c_oflag
	binary.NativeEndian.PutUint32(t[8:], 0x1000|0x30|0x40|0x80|0x800) // BOTHER|CS8|CSTOPB|CREAD|CLOCAL
	binary.NativeEndian.PutUint32(t[12:], 0)                          // c_lflag
	binary.NativeEndian.PutUint32(t[36:], 250000)                     // c_ispeed
	binary.NativeEndian.PutUint32(t[40:], 250000)                     // c_ospeed
	if err := ioctl(fd, tcsets2, uintptr(unsafe.Pointer(&t[0]))); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("TCSETS2: %w", err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return &serialPort{fd: fd}, nil
}

func (s *serialPort) send(frame []byte) error {
	if err := ioctl(s.fd, tcsbrk, 1); err != nil { // tcdrain
		return err
	}
	if err := ioctl(s.fd, tiocsbrk, 0); err != nil {
		return err
	}
	time.Sleep(120 * time.Microsecond)
	if err := ioctl(s.fd, tioccbrk, 0); err != nil {
		return err
	}
	time.Sleep(12 * time.Microsecond)
	data := append([]byte{0}, frame[:512]...)
	for len(data) > 0 {
		n, err := syscall.Write(s.fd, data)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n <= 0 {
			return errors.New("short DMX serial write")
		}
		data = data[n:]
	}
	return nil
}

func (s *serialPort) close() { syscall.Close(s.fd) }

// udmx is one uDMX (16c0:05dc) at a USB bus/address, driven through Linux usbfs
// (the former libusb-1.0 path issued the same control transfer).
type udmx struct{ fd int }

// usbdevfs_ctrltransfer: 8 bytes of setup, u32 timeout, then the data pointer.
type ctrlTransfer struct {
	requestType, request uint8
	value, index, length uint16
	timeout              uint32
	data                 unsafe.Pointer
}

func usbdevfsControl() uintptr {
	// _IOWR('U', 0, struct usbdevfs_ctrltransfer)
	return 0xC0000000 | uintptr(unsafe.Sizeof(ctrlTransfer{}))<<16 | 'U'<<8
}

func openUDMX(bus, address int) (*udmx, error) {
	path := fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, address)
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("uDMX 16c0:05dc not found at selected bus/address")
		}
		return nil, errors.New("uDMX open failed; check USB permissions")
	}
	var desc [18]byte
	n, err := syscall.Read(fd, desc[:])
	if err != nil || n < 18 || binary.LittleEndian.Uint16(desc[8:]) != 0x16C0 || binary.LittleEndian.Uint16(desc[10:]) != 0x05DC {
		syscall.Close(fd)
		return nil, errors.New("uDMX 16c0:05dc not found at selected bus/address")
	}
	return &udmx{fd: fd}, nil
}

func (u *udmx) send(frame []byte) error {
	data := make([]byte, 512)
	copy(data, frame)
	// uDMX SET_CHANNEL_RANGE: vendor OUT, request=2, count=512, start=0.
	ct := ctrlTransfer{requestType: 0x40, request: 2, value: 512, index: 0, length: 512, timeout: 100, data: unsafe.Pointer(&data[0])}
	var n uintptr
	var e syscall.Errno
	for {
		n, _, e = syscall.Syscall(syscall.SYS_IOCTL, uintptr(u.fd), usbdevfsControl(), uintptr(unsafe.Pointer(&ct)))
		if e != syscall.EINTR {
			break
		}
	}
	runtime.KeepAlive(data)
	if e != 0 {
		return fmt.Errorf("uDMX transfer failed: %v", e)
	}
	if n != 512 {
		return fmt.Errorf("uDMX transfer failed: %d", n)
	}
	return nil
}

func (u *udmx) close() { syscall.Close(u.fd) }

// udp is Art-Net (6454) or sACN (5568) to one IPv4 host, broadcast allowed.
type udp struct {
	conn *net.UDPConn
	addr *net.UDPAddr
}

func openUDP(host string, port int) (*udp, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	if raw, err := conn.SyscallConn(); err == nil {
		raw.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1) })
	}
	return &udp{conn: conn, addr: &net.UDPAddr{IP: net.ParseIP(host), Port: port}}, nil
}

// send writes one already-built packet (ArtNet or SACN).
func (u *udp) send(packet []byte) error {
	u.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := u.conn.WriteToUDP(packet, u.addr)
	return err
}

func (u *udp) close() { u.conn.Close() }

var (
	ptrSize    = unsafe.Sizeof(uintptr(0))
	sizeofCtrl = unsafe.Sizeof(ctrlTransfer{})
)
