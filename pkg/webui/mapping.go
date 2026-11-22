package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/mapping"
)

// Button mapping (the console's Buttons page): which controls the board has, what each gesture does in
// stock, the DJ's mappings, MIDI learn, and "restart Engine to apply". The mappings are applied by the
// guard at Engine start (engine-ui-guard, `stream-sync --mapping-generate`); nothing here runs while playing.
var (
	mappingsPath = mapping.FilePath
	// mappingStatusDir holds the guard's per-board status (mapping-<board>.json).
	mappingStatusDir = "/run/enginex-assignments"
)

// mapLearn is the one running MIDI learn: a read-only ALSA subscription that lives at most the learn window.
type mapLearn struct {
	mu       sync.Mutex
	state    string // idle | running | done | timeout | error
	result   *mapping.Result
	err      string
	deadline time.Time
	cancel   context.CancelFunc
	done     chan struct{}
}

type mappingControl struct {
	ID     string                   `json:"id"`
	Name   string                   `json:"name"`
	Module string                   `json:"module"`
	Note   int                      `json:"note"`
	Stock  map[string]string        `json:"stock"`
	Offers map[string]mapping.Offer `json:"offers"`
}

type mappingAction struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Output   string            `json:"output"`
	Label    map[string]string `json:"label"`
	Gestures []string          `json:"gestures"`
}

type mappingStatusView struct {
	State    string `json:"state"` // none | applied | rejected | safe-mode
	Mappings int    `json:"mappings"`
	Code     string `json:"code,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Time     string `json:"time,omitempty"`
}

type mappingView struct {
	Supported     bool              `json:"supported"`
	Board         string            `json:"board"`
	Boards        []string          `json:"boards"`
	Timing        mapping.Timing    `json:"timing"`
	Controls      []mappingControl  `json:"controls"`
	Actions       []mappingAction   `json:"actions"`
	Mappings      []mapping.Mapping `json:"mappings"`
	Status        mappingStatusView `json:"status"`
	RestartNeeded bool              `json:"restartNeeded"`
	Learn         mappingLearnView  `json:"learn"`
}

type mappingLearnView struct {
	State       string          `json:"state"`
	SecondsLeft int             `json:"secondsLeft,omitempty"`
	Result      *mapping.Result `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// mappingBoard is this deck's board code (JP07 ...), from the device tree.
func mappingBoard() string {
	for _, c := range deviceCompatible() {
		if strings.HasPrefix(c, "inmusic,") {
			return strings.ToUpper(strings.TrimPrefix(c, "inmusic,"))
		}
	}
	return ""
}

func (s *Server) mappingView() (mappingView, error) {
	cat := mapping.Default()
	board := mappingBoard()
	v := mappingView{Board: board, Boards: cat.Boards, Timing: cat.Timing, Supported: cat.Supports(board), Mappings: []mapping.Mapping{}}
	list, err := mapping.Load(mappingsPath)
	if err != nil {
		// A file that does not validate is shown as empty, with the reason; the guard
		// applies nothing from it either.
		v.Status = mappingStatusView{State: "rejected", Reason: err.Error()}
		var e *mapping.Error
		if errors.As(err, &e) {
			v.Status.Code = e.Code
		}
		list = nil
	}
	v.Mappings = append(v.Mappings, list...)
	for _, a := range cat.Actions {
		act := mappingAction{ID: a.ID, Kind: a.Kind, Output: a.Output, Label: a.Label}
		for _, g := range mapping.Gestures {
			if a.AllowsGesture(g) {
				act.Gestures = append(act.Gestures, g)
			}
		}
		v.Actions = append(v.Actions, act)
	}
	if !v.Supported {
		return v, nil
	}
	inv, _ := mapping.Board(board)
	for _, c := range inv.Controls {
		note, ok := c.Note()
		if !ok {
			continue
		}
		ctl := mappingControl{ID: c.ID, Name: c.Control, Module: c.Module, Note: note, Offers: map[string]mapping.Offer{},
			Stock: map[string]string{
				mapping.GesturePress: c.Press, mapping.GestureShift: c.ShiftPress,
				mapping.GestureDouble: c.DoublePress, mapping.GestureHold: c.LongPress,
			}}
		for _, g := range mapping.Gestures {
			ctl.Offers[g] = cat.Offer(board, c.ID, g)
		}
		v.Controls = append(v.Controls, ctl)
	}
	if v.Status.State == "" {
		st, _ := mapping.ReadStatus(mapping.StatusPath(mappingStatusDir, board))
		switch {
		case bootMarker(s.uiDir(), "safe-mode"):
			v.Status = mappingStatusView{State: "safe-mode"}
		case st.State == "":
			v.Status = mappingStatusView{State: "none"}
		default:
			v.Status = mappingStatusView{State: st.State, Mappings: st.Mappings, Code: st.Code, Reason: st.Reason, Time: st.Time}
		}
		applied := ""
		if st.State == "applied" {
			applied = st.Digest
		}
		v.RestartNeeded = v.Status.State != "safe-mode" && applied != mapping.Digest(mapping.ForBoard(list, board))
	}
	v.Learn = s.mappingLearnView()
	return v, nil
}

func mappingFail(w http.ResponseWriter, status int, err error) {
	body := map[string]string{"error": err.Error()}
	var e *mapping.Error
	if errors.As(err, &e) {
		body["code"] = e.Code
		body["mapping"] = e.Mapping.String()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func readMappingBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mapping.MaxFileBytes+1))
	if err != nil {
		mappingFail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the request is larger than %d bytes", mapping.MaxFileBytes))
		return nil, false
	}
	return b, true
}

// handleMapping: GET the page's data; POST {"mappings":[...]} saves the whole list (every entry validated).
func (s *Server) handleMapping(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		b, ok := readMappingBody(w, r)
		if !ok {
			return
		}
		var in struct {
			Mappings []mapping.Mapping `json:"mappings"`
		}
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.DisallowUnknownFields()
		if err := d.Decode(&in); err != nil {
			mappingFail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
			return
		}
		if err := s.saveMappings(in.Mappings); err != nil {
			mappingFail(w, statusOfMappingError(err), err)
			return
		}
	}
	v, err := s.mappingView()
	if err != nil {
		mappingFail(w, http.StatusInternalServerError, err)
		return
	}
	respond(w, v)
}

func statusOfMappingError(err error) int {
	var e *mapping.Error
	if errors.As(err, &e) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) saveMappings(list []mapping.Mapping) error {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	if err := mapping.Save(mappingsPath, list); err != nil {
		return err
	}
	log.Printf("stream-sync: button mappings saved: %d", len(list))
	return nil
}

// handleMappingExport sends mappings.json as it is saved.
func (s *Server) handleMappingExport(w http.ResponseWriter, r *http.Request) {
	list, err := mapping.Load(mappingsPath)
	if err != nil {
		mappingFail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="mappings.json"`)
	w.Write(mapping.Marshal(list))
}

// handleMappingImport replaces the mappings with an uploaded mappings.json: a list of
// {board, control, gesture, action}. Nothing is written unless every entry validates.
func (s *Server) handleMappingImport(w http.ResponseWriter, r *http.Request) {
	b, ok := readMappingBody(w, r)
	if !ok {
		return
	}
	list, err := mapping.Parse(strings.NewReader(string(b)))
	if err != nil {
		mappingFail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.saveMappings(list); err != nil {
		mappingFail(w, statusOfMappingError(err), err)
		return
	}
	v, err := s.mappingView()
	if err != nil {
		mappingFail(w, http.StatusInternalServerError, err)
		return
	}
	respond(w, v)
}

// handleMappingRestart restarts Engine so the guard applies the saved mappings.
func (s *Server) handleMappingRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.restartEngine(); err != nil {
		log.Printf("stream-sync: restart Engine for button mappings: %v", err)
		mappingFail(w, http.StatusInternalServerError, errors.New("Engine did not restart"))
		return
	}
	respond(w, map[string]bool{"ok": true})
}

func (s *Server) mappingLearnWindow() time.Duration {
	if s.cfg.MappingLearnWindow > 0 {
		return s.cfg.MappingLearnWindow
	}
	return time.Duration(mapping.Default().Timing.LearnSeconds) * time.Second
}

func (s *Server) openMappingSource() (mapping.Source, error) {
	if s.cfg.MappingSource != nil {
		return s.cfg.MappingSource()
	}
	return mapping.OpenSource()
}

// handleMappingLearn: POST starts a learn, GET reports it, DELETE stops it.
func (s *Server) handleMappingLearn(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		board := mappingBoard()
		if !mapping.Default().Supports(board) {
			mappingFail(w, http.StatusBadRequest, fmt.Errorf("button mapping supports JP07, JP08, JP13 and JP14, not %q", board))
			return
		}
		if err := s.startMappingLearn(board); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errLearnRunning) {
				status = http.StatusConflict
			}
			mappingFail(w, status, err)
			return
		}
	case http.MethodDelete:
		s.stopMappingLearn()
	}
	respond(w, s.mappingLearnView())
}

var errLearnRunning = errors.New("a learn is already running")

func (s *Server) startMappingLearn(board string) error {
	l := &s.mapLearn
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == "running" {
		return errLearnRunning
	}
	src, err := s.openMappingSource()
	if err != nil {
		l.state, l.result, l.err = "error", nil, "the ALSA sequencer is not available: "+err.Error()
		return errors.New(l.err)
	}
	window := s.mappingLearnWindow()
	ctx, cancel := context.WithTimeout(context.Background(), window+time.Second)
	l.state, l.result, l.err = "running", nil, ""
	l.deadline = time.Now().Add(window)
	l.cancel = cancel
	done := make(chan struct{})
	l.done = done
	go func() {
		defer close(done)
		defer cancel()
		res, err := mapping.Learn(ctx, src, board, window)
		l.mu.Lock()
		defer l.mu.Unlock()
		switch {
		case err == nil:
			l.state, l.result = "done", res
		case errors.Is(err, mapping.ErrLearnTimeout):
			l.state = "timeout"
		case errors.Is(err, context.Canceled):
			l.state = "idle"
		default:
			l.state, l.err = "error", err.Error()
		}
		l.cancel = nil
	}()
	return nil
}

// stopMappingLearn ends a running learn and waits until its subscription is gone.
func (s *Server) stopMappingLearn() {
	l := &s.mapLearn
	l.mu.Lock()
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	l.mu.Lock()
	if l.state != "done" {
		l.state, l.result = "idle", nil
	}
	l.mu.Unlock()
}

func (s *Server) mappingLearnView() mappingLearnView {
	l := &s.mapLearn
	l.mu.Lock()
	defer l.mu.Unlock()
	v := mappingLearnView{State: l.state, Result: l.result, Error: l.err}
	if v.State == "" {
		v.State = "idle"
	}
	if l.state == "running" {
		if left := time.Until(l.deadline); left > 0 {
			v.SecondsLeft = int(left/time.Second) + 1
		}
	}
	return v
}

func (s *Server) registerMappingRoutes() {
	s.mux.HandleFunc("GET /api/mapping", s.handleMapping)
	s.mux.HandleFunc("POST /api/mapping", s.handleMapping)
	s.mux.HandleFunc("GET /api/mapping/export", s.handleMappingExport)
	s.mux.HandleFunc("POST /api/mapping/import", s.handleMappingImport)
	s.mux.HandleFunc("POST /api/mapping/restart", s.handleMappingRestart)
	s.mux.HandleFunc("GET /api/mapping/learn", s.handleMappingLearn)
	s.mux.HandleFunc("POST /api/mapping/learn", s.handleMappingLearn)
	s.mux.HandleFunc("DELETE /api/mapping/learn", s.handleMappingLearn)
}
