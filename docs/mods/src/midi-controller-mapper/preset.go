package midimapper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

type object = map[string]any

var presetIDRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
var usbIDRE = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{4}$`)
var validActions = map[string]bool{
	"toggle_play": true, "hold_cue": true, "seek_forward": true, "seek_backward": true,
	"track_next": true, "track_prev": true, "loop_in": true, "loop_out": true,
	"reloop_exit": true, "cycle_tempo_range": true, "toggle_keylock": true,
	"toggle_vinyl_mode": true, "toggle_sync": true, "loop_halve": true,
	"loop_double": true, "load_selected_track": true, "toggle_browse_view": true, "browse_scroll": true,
}
var validJogEncodings = map[string]bool{"cdj_continuous_velocity": true, "relative_center_64": true, "relative_2s_complement": true, "relative_sign_magnitude": true}

func get(m object, key string, fallback any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return fallback
}
func obj(v any) object { m, _ := v.(map[string]any); return m }
func str(v any) string { s, _ := v.(string); return s }
func textValue(v any) string {
	if v == nil {
		return "None"
	}
	return fmt.Sprint(v)
}
func number(v any) float64                               { f, _ := modkit.Float(v); return f }
func num(m object, key string, fallback float64) float64 { return number(get(m, key, fallback)) }
func integer(m object, key string, fallback int) int     { return int(number(get(m, key, fallback))) }
func truth(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return number(v) != 0
	}
}
func sortedKeys(m object) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// orderedKeys retains the first declaration of each JSON object key, matching
// Python dict order even when a later declaration replaces the key's value.
// The sorted fallback is only for Go callers that supply an unordered map.
func orderedKeys(m object, declaration []string) []string {
	keys := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	for _, key := range declaration {
		if _, exists := m[key]; exists && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	for _, key := range sortedKeys(m) {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

// decodePresetJSON keeps the controls' declaration order beside the ordinary
// map. RawMessage retains the last controls object if that field is repeated;
// decoding its keys with Token preserves the first occurrence within it.
func decodePresetJSON(raw []byte) (any, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("extra data after the JSON value")
		}
		return nil, nil, err
	}
	if obj(value) == nil || obj(obj(value)["controls"]) == nil {
		return value, nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	controls := json.NewDecoder(bytes.NewReader(fields["controls"]))
	if _, err := controls.Token(); err != nil {
		return nil, nil, err
	}
	var declaration []string
	for controls.More() {
		key, err := controls.Token()
		if err != nil {
			return nil, nil, err
		}
		declaration = append(declaration, key.(string))
		var spec json.RawMessage
		if err := controls.Decode(&spec); err != nil {
			return nil, nil, err
		}
	}
	return value, orderedKeys(obj(obj(value)["controls"]), declaration), nil
}

func (f *mapper) controlKeys(preset object) []string {
	if declaration, exists := f.controlOrders[str(preset["id"])]; exists {
		return declaration
	}
	return sortedKeys(obj(preset["controls"]))
}
func strictInt(v any, name string, lo, hi int) (int, error) {
	n, ok := modkit.Int(v)
	if !ok {
		return 0, modkit.Bad("%s must be an integer", name)
	}
	if n < int64(lo) {
		return 0, modkit.Bad("%s must be >= %d", name, lo)
	}
	if n > int64(hi) {
		return 0, modkit.Bad("%s must be <= %d", name, hi)
	}
	return int(n), nil
}
func strictFloat(v any, name string, lo, hi float64) error {
	f, ok := modkit.Float(v)
	if !ok {
		return modkit.Bad("%s must be a number", name)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return modkit.Bad("%s must be a finite number", name)
	}
	if f < lo {
		return modkit.Bad("%s must be >= %s", name, modkit.Fmt(lo))
	}
	if f > hi {
		return modkit.Bad("%s must be <= %s", name, modkit.Fmt(hi))
	}
	return nil
}

func validatePreset(value any, declaration ...[]string) (object, error) {
	data := obj(value)
	if data == nil {
		return nil, modkit.Bad("preset must be a JSON object")
	}
	for _, key := range []string{"id", "name", "jog", "tempoSlider", "controls"} {
		if _, ok := data[key]; !ok {
			return nil, modkit.Bad("preset missing required field '%s'", key)
		}
	}
	pid, ok := data["id"].(string)
	if !ok || strings.Contains(pid, "..") || !presetIDRE.MatchString(pid) {
		return nil, modkit.Bad("invalid preset id")
	}
	name, ok := data["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return nil, modkit.Bad("preset 'name' must be a non-empty string")
	}
	if v := data["match"]; v != nil {
		m := obj(v)
		if m == nil {
			return nil, modkit.Bad("preset 'match' must be an object")
		}
		for _, key := range []string{"alsaClientPatterns", "usbIds"} {
			if v := m[key]; v != nil {
				list, ok := v.([]any)
				valid := ok
				for _, v := range list {
					s, ok := v.(string)
					if !ok || strings.TrimSpace(s) == "" || (key == "usbIds" && !usbIDRE.MatchString(strings.TrimSpace(s))) {
						valid = false
						break
					}
				}
				if !valid {
					if key == "usbIds" {
						return nil, modkit.Bad("preset 'match.usbIds' must be a list of 'vvvv:pppp' hex strings")
					}
					return nil, modkit.Bad("preset 'match.alsaClientPatterns' must be a list of non-empty strings")
				}
			}
		}
	}
	for _, field := range []struct {
		key    string
		lo, hi int
	}{{"malformedLimit", 1, 1000}, {"stormLimitPerSec", 10, 100000}, {"midiChannel", 0, 15}} {
		if v := data[field.key]; v != nil {
			if field.key == "midiChannel" && v == "any" {
				continue
			}
			if _, err := strictInt(v, field.key, field.lo, field.hi); err != nil {
				return nil, err
			}
		}
	}
	jog := obj(data["jog"])
	if jog == nil {
		return nil, modkit.Bad("preset 'jog' must be an object")
	}
	enc := get(jog, "encoding", "cdj_continuous_velocity")
	if !validJogEncodings[str(enc)] {
		return nil, modkit.Bad("unsupported jog encoding '%s'", textValue(enc))
	}
	for _, key := range []string{"touchNote", "scratchCc", "pitchBendCc", "centerValue", "maxOffset"} {
		if v := jog[key]; v != nil {
			lo := 0
			if key == "maxOffset" {
				lo = 1
			}
			if _, err := strictInt(v, "jog."+key, lo, 127); err != nil {
				return nil, err
			}
		}
	}
	for _, field := range []struct {
		key    string
		lo, hi float64
	}{{"scratchSensitivitySecPerSec", 0.01, 50}, {"pitchBendMaxAmount", 0.01, 1}} {
		if v := jog[field.key]; v != nil {
			if err := strictFloat(v, "jog."+field.key, field.lo, field.hi); err != nil {
				return nil, err
			}
		}
	}
	ts := obj(data["tempoSlider"])
	if ts == nil {
		return nil, modkit.Bad("preset 'tempoSlider' must be an object")
	}
	if _, err := strictInt(get(ts, "cc", 29), "tempoSlider.cc", 0, 127); err != nil {
		return nil, err
	}
	for _, field := range []struct {
		key    string
		lo, hi float64
	}{{"pickupThreshold", 0.001, 0.5}, {"neutralDeadband", 0, 0.2}} {
		if v := ts[field.key]; v != nil {
			if err := strictFloat(v, "tempoSlider."+field.key, field.lo, field.hi); err != nil {
				return nil, err
			}
		}
	}
	controls := obj(data["controls"])
	if len(controls) == 0 {
		return nil, modkit.Bad("preset 'controls' must be a non-empty object")
	}
	var controlOrder []string
	if len(declaration) != 0 {
		controlOrder = declaration[0]
	}
	for _, name := range orderedKeys(controls, controlOrder) {
		spec := obj(controls[name])
		if spec == nil {
			return nil, modkit.Bad("control '%s' must be an object", name)
		}
		typ := str(spec["type"])
		if typ != "note" && typ != "cc" {
			return nil, modkit.Bad("control '%s' has invalid type '%s'", name, textValue(spec["type"]))
		}
		if _, err := strictInt(get(spec, "number", -1), fmt.Sprintf("control '%s' number", name), 0, 127); err != nil {
			return nil, err
		}
		if !validActions[str(spec["action"])] {
			return nil, modkit.Bad("control '%s' has unknown action '%s'", name, textValue(spec["action"]))
		}
	}
	return data, nil
}

func (f *mapper) loadPresets() {
	f.presets = make(map[string]object)
	f.controlOrders = make(map[string][]string)
	f.presetOrder = nil
	f.presetErrors = []string{}
	for i, folder := range []string{filepath.Join(f.env.ModDir, "presets"), f.userPresetsDir()} {
		entries, err := os.ReadDir(folder)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(folder, entry.Name()))
			var raw any
			var controlOrder []string
			var p object
			if err == nil {
				raw, controlOrder, err = decodePresetJSON(data)
			}
			if err == nil {
				p, err = validatePreset(raw, controlOrder)
			}
			if err != nil {
				f.presetErrors = append(f.presetErrors, entry.Name()+": "+err.Error())
				continue
			}
			id := str(p["id"])
			if _, exists := f.presets[id]; exists && i == 1 {
				if st, err := os.Stat(filepath.Join(f.env.ModDir, "presets", id+".json")); err == nil && st.Mode().IsRegular() {
					f.presetErrors = append(f.presetErrors, fmt.Sprintf("%s: cannot override built-in preset '%s'", entry.Name(), id))
					continue
				}
			}
			if _, exists := f.presets[id]; !exists {
				f.presetOrder = append(f.presetOrder, id)
			}
			f.presets[id] = p
			f.controlOrders[id] = controlOrder
		}
	}
}
func (f *mapper) currentPreset() object {
	if p := f.presets[f.presetID]; p != nil {
		return p
	}
	if len(f.presetOrder) > 0 {
		return f.presets[f.presetOrder[0]]
	}
	return object{}
}
func (f *mapper) userPresetsDir() string { return filepath.Join(f.env.State, "presets") }
func (f *mapper) saveConfig() error {
	return modkit.SaveJSON(filepath.Join(f.env.State, "config.json"), object{"enabled": f.enabled, "targetDeck": f.targetDeck, "presetId": f.presetID, "selectedDevice": f.selectedDevice})
}
