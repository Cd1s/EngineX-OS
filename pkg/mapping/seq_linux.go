//go:build linux

package mapping

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The ALSA sequencer layouts are the Linux UAPI ones (include/uapi/sound/asequencer.h), as the
// midi-controller-mapper Mod uses them (docs/mods/src/midi-controller-mapper/seq_linux.go): port_info.kernel is
// a native pointer, the event is always 28 bytes. This client only ever receives: its one port
// can be written to by the controller's port, and cannot be read from.

type seqAddr struct{ Client, Port uint8 }

type seqClientInfo struct {
	Client, Type                   int32
	Name                           [64]byte
	Filter                         uint32
	MulticastFilter                [8]byte
	EventFilter                    [32]byte
	NumPorts, EventLost, Card, PID int32
	Reserved                       [56]byte
}

type seqPortInfo struct {
	Addr                                                     seqAddr
	Name                                                     [64]byte
	Capability, Type                                         uint32
	MIDIChannels, MIDIVoices, SynthVoices, ReadUse, WriteUse int32
	Kernel                                                   uintptr
	Flags                                                    uint32
	TimeQueue                                                uint8
	Reserved                                                 [59]byte
}

type seqSubscription struct {
	Sender, Dest  seqAddr
	Voices, Flags uint32
	Queue         uint8
	Pad           [3]byte
	Reserved      [64]byte
}

const (
	seqEventSize = 28

	capRead      = 1 << 0
	capWrite     = 1 << 1
	capSubsRead  = 1 << 5
	capSubsWrite = 1 << 6

	evNoteOn  = 6
	evNoteOff = 7
)

func seqIOCTL(direction, nr, size uintptr) uintptr { return direction<<30 | size<<16 | 'S'<<8 | nr }

var (
	ioctlClientID        = seqIOCTL(2, 0x01, 4)
	ioctlGetClientInfo   = seqIOCTL(3, 0x10, unsafe.Sizeof(seqClientInfo{}))
	ioctlSetClientInfo   = seqIOCTL(1, 0x11, unsafe.Sizeof(seqClientInfo{}))
	ioctlCreatePort      = seqIOCTL(3, 0x20, unsafe.Sizeof(seqPortInfo{}))
	ioctlSubscribePort   = seqIOCTL(1, 0x30, unsafe.Sizeof(seqSubscription{}))
	ioctlUnsubscribePort = seqIOCTL(1, 0x31, unsafe.Sizeof(seqSubscription{}))
	ioctlQueryNextClient = seqIOCTL(3, 0x51, unsafe.Sizeof(seqClientInfo{}))
	ioctlQueryNextPort   = seqIOCTL(3, 0x52, unsafe.Sizeof(seqPortInfo{}))
)

func seqIoctl(fd int, request uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
		runtime.KeepAlive(arg)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

// seqDevice is the sequencer's device node; the tests point it at nothing to skip.
var seqDevice = "/dev/snd/seq"

type kernelSource struct {
	fd, client, port int
	subscribed       *Port
}

// OpenSource opens an ALSA sequencer client of its own with one write-only port. Nothing is
// subscribed until Subscribe.
func OpenSource() (Source, error) {
	var fd int
	var err error
	for {
		fd, err = unix.Open(seqDevice, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	s := &kernelSource{fd: fd}
	ok := false
	defer func() {
		if !ok {
			unix.Close(fd)
		}
	}()
	var id int32
	if err = seqIoctl(fd, ioctlClientID, unsafe.Pointer(&id)); err != nil {
		return nil, err
	}
	s.client = int(id)
	info := seqClientInfo{Client: id}
	if err = seqIoctl(fd, ioctlGetClientInfo, unsafe.Pointer(&info)); err != nil {
		return nil, err
	}
	info.Name = [64]byte{}
	copy(info.Name[:], "EngineX-Button-Learn")
	if err = seqIoctl(fd, ioctlSetClientInfo, unsafe.Pointer(&info)); err != nil {
		return nil, err
	}
	port := seqPortInfo{Addr: seqAddr{Client: uint8(id)}, Capability: capWrite | capSubsWrite, Type: 1<<1 | 1<<20, MIDIChannels: 16}
	copy(port.Name[:], "Learn Input")
	if err = seqIoctl(fd, ioctlCreatePort, unsafe.Pointer(&port)); err != nil {
		return nil, err
	}
	s.port = int(port.Addr.Port)
	ok = true
	return s, nil
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(b), "�"))
}

// Ports lists the ports other clients let a reader subscribe to. Its own client and the
// System client are left out; PickPort chooses among the rest.
func (s *kernelSource) Ports() ([]Port, error) {
	var out []Port
	c := seqClientInfo{Client: -1}
	for {
		err := seqIoctl(s.fd, ioctlQueryNextClient, unsafe.Pointer(&c))
		if errors.Is(err, unix.ENOENT) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		cid := int(c.Client)
		if cid == 0 || cid == s.client {
			continue
		}
		name := cString(c.Name[:])
		p := seqPortInfo{Addr: seqAddr{Client: uint8(cid), Port: 255}}
		for {
			err := seqIoctl(s.fd, ioctlQueryNextPort, unsafe.Pointer(&p))
			if errors.Is(err, unix.ENOENT) {
				break
			}
			if err != nil {
				return nil, err
			}
			if p.Capability&capRead != 0 && p.Capability&capSubsRead != 0 {
				out = append(out, Port{Client: cid, Port: int(p.Addr.Port), ClientName: name, PortName: cString(p.Name[:])})
			}
		}
	}
}

func (s *kernelSource) subscription(p Port) seqSubscription {
	return seqSubscription{Sender: seqAddr{uint8(p.Client), uint8(p.Port)}, Dest: seqAddr{uint8(s.client), uint8(s.port)}}
}

// Subscribe connects the controller's port to this client's port: the controller sends,
// this client receives. Engine's connection to the same port is not touched.
func (s *kernelSource) Subscribe(p Port) error {
	sub := s.subscription(p)
	if err := seqIoctl(s.fd, ioctlSubscribePort, unsafe.Pointer(&sub)); err != nil {
		return err
	}
	s.subscribed = &p
	return nil
}

// Next waits up to wait for events and returns the notes among them.
func (s *kernelSource) Next(wait time.Duration) ([]Event, error) {
	p := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(p, int(wait.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	if p[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return nil, fmt.Errorf("sequencer poll failure (0x%x)", p[0].Revents)
	}
	var buf [4096]byte
	for {
		n, err := unix.Read(s.fd, buf[:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.EOF
		}
		return parseNotes(buf[:n])
	}
}

// parseNotes decodes sequencer events and keeps the note-on and note-off ones. A note-on with
// velocity 0 is a note-off. A variable-length event (sysex) carries its payload after the
// header and is skipped.
func parseNotes(raw []byte) ([]Event, error) {
	var out []Event
	for len(raw) > 0 {
		if len(raw) < seqEventSize {
			return nil, fmt.Errorf("short ALSA sequencer event (%d bytes)", len(raw))
		}
		b := raw[:seqEventSize]
		typ, flags := b[0], b[1]
		data := [3]uint32{binary.NativeEndian.Uint32(b[16:]), binary.NativeEndian.Uint32(b[20:]), binary.NativeEndian.Uint32(b[24:])}
		raw = raw[seqEventSize:]
		if flags&12 != 0 { // variable length or pointer: the payload follows
			size := uint64(data[0])
			if size > uint64(len(raw)) {
				return nil, fmt.Errorf("short ALSA variable event payload")
			}
			raw = raw[int(size):]
			continue
		}
		if typ != evNoteOn && typ != evNoteOff {
			continue
		}
		var d [12]byte
		for i, v := range data {
			binary.NativeEndian.PutUint32(d[i*4:], v)
		}
		out = append(out, Event{Channel: int(d[0] & 15), Note: int(d[1] & 127), On: typ == evNoteOn && d[2]&127 != 0})
	}
	return out, nil
}

// Close drops the subscription and the client. Closing the descriptor does the same, but
// the explicit unsubscribe means nothing is left subscribed even if Close is slow.
func (s *kernelSource) Close() error {
	if s.fd < 0 {
		return nil
	}
	if s.subscribed != nil {
		sub := s.subscription(*s.subscribed)
		seqIoctl(s.fd, ioctlUnsubscribePort, unsafe.Pointer(&sub))
		s.subscribed = nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	return err
}
