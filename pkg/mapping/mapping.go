package mapping

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Limits of mappings.json: it is typed in a console, never large.
const (
	MaxFileBytes = 64 << 10
	MaxMappings  = 256
)

// FilePath is where the DJ's mappings live on the deck.
const FilePath = "/data/enginex/mappings.json"

// Mapping is one entry of mappings.json: this gesture on this control runs this action.
type Mapping struct {
	Board   string `json:"board"`
	Control string `json:"control"` // an inventory id: <module>:<note>
	Gesture string `json:"gesture"` // press | shift | double | hold
	Action  string `json:"action"`  // an id from catalogue.json
}

// Error is a rejected mapping with a code the console can translate.
type Error struct {
	Code    string
	Mapping Mapping
	Message string
}

func (e *Error) Error() string {
	if e.Mapping == (Mapping{}) {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Mapping, e.Message)
}

func reject(m Mapping, code, format string, args ...any) error {
	return &Error{Code: code, Mapping: m, Message: fmt.Sprintf(format, args...)}
}

// Validate checks one mapping against the catalogue and the board's inventory.
func Validate(m Mapping) error {
	c := Default()
	if !c.Supports(m.Board) {
		return reject(m, ReasonBoard, "board %q is not one of the players (JP07, JP08, JP13, JP14)", m.Board)
	}
	if !validGesture(m.Gesture) {
		return reject(m, ReasonGesture, "gesture %q is not press, shift, double or hold", m.Gesture)
	}
	action, ok := c.Action(m.Action)
	if !ok {
		return reject(m, ReasonAction, "unknown action %q", m.Action)
	}
	offer := c.Offer(m.Board, m.Control, m.Gesture)
	if !offer.Mappable {
		switch offer.Reason {
		case ReasonControl:
			return reject(m, ReasonControl, "%q is not a button of %s", m.Control, m.Board)
		case ReasonReserved:
			return reject(m, ReasonReserved, "%s is Shift or a navigation control and is never mapped", m.Control)
		case ReasonTaken:
			by, _ := c.Taken(m.Control, m.Gesture)
			return reject(m, ReasonTaken, "%s on %s belongs to EngineX's %s", m.Gesture, m.Control, by)
		case ReasonStockUsed:
			return reject(m, ReasonStockUsed, "stock uses %s on %s and it cannot be taken without losing another function", m.Gesture, m.Control)
		default:
			return reject(m, offer.Reason, "%s is not offered for mapping in v1", m.Control)
		}
	}
	if !action.AllowsGesture(m.Gesture) {
		return reject(m, ReasonOutput, "%s is a %s target: %s cannot drive it", m.Action, action.Output, m.Gesture)
	}
	return nil
}

// ValidateAll checks a whole file: every entry, and no gesture of a control twice.
func ValidateAll(list []Mapping) error {
	if len(list) > MaxMappings {
		return &Error{Code: "size", Message: fmt.Sprintf("more than %d mappings", MaxMappings)}
	}
	seen := map[[3]string]bool{}
	for _, m := range list {
		if err := Validate(m); err != nil {
			return err
		}
		key := [3]string{m.Board, m.Control, m.Gesture}
		if seen[key] {
			return reject(m, ReasonDuplicate, "%s on %s is mapped twice", m.Gesture, m.Control)
		}
		seen[key] = true
	}
	return nil
}

// ForBoard keeps the mappings of one board, in file order.
func ForBoard(list []Mapping, board string) []Mapping {
	var out []Mapping
	for _, m := range list {
		if m.Board == board {
			out = append(out, m)
		}
	}
	return out
}

// Parse reads a mappings.json body: a JSON list of entries with no unknown field.
// It validates every entry; unknown ids are rejected.
func Parse(r io.Reader) ([]Mapping, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileBytes {
		return nil, &Error{Code: "size", Message: fmt.Sprintf("mappings.json is larger than %d bytes", MaxFileBytes)}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return []Mapping{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var list []Mapping
	if err := dec.Decode(&list); err != nil {
		return nil, &Error{Code: "json", Message: "mappings.json is not a list of {board, control, gesture, action}: " + err.Error()}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, &Error{Code: "json", Message: "mappings.json has more than one JSON value"}
		}
		return nil, &Error{Code: "json", Message: "mappings.json has trailing data: " + err.Error()}
	}
	if list == nil {
		list = []Mapping{}
	}
	if err := ValidateAll(list); err != nil {
		return nil, err
	}
	return list, nil
}

// Load reads mappings.json. A missing file is no mappings.
func Load(path string) ([]Mapping, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Mapping{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Marshal writes the file the way Save does: one entry per line, stable order.
func Marshal(list []Mapping) []byte {
	if len(list) == 0 {
		return []byte("[]\n")
	}
	var b bytes.Buffer
	b.WriteString("[\n")
	for i, m := range list {
		line, _ := json.Marshal(m)
		b.WriteString("  ")
		b.Write(line)
		if i < len(list)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	return b.Bytes()
}

// Save validates and writes mappings.json atomically.
func Save(path string, list []Mapping) error {
	if err := ValidateAll(list); err != nil {
		return err
	}
	if len(list) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mappings-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err = tmp.Write(Marshal(list)); err == nil {
		err = tmp.Chmod(0o644)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}
