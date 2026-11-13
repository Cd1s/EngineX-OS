package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type shadowSnapshotWriter struct {
	*httptest.ResponseRecorder
	ready, resume chan struct{}
}

func (w *shadowSnapshotWriter) Header() http.Header {
	if w.ready != nil {
		close(w.ready)
		w.ready = nil
		<-w.resume
	}
	return w.ResponseRecorder.Header()
}

func TestShadowDebugSnapshotSurvivesHeartbeat(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	s.shadow.beacon = false
	t.Cleanup(s.Close)
	s.shadow.own, s.shadow.ownAt = []deckState{{Deck: 1, Seq: 1}}, time.Now()
	ready, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	w := &shadowSnapshotWriter{httptest.NewRecorder(), ready, resume}
	go func() {
		s.shadowDebugGet(w, httptest.NewRequest("GET", "/v1/enginex/debug/shadow", nil))
		close(done)
	}()
	<-ready // the snapshot is built and unlocked; JSON encoding has not begun
	s.decksAlive(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/enginex/decks/alive", nil))
	close(resume)
	<-done
	var out struct{ Own []deckState }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Own) != 1 || out.Own[0].Seq != 1 {
		t.Fatalf("the heartbeat changed the unlocked debug snapshot: %+v", out.Own)
	}
}
