package midimapper

import (
	"fmt"
	"math"
)

// All MIDI and bridge methods run with mu held. Tests supply monotonic times
// directly; the live worker and QML bridge share the same monotonic clock.
func (f *mapper) recordMalformedLocked(detail string, now float64) {
	f.malformedCount++
	if f.malformedCount >= int64(integer(f.currentPreset(), "malformedLimit", 16)) {
		f.autoDisableLocked(fmt.Sprintf("Disabled due to repeated malformed MIDI data (%d errors: %s).", f.malformedCount, detail))
	}
}
func (f *mapper) processMIDILocked(raw []int, now float64) bool {
	if !f.enabled {
		return false
	}
	if len(raw) < 2 || len(raw) > 3 {
		f.recordMalformedLocked("invalid MIDI message length", now)
		return false
	}
	b0, b1, b2 := raw[0], raw[1], 0
	if len(raw) > 2 {
		b2 = raw[2]
	}
	if b0 < 0x80 || b0 > 0xef || b1 < 0 || b1 > 127 || b2 < 0 || b2 > 127 {
		f.recordMalformedLocked("invalid MIDI status or data byte", now)
		return false
	}
	preset := f.currentPreset()
	if now-f.rateWindowStart >= 1 {
		f.eventsPerSec = round(float64(f.rateWindowEvents)/math.Max(0.001, now-f.rateWindowStart), 1)
		f.rateWindowStart = now
		f.rateWindowEvents = 0
	}
	f.rateWindowEvents++
	limit := integer(preset, "stormLimitPerSec", 2400)
	if f.rateWindowEvents > limit {
		f.autoDisableLocked(fmt.Sprintf("MIDI message storm detected (>%d msg/s). Auto-disabled to protect audio engine.", limit))
		return false
	}
	if ch := get(preset, "midiChannel", "any"); ch != "any" && int(number(ch)) != b0&15 {
		return false
	}
	f.eventCount++
	f.lastMIDIHex = fmt.Sprintf("%02X %02X %02X", b0, b1, b2)
	switch b0 & 0xf0 {
	case 0x80, 0x90:
		return f.handleNoteLocked(b1, b0&0xf0 == 0x90 && b2 > 0, preset, now)
	case 0xb0:
		return f.handleCCLocked(b1, b2, preset, now)
	}
	return false
}
func (f *mapper) handleNoteLocked(note int, pressed bool, preset object, now float64) bool {
	jog := obj(preset["jog"])
	if touch := jog["touchNote"]; touch != nil && int(number(touch)) == note {
		if pressed {
			f.lastControlLabel = "Jog Platter Touch"
		} else {
			f.lastControlLabel = "Jog Platter Release"
		}
		f.markBatchLocked(now)
		f.jogTouched = pressed
		f.lastJogStepAt = now
		f.pendingValues["vinylMode"] = f.vinylMode
		f.pendingValues["jogTouch"] = pressed
		if !pressed {
			f.jogVelocity = 0
		}
		return true
	}
	controls := obj(preset["controls"])
	for _, key := range f.controlKeys(preset) {
		spec := obj(controls[key])
		if spec["type"] == "note" && integer(spec, "number", -1) == note {
			act := str(spec["action"])
			f.lastControlLabel = textValue(get(spec, "label", act))
			f.markBatchLocked(now)
			if act == "hold_cue" {
				f.pendingValues["cueHold"] = pressed
				return true
			}
			if !pressed {
				return true
			}
			if act == "toggle_vinyl_mode" {
				f.vinylMode = !f.vinylMode
				f.pendingValues["vinylMode"] = f.vinylMode
				return true
			}
			f.pendingActions = append(f.pendingActions, object{"action": act})
			return true
		}
	}
	return false
}
func (f *mapper) handleCCLocked(cc, value int, preset object, now float64) bool {
	jog := obj(preset["jog"])
	if scratch := jog["scratchCc"]; scratch != nil && int(number(scratch)) == cc {
		f.markBatchLocked(now)
		enc := str(get(jog, "encoding", "cdj_continuous_velocity"))
		center := integer(jog, "centerValue", 64)
		if enc == "cdj_continuous_velocity" {
			if f.jogVelocity != 0 && f.lastJogStepAt > 0 {
				f.integrateJogLocked(nil, now)
			}
			f.jogVelocity = value - center
			f.lastJogStepAt = now
			f.lastControlLabel = fmt.Sprintf("Jog Scratch Vel (%+d)", f.jogVelocity)
			if f.jogVelocity == 0 {
				f.pitchBendDir = 0
				f.pitchBendAmount = 0
				f.pendingValues["nudgeFwd"] = 0.0
				f.pendingValues["nudgeBwd"] = 0.0
			}
		} else {
			delta := decodeRelativeCC(value, enc, center)
			f.applyJogDeltaLocked(float64(delta)/63*0.016*num(jog, "scratchSensitivitySecPerSec", 1.8), now)
		}
		return true
	}
	if pb := jog["pitchBendCc"]; pb != nil && int(number(pb)) == cc {
		f.markBatchLocked(now)
		offset := value - integer(jog, "centerValue", 64)
		maxOff := max(1, integer(jog, "maxOffset", 63))
		maxAmt := num(jog, "pitchBendMaxAmount", 0.65)
		f.lastControlLabel = fmt.Sprintf("Jog Pitch Bend (%+d)", offset)
		if offset == 0 {
			f.pitchBendDir = 0
			f.pitchBendAmount = 0
			f.pendingValues["nudgeFwd"] = 0.0
			f.pendingValues["nudgeBwd"] = 0.0
		} else {
			amt := math.Min(1, math.Abs(float64(offset))/float64(maxOff)*maxAmt)
			f.pitchBendAmount = amt
			f.lastPitchBendAt = now
			if offset > 0 {
				f.pitchBendDir = 1
				f.pendingValues["nudgeFwd"] = round(amt, 4)
				f.pendingValues["nudgeBwd"] = 0.0
			} else {
				f.pitchBendDir = -1
				f.pendingValues["nudgeFwd"] = 0.0
				f.pendingValues["nudgeBwd"] = round(amt, 4)
			}
		}
		return true
	}
	ts := obj(preset["tempoSlider"])
	if integer(ts, "cc", 29) == cc {
		f.markBatchLocked(now)
		norm := float64(value) / 127
		if value == 64 {
			norm = 0.5
		}
		if truth(ts["invert"]) {
			norm = 1 - norm
		}
		f.handleTempoSliderLocked(norm, ts)
		return true
	}
	controls := obj(preset["controls"])
	for _, key := range f.controlKeys(preset) {
		spec := obj(controls[key])
		if spec["type"] == "cc" && integer(spec, "number", -1) == cc && spec["action"] == "browse_scroll" {
			steps := decodeRelativeCC(value, str(get(spec, "encoding", "relative_center_64")), 64)
			if steps != 0 {
				f.markBatchLocked(now)
				f.lastControlLabel = fmt.Sprintf("%s (%+d)", textValue(get(spec, "label", "Browse")), steps)
				f.pendingActions = append(f.pendingActions, object{"action": "browse_scroll", "steps": steps})
				return true
			}
		}
	}
	return false
}
func decodeRelativeCC(value int, encoding string, center int) int {
	switch encoding {
	case "relative_2s_complement":
		if value < 64 {
			return value
		}
		return value - 128
	case "relative_sign_magnitude":
		if value&0x40 != 0 {
			return -(value & 0x3f)
		}
		return value & 0x3f
	default:
		return value - center
	}
}
func (f *mapper) handleTempoSliderLocked(norm float64, cfg object) {
	pickup, deadband := num(cfg, "pickupThreshold", 0.025), num(cfg, "neutralDeadband", 0.004)
	speed := num(f.deckState, "speed", 0.5)
	wasSeen, prev := f.faderSeen, f.faderNorm
	f.faderNorm = norm
	f.faderSeen = true
	if !truth(get(cfg, "softTakeover", true)) {
		f.softLocked = true
	} else if !f.softLocked {
		closeEnough := math.Abs(norm-speed) <= pickup
		crossed := wasSeen && prev != norm && ((prev < speed && speed <= norm) || (norm <= speed && speed < prev))
		if crossed || closeEnough {
			f.softLocked = true
		}
	}
	if f.softLocked {
		f.speedOffsetUp = false
		f.speedOffsetDown = false
		f.speedNeutral = math.Abs(norm-0.5) <= deadband
		target := 0.5
		if !f.speedNeutral {
			target = round(norm, 5)
		}
		if _, queued := f.pendingValues["speed"]; !queued {
			f.preWriteSpeed = &speed
		}
		f.lastWrittenSpeed = &target
		f.lastWrittenSpeedAt = f.now()
		f.deckState["speed"] = target
		f.pendingValues["speed"] = target
		f.lastControlLabel = fmt.Sprintf("Tempo Slider (%+.1f%%)", (target-0.5)*200)
	} else {
		f.speedOffsetUp = norm < speed-pickup
		f.speedOffsetDown = norm > speed+pickup
		f.speedNeutral = false
		arrow := "●"
		if f.speedOffsetUp {
			arrow = "▲"
		} else if f.speedOffsetDown {
			arrow = "▼"
		}
		f.lastControlLabel = "Tempo Soft Takeover " + arrow
	}
	f.queueSpeedIndicatorsLocked()
}
func (f *mapper) integrateJogLocked(dtOverride *float64, now float64) float64 {
	if !f.enabled {
		return 0
	}
	jog := obj(f.currentPreset()["jog"])
	if f.jogVelocity == 0 {
		f.lastJogStepAt = now
		return 0
	}
	dt := 0.0
	if dtOverride != nil {
		dt = *dtOverride
	} else if f.lastJogStepAt > 0 {
		dt = clamp(now-f.lastJogStepAt, 0, 0.5)
	}
	f.lastJogStepAt = now
	if dt <= 0 {
		return 0
	}
	maxOff := float64(max(1, integer(jog, "maxOffset", 63)))
	if truth(f.deckState["playing"]) && !f.jogTouched && !f.wasPlayingBeforeTouch {
		amt := math.Min(1, math.Abs(float64(f.jogVelocity))/maxOff*num(jog, "pitchBendMaxAmount", 0.65))
		f.markBatchLocked(now)
		f.pitchBendAmount = amt
		if f.jogVelocity > 0 {
			f.pitchBendDir = 1
			f.pendingValues["nudgeFwd"] = round(amt, 4)
			f.pendingValues["nudgeBwd"] = 0.0
		} else {
			f.pitchBendDir = -1
			f.pendingValues["nudgeFwd"] = 0.0
			f.pendingValues["nudgeBwd"] = round(amt, 4)
		}
		return 0
	}
	delta := float64(f.jogVelocity) / maxOff * dt * num(jog, "scratchSensitivitySecPerSec", 1.8)
	f.applyJogDeltaLocked(delta, now)
	return delta
}
func (f *mapper) applyJogDeltaLocked(delta, now float64) {
	dur := num(f.deckState, "durationSec", 0)
	if dur <= 0.01 {
		dur = 180
	}
	base := num(f.deckState, "posNorm", 0)
	if f.virtualPosValid {
		base = f.virtualPosNorm
	}
	next := clamp(base+delta/dur, 0, 1)
	f.virtualPosNorm = next
	f.virtualPosValid = true
	f.deckState["posNorm"] = next
	f.lastDeltaPosSec = round(delta, 5)
	f.pendingValues["jogMoveTicks"] = round(num(f.pendingValues, "jogMoveTicks", 0)+delta*26325, 2)
	f.pendingValues["setPosNorm"] = round(next, 6)
	f.markBatchLocked(now)
}
