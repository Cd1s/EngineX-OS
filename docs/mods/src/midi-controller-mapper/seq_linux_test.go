//go:build linux

package midimapper

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestSequencerUAPILayout(t *testing.T) {
	// asequencer.h: client_info has no native long/pointer; port_info.kernel
	// follows write_use at byte 96. Both ABIs put event.data at byte 16.
	wantPort, wantFlags := uintptr(164), uintptr(100)
	switch runtime.GOARCH {
	case "amd64", "arm64":
		wantPort, wantFlags = 168, 104
	case "arm", "386":
	default:
		t.Skip("UAPI expectations are for the supported little-endian architectures")
	}
	for _, tc := range []struct {
		name      string
		got, want uintptr
	}{
		{"addr size", unsafe.Sizeof(seqAddr{}), 2},
		{"client_info size", unsafe.Sizeof(seqClientInfo{}), 188},
		{"client name", unsafe.Offsetof(seqClientInfo{}.Name), 8},
		{"client card", unsafe.Offsetof(seqClientInfo{}.Card), 124},
		{"client reserved", unsafe.Offsetof(seqClientInfo{}.Reserved), 132},
		{"port_info size", unsafe.Sizeof(seqPortInfo{}), wantPort},
		{"port capability", unsafe.Offsetof(seqPortInfo{}.Capability), 68},
		{"port kernel", unsafe.Offsetof(seqPortInfo{}.Kernel), 96},
		{"port flags", unsafe.Offsetof(seqPortInfo{}.Flags), wantFlags},
		{"subscription size", unsafe.Sizeof(seqSubscription{}), 80},
		{"event size", unsafe.Sizeof(seqEvent{}), 28},
		{"event source", unsafe.Offsetof(seqEvent{}.Source), 12},
		{"event data", unsafe.Offsetof(seqEvent{}.Data), 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s: got %d, want %d on %s", tc.name, tc.got, tc.want, runtime.GOARCH)
			}
		})
	}
	createPort := uintptr(0xc0a45320)
	if wantPort == 168 {
		createPort = 0xc0a85320
	}
	for _, tc := range []struct {
		name      string
		got, want uintptr
	}{
		{"CLIENT_ID", ioctlClientID, 0x80045301},
		{"GET_CLIENT_INFO", ioctlGetClientInfo, 0xc0bc5310},
		{"SET_CLIENT_INFO", ioctlSetClientInfo, 0x40bc5311},
		{"CREATE_PORT", ioctlCreatePort, createPort},
		{"SUBSCRIBE_PORT", ioctlSubscribePort, 0x40505330},
		{"UNSUBSCRIBE_PORT", ioctlUnsubscribePort, 0x40505331},
		{"QUERY_NEXT_CLIENT", ioctlQueryNextClient, 0xc0bc5351},
		{"QUERY_NEXT_PORT", ioctlQueryNextPort, (createPort &^ 0xff) | 0x52},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %#x, want %#x", tc.got, tc.want)
			}
		})
	}
}

func makeInput(eventType uint8, channel, param, value int) inputEvent {
	return inputEvent{Event: seqEvent{Type: eventType, Data: [3]uint32{uint32(channel), uint32(param), uint32(value)}}}
}
func makeNote(eventType uint8, channel, note, velocity int) inputEvent {
	var b [4]byte
	b[0], b[1], b[2] = byte(channel), byte(note), byte(velocity)
	return inputEvent{Event: seqEvent{Type: eventType, Data: [3]uint32{binary.NativeEndian.Uint32(b[:])}}}
}
func encodeEvent(e inputEvent) []byte {
	raw := make([]byte, 28+len(e.Payload))
	event := e.Event
	raw[0], raw[1], raw[2], raw[3] = event.Type, event.Flags, event.Tag, event.Queue
	raw[12], raw[13], raw[14], raw[15] = event.Source.Client, event.Source.Port, event.Dest.Client, event.Dest.Port
	for i, v := range event.Data {
		binary.NativeEndian.PutUint32(raw[16+i*4:], v)
	}
	copy(raw[28:], e.Payload)
	return raw
}
func TestDecodeALSAEvents(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event inputEvent
		want  []int
	}{
		{"note on", makeNote(6, 2, 32, 127), []int{0x92, 32, 127}},
		{"note off", makeNote(7, 3, 1, 64), []int{0x83, 1, 64}},
		{"poly pressure", makeNote(8, 4, 10, 50), []int{0xa4, 10, 50}},
		{"controller", makeInput(10, 1, 29, 64), []int{0xb1, 29, 64}},
		{"program", makeInput(11, 5, 0, 20), []int{0xc5, 20}},
		{"pressure", makeInput(12, 6, 0, 21), []int{0xd6, 21}},
		{"pitch min", makeInput(13, 0, 0, -8192), []int{0xe0, 0, 0}},
		{"pitch center", makeInput(13, 0, 0, 0), []int{0xe0, 0, 64}},
		{"pitch max", makeInput(13, 15, 0, 8191), []int{0xef, 127, 127}},
		{"14 bit controller", makeInput(14, 1, 3, 8193), []int{0xb1, 3, 64, 0xb1, 35, 1}},
		{"7 bit controller", makeInput(14, 1, 40, 65), []int{0xb1, 40, 65}},
		{"NRPN", makeInput(15, 1, 130, 259), []int{0xb1, 99, 1, 0xb1, 98, 2, 0xb1, 6, 2, 0xb1, 38, 3}},
		{"RPN", makeInput(16, 1, 130, 259), []int{0xb1, 101, 1, 0xb1, 100, 2, 0xb1, 6, 2, 0xb1, 38, 3}},
		{"song position", makeInput(20, 0, 0, 129), []int{0xf2, 1, 1}},
		{"song select", makeInput(21, 0, 0, 20), []int{0xf3, 20}},
		{"qframe", makeInput(22, 0, 0, 21), []int{0xf1, 21}},
		{"start", makeInput(30, 0, 0, 0), []int{0xfa}},
		{"continue", makeInput(31, 0, 0, 0), []int{0xfb}},
		{"stop", makeInput(32, 0, 0, 0), []int{0xfc}},
		{"clock", makeInput(36, 0, 0, 0), []int{0xf8}},
		{"tune", makeInput(40, 0, 0, 0), []int{0xf6}},
		{"reset", makeInput(41, 0, 0, 0), []int{0xff}},
		{"sensing", makeInput(42, 0, 0, 0), []int{0xfe}},
		{"unknown", makeInput(255, 0, 0, 0), nil},
		{"sysex", inputEvent{Event: seqEvent{Type: 130}, Payload: []byte{0xf0, 1, 2, 0xf7}}, []int{0xf0, 1, 2, 0xf7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeSeqMIDI(tc.event); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSequencerReadFraming(t *testing.T) {
	first := makeNote(6, 0, 0, 127)
	first.Event.Source = seqAddr{20, 1}
	variable := inputEvent{Event: seqEvent{Type: 130, Flags: 4, Data: [3]uint32{5}}, Payload: []byte{0xf0, 1, 2, 3, 0xf7}}
	last := makeInput(10, 0, 29, 64)
	buffer := append(append(encodeEvent(first), encodeEvent(variable)...), encodeEvent(last)...)
	events, err := parseSeqEvents(buffer)
	if err != nil || len(events) != 3 {
		t.Fatalf("%v %v", events, err)
	}
	if events[0].Event.Source != (seqAddr{20, 1}) || !reflect.DeepEqual(events[1].Payload, variable.Payload) || events[2].Event.Data != last.Event.Data {
		t.Fatal(events)
	}
	for _, raw := range [][]byte{{1}, buffer[:29], encodeEvent(variable)[:30]} {
		if _, err := parseSeqEvents(raw); err == nil {
			t.Fatal("accepted truncated read")
		}
	}
	if cString([]byte(" Port \x00garbage")) != "Port" || cString([]byte{'a', 0xff}) != "a�" {
		t.Fatal("C name decoding")
	}
}

func TestDeviceSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		device   midiDevice
		selected string
		want     bool
	}{
		{"unmatched", midiDevice{ID: "128:0", Client: 128, ClientName: "SandboxTwist", Label: "SandboxTwist [128:0]"}, "auto", false},
		{"name pattern", midiDevice{ID: "20:0", Client: 20, ClientName: "PIONEER CDJ-400", Label: "CDJ [20:0]"}, "auto", true},
		{"port pattern", midiDevice{ID: "20:0", Client: 20, ClientName: "USB MIDI", PortName: "CDJ-400 Port", Label: "USB [20:0]"}, "auto", true},
		{"usb", midiDevice{ID: "24:0", Client: 24, ClientName: "Generic", USBID: " 08E4:0143 ", Label: "Generic [24:0]"}, "auto", true},
		{"explicit port", midiDevice{ID: "128:0", Client: 128, ClientName: "SandboxTwist", Label: "SandboxTwist [128:0]"}, "128:0", true},
		{"explicit name", midiDevice{ID: "128:0", Client: 128, ClientName: "SandboxTwist", Label: "SandboxTwist [128:0]"}, "SandboxTwist", true},
		{"explicit missing", midiDevice{ID: "20:0", Client: 20, ClientName: "CDJ-400", Label: "CDJ [20:0]"}, "99:0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testMapper(t)
			f.enabled = true
			f.selectedDevice = tc.selected
			f.backend.(*fakeSequencer).devices = []midiDevice{tc.device}
			if got := f.tryConnectDeviceLocked(); got != tc.want {
				t.Fatalf("got %v, want %v: %s", got, tc.want, f.matchHint)
			}
			if tc.want && f.connectedClient != tc.device.Client {
				t.Fatal("wrong client")
			}
			if !tc.want && !strings.Contains(f.matchHint, tc.device.Label) {
				t.Fatal(f.matchHint)
			}
		})
	}
}

func TestWorkerReadsMIDIAndAnnounce(t *testing.T) {
	f := testMapper(t)
	f.backend = nil
	fake := &fakeSequencer{events: make(chan []inputEvent, 8), devices: []midiDevice{{ID: "20:0", Client: 20, ClientName: "CDJ-400", Label: "CDJ [20:0]"}}}
	f.openSequencer = func() (sequencer, error) { return fake, nil }
	st := action(t, f, object{"action": "enable"})
	mustBool(t, st["running"], true)
	fake.events <- []inputEvent{makeNote(6, 0, 0, 127), makeNote(6, 0, 1, 127)}
	// The worker's lock synchronizes these observations with transport decoding.
	waitFor := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			f.mu.Lock()
			ok := check()
			f.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("worker condition timed out")
	}
	waitFor(func() bool { return f.eventCount == 2 })
	b := action(t, f, object{"action": "poll"})
	mustBool(t, values(b)["cueHold"], true)
	if !reflect.DeepEqual(b["actions"], []object{{"action": "toggle_play"}}) {
		t.Fatal(b)
	}
	fake.events <- []inputEvent{makeInput(61, 99, 0, 0), makeInput(64, 20|1<<8, 0, 0)}
	fake.events <- []inputEvent{makeNote(6, 0, 0, 127)}
	waitFor(func() bool { return f.eventCount == 3 })
	fake.events <- []inputEvent{makeInput(64, 20, 0, 0)}
	waitFor(func() bool { return !f.enabled })
	flush := action(t, f, object{"action": "poll"})
	mustBool(t, values(flush)["cueHold"], false)
	mustBool(t, values(flush)["jogTouch"], false)
	if len(flush["actions"].([]object)) != 0 {
		t.Fatal("unplug dispatched stale action")
	}
	f.Close()
	f.Close() // Idempotent shutdown and no dangling worker.
	if _, err := f.Status(); err != nil {
		t.Fatal(fmt.Sprint(err))
	}
}
