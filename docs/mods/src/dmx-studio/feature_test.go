package dmxstudio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

func seq(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestPacketWireLayout(t *testing.T) {
	frame := append(seq(256), seq(256)...)
	art := ArtNet(2, 7, frame)
	if len(art) != 530 || string(art[:8]) != "Art-Net\x00" || binary.LittleEndian.Uint16(art[14:]) != 1 ||
		binary.BigEndian.Uint16(art[16:]) != 512 || !bytes.Equal(art[18:], frame) {
		t.Fatalf("Art-Net layout")
	}
	acn := SACN(513, 255, frame, seq(16), false)
	if len(acn) != 638 || !bytes.Equal(acn[22:38], seq(16)) {
		t.Fatalf("sACN size/cid")
	}
	for _, c := range []struct{ off, length int }{{16, 622}, {38, 600}, {115, 523}} {
		if int(binary.BigEndian.Uint16(acn[c.off:])&4095) != c.length {
			t.Fatalf("sACN length at %d", c.off)
		}
	}
	if acn[111] != 255 || binary.BigEndian.Uint16(acn[113:]) != 513 || binary.BigEndian.Uint16(acn[123:]) != 513 || !bytes.Equal(acn[126:], frame) {
		t.Fatalf("sACN fields")
	}
	if SACN(1, 1, frame, make([]byte, 16), true)[112] != 0x40 {
		t.Fatalf("terminated flag")
	}
}

type fake struct {
	broken bool
	calls  *[][]byte
}

func (f fake) send(b []byte) error {
	*f.calls = append(*f.calls, append([]byte{}, b...))
	if f.broken {
		return errors.New("disconnected")
	}
	return nil
}
func (f fake) close() {}

func n(v float64) any { return v }

func TestPatchEffectsSmokeAndBlackout(t *testing.T) {
	app, err := NewFeature(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	patch := []any{map[string]any{"profile": "Moving Head 3-in-1", "universe": n(1), "address": n(1)},
		map[string]any{"profile": "Smoke", "universe": n(1), "address": n(20)}}
	if _, err := app.Action(map[string]any{"action": "configure", "config": map[string]any{"patch": patch,
		"intensity": n(200), "smokeLevel": n(180), "smokeArmed": true, "smokeDuration": n(0)}}); err != nil {
		t.Fatal(err)
	}
	app.Running = true
	app.FlashUntil = 100
	if app.Render(99)[1][19] != 180 || app.Render(101)[1][19] != 0 || app.Render(101)[1][4] != 200 {
		t.Fatalf("smoke flash / intensity")
	}
	for _, effect := range []string{"Circle", "8-Figure", "Pan-Tilt Sweep"} {
		app.Config["effect"] = effect
		if bytes.Equal(app.Render(0)[1][:4], app.Render(0.5)[1][:4]) {
			t.Fatalf("%s does not move", effect)
		}
	}
	_, err = app.Validated(map[string]any{"patch": append(append([]any{}, patch...), map[string]any{"profile": "Smoke", "universe": n(1), "address": n(1)})})
	if !modkit.IsBad(err) {
		t.Fatalf("overlap not rejected: %v", err)
	}
	var calls [][]byte
	app.Routes = []route{{output: map[string]any{"protocol": "ftdi", "universe": n(1)}, adapter: fake{true, &calls}},
		{output: map[string]any{"protocol": "ftdi", "universe": n(2)}, adapter: fake{false, &calls}}}
	app.Blackout()
	if len(calls) != 6 {
		t.Fatalf("blackout frames %d", len(calls))
	}
	for _, c := range calls {
		if !bytes.Equal(c, make([]byte, 512)) {
			t.Fatalf("non-zero blackout frame")
		}
	}
	if app.Config["smokeArmed"] != false || app.Running {
		t.Fatalf("blackout must disarm smoke and stop")
	}
}

func TestValidationRejects(t *testing.T) {
	app, _ := NewFeature(t.TempDir())
	for name, cfg := range map[string]map[string]any{
		"efx":      {"effect": "Spiral"},
		"bpm":      {"bpm": n(10)},
		"smoke":    {"smokeDuration": n(40), "smokeInterval": n(30)},
		"armed":    {"smokeArmed": "yes"},
		"protocol": {"outputs": []any{map[string]any{"protocol": "dmx", "universe": n(1)}}},
		"host":     {"outputs": []any{map[string]any{"protocol": "artnet", "universe": n(1), "host": "::1"}}},
		"sacnU":    {"outputs": []any{map[string]any{"protocol": "sacn", "universe": n(64000), "host": "10.0.0.1"}}},
		"tty":      {"outputs": []any{map[string]any{"protocol": "ftdi", "universe": n(1), "device": "/dev/sda"}}},
		"sameUSB": {"outputs": []any{map[string]any{"protocol": "udmx", "universe": n(1), "bus": n(1), "address": n(2)},
			map[string]any{"protocol": "udmx", "universe": n(2), "bus": n(1), "address": n(2)}}},
		"address": {"patch": []any{map[string]any{"profile": "LED Par", "universe": n(1), "address": n(510)}}},
	} {
		if _, err := app.Validated(cfg); !modkit.IsBad(err) {
			t.Errorf("%s: expected a 400, got %v", name, err)
		}
	}
}

func TestStructSizes(t *testing.T) {
	// usbdevfs_ctrltransfer is 16 bytes on 32-bit and 24 on 64-bit kernels' ABI.
	want := map[uintptr]uintptr{4: 16, 8: 24}[ptrSize]
	if got := sizeofCtrl; got != want {
		t.Fatalf("usbdevfs_ctrltransfer size %d, want %d", got, want)
	}
}
