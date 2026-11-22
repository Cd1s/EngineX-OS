// Package mapping is the button mapping of the SC5000 / SC6000 players: a DJ maps a
// stock control and gesture to a function. It holds the one catalogue of functions,
// the rules for which gestures may be taken, the validation of /data/enginex/mappings.json,
// the generator that turns the file into stock assignment primitives at Engine start,
// the static check a generated file must pass, and the classifier behind MIDI learn.
package mapping

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed catalogue.json data/*.json
var assets embed.FS

// The four gestures a mapping can use.
const (
	GesturePress  = "press"
	GestureShift  = "shift"
	GestureDouble = "double"
	GestureHold   = "hold"
)

// Gestures lists them in the order the console shows them.
var Gestures = []string{GesturePress, GestureShift, GestureDouble, GestureHold}

// Output types of an action: how the stock primitive treats its target.
const (
	OutputAction    = "action"    // an Action property: state = true triggers it
	OutputToggle    = "toggle"    // a two-state property: each press flips it
	OutputMomentary = "momentary" // a hold-type property: on while the button is down
	OutputStep      = "step"      // a stepped value changed by IncDecValueOutput
)

// Status of a gesture on a control in stock.
const (
	StatusFree     = "free"     // stock leaves the gesture unused
	StatusTakeover = "takeover" // stock uses it, and its binding can be disabled alone
	StatusStock    = "stock"    // stock uses it and v1 cannot take it
	StatusReserved = "reserved" // never mappable: Shift and the navigation controls
	StatusTaken    = "taken"    // an EngineX function owns it (Shift + LOOP: the Beat Jump pad mode)
)

// Action is one function a mapping can run.
type Action struct {
	ID      string            `json:"id"`
	Kind    string            `json:"kind"`              // stock | enginex
	Root    string            `json:"root"`              // track | deck | abs | censor
	Path    string            `json:"path,omitempty"`    // relative to the root, or absolute for abs
	Output  string            `json:"output"`            // OutputAction ...
	Step    string            `json:"step,omitempty"`    // inc | dec, for OutputStep
	Setting string            `json:"setting,omitempty"` // the EngineX setting an enginex action changes
	Label   map[string]string `json:"label"`
}

// Takeover says how the stock binding of one gesture is switched off: a property of
// the module instance in the assignment file is set, which stock's own module reads.
type Takeover struct {
	NoteProperty string            `json:"noteProperty"` // the instance property that holds the control's note
	Set          map[string]string `json:"set"`
}

// GestureRule is stock's use of one gesture of a module.
type GestureRule struct {
	Status string `json:"status"`
	By     string `json:"by,omitempty"` // for StatusTaken: what owns the gesture
	Takeover
}

// ModuleRule is what v1 knows about a stock module.
type ModuleRule struct {
	Reserved bool                   `json:"reserved"`
	Scope    string                 `json:"scope"`
	Gestures map[string]GestureRule `json:"gestures"`
}

// Timing are the thresholds stock's ComboActionOutput uses, and the learn window.
type Timing struct {
	HoldMS       int `json:"holdMs"`
	DoubleMS     int `json:"doubleMs"`
	LearnSeconds int `json:"learnSeconds"`
}

// Catalogue is catalogue.json.
type Catalogue struct {
	Version int                   `json:"version"`
	Boards  []string              `json:"boards"`
	Timing  Timing                `json:"timing"`
	Actions []Action              `json:"actions"`
	Modules map[string]ModuleRule `json:"modules"`
	// Controls overrides one control's gesture, by inventory id: what EngineX itself puts
	// on a control (the inventory holds stock's functions only).
	Controls map[string]map[string]GestureRule `json:"controls"`
}

// Control is one row of the inventory of a board (tools/mapping/inventory.py).
type Control struct {
	ID          string `json:"id"`
	Control     string `json:"control"`
	Module      string `json:"module"`
	Type        string `json:"type"`
	Channel     int    `json:"channel"`
	Number      any    `json:"number"`
	Press       string `json:"press"`
	ShiftPress  string `json:"shift_press"`
	LongPress   string `json:"long_press"`
	DoublePress string `json:"double_press"`
	Config      string `json:"config"`
	SourceFile  string `json:"source_file"`
}

// Note is the MIDI note of a button; ok is false for anything that is not a note.
func (c Control) Note() (int, bool) {
	if c.Type != "Note" {
		return 0, false
	}
	switch n := c.Number.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case int:
		return n, true
	}
	return 0, false
}

// Inventory is the committed inventory file of one board.
type Inventory struct {
	Board    string    `json:"board"`
	Controls []Control `json:"controls"`
}

// Find returns the control with this id.
func (inv *Inventory) Find(id string) (Control, bool) {
	for _, c := range inv.Controls {
		if c.ID == id {
			return c, true
		}
	}
	return Control{}, false
}

// ByNote returns every button control that listens to the note.
func (inv *Inventory) ByNote(note int) []Control {
	var out []Control
	for _, c := range inv.Controls {
		if n, ok := c.Note(); ok && n == note {
			out = append(out, c)
		}
	}
	return out
}

// ShiftNote is the note of the Shift button.
func (inv *Inventory) ShiftNote() (int, bool) {
	for _, c := range inv.Controls {
		if c.Module == "Shift" {
			return c.Note()
		}
	}
	return 0, false
}

var (
	loadOnce  sync.Once
	loaded    *Catalogue
	loadErr   error
	inventory = map[string]*Inventory{}
)

func load() {
	b, err := assets.ReadFile("catalogue.json")
	if err != nil {
		loadErr = err
		return
	}
	var c Catalogue
	if err := json.Unmarshal(b, &c); err != nil {
		loadErr = fmt.Errorf("catalogue.json: %w", err)
		return
	}
	for _, board := range c.Boards {
		raw, err := assets.ReadFile("data/" + board + ".json")
		if err != nil {
			loadErr = err
			return
		}
		var inv Inventory
		if err := json.Unmarshal(raw, &inv); err != nil {
			loadErr = fmt.Errorf("data/%s.json: %w", board, err)
			return
		}
		inventory[board] = &inv
	}
	loaded = &c
}

// Default is the catalogue that ships in the binary. It panics only if the embedded
// files are broken, which the tests rule out.
func Default() *Catalogue {
	loadOnce.Do(load)
	if loadErr != nil {
		panic("mapping: " + loadErr.Error())
	}
	return loaded
}

// Board returns the inventory of a supported board.
func Board(board string) (*Inventory, bool) {
	Default()
	inv, ok := inventory[board]
	return inv, ok
}

// Action looks an action up by id.
func (c *Catalogue) Action(id string) (Action, bool) {
	for _, a := range c.Actions {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// Supports says whether the board is one of the four players.
func (c *Catalogue) Supports(board string) bool {
	for _, b := range c.Boards {
		if b == board {
			return true
		}
	}
	return false
}

// Reason codes of a gesture that cannot be mapped.
const (
	ReasonBoard      = "board"
	ReasonControl    = "control"
	ReasonGesture    = "gesture"
	ReasonReserved   = "reserved"
	ReasonTaken      = "taken"
	ReasonStockUsed  = "stock-used"
	ReasonNotOffered = "not-offered"
	ReasonAction     = "action"
	ReasonOutput     = "output"
	ReasonDuplicate  = "duplicate"
)

// Offer is what the console and the validation know about one control and gesture.
type Offer struct {
	Mappable bool   `json:"mappable"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

// Offer says whether the gesture of a control may be mapped, and why not.
func (c *Catalogue) Offer(board, control, gesture string) Offer {
	if !c.Supports(board) {
		return Offer{Status: StatusStock, Reason: ReasonBoard}
	}
	if !validGesture(gesture) {
		return Offer{Status: StatusStock, Reason: ReasonGesture}
	}
	inv, _ := Board(board)
	ctl, ok := inv.Find(control)
	if !ok {
		return Offer{Status: StatusStock, Reason: ReasonControl}
	}
	if _, isNote := ctl.Note(); !isNote {
		return Offer{Status: StatusStock, Reason: ReasonControl}
	}
	rule, known := c.Modules[ctl.Module]
	if !known {
		return Offer{Status: StatusStock, Reason: ReasonNotOffered}
	}
	if rule.Reserved {
		return Offer{Status: StatusReserved, Reason: ReasonReserved}
	}
	if o, ok := c.Controls[control][gesture]; ok && o.Status == StatusTaken {
		return Offer{Status: StatusTaken, Reason: ReasonTaken}
	}
	g, ok := rule.Gestures[gesture]
	if !ok {
		return Offer{Status: StatusStock, Reason: ReasonNotOffered}
	}
	switch g.Status {
	case StatusFree, StatusTakeover:
		return Offer{Mappable: true, Status: g.Status}
	}
	return Offer{Status: StatusStock, Reason: ReasonStockUsed}
}

// AllowsGesture says whether an action can be run by the gesture. A combo
// (double, hold) drives Action and two-state targets only; a hold-type or stepped
// target needs a plain note assignment (press, shift).
func (a Action) AllowsGesture(gesture string) bool {
	switch a.Output {
	case OutputAction, OutputToggle:
		return validGesture(gesture)
	case OutputMomentary, OutputStep:
		return gesture == GesturePress || gesture == GestureShift
	}
	return false
}

func validGesture(g string) bool {
	for _, x := range Gestures {
		if x == g {
			return true
		}
	}
	return false
}

// ActionIDs lists the action ids, sorted.
func (c *Catalogue) ActionIDs() []string {
	ids := make([]string, 0, len(c.Actions))
	for _, a := range c.Actions {
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	return ids
}

func (m Mapping) String() string {
	return strings.Join([]string{m.Board, m.Control, m.Gesture, m.Action}, "/")
}

// Taken says what owns a gesture of a control that EngineX has taken, if anything.
func (c *Catalogue) Taken(control, gesture string) (string, bool) {
	if g, ok := c.Controls[control][gesture]; ok && g.Status == StatusTaken {
		return g.By, true
	}
	return "", false
}
