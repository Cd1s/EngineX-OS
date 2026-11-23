package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/mapping"
)

// mappingDeck points the board, the mappings file and the guard's status at a temporary directory.
func mappingDeck(t *testing.T, board string) (mappings, statusDir string) {
	t.Helper()
	dir := t.TempDir()
	oldM, oldS, oldC, oldA, oldB := mappingsPath, mappingStatusDir, deviceCompatiblePath, mapping.AssignmentDir, mapping.BaseAssignmentDir
	mappingsPath = filepath.Join(dir, "data", "mappings.json")
	mappingStatusDir = filepath.Join(dir, "run")
	deviceCompatiblePath = filepath.Join(dir, "compatible")
	mapping.AssignmentDir = filepath.Join(dir, "assignments")
	mapping.BaseAssignmentDir = filepath.Join(dir, "base")
	fixture := []byte(`import airAssignments 1.0

MidiAssignment {
    GlobalAssignmentConfig {
        id: globalConfig
        midiChannel: 0
    }
    DeckAssignmentConfig {
        id: deckConfig
        midiChannel: globalConfig.midiChannel
    }
    PadModeSelect {
        buttonsModel: ListModel {
            ListElement { note: 27 }
            ListElement { note: 28 }
            ListElement { note: 29 }
            ListElement { note: 30 }
        }
    }
}
`)
	for _, b := range []string{"JP07", "JP08", "JP13", "JP14"} {
		path := filepath.Join(mapping.AssignmentDir, b, b+"_Controller_Assignments.qml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, fixture, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		mappingsPath, mappingStatusDir, deviceCompatiblePath, mapping.AssignmentDir, mapping.BaseAssignmentDir = oldM, oldS, oldC, oldA, oldB
	})
	if board != "" {
		if err := os.WriteFile(deviceCompatiblePath, []byte("inmusic,"+strings.ToLower(board)+"\x00inmusic,rk3288\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return mappingsPath, mappingStatusDir
}

func mappingServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.AccessToken = "t"
	cfg.UIDir = t.TempDir()
	cfg.StateDir = t.TempDir()
	s := New(cfg, nil)
	t.Cleanup(s.Close)
	return s
}

func mapReq(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func mapView(t *testing.T, w *httptest.ResponseRecorder) mappingView {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var v mappingView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

const waveformShift = `{"board":"JP07","control":"PadModeSelect:28","gesture":"shift","action":"waveform_cycle"}`

func TestMappingPageDataOfAPlayerBoard(t *testing.T) {
	mappingDeck(t, "JP07")
	v := mapView(t, mapReq(t, mappingServer(t, Config{}), "GET", "/api/mapping", ""))
	if !v.Supported || v.Board != "JP07" || len(v.Controls) < 30 || len(v.Actions) != 11 {
		t.Fatalf("%v %s %d controls %d actions", v.Supported, v.Board, len(v.Controls), len(v.Actions))
	}
	byID := map[string]mappingControl{}
	for _, c := range v.Controls {
		byID[c.ID] = c
	}
	pad := byID["PadModeSelect:28"]
	if pad.Note != 28 || !pad.Offers["shift"].Mappable || !pad.Offers["hold"].Mappable || pad.Offers["press"].Mappable {
		t.Fatalf("%+v", pad)
	}
	loop := byID["PadModeSelect:30"]
	if loop.Offers["shift"].Mappable || loop.Offers["shift"].Status != mapping.StatusTaken || loop.Taken["shift"] != "Beat Jump pad mode" || !loop.Offers["hold"].Mappable {
		t.Fatalf("Shift + LOOP is EngineX's: %+v %v", loop.Offers, loop.Taken)
	}
	if byID["Back:16"].Offers["press"].Reason != mapping.ReasonReserved || byID["Sync:20"].Offers["hold"].Mappable || byID["Sync:20"].Offers["hold"].Reason != mapping.ReasonStockUsed {
		t.Fatalf("%+v %+v", byID["Back:16"], byID["Sync:20"])
	}
	if pad.Stock["press"] == "" || pad.Stock["shift"] == "" {
		t.Errorf("the stock function of every gesture is shown: %+v", pad.Stock)
	}
	for _, a := range v.Actions {
		if a.ID == "censor" && len(a.Gestures) != 2 {
			t.Errorf("a hold-type action is offered for press and shift only: %v", a.Gestures)
		}
		if a.Label["en"] == "" || a.Label["zh-Hans"] == "" || a.Label["zh-Hant"] == "" || a.Label["es"] == "" {
			t.Errorf("%s: labels %v", a.ID, a.Label)
		}
	}
	if v.Status.State != "none" || v.RestartNeeded || v.Learn.State != "idle" {
		t.Fatalf("%+v %v %+v", v.Status, v.RestartNeeded, v.Learn)
	}
	// every other board gets the action list and no controls
	mappingDeck(t, "JC11")
	v = mapView(t, mapReq(t, mappingServer(t, Config{}), "GET", "/api/mapping", ""))
	if v.Supported || len(v.Controls) != 0 || len(v.Actions) == 0 {
		t.Fatalf("%+v", v)
	}
}

func TestMappingSaveValidatesAndTracksWhatNeedsARestart(t *testing.T) {
	file, statusDir := mappingDeck(t, "JP07")
	s := mappingServer(t, Config{})
	v := mapView(t, mapReq(t, s, "POST", "/api/mapping", `{"mappings":[`+waveformShift+`]}`))
	if len(v.Mappings) != 1 || !v.RestartNeeded {
		t.Fatalf("%+v %v", v.Mappings, v.RestartNeeded)
	}
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "waveform_cycle") {
		t.Fatalf("%v %s", err, b)
	}
	// the guard applied exactly this list: no restart needed
	list, _ := mapping.Load(file)
	if err := mapping.WriteStatus(mapping.StatusPath(statusDir, "JP07"), mapping.Status{State: "applied", Board: "JP07", Mappings: 1, Digest: mapping.Digest(list)}); err != nil {
		t.Fatal(err)
	}
	v = mapView(t, mapReq(t, s, "GET", "/api/mapping", ""))
	if v.Status.State != "applied" || v.Status.Mappings != 1 || v.RestartNeeded {
		t.Fatalf("%+v %v", v.Status, v.RestartNeeded)
	}
	// one more mapping: what the deck runs is no longer what is saved
	v = mapView(t, mapReq(t, s, "POST", "/api/mapping", `{"mappings":[`+waveformShift+`,{"board":"JP07","control":"PadModeSelect:27","gesture":"hold","action":"sync"}]}`))
	if !v.RestartNeeded || len(v.Mappings) != 2 {
		t.Fatalf("%v %d", v.RestartNeeded, len(v.Mappings))
	}
	// another board's entries do not ask this deck to restart
	other := mapping.Marshal(append(list, mapping.Mapping{Board: "JP13", Control: "PadModeSelect:27", Gesture: "hold", Action: "sync"}))
	if err := os.WriteFile(file, other, 0o644); err != nil {
		t.Fatal(err)
	}
	if v = mapView(t, mapReq(t, s, "GET", "/api/mapping", "")); v.RestartNeeded || len(v.Mappings) != 2 {
		t.Fatalf("%v %d", v.RestartNeeded, len(v.Mappings))
	}
	// removing every mapping removes the file
	mapView(t, mapReq(t, s, "POST", "/api/mapping", `{"mappings":[]}`))
	if _, err := os.Stat(file); err == nil {
		t.Fatal("an empty list leaves a file")
	}
}

func TestMappingSaveRejectsWhatDoesNotValidateAndKeepsTheFile(t *testing.T) {
	file, _ := mappingDeck(t, "JP07")
	s := mappingServer(t, Config{})
	mapView(t, mapReq(t, s, "POST", "/api/mapping", `{"mappings":[`+waveformShift+`]}`))
	before, _ := os.ReadFile(file)
	for name, c := range map[string]struct{ body, code string }{
		"an unknown action":  {`{"mappings":[{"board":"JP07","control":"PadModeSelect:28","gesture":"shift","action":"reboot"}]}`, "action"},
		"Shift + LOOP":       {`{"mappings":[{"board":"JP07","control":"PadModeSelect:30","gesture":"shift","action":"sync"}]}`, "taken"},
		"Shift itself":       {`{"mappings":[{"board":"JP07","control":"Shift:26","gesture":"press","action":"sync"}]}`, "reserved"},
		"a stock gesture":    {`{"mappings":[{"board":"JP07","control":"PadModeSelect:30","gesture":"press","action":"sync"}]}`, "stock-used"},
		"the same gesture 2": {`{"mappings":[` + waveformShift + `,` + waveformShift + `]}`, "duplicate"},
		"an unknown field":   {`{"mappings":[],"x":1}`, ""},
		"not JSON":           {`nope`, ""},
		"trailing JSON":      {`{"mappings":[]} {}`, ""},
	} {
		w := mapReq(t, s, "POST", "/api/mapping", c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, w.Code)
			continue
		}
		var e map[string]string
		json.Unmarshal(w.Body.Bytes(), &e)
		if e["error"] == "" || e["code"] != c.code {
			t.Errorf("%s: %v", name, e)
		}
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("a rejected save changed mappings.json")
	}
	if w := mapReq(t, s, "POST", "/api/mapping", strings.Repeat(" ", mapping.MaxFileBytes+10)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d", w.Code)
	}
}

func TestMappingSaveRejectsWhenTheStockAssignmentCannotGenerate(t *testing.T) {
	file, _ := mappingDeck(t, "JP07")
	stock := filepath.Join(mapping.AssignmentDir, "JP07", "JP07_Controller_Assignments.qml")
	if err := os.WriteFile(stock, []byte("Item {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := mappingServer(t, Config{})
	w := mapReq(t, s, "POST", "/api/mapping", `{"mappings":[`+waveformShift+`]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not a stock assignment") {
		t.Fatalf("save response %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("rejected save wrote mappings.json: %v", err)
	}
}

func TestMappingSaveAndImportUseTheUnmappedBaseAfterARejectedBind(t *testing.T) {
	file, _ := mappingDeck(t, "JP07")
	stock := filepath.Join(mapping.AssignmentDir, "JP07", "JP07_Controller_Assignments.qml")
	base := filepath.Join(mapping.BaseAssignmentDir, ".base-JP07_Controller_Assignments.qml")
	stockBytes, err := os.ReadFile(stock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stock, []byte("Item {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, stockBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	s := mappingServer(t, Config{})
	first := `{"mappings":[` + waveformShift + `]}`
	if w := mapReq(t, s, "POST", "/api/mapping", first); w.Code != http.StatusOK {
		t.Fatalf("first save: %d %s", w.Code, w.Body.String())
	}
	second := `{"mappings":[` + waveformShift + `,{"board":"JP07","control":"PadModeSelect:27","gesture":"hold","action":"sync"}]}`
	if w := mapReq(t, s, "POST", "/api/mapping", second); w.Code != http.StatusOK {
		t.Fatalf("second save: %d %s", w.Code, w.Body.String())
	}
	if w := mapReq(t, s, "POST", "/api/mapping/import", second[12:len(second)-1]); w.Code != http.StatusOK {
		t.Fatalf("import after save: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("mapping file missing: %v", err)
	}
}

func TestMappingSaveClearsEngineParseRejectionLatch(t *testing.T) {
	file, statusDir := mappingDeck(t, "JP07")
	marker := filepath.Join(filepath.Dir(file), ".mapping-rejected-JP07")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statusDir, ".mapping-rejected-JP07"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := mappingServer(t, Config{})
	mapView(t, mapReq(t, s, "POST", "/api/mapping", `{"mappings":[`+waveformShift+`]}`))
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("save did not clear rejection latch: %v", err)
	}
}

func TestMappingExportAndImportRoundTrip(t *testing.T) {
	file, _ := mappingDeck(t, "JP07")
	s := mappingServer(t, Config{})
	mapView(t, mapReq(t, s, "POST", "/api/mapping", `{"mappings":[`+waveformShift+`]}`))
	w := mapReq(t, s, "GET", "/api/mapping/export", "")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "mappings.json") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	exported := w.Body.String()
	if got, _ := mapping.Parse(strings.NewReader(exported)); len(got) != 1 {
		t.Fatalf("%s", exported)
	}
	os.Remove(file)
	v := mapView(t, mapReq(t, s, "POST", "/api/mapping/import", exported))
	if len(v.Mappings) != 1 || v.Mappings[0].Action != "waveform_cycle" {
		t.Fatalf("%+v", v.Mappings)
	}
	// an import that does not validate writes nothing
	before, _ := os.ReadFile(file)
	for _, body := range []string{`[{"board":"JP07","control":"PadModeSelect:28","gesture":"shift","action":"nope"}]`, `{"board":"JP07"}`, "junk"} {
		if w := mapReq(t, s, "POST", "/api/mapping/import", body); w.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", body, w.Code)
		}
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("a rejected import changed mappings.json")
	}
}

func TestMappingStatusShowsWhyAFileWasNotApplied(t *testing.T) {
	_, statusDir := mappingDeck(t, "JP07")
	s := mappingServer(t, Config{})
	if err := mapping.WriteStatus(mapping.StatusPath(statusDir, "JP07"), mapping.Status{State: "rejected", Board: "JP07", Code: "stock-used", Reason: "stock uses hold on Sync:20"}); err != nil {
		t.Fatal(err)
	}
	v := mapView(t, mapReq(t, s, "GET", "/api/mapping", ""))
	if v.Status.State != "rejected" || v.Status.Code != "stock-used" || !strings.Contains(v.Status.Reason, "Sync:20") {
		t.Fatalf("%+v", v.Status)
	}
	// safe mode: the guard bound nothing, whatever the status says
	if err := os.WriteFile(filepath.Join(s.uiDir(), "safe-mode"), []byte("crash-loop"), 0o644); err != nil {
		t.Fatal(err)
	}
	mapping.WriteStatus(mapping.StatusPath(statusDir, "JP07"), mapping.Status{State: "applied", Board: "JP07", Mappings: 2})
	if v = mapView(t, mapReq(t, s, "GET", "/api/mapping", "")); v.Status.State != "safe-mode" || v.RestartNeeded {
		t.Fatalf("%+v %v", v.Status, v.RestartNeeded)
	}
	// a mappings.json that does not validate is shown as rejected with its reason
	os.Remove(filepath.Join(s.uiDir(), "safe-mode"))
	os.MkdirAll(filepath.Dir(mappingsPath), 0o755)
	os.WriteFile(mappingsPath, []byte(`[{"board":"JP07","control":"Shift:26","gesture":"press","action":"sync"}]`), 0o644)
	if v = mapView(t, mapReq(t, s, "GET", "/api/mapping", "")); v.Status.State != "rejected" || v.Status.Code != "reserved" || len(v.Mappings) != 0 {
		t.Fatalf("%+v %v", v.Status, v.Mappings)
	}
}

func TestMappingRestartRestartsEngineOnce(t *testing.T) {
	mappingDeck(t, "JP07")
	n := 0
	s := mappingServer(t, Config{RestartEngine: func() error { n++; return nil }})
	if w := mapReq(t, s, "POST", "/api/mapping/restart", ""); w.Code != 200 || n != 1 {
		t.Fatalf("%d, %d restarts", w.Code, n)
	}
	s = mappingServer(t, Config{RestartEngine: func() error { return os.ErrPermission }})
	if w := mapReq(t, s, "POST", "/api/mapping/restart", ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("%d", w.Code)
	}
}

func TestMappingRoutesNeedTheAccessCode(t *testing.T) {
	mappingDeck(t, "JP07")
	s := mappingServer(t, Config{})
	for _, p := range []string{"/api/mapping", "/api/mapping/export", "/api/mapping/learn"} {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Errorf("%s answered without the access code", p)
		}
	}
}

// learnSource is a fake ALSA client: it counts opens and subscriptions, and plays events when told.
type learnSource struct {
	mu         sync.Mutex
	ports      []mapping.Port
	events     chan []mapping.Event
	subscribed int
	closed     int
}

func (l *learnSource) Ports() ([]mapping.Port, error) { return l.ports, nil }
func (l *learnSource) Subscribe(mapping.Port) error {
	l.mu.Lock()
	l.subscribed++
	l.mu.Unlock()
	return nil
}
func (l *learnSource) Next(wait time.Duration) ([]mapping.Event, error) {
	select {
	case e := <-l.events:
		return e, nil
	case <-time.After(wait):
		return nil, nil
	}
}
func (l *learnSource) Close() error {
	l.mu.Lock()
	l.closed++
	l.mu.Unlock()
	return nil
}
func (l *learnSource) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.subscribed, l.closed
}

func newLearnSource() *learnSource {
	return &learnSource{events: make(chan []mapping.Event, 8),
		ports: []mapping.Port{{Client: 20, Port: 0, ClientName: "SC5000 Prime Controller", PortName: "Jack 1"}}}
}

func learnState(t *testing.T, s *Server) mappingLearnView {
	t.Helper()
	w := mapReq(t, s, "GET", "/api/mapping/learn", "")
	var v mappingLearnView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func waitLearn(t *testing.T, s *Server, state string) mappingLearnView {
	t.Helper()
	for i := 0; i < 200; i++ {
		if v := learnState(t, s); v.State == state {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("learn never reached %q: %+v", state, learnState(t, s))
	return mappingLearnView{}
}

func TestMappingLearnReportsOneGestureAndClosesItsSubscription(t *testing.T) {
	mappingDeck(t, "JP07")
	src := newLearnSource()
	opened := 0
	s := mappingServer(t, Config{MappingSource: func() (mapping.Source, error) { opened++; return src, nil }})
	if opened != 0 {
		t.Fatal("the sequencer was opened before a learn")
	}
	if v := learnState(t, s); v.State != "idle" || opened != 0 {
		t.Fatalf("%+v opened %d: nothing subscribes when not learning", v, opened)
	}
	if w := mapReq(t, s, "POST", "/api/mapping/learn", ""); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if v := learnState(t, s); v.State != "running" || v.SecondsLeft < 1 {
		t.Fatalf("%+v", v)
	}
	if w := mapReq(t, s, "POST", "/api/mapping/learn", ""); w.Code != http.StatusConflict {
		t.Fatalf("a second learn while one runs: %d", w.Code)
	}
	src.events <- []mapping.Event{{Note: 26, On: true}}
	src.events <- []mapping.Event{{Note: 28, On: true}}
	v := waitLearn(t, s, "done")
	if v.Result == nil || v.Result.Gesture != "shift" || v.Result.Control != "PadModeSelect:28" || !v.Result.Offer.Mappable {
		t.Fatalf("%+v", v.Result)
	}
	if subs, closed := src.counts(); subs != 1 || closed != 1 || opened != 1 {
		t.Fatalf("subscribed %d, closed %d, opened %d", subs, closed, opened)
	}
	// the page can start again
	if w := mapReq(t, s, "POST", "/api/mapping/learn", ""); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
	if v := learnState(t, s); v.State != "running" || v.Result != nil {
		t.Fatalf("a new learn keeps the old result: %+v", v)
	}
}

func TestMappingLearnTimesOutCancelsAndNeverSubscribesOnABoardWithoutMapping(t *testing.T) {
	mappingDeck(t, "JP07")
	src := newLearnSource()
	s := mappingServer(t, Config{MappingLearnWindow: 120 * time.Millisecond, MappingSource: func() (mapping.Source, error) { return src, nil }})
	mapReq(t, s, "POST", "/api/mapping/learn", "")
	waitLearn(t, s, "timeout")
	if subs, closed := src.counts(); subs != 1 || closed != 1 {
		t.Fatalf("subscribed %d, closed %d", subs, closed)
	}
	// cancelled by the page: the subscription is gone when DELETE answers
	src2 := newLearnSource()
	s = mappingServer(t, Config{MappingSource: func() (mapping.Source, error) { return src2, nil }})
	mapReq(t, s, "POST", "/api/mapping/learn", "")
	mapWaitFor(t, func() bool { subs, _ := src2.counts(); return subs == 1 })
	w := mapReq(t, s, "DELETE", "/api/mapping/learn", "")
	if subs, closed := src2.counts(); w.Code != 200 || subs != 1 || closed != 1 {
		t.Fatalf("%d subscribed %d, closed %d", w.Code, subs, closed)
	}
	if v := learnState(t, s); v.State != "idle" {
		t.Fatalf("%+v", v)
	}
	// closing the server ends a learn that is still running
	src3 := newLearnSource()
	s3 := mappingServer(t, Config{MappingSource: func() (mapping.Source, error) { return src3, nil }})
	mapReq(t, s3, "POST", "/api/mapping/learn", "")
	mapWaitFor(t, func() bool { subs, _ := src3.counts(); return subs == 1 })
	s3.Close()
	if _, closed := src3.counts(); closed != 1 {
		t.Fatalf("closed %d", closed)
	}
	// a board without button mapping opens nothing
	mappingDeck(t, "JC11")
	opened := 0
	s = mappingServer(t, Config{MappingSource: func() (mapping.Source, error) { opened++; return src, nil }})
	if w := mapReq(t, s, "POST", "/api/mapping/learn", ""); w.Code != http.StatusBadRequest || opened != 0 {
		t.Fatalf("%d, opened %d", w.Code, opened)
	}
}

func TestMappingLearnSaysWhenTheSequencerCannotBeOpened(t *testing.T) {
	mappingDeck(t, "JP07")
	s := mappingServer(t, Config{MappingSource: func() (mapping.Source, error) { return nil, os.ErrNotExist }})
	if w := mapReq(t, s, "POST", "/api/mapping/learn", ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("%d", w.Code)
	}
	if v := learnState(t, s); v.State != "error" || !strings.Contains(v.Error, "ALSA") {
		t.Fatalf("%+v", v)
	}
	mapReq(t, s, "DELETE", "/api/mapping/learn", "")
	if v := learnState(t, s); v.State != "idle" || v.Error != "" {
		t.Fatalf("cancel clears the old learn error: %+v", v)
	}
	// no controller port: the learn fails with a reason and the source is closed
	src := &learnSource{events: make(chan []mapping.Event), ports: []mapping.Port{{Client: 14, ClientName: "Midi Through"}}}
	s = mappingServer(t, Config{MappingSource: func() (mapping.Source, error) { return src, nil }})
	mapReq(t, s, "POST", "/api/mapping/learn", "")
	if v := waitLearn(t, s, "error"); !strings.Contains(v.Error, "controller") {
		t.Fatalf("%+v", v)
	}
	if subs, closed := src.counts(); subs != 0 || closed != 1 {
		t.Fatalf("subscribed %d, closed %d", subs, closed)
	}
}

func mapWaitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never held")
}
