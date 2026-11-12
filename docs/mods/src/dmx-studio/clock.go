package dmxstudio

import (
	"math"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

// Beat clock (#209): keeps an in-memory beat clock fed either by EngineX's local
// stream-sync endpoints (without needing a deck Panel) or by explicit `clock`
// actions. Nothing is ever written to the state directory on clock updates.
//
// Between updates the beat count is extrapolated from the last sample. An
// update re-anchors the model where it stands when the sample arrives, so a
// tempo change only changes the slope. A phase error (nudge, MASTER handoff,
// seek) is taken in at once when it is below half a frame of slewing, otherwise
// slewed in at half the tempo so moving heads speed up or slow down instead of
// jumping. A paused master holds the look; with no deck (deck 0) or no update
// for clockStale seconds the manual BPM takes over from the current phase.
const (
	barBeats   = 4.0
	clockStale = 2.0  // s without an update: back to the manual BPM
	frameRate  = 30.0 // the render loop's rate
	maxLag     = 1.0  // s: an older sample time is treated as this old
)

type beatClock struct {
	deck         int     // 0: manual BPM
	sourceOrigin string  // "manual", "panel", "local", "peer", "prolink"
	sourceID     string  // identifies the deck instance across sources for TrackPosition continuity
	playing      bool    // the beat count advances
	bpm          float64 // the deck's played tempo
	anchor       float64 // beat count at anchorT
	anchorT      float64
	pending      float64 // phase error still being slewed in, beats
	pendingT     float64
	offset       float64 // manual: beats = t*bpm/60 + offset (0 keeps the old wall-clock phase)
	updated      float64 // monotonic time of the last deck update
	sampleT      float64 // its sample time
	position     float64 // last TrackPosition, samples/ms (NaN: unknown)
}

func newBeatClock() beatClock { return beatClock{sourceOrigin: "manual", position: math.NaN()} }

// slew is how fast a phase error is taken in: half the tempo, at least 1/4 beat/s.
func slew(bpm float64) float64 { return math.Max(0.25, 0.5*bpm/60) }

// snap is the error taken in at once: half of what one frame of slewing moves,
// so taking it never steps further than slewing would.
func snap(bpm float64) float64 { return slew(bpm) / frameRate / 2 }

// wrapBar maps a bar-phase difference into [-2, 2) beats.
func wrapBar(d float64) float64 {
	d = math.Mod(d+barBeats/2, barBeats)
	if d < 0 {
		d += barBeats
	}
	return d - barBeats/2
}

func (c *beatClock) deckBeats(t float64) float64 {
	b := c.anchor
	if c.playing && t > c.anchorT {
		b += (t - c.anchorT) * c.bpm / 60
		if c.pending != 0 && t > c.pendingT {
			step := math.Min(math.Abs(c.pending), (t-c.pendingT)*slew(c.bpm))
			b += math.Copysign(step, c.pending)
		}
	}
	return b
}

// beats is the beat count at monotonic time t; manualBPM is the show's BPM.
func (c *beatClock) beats(t, manualBPM float64) float64 {
	if c.deck != 0 && t-c.updated > clockStale {
		c.toManual(t, manualBPM)
	}
	if c.deck == 0 {
		return t*manualBPM/60 + c.offset
	}
	return c.deckBeats(t)
}

func (c *beatClock) toManual(t, manualBPM float64) {
	if c.deck != 0 {
		c.offset = c.deckBeats(t) - t*manualBPM/60
	}
	c.deck, c.sourceOrigin, c.sourceID, c.playing, c.pending, c.position = 0, "manual", "", false, 0, math.NaN()
}

type clockSample struct {
	deck         int
	sourceOrigin string // "panel", "local", "peer", "prolink"
	sourceID     string
	playing      bool
	beat         float64 // Engine's BeatNumber (0..4)
	bpm          float64
	position     float64 // NaN: not sent
}

// update takes one sample read at monotonic time t, arriving at now. The
// sample is extrapolated to now and the model re-anchored where it stands at
// now, so what is already rendered never changes after the fact.
func (c *beatClock) update(t, now, manualBPM float64, s clockSample) {
	if s.deck == 0 || s.bpm <= 0 {
		c.beats(now, manualBPM) // a stale deck clock hands over first
		c.toManual(now, manualBPM)
		return
	}
	if c.deck != 0 && t < c.sampleT {
		return // older than a sample the model already took
	}
	srcOrigin := s.sourceOrigin
	if srcOrigin == "" {
		srcOrigin = "panel"
	}
	srcID := s.sourceID
	if srcID == "" {
		srcID = srcOrigin
	}
	// CUE held plays with PlayState false: a moving position counts as playing.
	sameSource := c.deck == s.deck && c.sourceID == srcID
	moving := sameSource && !math.IsNaN(s.position) && !math.IsNaN(c.position) && s.position != c.position
	playing := (s.playing || moving) && s.bpm > 0
	observed := s.beat
	if playing && now > t {
		observed += (now - t) * s.bpm / 60
	}
	base := c.beats(now, manualBPM)
	c.deck, c.sourceOrigin, c.sourceID, c.bpm, c.anchor, c.anchorT, c.pending, c.playing = s.deck, srcOrigin, srcID, s.bpm, base, now, 0, playing
	if e := wrapBar(observed - base); math.Abs(e) <= snap(s.bpm) {
		c.anchor += e
	} else {
		c.pending, c.pendingT = e, now
	}
	c.updated, c.sampleT, c.position = now, t, s.position
}

func (c *beatClock) status(t, manualBPM, phraseBeats float64) map[string]any {
	b := c.beats(t, manualBPM)
	source, origin, bpm := "manual", "manual", manualBPM
	if c.deck != 0 {
		source, origin, bpm = "deck", c.sourceOrigin, c.bpm
	}
	beat := math.Mod(b, barBeats)
	if beat < 0 {
		beat += barBeats
	}
	out := map[string]any{
		"source":       source,
		"sourceOrigin": origin,
		"deck":         c.deck,
		"playing":      c.deck != 0 && c.playing,
		"bpm":          bpm,
		"beat":         beat,
	}
	if phraseBeats > 0 {
		idx, pBeat := phrasePosition(b, phraseBeats)
		out["phrase"] = idx + 1
		out["phraseBeat"] = pBeat
		out["phraseBeats"] = phraseBeats
	}
	return out
}

// parseClock validates a clock action. at is the sample time in Unix ms.
func parseClock(req map[string]any) (s clockSample, at float64, hasAt bool, err error) {
	s.position = math.NaN()
	s.sourceOrigin = "panel"
	d, err := get(req, "deck")
	if err != nil {
		return s, 0, false, err
	}
	if s.deck, err = modkit.Integer(d, 0, 4); err != nil || s.deck == 0 {
		return s, 0, false, err
	}
	for _, k := range []string{"beat", "bpm", "playing"} {
		if _, err := get(req, k); err != nil {
			return s, 0, false, err
		}
	}
	if s.beat, err = modkit.Number(req["beat"], -1e9, 1e9); err != nil {
		return s, 0, false, err
	}
	if s.bpm, err = modkit.Number(req["bpm"], 0, 999); err != nil {
		return s, 0, false, err
	}
	var ok bool
	if s.playing, ok = req["playing"].(bool); !ok {
		return s, 0, false, modkit.Bad("playing must be boolean")
	}
	if v, ok := req["position"]; ok && v != nil {
		if s.position, err = modkit.Number(v, -1e12, 1e12); err != nil {
			return s, 0, false, err
		}
	}
	if v, ok := req["at"]; ok && v != nil {
		if at, err = modkit.Number(v, 0, 1e14); err != nil {
			return s, 0, false, err
		}
		hasAt = true
	}
	return s, at, hasAt, nil
}

// clockAction applies one clock action at monotonic now; wall is Unix ms now.
func (f *Feature) clockAction(req map[string]any, now float64, wall float64) (any, error) {
	s, at, hasAt, err := parseClock(req)
	if err != nil {
		return nil, err
	}
	t := now
	if hasAt {
		t = now - math.Min(maxLag, math.Max(0, (wall-at)/1000))
	}
	manual := f.Config["bpm"].(float64)
	f.clock.update(t, now, manual, s)
	if s.deck != 0 {
		f.lastExternalClock = now
		f.pollerActive = false
	} else {
		f.lastExternalClock = -1e9
		f.pollerActive = false
	}
	return map[string]any{"clock": f.clockStatusLocked(now)}, nil
}

func wallMillis() float64 { return float64(time.Now().UnixNano()) / 1e6 }
