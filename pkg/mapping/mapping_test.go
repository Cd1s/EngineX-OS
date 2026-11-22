package mapping

import (
	"errors"
	"strings"
	"testing"
)

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want a *mapping.Error, got %v", err)
	}
	return e.Code
}

func TestValidateAcceptsAShiftAndAHoldOnAPadModeButton(t *testing.T) {
	for _, m := range []Mapping{
		{Board: "JP07", Control: "PadModeSelect:28", Gesture: "shift", Action: "waveform_cycle"},
		{Board: "JP13", Control: "PadModeSelect:27", Gesture: "hold", Action: "beat_jump_forward"},
		{Board: "JP08", Control: "PadModeSelect:28", Gesture: "double", Action: "sync"},
		{Board: "JP14", Control: "PadModeSelect:29", Gesture: "shift", Action: "censor"},
		{Board: "JP07", Control: "PadModeSelect:29", Gesture: "shift", Action: "loop_halve"},
		{Board: "JP07", Control: "PadModeSelect:29", Gesture: "shift", Action: "cue_countdown_toggle"},
	} {
		if err := Validate(m); err != nil {
			t.Errorf("%v: %v", m, err)
		}
	}
}

func TestValidateTakesAStockGestureOnlyWhereItCanBeDisabledAlone(t *testing.T) {
	// Stock's Sync hold (Instant Double, 2 s) is a module property; its binding can go alone.
	if err := Validate(Mapping{Board: "JP07", Control: "Sync:20", Gesture: "hold", Action: "waveform_cycle"}); err != nil {
		t.Fatal(err)
	}
	// Its press and shift fire stock functions that have no switch of their own.
	for _, g := range []string{"press", "shift", "double"} {
		err := Validate(Mapping{Board: "JP07", Control: "Sync:20", Gesture: g, Action: "waveform_cycle"})
		if codeOf(t, err) != ReasonStockUsed {
			t.Errorf("Sync %s: %v", g, err)
		}
	}
	// A press on a pad-mode button is the pad mode itself.
	if c := codeOf(t, Validate(Mapping{Board: "JP07", Control: "PadModeSelect:30", Gesture: "press", Action: "sync"})); c != ReasonStockUsed {
		t.Errorf("press on a pad-mode button: %s", c)
	}
}

func TestValidateNeverMapsShiftAndTheNavigationControls(t *testing.T) {
	for _, control := range []string{"Shift:26", "Shortcuts:12", "Back:16", "BrowseEncoder:18", "View:14", "Forward:17", "Media:13", "Layer:11"} {
		for _, g := range Gestures {
			err := Validate(Mapping{Board: "JP07", Control: control, Gesture: g, Action: "sync"})
			if codeOf(t, err) != ReasonReserved {
				t.Errorf("%s %s: %v", control, g, err)
			}
		}
	}
}

func TestValidateRejectsWhatIsUnknown(t *testing.T) {
	base := Mapping{Board: "JP07", Control: "PadModeSelect:28", Gesture: "shift", Action: "waveform_cycle"}
	for _, c := range []struct {
		name string
		edit func(*Mapping)
		code string
	}{
		{"unknown action", func(m *Mapping) { m.Action = "factory_reset" }, ReasonAction},
		{"empty action", func(m *Mapping) { m.Action = "" }, ReasonAction},
		{"unknown control", func(m *Mapping) { m.Control = "PadModeSelect:99" }, ReasonControl},
		{"a CC is not a button", func(m *Mapping) { m.Control = "AutoLoop:cc3" }, ReasonControl},
		{"unknown gesture", func(m *Mapping) { m.Gesture = "triple" }, ReasonGesture},
		{"another board", func(m *Mapping) { m.Board = "JC11" }, ReasonBoard},
		{"a module v1 does not offer", func(m *Mapping) { m.Control = "Censor:7" }, ReasonNotOffered},
	} {
		m := base
		c.edit(&m)
		if got := codeOf(t, Validate(m)); got != c.code {
			t.Errorf("%s: code %s, want %s", c.name, got, c.code)
		}
	}
}

func TestValidateChecksTheActionAgainstTheGesture(t *testing.T) {
	// A combo drives Action and two-state targets; a hold-type or stepped one needs a plain assignment.
	for _, action := range []string{"censor", "loop_halve", "loop_double"} {
		for _, g := range []string{"double", "hold"} {
			err := Validate(Mapping{Board: "JP07", Control: "PadModeSelect:28", Gesture: g, Action: action})
			if codeOf(t, err) != ReasonOutput {
				t.Errorf("%s on %s: %v", action, g, err)
			}
		}
	}
	for _, action := range []string{"key_lock", "slip", "quantize", "sync", "beat_jump_back", "waveform_cycle"} {
		for _, g := range []string{"shift", "double", "hold"} {
			if err := Validate(Mapping{Board: "JP07", Control: "PadModeSelect:28", Gesture: g, Action: action}); err != nil {
				t.Errorf("%s on %s: %v", action, g, err)
			}
		}
	}
}

func TestValidateAllRejectsTheSameGestureTwice(t *testing.T) {
	list := []Mapping{
		{Board: "JP07", Control: "PadModeSelect:28", Gesture: "shift", Action: "waveform_cycle"},
		{Board: "JP07", Control: "PadModeSelect:28", Gesture: "hold", Action: "sync"},
		{Board: "JP13", Control: "PadModeSelect:28", Gesture: "shift", Action: "sync"}, // another board: fine
	}
	if err := ValidateAll(list); err != nil {
		t.Fatal(err)
	}
	list = append(list, Mapping{Board: "JP07", Control: "PadModeSelect:28", Gesture: "shift", Action: "key_lock"})
	if c := codeOf(t, ValidateAll(list)); c != ReasonDuplicate {
		t.Fatalf("code %s", c)
	}
}

func TestEveryCatalogueActionIsWellFormed(t *testing.T) {
	c := Default()
	seen := map[string]bool{}
	for _, a := range c.Actions {
		if seen[a.ID] || a.ID == "" {
			t.Errorf("action id %q repeated or empty", a.ID)
		}
		seen[a.ID] = true
		for _, lang := range []string{"en", "zh-Hans", "zh-Hant", "es"} {
			if strings.TrimSpace(a.Label[lang]) == "" {
				t.Errorf("%s: no %s label", a.ID, lang)
			}
		}
		switch a.Output {
		case OutputAction, OutputToggle, OutputMomentary, OutputStep:
		default:
			t.Errorf("%s: output %q", a.ID, a.Output)
		}
		if (a.Output == OutputStep) != (a.Step == "inc" || a.Step == "dec") {
			t.Errorf("%s: step %q for output %s", a.ID, a.Step, a.Output)
		}
		switch a.Root {
		case "track", "deck":
			if a.Path == "" || strings.HasPrefix(a.Path, "/") {
				t.Errorf("%s: root %s needs a relative path", a.ID, a.Root)
			}
		case "abs":
			if !strings.HasPrefix(a.Path, "/") {
				t.Errorf("%s: abs path %q", a.ID, a.Path)
			}
		case "censor":
		default:
			t.Errorf("%s: root %q", a.ID, a.Root)
		}
		if a.Kind == "enginex" {
			if !strings.HasPrefix(a.Path, "/GUI/Scripted/EngineXMap") || a.Output != OutputAction || a.Setting == "" {
				t.Errorf("%s: an EngineX action writes an Action property under /GUI/Scripted", a.ID)
			}
		} else if a.Kind != "stock" {
			t.Errorf("%s: kind %q", a.ID, a.Kind)
		}
	}
	for _, want := range []string{"beat_jump_back", "beat_jump_forward", "loop_halve", "loop_double", "censor", "key_lock", "slip", "quantize", "sync", "waveform_cycle"} {
		if !seen[want] {
			t.Errorf("the v1 catalogue lacks %s", want)
		}
	}
}

func TestEveryModuleRuleNamesRealControlsAndGestures(t *testing.T) {
	c := Default()
	for board := range inventory {
		inv, _ := Board(board)
		modules := map[string]bool{}
		for _, ctl := range inv.Controls {
			modules[ctl.Module] = true
		}
		for name, rule := range c.Modules {
			if !modules[name] {
				t.Errorf("%s: catalogue names module %s that the board does not have", board, name)
			}
			if rule.Reserved {
				continue
			}
			for g, r := range rule.Gestures {
				if !validGesture(g) {
					t.Errorf("%s %s: gesture %q", board, name, g)
				}
				if r.Status == StatusTakeover && (r.NoteProperty == "" || len(r.Set) == 0) {
					t.Errorf("%s %s %s: a takeover needs the note property and what to set", board, name, g)
				}
			}
		}
	}
}

func TestParseReadsTheFileAndRejectsWhatItDoesNotKnow(t *testing.T) {
	good := `[{"board":"JP07","control":"PadModeSelect:28","gesture":"shift","action":"waveform_cycle"}]`
	list, err := Parse(strings.NewReader(good))
	if err != nil || len(list) != 1 || list[0].Action != "waveform_cycle" {
		t.Fatalf("%v %v", list, err)
	}
	for name, body := range map[string]string{
		"an unknown field":    `[{"board":"JP07","control":"PadModeSelect:28","gesture":"shift","action":"sync","x":1}]`,
		"an object":           `{"board":"JP07"}`,
		"two values":          good + good,
		"an unknown action":   `[{"board":"JP07","control":"PadModeSelect:28","gesture":"shift","action":"nope"}]`,
		"not JSON":            `nope`,
		"a repeated gesture":  strings.Replace(good, "]", ",", 1) + good[1:],
		"a number for a name": `[{"board":7}]`,
	} {
		if _, err := Parse(strings.NewReader(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if list, err := Parse(strings.NewReader("  \n")); err != nil || len(list) != 0 {
		t.Errorf("an empty file is no mappings: %v %v", list, err)
	}
	if _, err := Parse(strings.NewReader(strings.Repeat(" ", MaxFileBytes+1))); err == nil {
		t.Error("an oversized file was accepted")
	}
}

func TestSaveWritesAtomicallyAndAnEmptyListRemovesTheFile(t *testing.T) {
	path := t.TempDir() + "/data/mappings.json"
	list := []Mapping{
		{Board: "JP07", Control: "PadModeSelect:28", Gesture: "shift", Action: "waveform_cycle"},
		{Board: "JP07", Control: "PadModeSelect:28", Gesture: "hold", Action: "sync"},
	}
	if err := Save(path, list); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil || len(back) != 2 || back[1].Gesture != "hold" {
		t.Fatalf("%v %v", back, err)
	}
	if err := Save(path, []Mapping{{Board: "JP07", Control: "Shift:26", Gesture: "press", Action: "sync"}}); err == nil {
		t.Fatal("a reserved control was saved")
	}
	if back, _ := Load(path); len(back) != 2 {
		t.Fatal("a rejected save changed the file")
	}
	if err := Save(path, nil); err != nil {
		t.Fatal(err)
	}
	if back, err := Load(path); err != nil || len(back) != 0 {
		t.Fatalf("%v %v", back, err)
	}
}

// Shift + LOOP belongs to EngineX's Beat Jump pad mode (another lane replaces the pad modules in the UI
// pack): it is taken, on every board, whatever the action, and the way to say so is its own reason.
func TestValidateRefusesShiftLoopBecauseTheBeatJumpPadModeOwnsIt(t *testing.T) {
	for _, board := range []string{"JP07", "JP08", "JP13", "JP14"} {
		for _, action := range []string{"waveform_cycle", "sync", "key_lock", "beat_jump_forward"} {
			err := Validate(Mapping{Board: board, Control: "PadModeSelect:30", Gesture: "shift", Action: action})
			if codeOf(t, err) != ReasonTaken || !strings.Contains(err.Error(), "Beat Jump pad mode") {
				t.Errorf("%s %s: %v", board, action, err)
			}
		}
		if o := Default().Offer(board, "PadModeSelect:30", "shift"); o.Mappable || o.Status != StatusTaken || o.Reason != ReasonTaken {
			t.Errorf("%s: %+v", board, o)
		}
		// only that gesture: the other pad-mode buttons and the other gestures of LOOP are untouched
		for _, c := range []struct{ control, gesture string }{{"PadModeSelect:30", "hold"}, {"PadModeSelect:30", "double"}, {"PadModeSelect:27", "shift"}, {"PadModeSelect:28", "shift"}, {"PadModeSelect:29", "shift"}} {
			if !Default().Offer(board, c.control, c.gesture).Mappable {
				t.Errorf("%s %s %s must stay mappable", board, c.control, c.gesture)
			}
		}
	}
	if by, ok := Default().Taken("PadModeSelect:30", "shift"); !ok || by != "Beat Jump pad mode" {
		t.Errorf("%q %v", by, ok)
	}
	if _, ok := Default().Taken("PadModeSelect:30", "hold"); ok {
		t.Error("hold on LOOP is not taken")
	}
	// the override names real controls
	for control, gestures := range Default().Controls {
		for _, board := range Default().Boards {
			inv, _ := Board(board)
			if _, ok := inv.Find(control); !ok {
				t.Errorf("%s: the catalogue overrides %s, which the board does not have", board, control)
			}
		}
		for g := range gestures {
			if !validGesture(g) {
				t.Errorf("%s: gesture %q", control, g)
			}
		}
	}
}
