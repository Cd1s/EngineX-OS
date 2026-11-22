package mapping

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The elements a generated region may use. Everything else in the file must already be in stock's.
var generatedTypes = map[string]bool{
	"ValueNoteAssignment": true, "ActionOutput": true, "ComboActionOutput": true,
	"IncDecValueOutput": true, "PropertyTarget": true,
}

var (
	handlerLine = regexp.MustCompile(`^\s*on[A-Z][A-Za-z0-9_]*\s*:`)
	elementOpen = regexp.MustCompile(`\b([A-Z][A-Za-z0-9_]*)\s*\{`)
)

// scan walks a QML file once: the brace depth at the start of every line, and every line with
// comments and string contents removed. It fails on braces that do not balance.
func scan(text string) (depth []int, code []string, err error) {
	d := 0
	inBlock := false
	for n, raw := range strings.Split(text, "\n") {
		depth = append(depth, d)
		var out strings.Builder
		for i := 0; i < len(raw); i++ {
			c := raw[i]
			if inBlock {
				if c == '*' && i+1 < len(raw) && raw[i+1] == '/' {
					inBlock = false
					i++
				}
				continue
			}
			switch {
			case c == '/' && i+1 < len(raw) && raw[i+1] == '/':
				i = len(raw)
			case c == '/' && i+1 < len(raw) && raw[i+1] == '*':
				inBlock = true
				i++
			case c == '"' || c == '\'' || c == '`':
				q := c
				out.WriteByte(q)
				for i++; i < len(raw) && raw[i] != q; i++ {
					if raw[i] == '\\' {
						i++
					}
				}
				out.WriteByte(q)
			case c == '{':
				d++
				out.WriteByte(c)
			case c == '}':
				d--
				if d < 0 {
					return nil, nil, fmt.Errorf("line %d: a closing brace without its opening one", n+1)
				}
				out.WriteByte(c)
			default:
				out.WriteByte(c)
			}
		}
		code = append(code, out.String())
	}
	if d != 0 || inBlock {
		return nil, nil, fmt.Errorf("the braces or comments do not balance (depth %d at the end)", d)
	}
	return depth, code, nil
}

func typeSet(code []string) map[string]bool {
	set := map[string]bool{}
	for _, l := range code {
		for _, m := range elementOpen.FindAllStringSubmatch(l, -1) {
			set[m[1]] = true
		}
	}
	return set
}

// Check is the strict static check of a generated assignment file. Assignment files never load
// off the deck, so this is what stands between a bug and a deck that boots with the stock
// mapping gone: the file must be the stock file plus exactly the mappings, and nothing else.
//
//   - the braces balance, and no element carries an onXxx handler (the on-disk loader rejects them);
//   - every element is one stock's file already uses, or one of the five primitives the generator writes;
//   - one region, a direct child of the root object, equal to what the mappings generate;
//   - outside the region the file is stock's, but for the InputAssignment import and the module
//     properties that switch off the stock binding of a taken gesture.
func Check(board string, stock, out []byte, list []Mapping) error {
	mine := ForBoard(list, board)
	if len(mine) == 0 {
		return fmt.Errorf("check: no mappings for %s", board)
	}
	rs, err := resolve(board, mine)
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	depth, code, err := scan(string(out))
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}
	for i, l := range code {
		if handlerLine.MatchString(l) {
			return fmt.Errorf("check: line %d: a handler breaks the on-disk loader: %s", i+1, strings.TrimSpace(l))
		}
	}
	lines := strings.Split(string(out), "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch l {
		case regionBegin:
			if begin >= 0 {
				return fmt.Errorf("check: line %d: a second mapping region", i+1)
			}
			begin = i
		case regionEnd:
			if end >= 0 {
				return fmt.Errorf("check: line %d: a second end of the mapping region", i+1)
			}
			end = i
		}
	}
	if begin < 0 || end < begin {
		return fmt.Errorf("check: no mapping region")
	}
	if depth[begin] != 1 || depth[end] != 1 {
		return fmt.Errorf("check: the mapping region is not a direct child of the root object (depth %d)", depth[begin])
	}
	want := strings.Split(strings.TrimSuffix(region(rs), "\n"), "\n")
	got := lines[begin : end+1]
	if len(got) != len(want) {
		return fmt.Errorf("check: the mapping region has %d lines, the mappings generate %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf("check: mapping region line %d differs: %q, want %q", i+1, got[i], want[i])
		}
	}
	_, stockCode, err := scan(string(stock))
	if err != nil {
		return fmt.Errorf("check: the stock file does not scan: %w", err)
	}
	stockTypes := typeSet(stockCode)
	for t := range typeSet(code) {
		if !stockTypes[t] && !generatedTypes[t] {
			return fmt.Errorf("check: unknown element %s", t)
		}
	}
	rest := append(append([]string{}, lines[:begin]...), lines[end+1:]...)
	if err := diffOutside(strings.Split(string(stock), "\n"), rest, edits(rs)); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	for _, e := range edits(rs) {
		v, ok := lookupProp(rest, e)
		if !ok || v != e.Value {
			return fmt.Errorf("check: stock's %s of %s %s: %d is not switched off", e.Prop, e.Module, e.NoteProp, e.Note)
		}
	}
	return nil
}

// diffOutside compares the file without its region to stock's, line by line. Only these
// differences are allowed: the added import, a declared property replaced, a declared
// property added right after its note line.
func diffOutside(stock, rest []string, es []propEdit) error {
	type pattern struct {
		prop, note *regexp.Regexp
		edit       propEdit
	}
	var ps []pattern
	for _, e := range es {
		ps = append(ps, pattern{
			prop: regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(e.Prop) + `\s*:\s*\S.*$`),
			note: regexp.MustCompile(`^\s*` + regexp.QuoteMeta(e.NoteProp) + `\s*:\s*` + strconv.Itoa(e.Note) + `\s*$`),
			edit: e,
		})
	}
	importDone := false
	i, j := 0, 0
	for i < len(stock) || j < len(rest) {
		switch {
		case i < len(stock) && j < len(rest) && stock[i] == rest[j]:
			i++
			j++
			continue
		case j < len(rest) && rest[j] == importLine && !importDone && i > 0 && strings.HasPrefix(stock[i-1], "import ") &&
			(i >= len(stock) || !strings.HasPrefix(stock[i], "import ")):
			importDone = true
			j++
			continue
		}
		matched := false
		for _, p := range ps {
			if i < len(stock) && j < len(rest) {
				if m := p.prop.FindStringSubmatch(stock[i]); m != nil && rest[j] == m[1]+p.edit.Prop+": "+p.edit.Value {
					matched = true
					i++
					j++
					break
				}
			}
			if i > 0 && j > 0 && p.note.MatchString(stock[i-1]) && rest[j-1] == stock[i-1] && j < len(rest) {
				indent := stock[i-1][:len(stock[i-1])-len(strings.TrimLeft(stock[i-1], " \t"))]
				if rest[j] == indent+p.edit.Prop+": "+p.edit.Value {
					matched = true
					j++
					break
				}
			}
		}
		if !matched {
			at := j + 1
			return fmt.Errorf("line %d differs from stock beyond the mappings: %q", at, safeLine(rest, j))
		}
	}
	return nil
}

func safeLine(ls []string, i int) string {
	if i < len(ls) {
		return ls[i]
	}
	return "<end of file>"
}

// lookupProp reads Prop of the one instance of e.Module that holds e.NoteProp: e.Note.
func lookupProp(ls []string, e propEdit) (string, bool) {
	open := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(e.Module) + `\s*\{\s*$`)
	noteLine := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(e.NoteProp) + `\s*:\s*` + strconv.Itoa(e.Note) + `\s*$`)
	propLine := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(e.Prop) + `\s*:\s*(\S.*?)\s*$`)
	var value string
	count := 0
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
			return "", false
		}
		hit := false
		for j := i + 1; j < end; j++ {
			if noteLine.MatchString(ls[j]) {
				hit = true
			}
		}
		if !hit {
			continue
		}
		count++
		for j := i + 1; j < end; j++ {
			if m := propLine.FindStringSubmatch(ls[j]); m != nil {
				value = m[1]
				break
			}
		}
	}
	return value, count == 1 && value != ""
}
