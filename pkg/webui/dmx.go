package webui

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/dmx"
)

//go:embed dmx.html DMX.qml
var dmxAssets embed.FS

// Read only while DMX is enabled or explicitly requested. Nothing modifies
// SoundSwitch's processes, properties, service or QML.
func soundSwitchRunning() bool {
	entries, _ := os.ReadDir("/proc")
	for _, ent := range entries {
		if _, err := strconv.Atoi(ent.Name()); err != nil {
			continue
		}
		b, _ := os.ReadFile(filepath.Join("/proc", ent.Name(), "comm"))
		if strings.TrimSpace(string(b)) == "SoundSwitch" {
			return true
		}
	}
	return false
}

func (s *Server) initDMX() {
	s.dmx = dmx.New(soundSwitchRunning)
	c := dmx.DefaultConfig()
	if b, err := os.ReadFile(filepath.Join(s.uiDir(), "dmx.json")); err == nil && json.Unmarshal(b, &c) == nil && c.Validate() == nil {
		// Persist the opt-in and patch, but boot with the lights blacked out.
		c.Blackout = true
		if err := s.dmx.Apply(c); err != nil {
			c.Enabled = false
			_ = s.dmx.Apply(c)
		}
	}
}

func (s *Server) dmxPage(w http.ResponseWriter, r *http.Request) {
	if ok, _ := s.authorized(w, r); !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	b, _ := dmxAssets.ReadFile("dmx.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (s *Server) dmxQML(w http.ResponseWriter, r *http.Request) {
	b, _ := dmxAssets.ReadFile("DMX.qml")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

func (s *Server) handleDMX(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Action string      `json:"action"`
			Config *dmx.Config `json:"config,omitempty"`
		}
		if !input(w, r, &req) {
			return
		}
		s.dmxMu.Lock()
		defer s.dmxMu.Unlock()
		old := s.dmx.Status().Config
		c := old
		switch req.Action {
		case "configure":
			if req.Config == nil {
				fail(w, 400, errors.New("config required"))
				return
			}
			c = *req.Config
		case "blackout":
			c.Blackout = true
		case "disable":
			c.Enabled = false
		default:
			fail(w, 400, errors.New("unknown DMX action"))
			return
		}
		if err := c.Validate(); err != nil {
			fail(w, 400, err)
			return
		}
		if err := s.dmx.Apply(c); err != nil {
			fail(w, 409, err)
			return
		}
		b, _ := json.Marshal(c)
		dir := s.uiDir()
		err := os.MkdirAll(dir, 0o700)
		if err == nil {
			var f *os.File
			f, err = os.CreateTemp(dir, ".dmx-*")
			if err == nil {
				name := f.Name()
				defer os.Remove(name)
				_, err = f.Write(b)
				if err == nil {
					err = f.Sync()
				}
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
				if err == nil {
					err = os.Rename(name, filepath.Join(dir, "dmx.json"))
				}
			}
		}
		if err != nil {
			_ = s.dmx.Apply(old)
			fail(w, 500, errors.New("could not save DMX settings"))
			return
		}
	}
	st := s.dmx.Status()
	ss := st.Config.Enabled && soundSwitchRunning()
	respond(w, struct {
		dmx.Status
		SoundSwitch bool `json:"soundSwitch"`
	}{st, ss})
}

// This reuses the deck post schema already emitted by stream-sync's UI.
// The standalone sampler works without turning on four-deck shadow mode.
func (s *Server) dmxClockPost(w http.ResponseWriter, r *http.Request) {
	var decks []deckState
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&decks); err != nil || len(decks) > 4 {
		fail(w, 400, errors.New("invalid deck samples"))
		return
	}
	s.setDMXDecks(decks)
	respond(w, map[string]bool{"ok": true})
}

func (s *Server) setDMXDecks(decks []deckState) {
	if s.dmx == nil || !s.dmx.Enabled() {
		return
	}
	for _, d := range decks {
		if !d.Master {
			continue
		}
		at := time.UnixMilli(d.At)
		// Match existing shadow posts: timestamps refer to the local GUI clock.
		if d.At == 0 {
			at = time.Now()
		}
		s.dmx.SetClock(dmx.Clock{Deck: d.Deck, BPM: d.BPM * (1 + d.Pitch/100), Beat: d.Beat, Playing: d.Playing, At: at})
		return
	}
}
