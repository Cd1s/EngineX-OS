package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// uiViews is the deck's five Layout Manager views (#99): Engine has three and
// forgets the selected one at every start, so the QML keeps its own here. A
// view is a snapshot of the layout settings, by name: strings (a setting's
// current name) and booleans (switches). Loopback only (/v1/).
type uiViews struct {
	Active int                       `json:"active"`
	Views  map[string]map[string]any `json:"views"`
}

const viewsFile = "views.json"

func (v uiViews) valid() error {
	if v.Active < 1 || v.Active > 5 {
		return errors.New("views: active must be 1..5")
	}
	for k, snap := range v.Views {
		if n, err := strconv.Atoi(k); err != nil || n < 1 || n > 5 || k != strconv.Itoa(n) {
			return errors.New("views: views are 1..5")
		}
		if len(snap) > 16 {
			return errors.New("views: too many settings")
		}
		for name, val := range snap {
			s, isString := val.(string)
			if _, isBool := val.(bool); len(name) == 0 || len(name) > 32 || !isBool && (!isString || len(s) > 64) {
				return errors.New("views: settings are short names with a string or a boolean")
			}
		}
	}
	return nil
}

func (s *Server) viewsGet(w http.ResponseWriter, r *http.Request) {
	v := uiViews{Active: 1}
	s.uiMu.Lock()
	b, err := os.ReadFile(filepath.Join(s.uiDir(), viewsFile))
	s.uiMu.Unlock()
	if err == nil && (json.Unmarshal(b, &v) != nil || v.valid() != nil) {
		v = uiViews{Active: 1} // a damaged file: start over on View 1
	}
	if v.Views == nil {
		v.Views = map[string]map[string]any{}
	}
	respond(w, v)
}

func (s *Server) viewsPost(w http.ResponseWriter, r *http.Request) {
	var v uiViews
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&v); err != nil {
		fail(w, 400, err)
		return
	}
	if err := v.valid(); err != nil {
		fail(w, 400, err)
		return
	}
	b, _ := json.Marshal(v)
	s.uiMu.Lock()
	err := writeAtomic(s.uiDir(), viewsFile, b)
	s.uiMu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}

// writeAtomic replaces dir/name with data, never leaving a partial file.
func writeAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}
