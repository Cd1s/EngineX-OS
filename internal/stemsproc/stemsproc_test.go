package stemsproc

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

// The LAN renderer's worker is this test binary answering with LocalRender.
func TestMain(m *testing.M) {
	if os.Getenv("STEMS_TEST_WORKER") == "1" {
		if err := stems.LocalRender(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// engineInput is what Engine writes to the child: init, blocks, done.
func engineInput(blocks int) string {
	in := "numChannels=2\nsampleRate=44100\nmaxBlockSize=1024\n\n"
	for b := 0; b < blocks; b++ {
		pcm := make([]byte, 2*1024*4)
		for k := 0; k < 2*1024; k++ {
			binary.LittleEndian.PutUint32(pcm[k*4:], math.Float32bits(float32(0.4*math.Sin(float64(b*1024+k)/7))))
		}
		in += "action=block\nnumSamples=1024\n\n" + string(pcm)
	}
	return in + "action=done\n\n"
}

func checkSession(t *testing.T, in string, out []byte) {
	t.Helper()
	s := &stems.Session{}
	s.FeedIn([]byte(in))
	s.FeedOut(out)
	if err := s.Complete(); err != nil {
		t.Fatalf("Engine session: %v (%d bytes out)", err, len(out))
	}
}

func noLocal(t *testing.T) func(io.Reader, io.Writer) error {
	return func(io.Reader, io.Writer) error { t.Fatal("rendered locally"); return nil }
}

// Standalone deck, home computer found: the session is rendered there.
func TestOffloadsToLANRenderer(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("STEMS_TEST_WORKER", "1")
	var sessions atomic.Int32
	b := &stems.Backend{Worker: []string{exe}, Slot: make(chan struct{}, 1), Log: os.Stderr}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/engine" {
			sessions.Add(1)
		}
		b.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	in := engineInput(12)
	var out bytes.Buffer
	find := func(context.Context) (string, bool) { return srv.URL, true }
	if err := run(ctx, stems.Client{Source: "standalone"}, find, noLocal(t), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if sessions.Load() != 1 {
		t.Fatalf("%d LAN sessions", sessions.Load())
	}
	checkSession(t, in, out.Bytes())
}

// Relay down and no home computer: the deck renders the untouched stream itself.
func TestFallsBackToLocalRender(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()
	in := engineInput(3)
	var out bytes.Buffer
	asked := false
	find := func(context.Context) (string, bool) { asked = true; return "", false }
	client := stems.Client{Endpoint: dead, Secret: "pw", Source: "test relay"}
	if err := run(context.Background(), client, find, stems.LocalRender, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if !asked {
		t.Fatal("LAN not tried after the relay was unreachable")
	}
	checkSession(t, in, out.Bytes())
}

// gated releases its input only once opened, like Engine waiting for the greeting.
type gated struct {
	open chan struct{}
	r    io.Reader
}

func (g gated) Read(p []byte) (int, error) {
	<-g.open
	return g.r.Read(p)
}

// #41: a busy relay (503) before Engine sent anything falls back, and bytes a
// given-up upload read later still reach the next renderer in order.
func TestBusyRelayFallsBackWithoutSplittingTheStream(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer relay.Close()
	in := engineInput(3)
	g := gated{open: make(chan struct{}), r: strings.NewReader(in)}
	var out bytes.Buffer
	find := func(context.Context) (string, bool) { return "", false }
	local := func(r io.Reader, w io.Writer) error {
		close(g.open)
		return stems.LocalRender(r, w)
	}
	client := stems.Client{Endpoint: relay.URL, Secret: "pw", Source: "test relay"}
	if err := run(context.Background(), client, find, local, g, &out); err != nil {
		t.Fatal(err)
	}
	checkSession(t, in, out.Bytes())
}

// A renderer that read only the greeting may be replaced; once the stream is
// past the replay window it may not.
func TestMuxReplaysPrefixUntilCommitted(t *testing.T) {
	data := bytes.Repeat([]byte("x"), replayLimit+10)
	m := newStdinMux(bytes.NewReader(data))
	v1 := m.view()
	buf := make([]byte, 100)
	if _, err := v1.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !v1.replayable() {
		t.Fatal("not replayable after the greeting")
	}
	v1.abandon()
	v2 := m.view()
	got, err := io.ReadAll(io.LimitReader(v2, replayLimit))
	if err != nil || !bytes.Equal(got, data[:replayLimit]) {
		t.Fatalf("replay differs: %d bytes, %v", len(got), err)
	}
	if v2.replayable() {
		_, _ = v2.Read(buf)
	}
	if v2.replayable() {
		t.Fatal("still replayable past the window")
	}
}

func TestRelayAuthOrStatusFailureFallsBackToLANAndLocal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		defer relay.Close()

		in := engineInput(2)
		var out bytes.Buffer
		askedLAN := false
		find := func(context.Context) (string, bool) {
			askedLAN = true
			return "", false
		}
		client := stems.Client{Endpoint: relay.URL, Secret: "stale-password", Source: "deemix-config"}
		if err := run(context.Background(), client, find, stems.LocalRender, strings.NewReader(in), &out); err != nil {
			t.Fatalf("status %d aborted stems instead of falling back: %v", status, err)
		}
		if !askedLAN {
			t.Fatalf("status %d skipped LAN fallback", status)
		}
		checkSession(t, in, out.Bytes())
	}
}

func TestDefaultFindLANToleratesWiFiHealthLatency(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), stems.LANStateName)

	b := &stems.Backend{Slot: make(chan struct{}, 1), Name: "htdemucs"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			// Simulate 802.11 Wi-Fi power-save / DTIM wake latency (~500 ms) on PRIME GO.
			time.Sleep(500 * time.Millisecond)
		}
		b.ServeHTTP(w, r)
	}))
	defer srv.Close()

	if err := os.WriteFile(stateFile, []byte(`{"endpoint":"`+srv.URL+`","online":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint, ok := findLANAt(context.Background(), stateFile)
	if !ok || endpoint != srv.URL {
		t.Fatalf("findLANAt over simulated 500 ms Wi-Fi = (%q, %v), want (%q, true)", endpoint, ok, srv.URL)
	}
}
