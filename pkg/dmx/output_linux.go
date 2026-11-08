//go:build linux

package dmx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

func allowBroadcast(c *net.UDPConn) error {
	raw, err := c.SyscallConn(); if err != nil { return err }
	var inner error
	err = raw.Control(func(fd uintptr) { inner = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1) })
	if err != nil { return err }; return inner
}

// Linux usbfs and termios2 avoid Python, libusb and a new Go dependency.
// Only explicitly selected device nodes are opened, with O_NONBLOCK.
type usbDevice struct { file *os.File; udmx bool }

func ioctl(fd uintptr, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg))
	if errno != 0 { return errno }; return nil
}

func openUSB(o Output) (usbOutput, error) {
	path := o.Device
	if o.Protocol == "udmx" { path = fmt.Sprintf("/dev/bus/usb/%03d/%03d", o.Bus, o.Address) }
	// uDMX is a specific vendor protocol, never send vendor requests to an
	// arbitrary selected USB device. Verify the usbfs device descriptor first.
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil { return nil, err }
	ok := false; defer func(){ if !ok { f.Close() } }()
	if o.Protocol == "udmx" {
		desc := make([]byte, 18)
		if _, err := f.ReadAt(desc, 0); err != nil { return nil, err }
		if desc[8] != 0xc0 || desc[9] != 0x16 || desc[10] != 0xdc || desc[11] != 0x05 { return nil, errors.New("selected USB device is not uDMX 16c0:05dc") }
		ok = true; return &usbDevice{file:f, udmx:true}, nil
	}
	// asm-generic termios2 (ARM and x86): 250000 baud, 8N2, raw mode.
	var t struct { I, O, C, L uint32; Line byte; CC [19]byte; ISpeed, OSpeed uint32 }
	if err := ioctl(f.Fd(), 0x802c542a, unsafe.Pointer(&t)); err != nil { return nil, err }
	t.I, t.O, t.L, t.C = 0, 0, 0, 0x1000|0x30|0x40|0x80|0x800
	t.ISpeed, t.OSpeed = 250000, 250000
	if err := ioctl(f.Fd(), 0x402c542b, unsafe.Pointer(&t)); err != nil { return nil, err }
	ok = true; return &usbDevice{file:f}, nil
}

func (u *usbDevice) Send(frame [512]byte) error {
	if u.udmx {
		// USBDEVFS_CONTROL: ANYMA uDMX SET_CHANNEL_RANGE (request 2),
		// wValue=number of channels, wIndex=first zero-based channel.
		c := struct { Type, Request uint8; Value, Index, Length uint16; Timeout uint32; Data unsafe.Pointer }{0x40, 2, 512, 0, 512, 30, unsafe.Pointer(&frame[0])}
		request := uintptr(0xc0005500) | unsafe.Sizeof(c)<<16
		return ioctl(u.file.Fd(), request, unsafe.Pointer(&c))
	}
	if err := ioctl(u.file.Fd(), syscall.TIOCSBRK, nil); err != nil { return err }
	time.Sleep(120*time.Microsecond)
	if err := ioctl(u.file.Fd(), syscall.TIOCCBRK, nil); err != nil { return err }
	time.Sleep(12*time.Microsecond)
	p := make([]byte, 513); copy(p[1:], frame[:])
	// A serial route is bounded; a faulty adapter cannot hold up shutdown.
	deadline := time.Now().Add(30*time.Millisecond)
	for len(p)>0 {
		n, err := syscall.Write(int(u.file.Fd()), p)
		if n>0 { p=p[n:] }
		if err != nil && err != syscall.EAGAIN { return err }
		if len(p)>0 { if time.Now().After(deadline) { return os.ErrDeadlineExceeded }; time.Sleep(time.Millisecond) }
	}
	return nil
}

func (u *usbDevice) Close() error { return u.file.Close() }
