package mapping

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fixtureStock is shaped like the stock JP07 file: the same module instances, the same
// indentation, the same root-level Component.onCompleted. It is written for the tests;
// the golden tests below read the real files from the firmware lab when ENGINEX_LAB is set.
const fixtureStock = `import airAssignments 1.0
import ControlSurfaceModules 0.1
import QtQuick 2.12

MidiAssignment {
    objectName: "SC5000 Controller Assignment"

    GlobalAssignmentConfig {
        id: globalConfig
        midiChannel: 0
    }

    Shift {
        note: 26
        ledType: LedType.Simple
    }

    DeckAssignmentConfig {
        id: deckConfig
        midiChannel: globalConfig.midiChannel
    }

    Sync {
        syncNote: 20
        syncHoldAction: Action.InstantDouble
        masterNote: 21
    }

    PadModeSelect {
        ledType: LedType.RGB
        buttonsModel: ListModel {
            ListElement {
                note: 27
            }
            ListElement {
                note: 30
            }
            ListElement {
                note: 28
            }
            ListElement {
                note: 29
            }
        }
    }

    Component.onCompleted: {
        // Set front light pipe to dim
        Midi.sendNoteOn(globalConfig.midiChannel, 48, 1)
    }
}
`

var fixtureMappings = []Mapping{
	{Board: "JP07", Control: "PadModeSelect:27", Gesture: "shift", Action: "waveform_cycle"},
	{Board: "JP07", Control: "PadModeSelect:27", Gesture: "hold", Action: "beat_jump_forward"},
	{Board: "JP07", Control: "PadModeSelect:28", Gesture: "double", Action: "sync"},
	{Board: "JP07", Control: "PadModeSelect:29", Gesture: "shift", Action: "loop_halve"},
	{Board: "JP07", Control: "PadModeSelect:29", Gesture: "hold", Action: "key_lock"},
	{Board: "JP07", Control: "PadModeSelect:28", Gesture: "shift", Action: "loop_double"},
	{Board: "JP07", Control: "PadModeSelect:30", Gesture: "hold", Action: "slip"},
	{Board: "JP07", Control: "Sync:20", Gesture: "hold", Action: "cue_countdown_toggle"},
}

func mustGenerate(t *testing.T, board, stock string, list []Mapping) string {
	t.Helper()
	out, err := Generate(board, []byte(stock), list)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return string(out)
}

func lines(s string) []string { return strings.Split(strings.TrimSuffix(s, "\n"), "\n") }

func TestGenerateWritesStockPrimitivesForEachGesture(t *testing.T) {
	out := mustGenerate(t, "JP07", fixtureStock, fixtureMappings)
	for _, want := range []string{
		"import InputAssignment 0.1\n",
		// shift: a plain note assignment, enabled only while Shift is held
		`    ValueNoteAssignment {
        objectName: "EngineX Map PadModeSelect:27 shift"
        channel: deckConfig.midiChannel
        note: 27
        enabled: globalConfig.shift && deckConfig.inDJMode
        output: ActionOutput {
            target: PropertyTarget {
                path: "/GUI/Scripted/EngineXMapWaveform"
            }
        }
    }
`,
		// hold: stock's ComboActionOutput, unshifted
		`    ValueNoteAssignment {
        objectName: "EngineX Map PadModeSelect:27 hold"
        channel: deckConfig.midiChannel
        note: 27
        enabled: !globalConfig.shift && deckConfig.inDJMode
        output: ComboActionOutput {
            holdEnabled: true
            holdTarget: PropertyTarget {
                path: deckConfig.activeDeckTrackPath + "BeatJump/JumpForward"
            }
        }
    }
`,
		`        output: ComboActionOutput {
            doubleEnabled: true
            doubleTarget: PropertyTarget {
                path: deckConfig.activeDeckPath + "Sync"
            }
        }
`,
		// a stepped value: stock's IncDecValueOutput with the arguments AutoLoopButtons passes
		`        output: IncDecValueOutput {
            target: PropertyTarget {
                path: deckConfig.activeDeckTrackPath + "LoopHalveDouble"
            }
            changeAmount: 0.1
            increaseValue: false
            roundRobin: false
            minimum: 0.0
            maximum: 1.0
        }
`,
		`            increaseValue: true
            roundRobin: false
            minimum: 1.0
            maximum: 0.0
`,
		// a Slip toggle on a hold, through the combo
		`        output: ComboActionOutput {
            holdEnabled: true
            holdTarget: PropertyTarget {
                path: deckConfig.activeDeckTrackPath + "SlipMode"
            }
        }
`,
		// stock's Sync hold is switched off, alone
		"        syncHoldAction: Action.None\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing:\n%s\nin:\n%s", want, out)
		}
	}
	if strings.Contains(out, "InstantDouble") {
		t.Error("stock's Sync hold binding is still there")
	}
	if n := strings.Count(out, "import InputAssignment 0.1"); n != 1 {
		t.Errorf("%d InputAssignment imports", n)
	}
	for _, l := range lines(out) {
		if regexp.MustCompile(`^\s*on[A-Z][A-Za-z]*\s*:`).MatchString(l) {
			t.Errorf("a handler breaks the on-disk loader: %q", l)
		}
		if strings.Contains(l, "function") {
			t.Errorf("a function: %q", l)
		}
	}
}

// Censor is a hold-type target: it keeps stock's own path expression and runs from a plain note assignment.
func TestGenerateCensorKeepsStocksOwnPathAndShiftGuard(t *testing.T) {
	list := []Mapping{{Board: "JP07", Control: "PadModeSelect:29", Gesture: "shift", Action: "censor"}}
	out := mustGenerate(t, "JP07", fixtureStock, list)
	for _, want := range []string{
		"        enabled: globalConfig.shift && deckConfig.inDJMode\n        output: ActionOutput {\n",
		`                path: deckConfig.censorIsSilentCue ? deckConfig.activeDeckPath + "SilentCue" : deckConfig.activeDeckTrackPath + "Bleep"` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if err := Check("JP07", []byte(fixtureStock), []byte(out), list); err != nil {
		t.Fatal(err)
	}
	// and a Shift + LOOP mapping never reaches the generator
	if _, err := Generate("JP07", []byte(fixtureStock), []Mapping{{Board: "JP07", Control: "PadModeSelect:30", Gesture: "shift", Action: "sync"}}); codeOf(t, err) != ReasonTaken {
		t.Fatalf("%v", err)
	}
}

func TestGenerateKeepsEveryStockBindingExceptTheTakenGesture(t *testing.T) {
	out := mustGenerate(t, "JP07", fixtureStock, fixtureMappings)
	var rest []string
	in := false
	for _, l := range lines(out) {
		switch {
		case strings.Contains(l, "EngineX button mapping: begin"):
			in = true
		case strings.Contains(l, "EngineX button mapping: end"):
			in = false
		case in, l == "import InputAssignment 0.1":
		case l == "        syncHoldAction: Action.None":
			rest = append(rest, "        syncHoldAction: Action.InstantDouble")
		default:
			rest = append(rest, l)
		}
	}
	if got := strings.Join(rest, "\n") + "\n"; got != fixtureStock {
		t.Fatalf("stock changed beyond the taken gesture:\n%s", got)
	}
}

func TestGenerateTheRegionSitsInTheRootBeforeComponentOnCompleted(t *testing.T) {
	out := mustGenerate(t, "JP07", fixtureStock, fixtureMappings)
	begin := strings.Index(out, "    // EngineX button mapping: begin")
	end := strings.Index(out, "    // EngineX button mapping: end")
	if begin < 0 || end < begin || end > strings.Index(out, "    Component.onCompleted: {") {
		t.Fatal("the generated assignments must be root children placed before Component.onCompleted")
	}
	if err := Check("JP07", []byte(fixtureStock), []byte(out), fixtureMappings); err != nil {
		t.Fatal(err)
	}
	// without a Component.onCompleted the region goes before the root's closing brace
	bare := strings.Replace(fixtureStock, `    Component.onCompleted: {
        // Set front light pipe to dim
        Midi.sendNoteOn(globalConfig.midiChannel, 48, 1)
    }
`, "", 1)
	out = mustGenerate(t, "JP07", bare, fixtureMappings[:1])
	if !strings.HasSuffix(out, "    }\n    // EngineX button mapping: end\n}\n") {
		t.Fatalf("tail:\n%s", out[len(out)-200:])
	}
	if err := Check("JP07", []byte(bare), []byte(out), fixtureMappings[:1]); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateWithoutMappingsLeavesTheFileAlone(t *testing.T) {
	if _, err := Generate("JP07", []byte(fixtureStock), nil); !errors.Is(err, ErrNothingToApply) {
		t.Fatalf("no mappings: %v", err)
	}
	other := []Mapping{{Board: "JP13", Control: "PadModeSelect:28", Gesture: "shift", Action: "sync"}}
	if _, err := Generate("JP07", []byte(fixtureStock), other); !errors.Is(err, ErrNothingToApply) {
		t.Fatalf("another board's mappings: %v", err)
	}
}

func TestGenerateRefusesWhatCannotBeDoneExactly(t *testing.T) {
	bad := func(name string, stock string, list []Mapping) {
		t.Helper()
		if out, err := Generate("JP07", []byte(stock), list); err == nil || out != nil {
			t.Errorf("%s: no error", name)
		}
	}
	shift := Mapping{Board: "JP07", Control: "PadModeSelect:28", Gesture: "shift", Action: "sync"}
	bad("an unknown action", fixtureStock, []Mapping{{Board: "JP07", Control: "PadModeSelect:28", Gesture: "shift", Action: "nope"}})
	bad("a reserved control", fixtureStock, []Mapping{{Board: "JP07", Control: "Shift:26", Gesture: "press", Action: "sync"}})
	bad("a stock gesture without a switch", fixtureStock, []Mapping{{Board: "JP07", Control: "Sync:20", Gesture: "press", Action: "waveform_cycle"}})
	bad("the module instance missing from the file", strings.Replace(fixtureStock, "syncNote: 20", "syncNote: 99", 1),
		[]Mapping{{Board: "JP07", Control: "Sync:20", Gesture: "hold", Action: "waveform_cycle"}})
	bad("a file that is not an assignment file", "Item {}\n", []Mapping{shift})
	out := mustGenerate(t, "JP07", fixtureStock, []Mapping{shift})
	bad("a file that was generated before", out, []Mapping{shift})
}

func TestGenerateAddsTheSwitchLineWhenTheInstanceDoesNotSpellItOut(t *testing.T) {
	stock := strings.Replace(fixtureStock, "        syncHoldAction: Action.InstantDouble\n", "", 1)
	out := mustGenerate(t, "JP07", stock, []Mapping{{Board: "JP07", Control: "Sync:20", Gesture: "hold", Action: "sync"}})
	if !strings.Contains(out, "        syncNote: 20\n        syncHoldAction: Action.None\n        masterNote: 21\n") {
		t.Fatalf("the property goes right after the note line:\n%s", out)
	}
	if err := Check("JP07", []byte(stock), []byte(out), []Mapping{{Board: "JP07", Control: "Sync:20", Gesture: "hold", Action: "sync"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRejectsAFileThatIsNotStockPlusTheMappings(t *testing.T) {
	list := fixtureMappings
	good := mustGenerate(t, "JP07", fixtureStock, list)
	if err := Check("JP07", []byte(fixtureStock), []byte(good), list); err != nil {
		t.Fatalf("the generator's own file: %v", err)
	}
	region := func(s string) (int, int) {
		return strings.Index(s, "    // EngineX button mapping: begin"), strings.Index(s, "    // EngineX button mapping: end")
	}
	b, e := region(good)
	for name, edit := range map[string]func(string) string{
		"a handler": func(s string) string {
			return strings.Replace(s, "        note: 30\n", "        note: 30\n        onEnabledChanged: { }\n", 1)
		},
		"a stock handler":    func(s string) string { return strings.Replace(s, "    Shift {", "    onFoo: 1\n    Shift {", 1) },
		"an unknown element": func(s string) string { return strings.Replace(s, "ActionOutput {", "ScriptOutput {", 1) },
		"a changed path": func(s string) string {
			return strings.Replace(s, `"/GUI/Scripted/EngineXMapWaveform"`, `"/Engine/Deck1/Play"`, 1)
		},
		"a changed note": func(s string) string {
			return strings.Replace(s, "        note: 27\n        enabled", "        note: 26\n        enabled", 1)
		},
		"a deleted stock line": func(s string) string { return strings.Replace(s, "        ledType: LedType.Simple\n", "", 1) },
		"a changed stock line": func(s string) string { return strings.Replace(s, "note: 26", "note: 27", 1) },
		"a second import": func(s string) string {
			return strings.Replace(s, "import QtQuick 2.12\n", "import QtQuick 2.12\nimport QtQml 2.12\n", 1)
		},
		"a missing closing brace": func(s string) string { return strings.TrimSuffix(s, "}\n") },
		"an extra assignment": func(s string) string {
			return s[:e] + "    ValueNoteAssignment {\n        note: 26\n    }\n" + s[e:]
		},
		"a dropped assignment": func(s string) string {
			i := strings.Index(s[b:], "    ValueNoteAssignment {") + b
			j := strings.Index(s[i:], "\n    }\n") + i + len("\n    }\n")
			return s[:i] + s[j:]
		},
		"the region inside another object": func(s string) string {
			cut := s[b : e+len("    // EngineX button mapping: end\n")]
			s = strings.Replace(s, cut, "", 1)
			return strings.Replace(s, "        id: globalConfig\n", "        id: globalConfig\n"+cut, 1)
		},
		"a real Sync hold left in": func(s string) string {
			return strings.Replace(s, "syncHoldAction: Action.None", "syncHoldAction: Action.InstantDouble", 1)
		},
		"a taken property changed alone": func(s string) string {
			return strings.Replace(s, "syncHoldAction: Action.None", "syncHoldAction: Action.KeySync", 1)
		},
	} {
		err := Check("JP07", []byte(fixtureStock), []byte(edit(good)), list)
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		t.Logf("%s: %v", name, err)
	}
	if err := Check("JP07", []byte(fixtureStock), []byte(fixtureStock), list); err == nil {
		t.Error("a file with none of the mappings was accepted")
	}
}

// qmlformat is a real QML parser. Where it is installed it must accept what the generator writes.
func TestGeneratedFileParsesWithQmlformat(t *testing.T) {
	bin := qtTool("qmlformat")
	if bin == "" {
		t.Skip("qmlformat is not installed")
	}
	parses(t, bin, "stock", fixtureStock)
	parses(t, bin, "generated", mustGenerate(t, "JP07", fixtureStock, fixtureMappings))
	// the parser is strict enough to catch a broken file
	broken := strings.Replace(mustGenerate(t, "JP07", fixtureStock, fixtureMappings), "    }\n    // EngineX button mapping: end", "    // EngineX button mapping: end", 1)
	if err := exec.Command(bin, writeQML(t, broken)).Run(); err == nil {
		t.Error("qmlformat accepted a file with a missing brace")
	}
}

func qtTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/lib/qt6/bin", "/usr/lib/x86_64-linux-gnu/qt6/bin"} {
		if p := filepath.Join(dir, name); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func writeQML(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Assignments.qml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func parses(t *testing.T, bin, what, text string) {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command(bin, writeQML(t, text))
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("qmlformat rejects the %s file: %v\n%s", what, err, stderr.String())
	}
}
