package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// BH-GC2-12: GET /v1/sc5000/ui, which Engine's QML polls every 3 s on its GUI thread, probed a
// configured Stems relay inside the request and waited up to 1.5 s for one that did not answer.
// The probes run in the background now; the handler answers from what they last found.
func TestUIStatusNeverWaitsOnTheRelay(t *testing.T) {
	answer := make(chan bool, 1) // false: hang; true: answer
	answer <- false
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := <-answer
		answer <- ok
		if !ok {
			select { // accepted, never answered: an unreachable relay behind a NAT or a dead tunnel
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		w.Write([]byte(`{"ok":true,"device":"relay-pc","backend":"htdemucs"}`))
	}))
	defer relay.Close()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	url, pw := relay.URL, "pw" // 127.0.0.1 over http counts as a private network
	if err := s.updateRelayConfig(&url, &pw); err != nil {
		t.Fatal(err)
	}
	get := func() (*httptest.ResponseRecorder, time.Duration) {
		r := httptest.NewRequest("GET", "/v1/sc5000/ui", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		start := time.Now()
		s.ServeHTTP(w, r)
		return w, time.Since(start)
	}
	var digest string
	for i := 0; i < 5; i++ {
		w, took := get()
		if w.Code != 200 || took > 50*time.Millisecond {
			t.Fatalf("poll %d with an unreachable relay: %d after %v, want 200 within 50 ms", i, w.Code, took)
		}
		if d := w.Header().Get("X-UI-Digest"); i > 0 && d != digest {
			t.Fatalf("X-UI-Digest changed while nothing did: %s -> %s", digest, d)
		} else {
			digest = d
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The relay answers again; a settings change probes at once, in the background.
	<-answer
	answer <- true
	if err := s.updateRelayConfig(&url, &pw); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		w, took := get()
		if took > 50*time.Millisecond {
			t.Fatalf("poll took %v", took)
		}
		if strings.Contains(w.Body.String(), `"relay-pc"`) {
			if w.Header().Get("X-UI-Digest") == digest {
				t.Fatal("X-UI-Digest did not change with the relay's answer")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the background probe's answer never reached the status: %s", w.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
