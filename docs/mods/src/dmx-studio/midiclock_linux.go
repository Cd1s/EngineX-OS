//go:build linux

package dmxstudio

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ALSA sequencer UAPI structures (include/uapi/sound/asequencer.h).
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

type seqEvent struct {
	Type, Flags, Tag, Queue uint8
	Time                    [2]uint32
	Source, Dest            seqAddr
	Data                    [3]uint32
}

func seqIOCTL(direction, nr, size uintptr) uintptr { return direction<<30 | size<<16 | 'S'<<8 | nr }

var (
	ioctlSeqClientID      = seqIOCTL(2, 0x01, 4)
	ioctlSeqGetClientInfo = seqIOCTL(3, 0x10, unsafe.Sizeof(seqClientInfo{}))
	ioctlSeqSetClientInfo = seqIOCTL(1, 0x11, unsafe.Sizeof(seqClientInfo{}))
	ioctlSeqCreatePort    = seqIOCTL(3, 0x20, unsafe.Sizeof(seqPortInfo{}))
)

func seqIoctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
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

type alsaMIDISink struct {
	fd     int
	client uint8
	port   uint8
}

func openALSAMIDISink() (midiSink, error) {
	var fd int
	var err error
	for {
		fd, err = unix.Open("/dev/snd/seq", unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	s := &alsaMIDISink{fd: fd}
	ok := false
	defer func() {
		if !ok {
			_ = s.Close()
		}
	}()
	var id int32
	if err = seqIoctl(fd, ioctlSeqClientID, unsafe.Pointer(&id)); err != nil {
		return nil, err
	}
	s.client = uint8(id)
	client := seqClientInfo{Client: id}
	if err = seqIoctl(fd, ioctlSeqGetClientInfo, unsafe.Pointer(&client)); err != nil {
		return nil, err
	}
	client.Name = [64]byte{}
	copy(client.Name[:], "EngineX-DMX-Clock")
	if err = seqIoctl(fd, ioctlSeqSetClientInfo, unsafe.Pointer(&client)); err != nil {
		return nil, err
	}
	// CAP_READ | CAP_SUBS_READ, TYPE_MIDI_GENERIC | TYPE_APPLICATION
	port := seqPortInfo{
		Addr:         seqAddr{Client: s.client},
		Capability:   (1 << 0) | (1 << 5),
		Type:         (1 << 1) | (1 << 20),
		MIDIChannels: 16,
	}
	copy(port.Name[:], "DMX Beat Clock Out")
	if err = seqIoctl(fd, ioctlSeqCreatePort, unsafe.Pointer(&port)); err != nil {
		return nil, err
	}
	s.port = port.Addr.Port
	ok = true
	return s, nil
}

func (s *alsaMIDISink) Send(kind midiEventKind) error {
	var evType uint8
	switch kind {
	case midiStart:
		evType = 30 // SNDRV_SEQ_EVENT_START
	case midiContinue:
		evType = 31 // SNDRV_SEQ_EVENT_CONTINUE
	case midiStop:
		evType = 32 // SNDRV_SEQ_EVENT_STOP
	case midiClock:
		evType = 36 // SNDRV_SEQ_EVENT_CLOCK
	default:
		return nil
	}
	ev := seqEvent{
		Type:   evType,
		Queue:  253, // SNDRV_SEQ_QUEUE_DIRECT
		Source: seqAddr{Client: s.client, Port: s.port},
		Dest:   seqAddr{Client: 254, Port: 253}, // SNDRV_SEQ_ADDRESS_SUBSCRIBERS, UNKNOWN
	}
	buf := (*[unsafe.Sizeof(seqEvent{})]byte)(unsafe.Pointer(&ev))[:]
	for {
		_, err := unix.Write(s.fd, buf)
		runtime.KeepAlive(&ev)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			return nil
		}
		return err
	}
}

func (s *alsaMIDISink) Close() error {
	return unix.Close(s.fd)
}
