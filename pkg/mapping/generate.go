package mapping

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ErrNothingToApply means the board has no mappings: the stock file stays as it is.
var ErrNothingToApply = errors.New("no mappings for this board")

const (
	regionBegin = "    // EngineX button mapping: begin"
	regionEnd   = "    // EngineX button mapping: end"
	importLine  = "import InputAssignment 0.1"
)

// propEdit switches one stock gesture off: Prop of the Module instance whose NoteProp is Note.
type propEdit struct {
	Module   string
	NoteProp string
	Note     int
	Prop     string
	Value    string
}

type resolved struct {
	Mapping Mapping
	Control Control
	Note    int
	Action  Action
	Rule    ModuleRule
}

// resolve looks every mapping up. The mappings are validated first.
func resolve(board string, list []Mapping) ([]resolved, error) {
	if err := ValidateAll(list); err != nil {
		return nil, err
	}
	c := Default()
	inv, ok := Board(board)
	if !ok {
		return nil, reject(Mapping{Board: board}, ReasonBoard, "board %q is not supported", board)
	}
	var out []resolved
	for _, m := range list {
		ctl, _ := inv.Find(m.Control)
		note, _ := ctl.Note()
		a, _ := c.Action(m.Action)
		out = append(out, resolved{Mapping: m, Control: ctl, Note: note, Action: a, Rule: c.Modules[ctl.Module]})
	}
	return out, nil
}

// edits are the stock bindings the mappings take over, in mapping order, once each.
func edits(rs []resolved) []propEdit {
	var out []propEdit
	seen := map[string]bool{}
	for _, r := range rs {
		g := r.Rule.Gestures[r.Mapping.Gesture]
		if g.Status != StatusTakeover {
			continue
		}
		props := make([]string, 0, len(g.Set))
		for p := range g.Set {
			props = append(props, p)
		}
		sort.Strings(props)
		for _, p := range props {
			e := propEdit{Module: r.Control.Module, NoteProp: g.NoteProperty, Note: r.Note, Prop: p, Value: g.Set[p]}
			key := fmt.Sprintf("%s/%d/%s", e.Module, e.Note, e.Prop)
			if !seen[key] {
				seen[key] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// pathExpr is the QML expression of an action's target path, as stock's own modules write it.
func pathExpr(a Action) string {
	switch a.Root {
	case "track":
		return fmt.Sprintf("deckConfig.activeDeckTrackPath + %q", a.Path)
	case "deck":
		return fmt.Sprintf("deckConfig.activeDeckPath + %q", a.Path)
	case "abs":
		return strconv.Quote(a.Path)
	case "censor":
		// Stock's Censor module: Config.device.hasSilentCue boards use the silent cue.
		return `deckConfig.censorIsSilentCue ? deckConfig.activeDeckPath + "SilentCue" : deckConfig.activeDeckTrackPath + "Bleep"`
	}
	return `""`
}

// block is one assignment: the stock primitive for the gesture, as stock's own modules compose it.
func block(r resolved) string {
	var b strings.Builder
	m := r.Mapping
	name := fmt.Sprintf("%s %s", m.Control, m.Gesture)
	fmt.Fprintf(&b, "    // EngineX button mapping: %s -> %s\n", name, m.Action)
	b.WriteString("    ValueNoteAssignment {\n")
	fmt.Fprintf(&b, "        objectName: \"EngineX Map %s\"\n", name)
	b.WriteString("        channel: deckConfig.midiChannel\n")
	fmt.Fprintf(&b, "        note: %d\n", r.Note)
	if m.Gesture == GestureShift {
		b.WriteString("        enabled: globalConfig.shift && deckConfig.inDJMode\n")
	} else {
		b.WriteString("        enabled: !globalConfig.shift && deckConfig.inDJMode\n")
	}
	path := pathExpr(r.Action)
	switch {
	case r.Action.Output == OutputStep:
		// IncDecValueOutput calls incValue(amount, maximum, minimum, ...) and decValue(amount,
		// minimum, maximum, ...); stock's AutoLoopButtons calls both with (0.1, 0.0, 1.0, false, false).
		min, max := "0.0", "1.0"
		up := "false"
		if r.Action.Step == "inc" {
			up = "true"
		}
		b.WriteString("        output: IncDecValueOutput {\n")
		b.WriteString("            target: PropertyTarget {\n")
		fmt.Fprintf(&b, "                path: %s\n", path)
		b.WriteString("            }\n")
		b.WriteString("            changeAmount: 0.1\n")
		fmt.Fprintf(&b, "            increaseValue: %s\n", up)
		b.WriteString("            roundRobin: false\n")
		fmt.Fprintf(&b, "            minimum: %s\n", min)
		fmt.Fprintf(&b, "            maximum: %s\n", max)
		b.WriteString("        }\n")
	case m.Gesture == GestureDouble || m.Gesture == GestureHold:
		b.WriteString("        output: ComboActionOutput {\n")
		fmt.Fprintf(&b, "            %sEnabled: true\n", m.Gesture)
		fmt.Fprintf(&b, "            %sTarget: PropertyTarget {\n", m.Gesture)
		fmt.Fprintf(&b, "                path: %s\n", path)
		b.WriteString("            }\n")
		b.WriteString("        }\n")
	default:
		b.WriteString("        output: ActionOutput {\n")
		b.WriteString("            target: PropertyTarget {\n")
		fmt.Fprintf(&b, "                path: %s\n", path)
		b.WriteString("            }\n")
		b.WriteString("        }\n")
	}
	b.WriteString("    }\n")
	return b.String()
}

func region(rs []resolved) string {
	var b strings.Builder
	b.WriteString(regionBegin + "\n")
	for _, r := range rs {
		b.WriteString(block(r))
	}
	b.WriteString(regionEnd + "\n")
	return b.String()
}

// Generate returns the stock assignment file with the board's mappings added as stock
// primitives (ValueNoteAssignment with ActionOutput, ComboActionOutput or IncDecValueOutput,
// enabled by globalConfig.shift), and the stock binding of every taken gesture switched off.
// The result has passed Check; on any error nothing is returned and the stock file stays.
func Generate(board string, stock []byte, list []Mapping) ([]byte, error) {
	mine := ForBoard(list, board)
	if len(mine) == 0 {
		return nil, ErrNothingToApply
	}
	rs, err := resolve(board, mine)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(stock, []byte("EngineX button mapping")) {
		return nil, errors.New("the file already carries a button mapping region")
	}
	ls := strings.Split(string(stock), "\n")
	if !regexp.MustCompile(`(?m)^MidiAssignment \{`).Match(stock) {
		return nil, errors.New("not a stock assignment file: no MidiAssignment root")
	}
	for _, e := range edits(rs) {
		if ls, err = applyEdit(ls, e); err != nil {
			return nil, err
		}
	}
	ls = insertImport(ls)
	if ls, err = insertRegion(ls, strings.Split(strings.TrimSuffix(region(rs), "\n"), "\n")); err != nil {
		return nil, err
	}
	out := []byte(strings.Join(ls, "\n"))
	if err := Check(board, stock, out, mine); err != nil {
		return nil, err
	}
	return out, nil
}

var braceFree = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|//.*$`)

func braceDelta(line string) int {
	s := braceFree.ReplaceAllString(line, "")
	return strings.Count(s, "{") - strings.Count(s, "}")
}

// applyEdit sets e.Prop in the one instance of e.Module whose e.NoteProp is e.Note.
func applyEdit(ls []string, e propEdit) ([]string, error) {
	open := regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(e.Module) + `\s*\{\s*$`)
	noteLine := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(e.NoteProp) + `\s*:\s*` + strconv.Itoa(e.Note) + `\s*$`)
	propLine := regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(e.Prop) + `\s*:\s*\S.*$`)
	type span struct{ start, end int }
	var found []span
	for i, l := range ls {
		if !open.MatchString(l) {
			continue
		}
		depth, end := 0, -1
		for j := i; j < len(ls); j++ {
			depth += braceDelta(ls[j])
			if depth == 0 {
				end = j
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("%s instance at line %d is not closed", e.Module, i+1)
		}
		for j := i + 1; j < end; j++ {
			if noteLine.MatchString(ls[j]) {
				found = append(found, span{i, end})
				break
			}
		}
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("cannot disable stock's %s: %d %s instances with %s: %d (want exactly one)", e.Prop, len(found), e.Module, e.NoteProp, e.Note)
	}
	s := found[0]
	for j := s.start + 1; j < s.end; j++ {
		if m := propLine.FindStringSubmatch(ls[j]); m != nil {
			ls[j] = m[1] + e.Prop + ": " + e.Value
			return ls, nil
		}
	}
	for j := s.start + 1; j < s.end; j++ {
		if noteLine.MatchString(ls[j]) {
			indent := ls[j][:len(ls[j])-len(strings.TrimLeft(ls[j], " \t"))]
			ls = append(ls[:j+1], append([]string{indent + e.Prop + ": " + e.Value}, ls[j+1:]...)...)
			return ls, nil
		}
	}
	return nil, errors.New("unreachable")
}

func insertImport(ls []string) []string {
	last := -1
	for i, l := range ls {
		if strings.HasPrefix(l, "import ") {
			last = i
			if l == importLine {
				return ls
			}
		}
	}
	at := last + 1
	return append(ls[:at], append([]string{importLine}, ls[at:]...)...)
}

var onCompleted = regexp.MustCompile(`^    Component\.onCompleted\s*:`)

func insertRegion(ls, block []string) ([]string, error) {
	at := -1
	for i, l := range ls {
		if onCompleted.MatchString(l) {
			at = i
			break
		}
	}
	if at < 0 {
		for i := len(ls) - 1; i >= 0; i-- {
			if strings.TrimSpace(ls[i]) == "" {
				continue
			}
			if ls[i] != "}" {
				return nil, errors.New("the root object does not end with a closing brace")
			}
			at = i
			break
		}
	}
	if at < 0 {
		return nil, errors.New("no place for the mappings in the file")
	}
	return append(ls[:at], append(append([]string{}, block...), ls[at:]...)...), nil
}
