// Package midimapper implements the Generic MIDI Controller Mapper helper.
package midimapper

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

type mapper struct {
	env                                          modkit.Env
	mu                                           sync.Mutex
	lifecycle                                    sync.Mutex
	presets                                      map[string]object
	controlOrders                                map[string][]string
	presetOrder                                  []string
	presetErrors                                 []string
	enabled                                      bool
	targetDeck                                   int
	presetID, selectedDevice                     string
	vinylMode                                    bool
	deckState                                    object
	jogTouched, wasPlayingBeforeTouch            bool
	jogVelocity                                  int
	lastJogStepAt, virtualPosNorm                float64
	virtualPosValid                              bool
	pitchBendDir                                 int
	pitchBendAmount, lastPitchBendAt             float64
	faderNorm                                    float64
	faderSeen, softLocked                        bool
	preWriteSpeed, lastWrittenSpeed              *float64
	lastWrittenSpeedAt                           float64
	speedOffsetUp, speedOffsetDown, speedNeutral bool
	pendingValues                                object
	pendingActions                               []object
	pendingBatchT0                               *float64
	seq                                          int64
	inflightBatches                              map[int64]float64
	errorText, matchHint                         string
	connected                                    bool
	connectedClient, connectedPort               int
	connectedName                                string
	eventCount, malformedCount                   int64
	rateWindowStart                              float64
	rateWindowEvents                             int
	eventsPerSec                                 float64
	lastMIDIHex, lastControlLabel                string
	lastDeltaPosSec                              float64
	latencySamples                               []float64
	lastPollAt                                   float64
	niceLevel                                    int
	lastCPUTime, lastWallTime, cpuPercent        float64
	backend                                      sequencer
	stop, done                                   chan struct{}
	workerAlive                                  bool
	threadError                                  string
	lastThreadHeartbeat                          float64
	openSequencer                                func() (sequencer, error)
	now                                          func() float64
}

var monotonicOrigin = time.Now()

func monotonic() float64                  { return time.Since(monotonicOrigin).Seconds() + 1 }
func round(v float64, places int) float64 { p := math.Pow10(places); return math.RoundToEven(v*p) / p }
func clamp(v, lo, hi float64) float64     { return math.Max(lo, math.Min(hi, v)) }

// New starts the low-priority sequencer worker. Missing ALSA is a status error,
// not a constructor error: the panel must remain available to diagnose it.
func New(env modkit.Env) (modkit.Feature, error) {
	f, err := newMapper(env)
	if err != nil {
		return nil, err
	}
	f.niceLevel = applyLowPriority()
	f.mu.Lock()
	f.startWorkerLocked()
	f.mu.Unlock()
	return f, nil
}

func newMapper(env modkit.Env) (*mapper, error) {
	if err := os.MkdirAll(filepath.Join(env.State, "presets"), 0o755); err != nil {
		return nil, err
	}
	f := &mapper{env: env, targetDeck: 3, selectedDevice: "auto", vinylMode: true,
		deckState: object{"songLoaded": false, "playing": false, "posNorm": 0.0, "durationSec": 0.0, "speed": 0.5, "speedRangeIndex": 0, "speedRangeCount": 4, "keylock": false, "sync": false, "loopEnabled": false, "browseOpen": false},
		faderNorm: 0.5, speedNeutral: true, pendingValues: object{}, pendingActions: []object{},
		inflightBatches: map[int64]float64{}, connectedClient: -1, connectedPort: -1, now: monotonic, openSequencer: openKernelSequencer}
	f.loadPresets()
	if _, ok := f.presets["pioneer-cdj-400"]; ok {
		f.presetID = "pioneer-cdj-400"
	} else if len(f.presetOrder) > 0 {
		f.presetID = f.presetOrder[0]
	}
	var saved object
	if modkit.LoadJSON(filepath.Join(env.State, "config.json"), &saved) == nil {
		f.enabled, _ = saved["enabled"].(bool)
		if td, err := strictInt(saved["targetDeck"], "targetDeck", 1, 4); err == nil {
			f.targetDeck = td
		}
		if pid, ok := saved["presetId"].(string); ok && f.presets[pid] != nil {
			f.presetID = pid
		}
		if dev, ok := saved["selectedDevice"].(string); ok && lenTrim(dev) > 0 {
			f.selectedDevice = dev
		}
	}
	f.vinylMode = get(obj(f.currentPreset()["jog"]), "modeDefault", "vinyl") == "vinyl"
	f.rateWindowStart = f.now()
	f.lastWallTime = f.now()
	f.lastCPUTime = processCPUTime()
	return f, nil
}

func (f *mapper) Status() (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusLocked(), nil
}
func (f *mapper) threadAliveLocked() bool {
	return f.backend != nil && f.workerAlive && (f.lastThreadHeartbeat <= 0 || f.now()-f.lastThreadHeartbeat <= 5)
}
func (f *mapper) statusLocked() object {
	now := f.now()
	cpu := processCPUTime()
	if dt := now - f.lastWallTime; dt >= 0.25 {
		f.cpuPercent = round(math.Max(0, cpu-f.lastCPUTime)/dt*100, 2)
		f.lastWallTime = now
		f.lastCPUTime = cpu
	}
	alive := f.threadAliveLocked()
	if f.backend != nil && !alive && f.stop != nil && f.errorText == "" {
		f.errorText = f.threadError
		if f.errorText == "" {
			f.errorText = "ALSA sequencer worker thread stopped unexpectedly."
		}
	}
	if f.enabled && !f.connected {
		f.tryConnectDeviceLocked()
	}
	preset := f.currentPreset()
	led := obj(preset["ledFeedback"])
	presets := make([]object, 0, len(f.presets))
	ids := make([]string, 0, len(f.presets))
	for id := range f.presets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := f.presets[id]
		presets = append(presets, object{"id": id, "name": get(p, "name", id), "model": get(p, "model", "")})
	}
	hint := ""
	if f.enabled && !f.connected {
		hint = f.matchHint
	}
	indicator := "●"
	if f.speedOffsetUp {
		indicator = "▲"
	} else if f.speedOffsetDown {
		indicator = "▼"
	}
	return object{
		"enabled": f.enabled, "running": f.enabled && f.connected, "connected": f.connected, "connectedDevice": f.connectedName, "selectedDevice": f.selectedDevice, "matchHint": hint,
		"alsaAvailable": f.backend != nil, "threadAlive": alive, "threadError": f.threadError, "devices": f.listDevicesLocked(), "targetDeck": f.targetDeck,
		"presetId": f.presetID, "presetName": get(preset, "name", f.presetID), "presets": presets, "presetErrors": append([]string{}, f.presetErrors...), "vinylMode": f.vinylMode,
		"softTakeover":         object{"locked": f.softLocked, "faderNorm": round(f.faderNorm, 4), "deckSpeedNorm": round(num(f.deckState, "speed", 0.5), 4), "speedOffsetUp": f.speedOffsetUp, "speedOffsetDown": f.speedOffsetDown, "speedNeutral": f.speedNeutral, "indicator": indicator},
		"telemetry":            object{"nice": f.niceLevel, "cpuPercent": f.cpuPercent, "eventCount": f.eventCount, "malformedCount": f.malformedCount, "eventsPerSec": f.eventsPerSec, "lastMidiHex": f.lastMIDIHex, "lastControl": f.lastControlLabel, "jogVelocity": f.jogVelocity, "jogTouched": f.jogTouched, "lastDeltaPosSec": f.lastDeltaPosSec, "latency": f.latencySummaryLocked()},
		"ledFeedbackSupported": truth(led["supported"]), "ledFeedbackNote": get(led, "note", ""), "error": f.errorText,
	}
}

func (f *mapper) Action(req map[string]any) (any, error) {
	return f.action(req, nil, nil)
}

// ActionRaw is the optional runtime hook needed for ordered preset JSON. All
// other actions continue to use the ordinary shared Feature contract.
func (f *mapper) ActionRaw(req map[string]any, body []byte) (any, error) {
	if str(req["action"]) != "save_preset" {
		return f.Action(req)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, modkit.AsBad(err)
	}
	raw := fields["preset"]
	if len(raw) == 0 {
		return f.Action(req)
	}
	preset, declaration, err := decodePresetJSON(raw)
	if err != nil {
		return nil, modkit.AsBad(err)
	}
	// Use the decoded raw preset for validation and persistence alike.
	orderedReq := object{"action": "save_preset", "preset": preset}
	return f.action(orderedReq, raw, declaration)
}

func (f *mapper) action(req map[string]any, rawPreset json.RawMessage, declaration []string) (any, error) {
	f.lifecycle.Lock()
	defer f.lifecycle.Unlock()
	act := str(req["action"])
	if act == "restart_worker" {
		f.stopWorker()
	}
	if act == "enable" {
		f.mu.Lock()
		restart := f.backend != nil && !f.threadAliveLocked()
		f.mu.Unlock()
		if restart {
			f.stopWorker()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch act {
	case "poll":
		return f.pollRequestLocked(req)
	case "enable":
		f.errorText = ""
		f.malformedCount = 0
		f.enabled = true
		if f.backend == nil {
			f.startWorkerLocked()
		}
		f.tryConnectDeviceLocked()
		if err := f.saveConfig(); err != nil {
			return nil, err
		}
	case "disable":
		f.enabled = false
		f.queueSafetyResetLocked()
		f.disconnectLocked()
		if err := f.saveConfig(); err != nil {
			return nil, err
		}
	case "restart_worker":
		f.errorText = ""
		f.malformedCount = 0
		f.startWorkerLocked()
		if f.enabled {
			f.tryConnectDeviceLocked()
		}
	case "configure":
		if v, ok := req["targetDeck"]; ok {
			td, err := strictInt(v, "targetDeck", 1, 4)
			if err != nil {
				return nil, err
			}
			f.targetDeck = td
		}
		if v, ok := req["presetId"]; ok {
			pid, ok := v.(string)
			if !ok {
				return nil, modkit.Bad("presetId must be a string")
			}
			f.loadPresets()
			if f.presets[pid] == nil {
				return nil, modkit.Bad("unknown preset '%s'", pid)
			}
			f.presetID = pid
			f.vinylMode = get(obj(f.currentPreset()["jog"]), "modeDefault", "vinyl") == "vinyl"
		}
		if v, ok := req["selectedDevice"]; ok {
			dev, ok := v.(string)
			if !ok {
				return nil, modkit.Bad("selectedDevice must be a string")
			}
			f.selectedDevice = dev
			f.disconnectLocked()
		}
		if v, ok := req["enabled"]; ok {
			enabled, ok := v.(bool)
			if !ok {
				return nil, modkit.Bad("enabled must be a boolean")
			}
			f.enabled = enabled
			if enabled {
				f.errorText = ""
				f.malformedCount = 0
			} else {
				f.queueSafetyResetLocked()
			}
		}
		if f.enabled {
			f.tryConnectDeviceLocked()
		} else {
			f.disconnectLocked()
		}
		if err := f.saveConfig(); err != nil {
			return nil, err
		}
	case "refresh_devices":
		f.loadPresets()
		if f.enabled {
			f.tryConnectDeviceLocked()
		}
	case "save_preset":
		p, err := validatePreset(req["preset"], declaration)
		if err != nil {
			return nil, err
		}
		var saved any = p
		if rawPreset != nil {
			saved = rawPreset
		}
		if err := modkit.SaveJSON(filepath.Join(f.userPresetsDir(), str(p["id"])+".json"), saved); err != nil {
			return nil, err
		}
		f.loadPresets()
	case "reset_telemetry":
		f.latencySamples = nil
		clear(f.inflightBatches)
		f.pendingBatchT0 = nil
		f.eventCount = 0
		f.malformedCount = 0
	default:
		return nil, modkit.Bad("unknown action '%s'", textValue(req["action"]))
	}
	return f.statusLocked(), nil
}

func (f *mapper) queueSafetyResetLocked() {
	f.jogVelocity = 0
	f.jogTouched = false
	f.pitchBendDir = 0
	f.pitchBendAmount = 0
	f.pendingValues["nudgeFwd"] = 0.0
	f.pendingValues["nudgeBwd"] = 0.0
	f.pendingValues["cueHold"] = false
	f.pendingValues["jogTouch"] = false
}
func (f *mapper) autoDisableLocked(reason string) {
	f.enabled = false
	f.errorText = reason
	f.queueSafetyResetLocked()
	f.disconnectLocked()
}
func (f *mapper) simulateDisconnectLocked(client, port int) bool {
	if client >= 0 {
		if !f.connected || client != f.connectedClient {
			return false
		}
		if port >= 0 && f.connectedPort >= 0 && port != f.connectedPort {
			return false
		}
	} else if !f.enabled && !f.connected {
		return false
	}
	name := f.connectedName
	if name == "" {
		name = "USB MIDI controller"
	}
	f.autoDisableLocked(fmt.Sprintf("MIDI controller disconnected (%s). Mapper auto-disabled for deck safety.", name))
	return true
}
func (f *mapper) markBatchLocked(now float64) { f.pendingBatchT0 = &now }

func (f *mapper) updateDeckStateLocked(state object) error {
	if state == nil {
		return nil
	}
	// Convert numeric fields before applying them so malformed panel data cannot
	// poison the next /status JSON response with NaN or an invalid value.
	converted := object{}
	for _, key := range []string{"posNorm", "durationSec", "speed"} {
		if v := state[key]; v != nil {
			n, err := modkit.Number(v, -math.MaxFloat64, math.MaxFloat64)
			if err != nil {
				return err
			}
			converted[key] = n
		}
	}
	for _, key := range []string{"songLoaded", "playing", "posNorm", "durationSec", "speedRangeIndex", "speedRangeCount", "keylock", "sync", "loopEnabled", "browseOpen"} {
		if v := state[key]; v != nil {
			if n, ok := converted[key]; ok {
				v = n
			}
			f.deckState[key] = v
		}
	}
	if !f.jogTouched && f.jogVelocity == 0 && state["posNorm"] != nil {
		f.virtualPosNorm = number(converted["posNorm"])
		f.virtualPosValid = true
	}
	if v := state["speed"]; v != nil {
		speed := number(converted["speed"])
		if _, queued := f.pendingValues["speed"]; queued && f.preWriteSpeed != nil && math.Abs(speed-*f.preWriteSpeed) <= 1e-4 {
			return nil
		}
		f.deckState["speed"] = speed
		pickup := num(obj(f.currentPreset()["tempoSlider"]), "pickupThreshold", 0.025)
		if f.faderSeen && math.Abs(speed-f.faderNorm) > pickup && (f.lastWrittenSpeed == nil || math.Abs(speed-*f.lastWrittenSpeed) > pickup*0.6) {
			f.softLocked = false
			delete(f.pendingValues, "speed")
			f.preWriteSpeed = nil
			f.speedOffsetUp = f.faderNorm < speed-pickup
			f.speedOffsetDown = f.faderNorm > speed+pickup
			f.speedNeutral = false
			f.queueSpeedIndicatorsLocked()
		}
	}
	return nil
}
func (f *mapper) queueSpeedIndicatorsLocked() {
	f.pendingValues["speedOffsetUp"] = f.speedOffsetUp
	f.pendingValues["speedOffsetDown"] = f.speedOffsetDown
	f.pendingValues["speedNeutral"] = f.speedNeutral
}

func (f *mapper) pollRequestLocked(req object) (any, error) {
	var ack *int64
	var apply *float64
	if v := req["ackSeq"]; v != nil {
		n, err := modkit.Number(v, -math.MaxInt64, math.MaxInt64)
		if err != nil {
			return nil, err
		}
		a := int64(n)
		ack = &a
	}
	if v := req["ackApplyMs"]; v != nil {
		n, err := modkit.Number(v, -math.MaxFloat64, math.MaxFloat64)
		if err != nil {
			return nil, err
		}
		apply = &n
	}
	return f.pollBridgeLocked(obj(req["deckState"]), ack, apply, f.now())
}
func (f *mapper) pollBridgeLocked(state object, ack *int64, apply *float64, now float64) (object, error) {
	f.lastPollAt = now
	if ack != nil {
		if wait, ok := f.inflightBatches[*ack]; ok {
			delete(f.inflightBatches, *ack)
			ms := 2.5
			if apply != nil {
				ms = *apply
			}
			f.latencySamples = append(f.latencySamples, round(clamp(wait+math.Max(0.2, ms), 0.1, 250), 3))
			if len(f.latencySamples) > 500 {
				f.latencySamples = append([]float64{}, f.latencySamples[len(f.latencySamples)-500:]...)
			}
		}
		if len(f.inflightBatches) > 64 {
			clear(f.inflightBatches)
		}
	}
	if err := f.updateDeckStateLocked(state); err != nil {
		return nil, err
	}
	if f.enabled && f.jogVelocity != 0 {
		f.integrateJogLocked(nil, now)
	}
	if len(f.pendingValues) == 0 && len(f.pendingActions) == 0 {
		return object{"enabled": f.enabled, "targetDeck": f.targetDeck, "seq": int64(0), "values": object{}, "actions": []object{}}, nil
	}
	f.seq++
	t0 := now
	if f.pendingBatchT0 != nil {
		t0 = *f.pendingBatchT0
	}
	f.inflightBatches[f.seq] = math.Max(0.05, (now-t0)*1000)
	f.pendingBatchT0 = nil
	f.preWriteSpeed = nil
	values, actions := f.pendingValues, f.pendingActions
	if !f.enabled {
		actions = []object{}
	}
	f.pendingValues = object{}
	f.pendingActions = []object{}
	return object{"enabled": f.enabled, "targetDeck": f.targetDeck, "seq": f.seq, "values": values, "actions": actions}, nil
}
func percentile(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := float64(len(sorted)-1) * pct / 100
	lo := int(idx)
	hi := min(lo+1, len(sorted)-1)
	frac := idx - float64(lo)
	return round(sorted[lo]*(1-frac)+sorted[hi]*frac, 3)
}
func (f *mapper) latencySummaryLocked() object {
	s := append([]float64{}, f.latencySamples...)
	sort.Float64s(s)
	p50, p95, maxMS, avg := 0.0, 0.0, 0.0, 0.0
	if len(s) > 0 {
		p50 = percentile(s, 50)
		p95 = percentile(s, 95)
		maxMS = round(s[len(s)-1], 3)
		for _, v := range s {
			avg += v
		}
		avg = round(avg/float64(len(s)), 3)
	}
	return object{"count": len(s), "p50Ms": p50, "p95Ms": p95, "maxMs": maxMS, "avgMs": avg}
}
func (f *mapper) Close() { f.lifecycle.Lock(); defer f.lifecycle.Unlock(); f.stopWorker() }
