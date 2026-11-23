package mapping

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files from the firmware lab")

// The lab trees are the unpacked 5.3.0 firmware: the stock assignment files and the qrc
// modules they use. They are not in the repository (vendor files); set ENGINEX_LAB to the
// lab root to run these tests.
func lab(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("ENGINEX_LAB")
	if dir == "" {
		t.Skip("ENGINEX_LAB (the firmware lab root) is not set")
	}
	return filepath.Join(dir, "5.3.0")
}

func stockFile(t *testing.T, board string) []byte {
	t.Helper()
	p := filepath.Join(lab(t), "tree/usr/Engine/AssignmentFiles/PresetAssignmentFiles", board, board+"_Controller_Assignments.qml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", board, err)
	}
	return b
}

func labMappings(board string) []Mapping {
	var out []Mapping
	for _, m := range fixtureMappings {
		m.Board = board
		out = append(out, m)
	}
	return out
}

// lineDiff is a unified-style line diff with no context: "+ line" added, "- line" removed.
func lineDiff(a, b []string) []string {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, "+ "+b[j])
			j++
		default:
			out = append(out, "- "+a[i])
			i++
		}
	}
	return out
}

func TestGoldenPatchedFilesOfTheLabTrees(t *testing.T) {
	for _, board := range []string{"JP07", "JP13", "JP08", "JP14"} {
		t.Run(board, func(t *testing.T) {
			stock := stockFile(t, board)
			list := labMappings(board)
			out, err := Generate(board, stock, list)
			if err != nil {
				t.Fatal(err)
			}
			diff := lineDiff(strings.Split(string(stock), "\n"), strings.Split(string(out), "\n"))
			var removed []string
			for _, l := range diff {
				if strings.HasPrefix(l, "- ") {
					removed = append(removed, l)
				}
			}
			// The v1 catalogue only adds free pad-mode gestures, so stock bindings stay intact.
			if len(removed) != 0 {
				t.Errorf("stock lines removed: %q", removed)
			}
			text := strings.Join(diff, "\n") + "\n"
			golden := filepath.Join("testdata", "golden", board+".diff")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("no golden file (run with -update): %v", err)
			}
			if string(want) != text {
				t.Errorf("the patch of %s differs from %s; run with -update if the change is meant\n%s", board, golden, text)
			}
			if err := Check(board, stock, out, list); err != nil {
				t.Errorf("the static check: %v", err)
			}
			if bin := qtTool("qmlformat"); bin != "" {
				parses(t, bin, board+" stock", string(stock))
				parses(t, bin, board+" generated", string(out))
			}
			if regexp.MustCompile(`(?m)^\s*on[A-Z]\w*\s*:`).Match(out) {
				t.Error("a handler in the generated file")
			}
		})
	}
}

// Every element and property the generator writes exists in the 5.3.0 qrc: assignment files are
// not loaded off the deck, so the real component files are the evidence.
func TestEveryOfferedGestureGeneratesForEveryLabBoard(t *testing.T) {
	cat := Default()
	for _, board := range cat.Boards {
		t.Run(board, func(t *testing.T) {
			inv, ok := Board(board)
			if !ok {
				t.Fatal("missing inventory")
			}
			var list []Mapping
			for _, control := range inv.Controls {
				for _, gesture := range Gestures {
					if cat.Offer(board, control.ID, gesture).Mappable {
						list = append(list, Mapping{Board: board, Control: control.ID, Gesture: gesture, Action: "sync"})
					}
				}
			}
			if len(list) == 0 {
				t.Fatal("no offered gestures")
			}
			if _, err := Generate(board, stockFile(t, board), list); err != nil {
				t.Fatalf("offered gestures must generate: %v", err)
			}
		})
	}
}

func TestGeneratedElementsExistInTheQrc(t *testing.T) {
	root := lab(t)
	qrc := filepath.Join(root, "qrc")
	props := func(files ...string) map[string]bool {
		set := map[string]bool{"objectName": true, "enabled": true}
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(qrc, "airAssignments/InputAssignment", f+".qml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range regexp.MustCompile(`(?m)^\s*(?:readonly\s+)?property\s+(?:alias\s+)?(?:[\w.]+\s+)?(\w+)\s*[:;]?`).FindAllStringSubmatch(string(b), -1) {
				set[m[1]] = true
			}
		}
		return set
	}
	allowed := map[string]map[string]bool{
		"ValueNoteAssignment": props("ValueNoteAssignment", "NoteAssignment", "InputAssignment"),
		"ActionOutput":        props("ActionOutput"),
		"ComboActionOutput":   props("ComboActionOutput"),
		"IncDecValueOutput":   props("IncDecValueOutput"),
		"PropertyTarget":      props("PropertyTarget"),
	}
	qmldir, err := os.ReadFile(filepath.Join(qrc, "airAssignments/InputAssignment/qmldir"))
	if err != nil {
		t.Fatal(err)
	}
	for name := range allowed {
		if !strings.Contains(string(qmldir), name+" 0.1 ") {
			t.Errorf("%s is not exported by the InputAssignment module", name)
		}
	}
	rs, err := resolve("JP07", labMappings("JP07"))
	if err != nil {
		t.Fatal(err)
	}
	stack := []string{}
	for _, l := range strings.Split(region(rs), "\n") {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "//") || trim == "" {
			continue
		}
		if m := regexp.MustCompile(`^(?:\w+:\s*)?([A-Z]\w+) \{$`).FindStringSubmatch(trim); m != nil {
			if _, ok := allowed[m[1]]; !ok {
				t.Errorf("element %s is not one of the generator's", m[1])
			}
			stack = append(stack, m[1])
			continue
		}
		if trim == "}" {
			stack = stack[:len(stack)-1]
			continue
		}
		key := strings.SplitN(trim, ":", 2)[0]
		if len(stack) == 0 {
			continue
		}
		if !allowed[stack[len(stack)-1]][key] {
			t.Errorf("%s has no property %q in the 5.3.0 qrc", stack[len(stack)-1], key)
		}
	}
	// ComboActionOutput's and IncDecValueOutput's own parameters, by name.
	for _, want := range []struct{ file, text string }{
		{"airAssignments/InputAssignment/ComboActionOutput.qml", "property QtObject holdTarget"},
		{"airAssignments/InputAssignment/ComboActionOutput.qml", "property QtObject doubleTarget"},
		{"airAssignments/InputAssignment/IncDecValueOutput.qml", "t.incValue( changeAmount, maximum, minimum, roundRobin, false);"},
		{"airAssignments/InputAssignment/IncDecValueOutput.qml", "t.decValue( changeAmount, minimum, maximum, roundRobin, false);"},
		{"ControlSurfaceModules/ControlSurfaceModules/private/AutoLoopButtons.qml", "decValue(0.1, 0.0, 1.0, false, false)"},
		{"ControlSurfaceModules/ControlSurfaceModules/private/AutoLoopButtons.qml", "incValue(0.1, 0.0, 1.0, false, false)"},
	} {
		b, err := os.ReadFile(filepath.Join(qrc, want.file))
		if err != nil || !strings.Contains(string(b), want.text) {
			t.Errorf("%s no longer says %q", want.file, want.text)
		}
	}
}

// The catalogue's targets are the ones stock's own modules write, and the takeover switches
// are properties stock's own module reads.
func TestCatalogueMatchesTheStockModules(t *testing.T) {
	modules := filepath.Join(lab(t), "qrc/ControlSurfaceModules/ControlSurfaceModules")
	var all strings.Builder
	err := filepath.Walk(modules, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".qml") {
			b, _ := os.ReadFile(p)
			all.Write(b)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	text := all.String()
	for _, a := range Default().Actions {
		if a.Kind != "stock" {
			continue
		}
		var needle string
		switch a.Root {
		case "censor":
			needle = "Bleep"
		default:
			needle = strings.TrimPrefix(a.Path, "/Engine/Master/")
			needle = strings.TrimPrefix(needle, "/Engine/")
		}
		if !strings.Contains(text, needle) {
			t.Errorf("%s: stock's modules never mention %q", a.ID, needle)
		}
	}
	sync, err := os.ReadFile(filepath.Join(modules, "Sync.qml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"property int syncHoldAction: Action.None", "holdEnabled: syncHoldAction !== Action.None"} {
		if !strings.Contains(string(sync), want) {
			t.Errorf("Sync.qml no longer says %q", want)
		}
	}
	action, err := os.ReadFile(filepath.Join(modules, "Action.qml"))
	if err != nil || !strings.Contains(string(action), "None") {
		t.Errorf("Action.qml has no None: %v", err)
	}
}
