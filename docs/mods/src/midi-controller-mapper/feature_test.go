package midimapper

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

// No MIDI hardware, scheduling changes, or background goroutine is needed for
// the Python regression cases. Their explicit timestamps remain unchanged.
func testMapper(t *testing.T) *mapper {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	f, err := newMapper(modkit.Env{State: t.TempDir(), ModDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	f.backend = &fakeSequencer{}
	f.workerAlive = true
	f.openSequencer = func() (sequencer, error) { return &fakeSequencer{}, nil }
	t.Cleanup(f.Close)
	return f
}
func action(t *testing.T, f *mapper, req object) object {
	t.Helper()
	v, err := f.Action(req)
	if err != nil {
		t.Fatal(err)
	}
	return obj(v)
}
func status(t *testing.T, f *mapper) object {
	t.Helper()
	v, err := f.Status()
	if err != nil {
		t.Fatal(err)
	}
	return obj(v)
}
func poll(t *testing.T, f *mapper, now float64) object {
	t.Helper()
	v, err := f.pollBridgeLocked(nil, nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func values(batch object) object { return obj(batch["values"]) }
func near(t *testing.T, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("got %.9f, want %.9f", got, want)
	}
}
func mustBool(t *testing.T, v any, want bool) {
	t.Helper()
	if v != want {
		t.Fatalf("got %v, want %v", v, want)
	}
}
func cloneObject(t *testing.T, p object) object {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := modkit.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPresetValidationAndCustomPresets(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(`{"targetDeck":"four","enabled":"yes","presetId":123}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := newMapper(modkit.Env{State: state, ModDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	st := status(t, f)
	if st["presetId"] != "pioneer-cdj-400" || st["targetDeck"] != 3 || st["enabled"] != false {
		t.Fatalf("corrupt config defaults: %v", st)
	}
	mustBool(t, st["ledFeedbackSupported"], false)
	if !strings.Contains(str(st["ledFeedbackNote"]), "transmit-only") {
		t.Fatal(st["ledFeedbackNote"])
	}
	custom := cloneObject(t, f.currentPreset())
	custom["id"] = "custom.cdj.400"
	custom["name"] = "Custom CDJ 400"
	action(t, f, object{"action": "save_preset", "preset": custom})
	st = action(t, f, object{"action": "configure", "presetId": "custom.cdj.400", "targetDeck": 4})
	if st["presetId"] != "custom.cdj.400" || st["targetDeck"] != 4 {
		t.Fatal(st)
	}
	if err := os.WriteFile(filepath.Join(f.userPresetsDir(), "broken.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st = action(t, f, object{"action": "refresh_devices"})
	if !strings.Contains(strings.Join(st["presetErrors"].([]string), "\n"), "broken.json") {
		t.Fatal(st["presetErrors"])
	}
	if _, err := f.Action(object{"action": "inject_midi", "messages": []any{[]any{0x90, 0, 127}}}); err == nil || !modkit.IsBad(err) {
		t.Fatalf("production test backdoor accepted: %v", err)
	}
	tests := []struct {
		name  string
		patch object
		want  string
	}{
		{"traversal", object{"id": "../escape"}, "invalid preset id"},
		{"double dots", object{"id": "bad..dots"}, "invalid preset id"},
		{"object id", object{"id": object{"nested": 1}}, "invalid preset id"},
		{"jog encoding", object{"jog": object{"encoding": "unknown_mode"}}, "unsupported jog encoding 'unknown_mode'"},
		{"boolean touch", object{"jog": object{"touchNote": true}}, "jog.touchNote must be an integer"},
		{"boolean cc", object{"tempoSlider": object{"cc": false}}, "tempoSlider.cc must be an integer"},
		{"low malformed limit", object{"malformedLimit": 0}, "malformedLimit must be >= 1"},
		{"boolean malformed limit", object{"malformedLimit": true}, "malformedLimit must be an integer"},
		{"huge storm limit", object{"stormLimitPerSec": int64(1000000000000)}, "stormLimitPerSec must be <= 100000"},
		{"pattern string", object{"match": object{"alsaClientPatterns": "CDJ"}}, "list of non-empty strings"},
		{"numeric pattern", object{"match": object{"alsaClientPatterns": []any{123}}}, "list of non-empty strings"},
		{"usb string", object{"match": object{"usbIds": "08e4:0143"}}, "list of 'vvvv:pppp' hex strings"},
		{"bad usb", object{"match": object{"usbIds": []any{"not-a-usb-id"}}}, "list of 'vvvv:pppp' hex strings"},
		{"note range", object{"controls": object{"x": object{"type": "note", "number": 200, "action": "toggle_play"}}}, "number must be <= 127"},
		{"boolean note", object{"controls": object{"x": object{"type": "note", "number": true, "action": "toggle_play"}}}, "number must be an integer"},
		{"unknown control action", object{"controls": object{"x": object{"type": "note", "number": 1, "action": "launch_missiles"}}}, "unknown action 'launch_missiles'"},
		{"float channel", object{"midiChannel": json.Number("1.0")}, "midiChannel must be an integer"},
		{"boolean sensitivity", object{"jog": object{"scratchSensitivitySecPerSec": true}}, "must be a number"},
		{"low pickup", object{"tempoSlider": object{"pickupThreshold": 0.0001}}, "pickupThreshold must be >= 0.001"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := cloneObject(t, custom)
			for k, v := range tc.patch {
				p[k] = v
			}
			_, err := validatePreset(p)
			if err == nil || !modkit.IsBad(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := validatePreset(object{"id": "bad"}); err == nil || err.Error() != "preset missing required field 'name'" {
		t.Fatal(err)
	}
	// A saved user file is retained, but must not override the packaged preset.
	builtin := cloneObject(t, f.presets["pioneer-cdj-400"])
	builtin["name"] = "Override"
	st = action(t, f, object{"action": "save_preset", "preset": builtin})
	if f.presets["pioneer-cdj-400"]["name"] != "Pioneer CDJ-400" || !strings.Contains(strings.Join(st["presetErrors"].([]string), "\n"), "cannot override built-in") {
		t.Fatal(st)
	}
}

func TestCDJ400ControlsAndVelocityIntegration(t *testing.T) {
	f := testMapper(t)
	action(t, f, object{"action": "enable"})
	if err := f.updateDeckStateLocked(object{"songLoaded": true, "playing": false, "posNorm": 0.25, "durationSec": 180.0, "speed": 0.5}); err != nil {
		t.Fatal(err)
	}
	f.processMIDILocked([]int{0x90, 1, 127}, 10)
	mustBool(t, values(poll(t, f, 10.01))["cueHold"], true)
	f.processMIDILocked([]int{0x90, 1, 0}, 10.02)
	mustBool(t, values(poll(t, f, 10.03))["cueHold"], false)
	tests := []struct {
		note   int
		action string
	}{
		{0x00, "toggle_play"}, {0x02, "seek_forward"}, {0x03, "seek_backward"}, {0x04, "track_next"}, {0x05, "track_prev"},
		{0x06, "loop_in"}, {0x07, "loop_out"}, {0x08, "reloop_exit"}, {0x10, "cycle_tempo_range"}, {0x11, "toggle_keylock"},
		{0x16, "loop_halve"}, {0x17, "loop_double"}, {0x22, "toggle_sync"}, {0x32, "toggle_browse_view"}, {0x33, "load_selected_track"},
	}
	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			f.processMIDILocked([]int{0x90, tc.note, 127}, 11)
			batch := poll(t, f, 11.005)
			if !reflect.DeepEqual(batch["actions"], []object{{"action": tc.action}}) {
				t.Fatal(batch)
			}
		})
	}
	f.processMIDILocked([]int{0xb0, 0x4f, 67}, 11.01)
	batch := poll(t, f, 11.02)
	if !reflect.DeepEqual(batch["actions"], []object{{"action": "browse_scroll", "steps": 3}}) {
		t.Fatal(batch)
	}
	f.processMIDILocked([]int{0x90, 0x20, 127}, 20)
	f.processMIDILocked([]int{0xb0, 0x10, 127}, 20)
	near(t, f.integrateJogLocked(nil, 20.35), 0.63, 1e-4)
	f.processMIDILocked([]int{0xb0, 0x10, 64}, 20.35)
	if f.jogVelocity != 0 || f.integrateJogLocked(nil, 20.60) != 0 {
		t.Fatal("64 did not stop jog")
	}
	ack := batch["seq"].(int64)
	apply := 1.5
	jogBatch, err := f.pollBridgeLocked(nil, &ack, &apply, 20.61)
	if err != nil {
		t.Fatal(err)
	}
	near(t, number(values(jogBatch)["jogMoveTicks"]), round(0.63*26325, 2), 0.001)
	if number(values(jogBatch)["setPosNorm"]) <= 0.25 {
		t.Fatal(jogBatch)
	}
	f.processMIDILocked([]int{0xb0, 0x30, 96}, 22)
	pb := values(poll(t, f, 22.01))
	if number(pb["nudgeFwd"]) <= 0.3 || number(pb["nudgeBwd"]) != 0 {
		t.Fatal(pb)
	}
	hold := values(poll(t, f, 22.31))
	if _, ok := hold["nudgeFwd"]; ok || f.pitchBendDir != 1 {
		t.Fatal("pitch bend expired without 64 stop", hold)
	}
	f.processMIDILocked([]int{0xb0, 0x30, 64}, 22.40)
	near(t, number(values(poll(t, f, 22.41))["nudgeFwd"]), 0, 0)
	// Vinyl mode is an immediate property, never a queued transport action.
	f.processMIDILocked([]int{0x90, 0x12, 127}, 23)
	vinyl := poll(t, f, 23.01)
	mustBool(t, values(vinyl)["vinylMode"], false)
	f.processMIDILocked([]int{0x80, 0x12, 64}, 23.02)
	if len(poll(t, f, 23.03)["actions"].([]object)) != 0 {
		t.Fatal("note-off triggered transport")
	}
}

func TestSoftTakeoverAndSafetyAutoDisable(t *testing.T) {
	f := testMapper(t)
	action(t, f, object{"action": "enable"})
	_ = f.updateDeckStateLocked(object{"speed": 0.60})
	f.processMIDILocked([]int{0xb0, 0x1d, 102}, 29)
	initial := values(poll(t, f, 29.01))
	if f.softLocked {
		t.Fatal("unseen fader crossed from default position")
	}
	if _, ok := initial["speed"]; ok {
		t.Fatal(initial)
	}
	mustBool(t, initial["speedOffsetUp"], false)
	mustBool(t, initial["speedOffsetDown"], true)
	_ = f.updateDeckStateLocked(object{"speed": 0.70})
	f.softLocked = false
	f.faderNorm = 32.0 / 127
	f.processMIDILocked([]int{0xb0, 0x1d, 33}, 30)
	wait := values(poll(t, f, 30.01))
	if f.softLocked {
		t.Fatal("locked outside pickup")
	}
	if _, ok := wait["speed"]; ok {
		t.Fatal(wait)
	}
	mustBool(t, wait["speedOffsetUp"], true)
	mustBool(t, wait["speedOffsetDown"], false)
	f.processMIDILocked([]int{0xb0, 0x1d, 90}, 30.05)
	locked := values(poll(t, f, 30.06))
	if !f.softLocked {
		t.Fatal("sweep did not pick up")
	}
	near(t, number(locked["speed"]), 90.0/127, 1e-4)
	mustBool(t, locked["speedOffsetUp"], false)
	mustBool(t, locked["speedOffsetDown"], false)
	_ = f.updateDeckStateLocked(object{"speed": 0.40})
	if f.softLocked || !f.speedOffsetDown {
		t.Fatal("external speed did not immediately unlock")
	}
	f.processMIDILocked([]int{0xb0, 0x1d, 89}, 30.07)
	still := values(poll(t, f, 30.08))
	if f.softLocked {
		t.Fatal("incorrect repickup")
	}
	if _, ok := still["speed"]; ok {
		t.Fatal(still)
	}
	mustBool(t, still["speedOffsetDown"], true)
	fake := f.backend.(*fakeSequencer)
	fake.devices = []midiDevice{{ID: "128:0", Client: 128, ClientName: "SandboxTwist", PortName: "SandboxTwist Port 0", Label: "SandboxTwist [128:0]"}}
	if f.tryConnectDeviceLocked() || f.connected {
		t.Fatal("auto selected unrelated port")
	}
	if !strings.Contains(str(status(t, f)["matchHint"]), "SandboxTwist") {
		t.Fatal("missing unmatched hint")
	}
	fake.devices = []midiDevice{{ID: "24:0", Client: 24, ClientName: "USB Audio MIDI", PortName: "MIDI 1", USBID: "08e4:0143", Label: "USB Audio MIDI [24:0]"}}
	if !f.tryConnectDeviceLocked() || f.connectedClient != 24 || status(t, f)["matchHint"] != "" {
		t.Fatal("USB match failed")
	}
	f.connected = true
	f.connectedClient = 20
	f.connectedPort = 0
	f.connectedName = "Pioneer CDJ-400 [20:0]"
	if f.simulateDisconnectLocked(99, -1) || !f.enabled || f.simulateDisconnectLocked(20, 1) {
		t.Fatal("unrelated exit disabled mapper")
	}
	f.processMIDILocked([]int{0xb0, 0x30, 96}, 31)
	if !f.simulateDisconnectLocked(20, -1) || f.enabled || !strings.Contains(f.errorText, "disconnected") {
		t.Fatal("hot unplug did not disable", f.errorText)
	}
	flush := poll(t, f, 31.01)
	mustBool(t, flush["enabled"], false)
	near(t, number(values(flush)["nudgeFwd"]), 0, 0)
	mustBool(t, values(flush)["cueHold"], false)
	action(t, f, object{"action": "enable"})
	for i := 0; i < 16; i++ {
		f.processMIDILocked([]int{0x10, 0}, 40)
	}
	if f.enabled || !strings.Contains(f.errorText, "malformed") {
		t.Fatal("malformed threshold not enforced")
	}
	restarted := action(t, f, object{"action": "restart_worker"})
	if restarted["error"] != "" {
		t.Fatal(restarted["error"])
	}
}

func TestTempoEchoAndNeutral(t *testing.T) {
	f := testMapper(t)
	f.enabled = true
	_ = f.updateDeckStateLocked(object{"speed": 0.70})
	f.processMIDILocked([]int{0xb0, 29, 89}, 10)
	if !f.softLocked {
		t.Fatal("pickup failed")
	}
	speed := number(f.pendingValues["speed"])
	_ = f.updateDeckStateLocked(object{"speed": 0.70})
	near(t, number(f.pendingValues["speed"]), speed, 0)
	poll(t, f, 10.01)
	_ = f.updateDeckStateLocked(object{"speed": 0.4})
	if f.softLocked {
		t.Fatal("external speed incorrectly treated as queued echo")
	}
	f.processMIDILocked([]int{0xb0, 29, 64}, 11)
	if f.softLocked {
		t.Fatal("center must not bypass takeover")
	}
	_ = f.updateDeckStateLocked(object{"speed": 0.5})
	f.processMIDILocked([]int{0xb0, 29, 64}, 12)
	neutral := values(poll(t, f, 12.01))
	near(t, number(neutral["speed"]), 0.5, 0)
	mustBool(t, neutral["speedNeutral"], true)
	cfg := obj(f.currentPreset()["tempoSlider"])
	cfg["softTakeover"] = false
	cfg["invert"] = true
	f.processMIDILocked([]int{0xb0, 29, 127}, 13)
	near(t, number(values(poll(t, f, 13.01))["speed"]), 0, 0)
}

func TestRelativeCCAndJogModes(t *testing.T) {
	for _, tc := range []struct {
		encoding    string
		value, want int
	}{
		{"relative_center_64", 67, 3}, {"relative_center_64", 64, 0}, {"relative_center_64", 0, -64},
		{"relative_2s_complement", 63, 63}, {"relative_2s_complement", 64, -64}, {"relative_2s_complement", 127, -1},
		{"relative_sign_magnitude", 65, -1}, {"relative_sign_magnitude", 63, 63}, {"relative_sign_magnitude", 64, 0},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.encoding, tc.value), func(t *testing.T) {
			if got := decodeRelativeCC(tc.value, tc.encoding, 64); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
	for _, encoding := range []string{"relative_center_64", "relative_2s_complement", "relative_sign_magnitude"} {
		t.Run(encoding, func(t *testing.T) {
			f := testMapper(t)
			f.enabled = true
			jog := obj(f.currentPreset()["jog"])
			jog["encoding"] = encoding
			value := 65
			if encoding != "relative_center_64" {
				value = 1
			}
			f.processMIDILocked([]int{0xb0, 16, value}, 10)
			near(t, number(f.pendingValues["jogMoveTicks"]), round(1.0/63*0.016*1.8*26325, 2), 0.001)
		})
	}
	t.Run("playing platter bends until touched", func(t *testing.T) {
		f := testMapper(t)
		f.enabled = true
		_ = f.updateDeckStateLocked(object{"playing": true, "posNorm": 0.5, "durationSec": 180})
		f.processMIDILocked([]int{0xb0, 16, 1}, 10)
		if delta := f.integrateJogLocked(nil, 10.25); delta != 0 || f.pitchBendDir != -1 {
			t.Fatal("playing jog must bend")
		}
		near(t, number(f.pendingValues["nudgeBwd"]), 0.65, 0)
		f.processMIDILocked([]int{0x90, 32, 127}, 11)
		near(t, f.integrateJogLocked(nil, 11.25), -0.45, 1e-6)
		f.processMIDILocked([]int{0x80, 32, 0}, 11.3)
		if f.jogTouched || f.jogVelocity != 0 {
			t.Fatal("touch release did not stop integration")
		}
	})
	t.Run("dt cap and position bounds", func(t *testing.T) {
		f := testMapper(t)
		f.enabled = true
		_ = f.updateDeckStateLocked(object{"posNorm": 0.999, "durationSec": 0})
		f.processMIDILocked([]int{0xb0, 16, 127}, 10)
		near(t, f.integrateJogLocked(nil, 20), 0.9, 1e-6)
		near(t, f.virtualPosNorm, 1, 0)
		f.applyJogDeltaLocked(-999, 21)
		near(t, f.virtualPosNorm, 0, 0)
	})
}

func TestStormMalformedAndChannelFiltering(t *testing.T) {
	f := testMapper(t)
	f.enabled = true
	p := f.currentPreset()
	p["stormLimitPerSec"] = 10
	f.rateWindowStart = 10
	for i := 0; i < 10; i++ {
		f.processMIDILocked([]int{0x90, 0, 127}, 10.01)
	}
	if !f.enabled || f.eventCount != 10 {
		t.Fatal("storm limit prematurely triggered")
	}
	f.processMIDILocked([]int{0x90, 0, 127}, 10.02)
	if f.enabled || !strings.Contains(f.errorText, ">10 msg/s") {
		t.Fatal("storm did not auto disable")
	}
	b := poll(t, f, 10.03)
	if len(b["actions"].([]object)) != 0 {
		t.Fatal("disabled batch dispatched actions")
	}
	mustBool(t, values(b)["jogTouch"], false)
	f.enabled = true
	f.rateWindowEvents = 0
	p["midiChannel"] = 1
	if f.processMIDILocked([]int{0x90, 0, 127}, 12) || f.eventCount != 10 {
		t.Fatal("wrong channel processed")
	}
	if !f.processMIDILocked([]int{0x91, 0, 127}, 12.01) || f.eventCount != 11 {
		t.Fatal("configured channel not processed")
	}
	for _, raw := range [][]int{nil, {0x90}, {0x90, 0, 127, 1}, {0x7f, 0}, {0x90, -1}, {0x90, 0, 128}, {0xf0, 0}} {
		f.processMIDILocked(raw, 13)
	}
	if f.malformedCount != 7 {
		t.Fatal(f.malformedCount)
	}
	action(t, f, object{"action": "reset_telemetry"})
	if f.malformedCount != 0 || f.eventCount != 0 {
		t.Fatal("reset did not clear counts")
	}
}

func TestBridgeLatencyAndActionContract(t *testing.T) {
	f := testMapper(t)
	f.enabled = true
	f.now = func() float64 { return 10 }
	f.processMIDILocked([]int{0x90, 0, 127}, 10)
	f.processMIDILocked([]int{0xb0, 29, 64}, 10.01)
	first := poll(t, f, 10.02)
	if first["seq"] != int64(1) || first["targetDeck"] != 3 {
		t.Fatal(first)
	}
	ack := int64(1)
	apply := 1.5
	b, err := f.pollBridgeLocked(nil, &ack, &apply, 10.03)
	if err != nil {
		t.Fatal(err)
	}
	if b["seq"] != int64(0) {
		t.Fatal(b)
	}
	summary := f.latencySummaryLocked()
	near(t, number(summary["p50Ms"]), 11.5, 0.001)
	_, _ = f.pollBridgeLocked(nil, &ack, &apply, 10.04)
	if len(f.latencySamples) != 1 {
		t.Fatal("duplicate ack sampled twice")
	}
	for i := 0; i < 510; i++ {
		seq := int64(i + 2)
		f.inflightBatches[seq] = 1000
		_, _ = f.pollBridgeLocked(nil, &seq, &apply, 11)
	}
	if len(f.latencySamples) != 500 {
		t.Fatal(len(f.latencySamples))
	}
	near(t, number(f.latencySummaryLocked()["maxMs"]), 250, 0)
	for i := 0; i < 65; i++ {
		f.inflightBatches[int64(1000+i)] = 1
	}
	_, _ = f.pollBridgeLocked(nil, &ack, nil, 12)
	if len(f.inflightBatches) != 0 {
		t.Fatal("stale inflight not bounded on ack")
	}
	action(t, f, object{"action": "reset_telemetry"})
	if len(f.latencySamples) != 0 || len(f.inflightBatches) != 0 {
		t.Fatal("latency reset failed")
	}
	disabled := action(t, f, object{"action": "disable"})
	mustBool(t, disabled["enabled"], false)
	flush := action(t, f, object{"action": "poll", "deckState": object{"speed": 0.4}, "ackSeq": json.Number("0"), "ackApplyMs": json.Number("2.5")})
	mustBool(t, values(flush)["cueHold"], false)
	for _, req := range []object{
		{"action": "configure", "targetDeck": true}, {"action": "configure", "targetDeck": json.Number("1.0")}, {"action": "configure", "targetDeck": 0}, {"action": "configure", "targetDeck": 5},
		{"action": "configure", "presetId": 1}, {"action": "configure", "presetId": "missing"}, {"action": "configure", "selectedDevice": false}, {"action": "configure", "enabled": "yes"},
		{"action": "poll", "ackSeq": "bad"}, {"action": "poll", "deckState": object{"speed": "not a number"}}, {"action": "poll", "ackApplyMs": "nan"}, {"action": "missing"},
	} {
		if _, err := f.Action(req); err == nil || !modkit.IsBad(err) {
			t.Fatalf("accepted invalid action %v: %v", req, err)
		}
	}
	action(t, f, object{"action": "configure", "targetDeck": 4, "enabled": false, "selectedDevice": "auto"})
	var config object
	if err := modkit.LoadJSON(filepath.Join(f.env.State, "config.json"), &config); err != nil {
		t.Fatal(err)
	}
	if number(config["targetDeck"]) != 4 || config["enabled"] != false || config["selectedDevice"] != "auto" {
		t.Fatal(config)
	}
	if _, err := json.Marshal(status(t, f)); err != nil {
		t.Fatal("unencodable status", err)
	}
}

type fakeSequencer struct {
	devices      []midiDevice
	subscribeErr error
	waitErr      error
	readErr      error
	events       chan []inputEvent
	readMu       sync.Mutex
	pending      []inputEvent
}

func (s *fakeSequencer) Devices() ([]midiDevice, error)     { return s.devices, nil }
func (s *fakeSequencer) Subscribe(client, port int) error   { return s.subscribeErr }
func (s *fakeSequencer) Unsubscribe(client, port int) error { return nil }
func (s *fakeSequencer) Close() error                       { return nil }
func (s *fakeSequencer) Wait(ms int) error {
	if s.waitErr != nil {
		return s.waitErr
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case events := <-s.events:
		s.readMu.Lock()
		s.pending = events
		s.readMu.Unlock()
	case <-timer.C:
	}
	return nil
}
func (s *fakeSequencer) Read() ([]inputEvent, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	events := s.pending
	s.pending = nil
	return events, s.readErr
}

func TestWorkerFailureAndUnavailableALSA(t *testing.T) {
	t.Run("unavailable still serves status", func(t *testing.T) {
		f := testMapper(t)
		f.backend = nil
		f.openSequencer = func() (sequencer, error) { return nil, os.ErrNotExist }
		f.startWorkerLocked()
		st := status(t, f)
		mustBool(t, st["alsaAvailable"], false)
		mustBool(t, st["threadAlive"], false)
		if !strings.Contains(str(st["error"]), "ALSA sequencer unavailable") {
			t.Fatal(st["error"])
		}
		st = action(t, f, object{"action": "enable"})
		mustBool(t, st["enabled"], true)
		mustBool(t, st["running"], false)
	})
	for _, kind := range []string{"poll", "read"} {
		t.Run(kind, func(t *testing.T) {
			f := testMapper(t)
			f.backend = nil
			fake := &fakeSequencer{events: make(chan []inputEvent, 1)}
			failure := errors.New("device failed")
			if kind == "poll" {
				fake.waitErr = failure
			} else {
				fake.readErr = failure
				fake.events <- nil
			}
			f.openSequencer = func() (sequencer, error) { return fake, nil }
			f.enabled = true
			f.mu.Lock()
			f.startWorkerLocked()
			done := f.done
			f.mu.Unlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("worker did not stop on failure")
			}
			st := status(t, f)
			mustBool(t, st["enabled"], false)
			mustBool(t, st["threadAlive"], false)
			if !strings.Contains(str(st["threadError"]), "device failed") {
				t.Fatal(st)
			}
			f.openSequencer = func() (sequencer, error) { return &fakeSequencer{}, nil }
			action(t, f, object{"action": "restart_worker"})
		})
	}
}

func TestPresetControlsFollowDeclarationOrder(t *testing.T) {
	for _, kind := range []string{"note", "cc"} {
		t.Run(kind, func(t *testing.T) {
			f := testMapper(t)
			first, second := `{"type":"note","number":0,"action":"toggle_play"}`, `{"type":"note","number":0,"action":"toggle_sync"}`
			message, want := []int{0x90, 0, 127}, []object{{"action": "toggle_play"}}
			if kind == "cc" {
				first = `{"type":"cc","number":79,"action":"browse_scroll","encoding":"relative_2s_complement"}`
				second = `{"type":"cc","number":79,"action":"browse_scroll","encoding":"relative_center_64"}`
				message = []int{0xb0, 79, 65}
				want = []object{{"action": "browse_scroll", "steps": -63}}
			}
			raw := fmt.Sprintf(`{"id":"ordered-%s","name":"Declaration order","jog":{},"tempoSlider":{},"controls":{"zeta":%s,"alpha":%s}}`, kind, first, second)
			if err := os.WriteFile(filepath.Join(f.userPresetsDir(), "ordered.json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			action(t, f, object{"action": "configure", "presetId": "ordered-" + kind})
			f.enabled = true
			if !f.processMIDILocked(message, 10) {
				t.Fatal("control not dispatched")
			}
			if batch := poll(t, f, 10.01); !reflect.DeepEqual(batch["actions"], want) {
				t.Fatalf("first-declared zeta must win: got %v, want %v", batch["actions"], want)
			}
		})
	}
}

func TestPresetReportsFirstDeclaredInvalidControl(t *testing.T) {
	f := testMapper(t)
	raw := `{"id":"invalid-order","name":"Declaration order","jog":{},"tempoSlider":{},"controls":{"zeta":{"type":"note","number":200,"action":"toggle_play"},"alpha":{"type":"note","number":201,"action":"toggle_sync"}}}`
	if err := os.WriteFile(filepath.Join(f.userPresetsDir(), "invalid-order.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st := action(t, f, object{"action": "refresh_devices"})
	errs := st["presetErrors"].([]string)
	if len(errs) != 1 || errs[0] != "invalid-order.json: control 'zeta' number must be <= 127" {
		t.Fatalf("first-declared invalid control must be reported: %v", errs)
	}
	body := []byte(`{"action":"save_preset","preset":` + raw + `}`)
	req, err := modkit.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.ActionRaw(req, body)
	if err == nil || !modkit.IsBad(err) || err.Error() != "control 'zeta' number must be <= 127" {
		t.Fatalf("raw POST must report first invalid control as HTTP 400: %v", err)
	}
}

func TestSavePresetPreservesDeclarationOrderAcrossReload(t *testing.T) {
	f := testMapper(t)
	raw := []byte(`{"action":"save_preset","preset":{"id":"ordered-save","name":"Declaration order","jog":{},"tempoSlider":{},"controls":{"zeta":{"type":"note","number":0,"action":"toggle_play"},"alpha":{"type":"note","number":0,"action":"toggle_sync"}}}}`)
	req, err := modkit.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	// This fallback makes the regression fail on the original implementation's
	// observable result, rather than merely fail to compile without ActionRaw.
	var result any
	if feature, ok := any(f).(interface {
		ActionRaw(map[string]any, []byte) (any, error)
	}); ok {
		result, err = feature.ActionRaw(req, raw)
	} else {
		result, err = f.Action(req)
	}
	if err != nil {
		t.Fatalf("save: %v (%v)", err, result)
	}
	action(t, f, object{"action": "configure", "presetId": "ordered-save"})
	reloaded, err := newMapper(f.env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reloaded.Close)
	reloaded.enabled = true
	reloaded.processMIDILocked([]int{0x90, 0, 127}, 10)
	want := []object{{"action": "toggle_play"}}
	if batch := poll(t, reloaded, 10.01); !reflect.DeepEqual(batch["actions"], want) {
		t.Fatalf("save + reload lost control declaration order: got %v, want %v", batch["actions"], want)
	}
}
