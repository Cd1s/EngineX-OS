package main

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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/sc5000-custom-os/internal/stems"
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
