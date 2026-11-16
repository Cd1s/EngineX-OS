package samplersequencer

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

const (
	numSlots   = 8
	numSteps   = 8
	minBPM     = 40.0
	maxBPM     = 240.0
	defaultBPM = 120.0
	maxSwing   = 75
)

type patternState struct {
	BPM        float64                  `json:"bpm"`
	Division   string                   `json:"division"`
	Swing      int                      `json:"swing"`
	FollowDeck bool                     `json:"followDeck"`
	Steps      [numSlots][numSteps]bool `json:"steps"`
}

func defaultPattern() patternState {
	return patternState{
		BPM:        defaultBPM,
		Division:   "1/16",
		Swing:      0,
		FollowDeck: true,
	}
}

func validDivision(div string) bool {
	switch div {
	case "1/4", "1/8", "1/16":
		return true
	default:
		return false
	}
}

// Feature implements the sampler-sequencer Mod helper behind GET /status and
// POST /action, persisting the 8x8 pattern in the Mod's own state directory
// and following the MASTER deck clock via /v1/enginex/clock.
type Feature struct {
	mu                  sync.Mutex
	statePath           string
	webUIURL            string
	now                 func() time.Time
	clockAPIAbsentUntil time.Time
	state               patternState
}

// New constructs a sampler-sequencer Feature backed by env.State/pattern.json.
func New(env modkit.Env) (modkit.Feature, error) {
	f := &Feature{
		statePath: filepath.Join(env.State, "pattern.json"),
		webUIURL:  envWebUIURL(),
		now:       time.Now,
		state:     defaultPattern(),
	}
	if err := f.load(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Feature) load() error {
	var loaded patternState
	err := modkit.LoadJSON(f.statePath, &loaded)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if loaded.BPM < minBPM || loaded.BPM > maxBPM {
		loaded.BPM = defaultBPM
	}
	if !validDivision(loaded.Division) {
		loaded.Division = "1/16"
	}
	if loaded.Swing < 0 || loaded.Swing > maxSwing {
		loaded.Swing = 0
	}
	f.state = loaded
	return nil
}

func (f *Feature) snapshot(note string, clock clockInfo) map[string]any {
	steps := make([][]bool, numSlots)
	for r := 0; r < numSlots; r++ {
		row := make([]bool, numSteps)
		copy(row, f.state.Steps[r][:])
		steps[r] = row
	}
	return map[string]any{
		"bpm":        f.state.BPM,
		"division":   f.state.Division,
		"swing":      f.state.Swing,
		"followDeck": f.state.FollowDeck,
		"steps":      steps,
		"clock":      clock,
		"note":       note,
	}
}

// Status returns the saved pattern and current MASTER clock snapshot without
// writing to disk.
func (f *Feature) Status() (any, error) {
	f.mu.Lock()
	follow := f.state.FollowDeck
	f.mu.Unlock()

	clock := clockInfo{Source: "manual"}
	if follow {
		clock = f.fetchMasterClock()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot("Ready.", clock), nil
}

// Action mutates the pattern or sequencer settings and persists pattern.json.
func (f *Feature) Action(req map[string]any) (any, error) {
	actVal, err := modkit.Field(req, "action")
	if err != nil {
		return nil, err
	}
	act, ok := actVal.(string)
	if !ok {
		return nil, modkit.Bad("'action' must be a string")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	note := "Saved."
	switch act {
	case "toggle":
		slotVal, err := modkit.Field(req, "slot")
		if err != nil {
			return nil, err
		}
		stepVal, err := modkit.Field(req, "step")
		if err != nil {
			return nil, err
		}
		slot, ok := modkit.Int(slotVal)
		if !ok || slot < 1 || slot > numSlots {
			return nil, modkit.Bad("'slot' must be 1..8")
		}
		step, ok := modkit.Int(stepVal)
		if !ok || step < 1 || step > numSteps {
			return nil, modkit.Bad("'step' must be 1..8")
		}
		next := !f.state.Steps[slot-1][step-1]
		if rawEnabled, exists := req["enabled"]; exists {
			b, ok := rawEnabled.(bool)
			if !ok {
				return nil, modkit.Bad("'enabled' must be a boolean")
			}
			next = b
		}
		f.state.Steps[slot-1][step-1] = next
		note = "Step updated."

	case "clear":
		f.state.Steps = [numSlots][numSteps]bool{}
		note = "Pattern cleared."

	case "set":
		if rawBPM, exists := req["bpm"]; exists {
			bpm, ok := modkit.Float(rawBPM)
			if !ok || bpm < minBPM || bpm > maxBPM {
				return nil, modkit.Bad("'bpm' must be between 40 and 240")
			}
			f.state.BPM = bpm
		}
		if rawDiv, exists := req["division"]; exists {
			div, ok := rawDiv.(string)
			if !ok || !validDivision(div) {
				return nil, modkit.Bad("'division' must be 1/4, 1/8, or 1/16")
			}
			f.state.Division = div
		}
		if rawSwing, exists := req["swing"]; exists {
			swing, ok := modkit.Int(rawSwing)
			if !ok || swing < 0 || swing > maxSwing {
				return nil, modkit.Bad("'swing' must be 0..75")
			}
			f.state.Swing = int(swing)
		}
		if rawFollow, exists := req["followDeck"]; exists {
			follow, ok := rawFollow.(bool)
			if !ok {
				return nil, modkit.Bad("'followDeck' must be a boolean")
			}
			f.state.FollowDeck = follow
		}
		note = "Settings updated."

	default:
		return nil, modkit.Bad("unknown action %q", act)
	}

	if err := modkit.SaveJSON(f.statePath, f.state); err != nil {
		return nil, err
	}
	return f.snapshot(note, clockInfo{Source: "manual"}), nil
}

// Close satisfies modkit.Feature.
func (f *Feature) Close() {}
