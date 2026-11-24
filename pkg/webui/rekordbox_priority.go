package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge/v2"
)

func (s *Server) rbPriorityQueue() *bridge.PriorityQueue {
	s.rbPriorityMu.Lock()
	defer s.rbPriorityMu.Unlock()
	if s.rbPriority == nil {
		s.rbPriority = bridge.NewPriorityQueue()
	}
	return s.rbPriority
}

// resolveMediaVolumePath splits a resolved local file path inside mediaRoot
// into its mounted volume root ("<mediaRoot>/<vol>") and Engine-relative path
// ("../<rel>"). Paths outside mediaRoot (such as /data) return ok=false.
func resolveMediaVolumePath(file, mediaRoot string) (mount, enginePath string, ok bool) {
	if file == "" || mediaRoot == "" {
		return "", "", false
	}
	cleanRoot := filepath.Clean(mediaRoot)
	cleanFile := filepath.Clean(file)
	rel, err := filepath.Rel(cleanRoot, cleanFile)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", "", false
	}
	slash := filepath.ToSlash(rel)
	vol, inner, found := strings.Cut(slash, "/")
	if !found || vol == "" || inner == "" || vol == "." || vol == ".." {
		return "", "", false
	}
	mount = filepath.Join(cleanRoot, vol)
	if st, err := os.Stat(mount); err != nil || !st.IsDir() {
		return "", "", false
	}
	return mount, "../" + inner, true
}

// noteDeckPriority records deck activity (PlayState, CUE preview, scratch, or
// platter movement) and enqueues newly loaded or CUE-previewed rekordbox stick
// tracks for immediate background processing.
func (s *Server) noteDeckPriority(decks []deckState, now time.Time) {
	if s == nil || s.shadow == nil {
		return
	}
	q := s.rbPriorityQueue()
	anyActive := false
	for _, d := range decks {
		if d.Playing || d.Cue || d.Scratch {
			anyActive = true
			break
		}
	}
	q.UpdateDeckActivity(anyActive, now)

	mediaRoot := s.shadow.mediaRoot
	if mediaRoot == "" {
		mediaRoot = s.mediaRoot()
	}
	for _, d := range decks {
		if d.Deck < 1 || d.Deck > 4 {
			continue
		}
		f := d.Path
		if f == "" && d.Net != "" {
			f = resolveNetPathFile(d.Net, s.shadow.dataRoot, mediaRoot, false)
		}
		s.rbPriorityMu.Lock()
		prevPath := s.rbDeckLoaded[d.Deck]
		prevCue := s.rbDeckCue[d.Deck]
		s.rbDeckLoaded[d.Deck] = f
		s.rbDeckCue[d.Deck] = d.Cue
		s.rbPriorityMu.Unlock()

		if f == "" {
			continue
		}
		isFreshLoad := f != prevPath
		isCueEdge := d.Cue && (!prevCue || isFreshLoad)
		if !isFreshLoad && !isCueEdge {
			continue
		}
		mount, enginePath, ok := resolveMediaVolumePath(f, mediaRoot)
		if !ok {
			continue
		}
		source := "deck"
		if isCueEdge && !d.Playing {
			source = "cue"
		}
		_, _ = q.Enqueue(mount, enginePath, source, d.Deck)
	}
}

type rbPriorityRequest struct {
	Net    string `json:"net"`
	Path   string `json:"path"`
	Source string `json:"source"` // "preview", "focus", "deck", "cue"
	Deck   int    `json:"deck"`
}

// handleRekordboxPriority accepts loopback-only track focus and browse preview
// signals from Engine QML so rb-bridge imports those tracks first.
func (s *Server) handleRekordboxPriority(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !writeBackFromDeck(r) {
		fail(w, http.StatusForbidden, errors.New("loopback only"))
		return
	}
	var req rbPriorityRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	mediaRoot := s.shadow.mediaRoot
	if mediaRoot == "" {
		mediaRoot = s.mediaRoot()
	}
	f := strings.TrimSpace(req.Path)
	if f == "" && req.Net != "" {
		f = resolveNetPathFile(req.Net, s.shadow.dataRoot, mediaRoot, false)
	}
	if f != "" {
		f = filepath.Clean(f)
		if !regularAudioFile(f) {
			f = ""
		}
	}
	mount, enginePath, ok := resolveMediaVolumePath(f, mediaRoot)
	if !ok {
		respond(w, map[string]any{"ok": false})
		return
	}
	source := strings.TrimSpace(req.Source)
	switch source {
	case "preview", "focus", "deck", "cue":
	default:
		source = "focus"
	}
	seq, err := s.rbPriorityQueue().Enqueue(mount, enginePath, source, req.Deck)
	if err != nil {
		respond(w, map[string]any{"ok": false})
		return
	}
	respond(w, map[string]any{"ok": true, "seq": seq})
}
