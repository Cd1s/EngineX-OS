// Package modkit is the shared contract of the official EngineX Mod helpers: a
// loopback-only JSON endpoint for the Mod's Panel.qml. The deck has no Python:
// every helper is one static Go binary (linux/arm GOARM=7 and linux/arm64).
package modkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Feature is one Mod's behaviour behind GET /status and POST /action.
type Feature interface {
	Status() (any, error)
	Action(req map[string]any) (any, error)
	Close()
}

// Env is what a Feature is constructed with.
type Env struct {
	State  string // ENGINEX_MOD_STATE/<mod id>, absolute
	ModDir string // the installed Mod directory (presets and other packaged data)
}

// badRequest marks an error as the client's (HTTP 400). Everything else is a 500,
// as with the former helper (ValueError/KeyError/TypeError/FileNotFoundError: 400).
type badRequest struct{ error }

func (b badRequest) Unwrap() error { return b.error }

// Bad returns an HTTP 400 error.
func Bad(format string, args ...any) error { return badRequest{fmt.Errorf(format, args...)} }

// AsBad wraps an existing error as HTTP 400 (nil stays nil).
func AsBad(err error) error {
	if err == nil {
		return nil
	}
	return badRequest{err}
}

// IsBad reports whether err is a client error.
func IsBad(err error) bool {
	var b badRequest
	return errors.As(err, &b) || errors.Is(err, os.ErrNotExist)
}

// Decode parses a JSON request body as an object, keeping integers distinguishable
// from floats (json.Number).
func Decode(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, Bad("%v", err)
	}
	if dec.More() {
		return nil, Bad("extra data after the JSON value")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, Bad("expected an object")
	}
	return m, nil
}

// LoadJSON reads a JSON file with json.Number numbers.
func LoadJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(out)
}

// SaveJSON writes value atomically (temp file + rename), indented, newline-terminated.
func SaveJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temp := strings.TrimSuffix(path, filepath.Ext(path)) + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// Field returns req[key] or a 400 naming the missing key.
func Field(req map[string]any, key string) (any, error) {
	v, ok := req[key]
	if !ok {
		return nil, Bad("'%s'", key)
	}
	return v, nil
}

// Int returns v when it is a JSON integer (not a float, not a bool).
func Int(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		if strings.ContainsAny(string(n), ".eE") {
			return 0, false
		}
		i, err := n.Int64()
		return i, err == nil
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// Float returns any JSON number (int or float, never a bool) as float64.
func Float(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// Number is the former float(value) range check: numbers and numeric strings.
func Number(v any, low, high float64) (float64, error) {
	f, ok := Float(v)
	if !ok {
		s, isStr := v.(string)
		if b, isBool := v.(bool); isBool {
			if b {
				f, ok = 1, true
			} else {
				f, ok = 0, true
			}
		} else if isStr {
			var err error
			f, err = parseFloat(strings.TrimSpace(s))
			ok = err == nil
		}
	}
	if !ok {
		return 0, Bad("could not convert to a number: %v", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < low || f > high {
		return 0, Bad("expected number in %s..%s", Fmt(low), Fmt(high))
	}
	return f, nil
}

// Integer is Number restricted to whole values.
func Integer(v any, low, high float64) (int, error) {
	f, err := Number(v, low, high)
	if err != nil {
		return 0, err
	}
	if math.Trunc(f) != f {
		return 0, Bad("expected an integer")
	}
	return int(f), nil
}

// Fmt prints a bound the way the messages always did (120, not 120.0).
func Fmt(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprint(f)
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscan(s, &f)
	return f, err
}

// MediaFile resolves value to an existing regular file under /media, /mnt or state.
func MediaFile(value any, state string) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", Bad("source must be a path")
	}
	path, err := filepath.EvalSymlinks(s)
	if err != nil {
		return "", AsBad(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", AsBad(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", AsBad(err)
	}
	if st.Mode().IsRegular() {
		for _, root := range []string{"/media", "/mnt", state} {
			if Within(path, root) {
				return path, nil
			}
		}
	}
	return "", Bad("choose an existing file under /media, /mnt or this Mod's state directory")
}

// Within reports whether path is root or below it (both clean and absolute).
func Within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
