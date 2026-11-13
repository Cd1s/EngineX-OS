package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// relayFake answers /healthz/stems as device name, or hangs (accepts, never answers) while
// hang is set. entered counts the requests that reached it; cancelled those whose request
// context ended before 5 s.
type relayFake struct {
	*httptest.Server
	hang      atomic.Bool
	entered   chan struct{}
	cancelled chan time.Duration
}

func newRelayFake(t *testing.T, name string, hang bool) *relayFake {
	f := &relayFake{entered: make(chan struct{}, 16), cancelled: make(chan time.Duration, 16)}
	f.hang.Store(hang)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.entered <- struct{}{}
		if f.hang.Load() {
			start := time.Now()
			select {
			case <-r.Context().Done():
				f.cancelled <- time.Since(start)
			case <-time.After(5 * time.Second):
			}
			return
		}
		w.Write([]byte(`{"ok":true,"device":"` + name + `","backend":"htdemucs"}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func pollUI(t *testing.T, s *Server) (string, time.Duration) {
	t.Helper()
	r := httptest.NewRequest("GET", "/v1/sc5000/ui", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	w := httptest.NewRecorder()
	start := time.Now()
	s.ServeHTTP(w, r)
	return w.Body.String(), time.Since(start)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Review of 25cea50: the probe held relayProbeMu for its whole request, so a settings write
// during a hung probe waited for it (updateRelayConfig 1.5 s, under uiMu, and with it
// GET /v1/sc5000/ui).
func TestSettingsWriteNeverWaitsOnAHungProbe(t *testing.T) {
	relay := newRelayFake(t, "hung", true)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	url, pw := relay.URL, "pw"
	if err := s.updateRelayConfig(&url, &pw); err != nil {
		t.Fatal(err)
	}
	pollUI(t, s) // starts the refresher, whose probe hangs
	select {
	case <-relay.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the background probe never reached the relay")
	}
	other, pw2 := url+"/", "pw2"
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		s.updateRelayConfig(&other, &pw2)
		done <- time.Since(start)
	}()
	time.Sleep(10 * time.Millisecond)
	if _, took := pollUI(t, s); took > 50*time.Millisecond {
		t.Fatalf("GET /v1/sc5000/ui during a settings write took %v", took)
	}
	select {
	case took := <-done:
		if took > 50*time.Millisecond {
			t.Fatalf("the settings write took %v", took)
		}
	case <-time.After(time.Second):
		t.Fatal("the settings write waited for the hung probe")
	}
}

// Review of 25cea50: a settings change left the probe in flight running and kept what had been
// found for the old relay; and a fresh, online answer of relay A meant relay B was never
// asked after switching, the deck went on showing A.
func TestRelaySwitchDropsTheOldRelayAndAsksTheNewOne(t *testing.T) {
	a := newRelayFake(t, "relay-a", false)
	b := newRelayFake(t, "relay-b", true)
	c := newRelayFake(t, "relay-c", false)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	pw := "pw"
	set := func(u string) {
		if err := s.updateRelayConfig(&u, &pw); err != nil {
			t.Fatal(err)
		}
	}
	set(a.URL)
	waitFor(t, "relay A's answer", func() bool { body, _ := pollUI(t, s); return strings.Contains(body, `"relay-a"`) })

	// To B, which hangs: A is gone at once, and B is asked although A's answer is fresh.
	set(b.URL)
	if body, _ := pollUI(t, s); strings.Contains(body, `"relay-a"`) {
		t.Fatal("still showing relay A after switching to relay B")
	}
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("relay B was never asked: relay A's fresh answer stopped the probe")
	}

	// To C while B's probe hangs: B's request is cancelled, C answers.
	set(c.URL)
	select {
	case took := <-b.cancelled:
		if took > 500*time.Millisecond {
			t.Fatalf("relay B's request ran %v after the settings changed", took)
		}
	case <-time.After(time.Second):
		t.Fatal("relay B's request was not cancelled by the settings change")
	}
	waitFor(t, "relay C's answer", func() bool { body, _ := pollUI(t, s); return strings.Contains(body, `"relay-c"`) })
	if body, _ := pollUI(t, s); strings.Contains(body, `"relay-a"`) || strings.Contains(body, `"relay-b"`) {
		t.Fatalf("an old relay is still shown: %s", body)
	}
}

// Close cancels a probe in flight.
func TestCloseCancelsTheProbeInFlight(t *testing.T) {
	relay := newRelayFake(t, "hung", true)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	url, pw := relay.URL, "pw"
	if err := s.updateRelayConfig(&url, &pw); err != nil {
		t.Fatal(err)
	}
	pollUI(t, s)
	select {
	case <-relay.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the background probe never reached the relay")
	}
	s.Close()
	select {
	case took := <-relay.cancelled:
		if took > 500*time.Millisecond {
			t.Fatalf("the probe ran %v after Close", took)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the probe")
	}
}
