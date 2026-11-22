//go:build linux

package mapping

import (
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestParseNotesDecodesTheKernelsEventLayout(t *testing.T) {
	ev := func(typ, flags uint8, ch, note, vel byte) []byte {
		b := make([]byte, seqEventSize)
		b[0], b[1] = typ, flags
		b[16], b[17], b[18] = ch, note, vel
		return b
	}
	raw := append(ev(evNoteOn, 0, 0, 30, 127), ev(evNoteOn, 0, 1, 30, 0)...) // velocity 0 is a release
	raw = append(raw, ev(evNoteOff, 0, 0, 27, 64)...)
	raw = append(raw, ev(10, 0, 0, 7, 99)...) // a controller change: not a note
	// a system-exclusive event: header, then its payload, which must not be read as events
	sysex := make([]byte, seqEventSize)
	sysex[0], sysex[1] = 130, 4
	binary.NativeEndian.PutUint32(sysex[16:], 56)
	raw = append(raw, sysex...)
	raw = append(raw, make([]byte, 56)...)
	raw = append(raw, ev(evNoteOn, 0, 2, 99, 100)...)
	got, err := parseNotes(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{{Channel: 0, Note: 30, On: true}, {Channel: 1, Note: 30}, {Channel: 0, Note: 27}, {Channel: 2, Note: 99, On: true}}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := parseNotes(raw[:seqEventSize-1]); err == nil {
		t.Error("a short event was accepted")
	}
}

// writer is a second sequencer client that plays the controller's role: it writes note events
// to the kernel's Midi Through port, which hands them to every subscriber.
type writer struct {
	fd, client, port int
}

func newWriter(t *testing.T) *writer {
	t.Helper()
	fd, err := unix.Open(seqDevice, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no ALSA sequencer: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	var id int32
	if err := seqIoctl(fd, ioctlClientID, unsafe.Pointer(&id)); err != nil {
		t.Fatal(err)
	}
	port := seqPortInfo{Addr: seqAddr{Client: uint8(id)}, Capability: capRead | capWrite | capSubsRead | capSubsWrite, Type: 1<<1 | 1<<20, MIDIChannels: 16}
	copy(port.Name[:], "test writer")
	if err := seqIoctl(fd, ioctlCreatePort, unsafe.Pointer(&port)); err != nil {
		t.Fatal(err)
	}
	return &writer{fd: fd, client: int(id), port: int(port.Addr.Port)}
}

func (w *writer) note(t *testing.T, to Port, typ uint8, channel, note, velocity byte) {
	t.Helper()
	b := make([]byte, seqEventSize)
	b[0] = typ
	b[3] = 253 // SNDRV_SEQ_QUEUE_DIRECT
	b[12], b[13] = byte(w.client), byte(w.port)
	b[14], b[15] = byte(to.Client), byte(to.Port)
	b[16], b[17], b[18] = channel, note, velocity
	if _, err := unix.Write(w.fd, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func seqClients(t *testing.T) string {
	b, err := os.ReadFile("/proc/asound/seq/clients")
	if err != nil {
		t.Skipf("no /proc/asound/seq/clients: %v", err)
	}
	return string(b)
}

// The kernel path, end to end, on the sequencer's own loopback port (Midi Through): the real
// ioctl layouts, a real subscription, real events, and the unsubscribe on Close. No controller
// and no Engine are involved; nothing outside this test's two clients is touched.
func TestKernelSourceLearnsFromARealSubscription(t *testing.T) {
	if !strings.Contains(seqClients(t), `"Midi Through"`) {
		t.Skip("no Midi Through client (snd-seq-dummy)")
	}
	w := newWriter(t)
	src, err := OpenSource()
	if err != nil {
		t.Skipf("cannot open the sequencer: %v", err)
	}
	through := Port{Client: 14, Port: 0}
	found := false
	ports, err := src.Ports()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ports {
		if p.Client == 14 && p.Port == 0 {
			found = true
		}
		if strings.HasPrefix(p.ClientName, "EngineX-Button-Learn") {
			t.Errorf("Ports lists the learn client itself: %+v", p)
		}
	}
	if !found {
		src.Close()
		t.Skip("Midi Through has no readable port 0")
	}
	if strings.Contains(seqClients(t), "EngineX-Button-Learn") && strings.Contains(seqClients(t), "Connected From: 14:0") {
		t.Fatal("nothing may be subscribed before Subscribe")
	}
	if err := src.Subscribe(through); err != nil {
		t.Fatal(err)
	}
	if text := seqClients(t); !strings.Contains(text, "EngineX-Button-Learn") || !strings.Contains(text, "Connected From: 14:0") {
		t.Fatalf("the subscription is not in the kernel's table:\n%s", text)
	}
	l, err := NewLearner("JP07")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		w.note(t, through, evNoteOn, 0, noteShift, 127)
		time.Sleep(20 * time.Millisecond)
		w.note(t, through, evNoteOn, 0, noteLoop, 100)
		w.note(t, through, evNoteOff, 0, noteLoop, 0)
	}()
	r, err := learnLoop(context.Background(), src, l, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if r.Gesture != GestureShift || r.Control != "PadModeSelect:30" || !r.Offer.Mappable {
		t.Fatalf("%+v", r)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seqClients(t), "EngineX-Button-Learn") {
		t.Fatal("the learn client is still in the kernel's table after Close")
	}
	if err := src.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

func TestKernelSourceHoldAndDoubleOnTheRealPath(t *testing.T) {
	if !strings.Contains(seqClients(t), `"Midi Through"`) {
		t.Skip("no Midi Through client (snd-seq-dummy)")
	}
	through := Port{Client: 14, Port: 0}
	for name, play := range map[string]func(*writer, *testing.T){
		"hold": func(w *writer, t *testing.T) { w.note(t, through, evNoteOn, 0, noteHotCue, 90) },
		"double": func(w *writer, t *testing.T) {
			w.note(t, through, evNoteOn, 0, noteHotCue, 90)
			w.note(t, through, evNoteOff, 0, noteHotCue, 0)
			time.Sleep(100 * time.Millisecond)
			w.note(t, through, evNoteOn, 0, noteHotCue, 90)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWriter(t)
			src, err := OpenSource()
			if err != nil {
				t.Skipf("cannot open the sequencer: %v", err)
			}
			defer src.Close()
			if err := src.Subscribe(through); err != nil {
				t.Skipf("cannot subscribe to Midi Through: %v", err)
			}
			l, _ := NewLearner("JP07")
			go func() { time.Sleep(40 * time.Millisecond); play(w, t) }()
			r, err := learnLoop(context.Background(), src, l, 3*time.Second)
			if err != nil || r.Gesture != name || r.Control != "PadModeSelect:27" {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}
