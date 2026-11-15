package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

// The deck shows the renderer a track goes to (#238 review). With a relay that
// only offers hpss and a home computer that has the neural model, tracks go to
// the computer (stemsproc: TestOnlyANeuralRendererGetsTheTrack), so the status
// must name the computer, and the relay's later answers must not put the relay
// back. A home computer that only has hpss changes nothing, and when the
// neural one goes away the relay is reported again.
func TestStemsStatusShowsTheRendererInUse(t *testing.T) {
	prevProbe, prevEvery := lanStemsProbe, lanProbeEvery
	lanProbeEvery = 5 * time.Millisecond
	t.Cleanup(func() { lanStemsProbe, lanProbeEvery = prevProbe, prevEvery })

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz/stems" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"ok":true,"device":"relay-box","backend":"hpss"}`))
	}))
	defer relay.Close()
	var lan atomic.Pointer[stems.LANState] // what answers on the LAN, if anything
	lanStemsProbe = func(context.Context, *stems.LANState, time.Duration) (stems.LANState, bool) {
		if st := lan.Load(); st != nil {
			return *st, true
		}
		return stems.LANState{}, false
	}

	state := t.TempDir()
	s := New(Config{UIDir: t.TempDir(), StateDir: state}, nil)
	defer s.Close()
	relayURL, password := relay.URL, "pw"
	if err := s.updateRelayConfig(&relayURL, &password); err != nil {
		t.Fatal(err)
	}
	shown := func() string {
		if st := s.relayStatusView().LANStems; st != nil && st.Online {
			return st.Hostname + " " + st.Backend
		}
		return ""
	}
	// relayAnswers is the relay's next 30-second answer, now.
	relayAnswers := func() {
		relayProbeMu.Lock()
		delete(relayProbed, state)
		relayProbeMu.Unlock()
		s.refreshRelayStatus()
	}
	settles := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for shown() != want {
			if time.Now().After(deadline) {
				t.Fatalf("the deck shows %q, want %q", shown(), want)
			}
			time.Sleep(10 * time.Millisecond)
			relayAnswers()
		}
		for i := 0; i < 5; i++ { // and it stays, through LAN probes and relay answers
			relayAnswers()
			time.Sleep(10 * time.Millisecond)
			if got := shown(); got != want {
				t.Fatalf("the deck shows %q after settling on %q", got, want)
			}
		}
	}

	settles("relay-box hpss")
	lan.Store(&stems.LANState{Endpoint: "http://192.168.1.50:17890", Hostname: "old-pc", Backend: "hpss"})
	settles("relay-box hpss")
	lan.Store(&stems.LANState{Endpoint: "http://192.168.1.51:17890", Hostname: "studio-pc", Backend: stems.Neural})
	settles("studio-pc " + stems.Neural)
	lan.Store(nil)
	settles("relay-box hpss")
}
