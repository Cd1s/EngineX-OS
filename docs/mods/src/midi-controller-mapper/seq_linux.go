//go:build linux

package midimapper

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// These are the Linux UAPI layouts from include/uapi/sound/asequencer.h,
// not libasound's opaque structs. In particular port_info.kernel is a native
// pointer, while ev_ext is packed and the event union is always 12 bytes.
// Keep the old reserved tail sizes: newer UMP fields occupy reserved bytes.
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
type seqEvent struct {
	Type, Flags, Tag, Queue uint8
	Time                    [2]uint32
	Source, Dest            seqAddr
	Data                    [3]uint32
}

const seqEventSize = 28

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

type midiDevice struct {
	ID         string `json:"id"`
	Client     int    `json:"client"`
	Port       int    `json:"port"`
	ClientName string `json:"clientName"`
	PortName   string `json:"portName"`
	USBID      string `json:"usbId"`
	Label      string `json:"label"`
}
type inputEvent struct {
	Event   seqEvent
	Payload []byte
}
type sequencer interface {
	Devices() ([]midiDevice, error)
	Subscribe(client, port int) error
	Unsubscribe(client, port int) error
	Wait(timeoutMS int) error
	Read() ([]inputEvent, error)
	Close() error
}
type kernelSequencer struct{ fd, client, port int }

func openKernelSequencer() (sequencer, error) {
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
	s := &kernelSequencer{fd: fd}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	var id int32
	if err = seqIoctl(fd, ioctlClientID, unsafe.Pointer(&id)); err != nil {
		return nil, err
	}
	s.client = int(id)
	client := seqClientInfo{Client: id}
	if err = seqIoctl(fd, ioctlGetClientInfo, unsafe.Pointer(&client)); err != nil {
		return nil, err
	}
	client.Name = [64]byte{}
	copy(client.Name[:], "EngineX-MIDI-Mapper")
	if err = seqIoctl(fd, ioctlSetClientInfo, unsafe.Pointer(&client)); err != nil {
		return nil, err
	}
	port := seqPortInfo{Addr: seqAddr{Client: uint8(id)}, Capability: 1 | 2 | 1<<5 | 1<<6, Type: 1<<1 | 1<<20, MIDIChannels: 16}
	copy(port.Name[:], "MIDI Mapper Input")
	if err = seqIoctl(fd, ioctlCreatePort, unsafe.Pointer(&port)); err != nil {
		return nil, err
	}
	s.port = int(port.Addr.Port)
	// Announce is mandatory: failure to subscribe would silently remove the
	// immediate hot-unplug safety path.
	if err = s.Subscribe(0, 1); err != nil {
		return nil, fmt.Errorf("subscribe System:Announce: %w", err)
	}
	ok = true
	return s, nil
}
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(b), "�"))
}
func lenTrim(s string) int { return len(strings.TrimSpace(s)) }
func readCardUSBID(card int) string {
	if card < 0 {
		return ""
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/asound/card%d/usbid", card))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(b)))
}
func (s *kernelSequencer) Devices() ([]midiDevice, error) {
	devices := []midiDevice{}
	c := seqClientInfo{Client: -1}
	for {
		err := seqIoctl(s.fd, ioctlQueryNextClient, unsafe.Pointer(&c))
		if errors.Is(err, unix.ENOENT) {
			return devices, nil
		}
		if err != nil {
			return nil, err
		}
		cid := int(c.Client)
		if cid == 0 || cid == 14 || cid == s.client {
			continue
		}
		name := cString(c.Name[:])
		if strings.HasPrefix(name, "Midi::Out::") || strings.HasPrefix(name, "EngineX-") {
			continue
		}
		usb := readCardUSBID(int(c.Card))
		p := seqPortInfo{Addr: seqAddr{Client: uint8(cid), Port: 255}}
		for {
			err := seqIoctl(s.fd, ioctlQueryNextPort, unsafe.Pointer(&p))
			if errors.Is(err, unix.ENOENT) {
				break
			}
			if err != nil {
				return nil, err
			}
			if p.Capability&1 != 0 && p.Capability&(1<<5) != 0 {
				pid := int(p.Addr.Port)
				id := fmt.Sprintf("%d:%d", cid, pid)
				devices = append(devices, midiDevice{ID: id, Client: cid, Port: pid, ClientName: name, PortName: cString(p.Name[:]), USBID: usb, Label: fmt.Sprintf("%s [%s]", name, id)})
			}
		}
	}
}
func (s *kernelSequencer) subscription(client, port int) seqSubscription {
	return seqSubscription{Sender: seqAddr{uint8(client), uint8(port)}, Dest: seqAddr{uint8(s.client), uint8(s.port)}}
}
func (s *kernelSequencer) Subscribe(client, port int) error {
	sub := s.subscription(client, port)
	return seqIoctl(s.fd, ioctlSubscribePort, unsafe.Pointer(&sub))
}
func (s *kernelSequencer) Unsubscribe(client, port int) error {
	sub := s.subscription(client, port)
	return seqIoctl(s.fd, ioctlUnsubscribePort, unsafe.Pointer(&sub))
}
func (s *kernelSequencer) Wait(timeoutMS int) error {
	p := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
	for {
		_, err := unix.Poll(p, timeoutMS)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if p[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return fmt.Errorf("sequencer poll failure (0x%x)", p[0].Revents)
		}
		return nil
	}
}
func (s *kernelSequencer) Read() ([]inputEvent, error) {
	var buffer [8192]byte
	for {
		n, err := unix.Read(s.fd, buffer[:])
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
		return parseSeqEvents(buffer[:n])
	}
}
func parseSeqEvents(raw []byte) ([]inputEvent, error) {
	events := make([]inputEvent, 0, len(raw)/seqEventSize)
	for len(raw) > 0 {
		if len(raw) < seqEventSize {
			return nil, fmt.Errorf("short ALSA sequencer event (%d bytes)", len(raw))
		}
		b := raw[:seqEventSize]
		e := seqEvent{Type: b[0], Flags: b[1], Tag: b[2], Queue: b[3], Time: [2]uint32{binary.NativeEndian.Uint32(b[4:]), binary.NativeEndian.Uint32(b[8:])}, Source: seqAddr{b[12], b[13]}, Dest: seqAddr{b[14], b[15]}, Data: [3]uint32{binary.NativeEndian.Uint32(b[16:]), binary.NativeEndian.Uint32(b[20:]), binary.NativeEndian.Uint32(b[24:])}}
		raw = raw[seqEventSize:]
		event := inputEvent{Event: e}
		if e.Flags&12 != 0 {
			size := uint64(e.Data[0])
			if size > uint64(len(raw)) {
				return nil, fmt.Errorf("short ALSA variable event payload")
			}
			event.Payload = append([]byte{}, raw[:int(size)]...)
			raw = raw[int(size):]
		}
		events = append(events, event)
	}
	return events, nil
}
func (s *kernelSequencer) Close() error { return unix.Close(s.fd) }

// Decode the legacy events snd_midi_event_decode accepted. Multi-controller
// messages remain a single MIDI slice, so the mapper rejects their >3-byte
// length just as the Python implementation did. System messages likewise go
// through its normal status/length validation rather than controlling a deck.
func decodeSeqMIDI(e inputEvent) []int {
	event := e.Event
	var data [12]byte
	for i, v := range event.Data {
		binary.NativeEndian.PutUint32(data[i*4:], v)
	}
	channel := int(data[0] & 15)
	param := int(event.Data[1])
	value := int(int32(event.Data[2]))
	switch event.Type {
	case 6:
		return []int{0x90 | channel, int(data[1] & 127), int(data[2] & 127)}
	case 7:
		return []int{0x80 | channel, int(data[1] & 127), int(data[2] & 127)}
	case 8:
		return []int{0xa0 | channel, int(data[1] & 127), int(data[2] & 127)}
	case 10:
		return []int{0xb0 | channel, param & 127, value & 127}
	case 11:
		return []int{0xc0 | channel, value & 127}
	case 12:
		return []int{0xd0 | channel, value & 127}
	case 13:
		v := value + 8192
		return []int{0xe0 | channel, v & 127, (v >> 7) & 127}
	case 14:
		if param < 32 {
			return []int{0xb0 | channel, param & 127, (value >> 7) & 127, 0xb0 | channel, (param + 32) & 127, value & 127}
		}
		return []int{0xb0 | channel, param & 127, value & 127}
	case 15, 16:
		msb, lsb := 99, 98
		if event.Type == 16 {
			msb, lsb = 101, 100
		}
		return []int{0xb0 | channel, msb, (param >> 7) & 127, 0xb0 | channel, lsb, param & 127, 0xb0 | channel, 6, (value >> 7) & 127, 0xb0 | channel, 38, value & 127}
	case 20:
		return []int{0xf2, value & 127, (value >> 7) & 127}
	case 21:
		return []int{0xf3, value & 127}
	case 22:
		return []int{0xf1, value & 127}
	case 30:
		return []int{0xfa}
	case 31:
		return []int{0xfb}
	case 32:
		return []int{0xfc}
	case 36:
		return []int{0xf8}
	case 40:
		return []int{0xf6}
	case 41:
		return []int{0xff}
	case 42:
		return []int{0xfe}
	case 130:
		if len(e.Payload) > 32 {
			return nil
		}
		raw := make([]int, len(e.Payload))
		for i, b := range e.Payload {
			raw[i] = int(b)
		}
		return raw
	}
	return nil
}

func applyLowPriority() int {
	// Linux nice is per-thread. Lower every existing runtime thread, then lower
	// the locked worker thread too; subsequently created threads inherit nice.
	entries, _ := os.ReadDir("/proc/self/task")
	for _, entry := range entries {
		if tid, err := strconv.Atoi(entry.Name()); err == nil {
			_ = syscall.Setpriority(syscall.PRIO_PROCESS, tid, 19)
		}
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19); err != nil {
		return 0
	}
	return 19
}
func processCPUTime() float64 {
	var r unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &r) != nil {
		return 0
	}
	return float64(r.Utime.Sec) + float64(r.Utime.Usec)/1e6 + float64(r.Stime.Sec) + float64(r.Stime.Usec)/1e6
}

func (f *mapper) listDevicesLocked() []midiDevice {
	if f.backend == nil {
		return []midiDevice{}
	}
	devices, err := f.backend.Devices()
	if err != nil || devices == nil {
		return []midiDevice{}
	}
	return devices
}
func (f *mapper) tryConnectDeviceLocked() bool {
	devices := f.listDevicesLocked()
	preset := f.currentPreset()
	name := textValue(get(preset, "name", f.presetID))
	if name == "" {
		name = "MIDI Controller"
	}
	if len(devices) == 0 {
		f.disconnectLocked()
		f.matchHint = fmt.Sprintf("Waiting for USB MIDI controller matching preset '%s'...", name)
		return false
	}
	var chosen *midiDevice
	for i := range devices {
		d := &devices[i]
		if f.selectedDevice != "" && f.selectedDevice != "auto" {
			if d.ID == f.selectedDevice || d.ClientName == f.selectedDevice {
				chosen = d
				break
			}
		} else if matchesPreset(*d, preset) {
			chosen = d
			break
		}
	}
	if chosen == nil {
		f.disconnectLocked()
		labels := []string{}
		for _, d := range devices[:min(3, len(devices))] {
			labels = append(labels, d.Label)
		}
		unmatched := strings.Join(labels, ", ")
		if f.selectedDevice != "" && f.selectedDevice != "auto" {
			f.matchHint = fmt.Sprintf("Selected MIDI port '%s' not found (available: %s).", f.selectedDevice, unmatched)
		} else {
			f.matchHint = fmt.Sprintf("No MIDI controller matched preset '%s' (ignored unmatched port: %s).", name, unmatched)
		}
		return false
	}
	if f.connected && f.connectedClient == chosen.Client && f.connectedPort == chosen.Port {
		f.matchHint = ""
		return true
	}
	f.disconnectLocked()
	if err := f.backend.Subscribe(chosen.Client, chosen.Port); err != nil {
		return false
	}
	f.connected = true
	f.connectedClient = chosen.Client
	f.connectedPort = chosen.Port
	f.connectedName = chosen.Label
	f.matchHint = ""
	f.errorText = ""
	return true
}
func matchesPreset(d midiDevice, preset object) bool {
	cfg := obj(preset["match"])
	usb := strings.ToLower(strings.TrimSpace(d.USBID))
	if ids, ok := cfg["usbIds"].([]any); ok && usb != "" {
		for _, v := range ids {
			if strings.ToLower(strings.TrimSpace(str(v))) == usb {
				return true
			}
		}
	}
	if pats, ok := cfg["alsaClientPatterns"].([]any); ok {
		for _, v := range pats {
			pat := strings.ToLower(strings.TrimSpace(str(v)))
			if pat != "" && (strings.Contains(strings.ToLower(d.ClientName), pat) || strings.Contains(strings.ToLower(d.PortName), pat)) {
				return true
			}
		}
	}
	return false
}
func (f *mapper) disconnectLocked() {
	if f.backend != nil && f.connectedClient >= 0 && f.connectedPort >= 0 {
		_ = f.backend.Unsubscribe(f.connectedClient, f.connectedPort)
	}
	f.connected = false
	f.connectedClient = -1
	f.connectedPort = -1
	f.connectedName = ""
}
func (f *mapper) startWorkerLocked() {
	f.threadError = ""
	backend, err := f.openSequencer()
	if err != nil {
		f.errorText = "ALSA sequencer unavailable: " + err.Error()
		return
	}
	f.backend = backend
	f.stop = make(chan struct{})
	f.done = make(chan struct{})
	f.workerAlive = true
	f.lastThreadHeartbeat = f.now()
	if f.enabled {
		f.tryConnectDeviceLocked()
	}
	go f.alsaLoop(backend, f.stop, f.done)
}
func (f *mapper) stopWorker() {
	f.mu.Lock()
	stop, done := f.stop, f.done
	f.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnectLocked()
	if f.backend != nil {
		_ = f.backend.Close()
		f.backend = nil
	}
	f.stop = nil
	f.done = nil
	f.workerAlive = false
}
func (f *mapper) alsaLoop(backend sequencer, stop <-chan struct{}, done chan<- struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
	defer func() { f.mu.Lock(); f.workerAlive = false; f.mu.Unlock(); close(done) }()
	for {
		select {
		case <-stop:
			return
		default:
		}
		f.mu.Lock()
		f.lastThreadHeartbeat = f.now()
		active := f.enabled && (f.jogVelocity != 0 || f.pitchBendDir != 0)
		f.mu.Unlock()
		timeout := 250
		if active {
			timeout = 16
		}
		err := backend.Wait(timeout)
		select {
		case <-stop:
			return
		default:
		}
		f.mu.Lock()
		f.lastThreadHeartbeat = f.now()
		var events []inputEvent
		if err == nil {
			events, err = backend.Read()
		}
		if err != nil {
			f.threadError = "ALSA sequencer worker error: " + err.Error()
			f.autoDisableLocked(f.threadError)
			f.mu.Unlock()
			return
		}
		now := f.now()
		for _, event := range events {
			switch event.Event.Type {
			case 61:
				f.simulateDisconnectLocked(int(event.Event.Data[0]&255), -1)
				continue
			case 64:
				f.simulateDisconnectLocked(int(event.Event.Data[0]&255), int(event.Event.Data[0]>>8&255))
				continue
			}
			if !f.enabled {
				continue
			}
			if raw := decodeSeqMIDI(event); len(raw) > 0 {
				f.processMIDILocked(raw, now)
			} else {
				switch event.Event.Type {
				case 60, 62, 63, 65, 66, 67:
				default:
					f.recordMalformedLocked("unrecognized ALSA event payload", now)
				}
			}
		}
		if f.enabled && f.jogVelocity != 0 {
			f.integrateJogLocked(nil, f.now())
		}
		f.mu.Unlock()
	}
}
