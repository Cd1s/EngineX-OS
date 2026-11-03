package stems

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const secret = "abcdefghijklmnopqrstuvwxyz012345"

func TestWorker(t *testing.T) {
	if os.Args[len(os.Args)-1] != "bridge-worker" {
		return
	}
	os.Stdout.Write([]byte("session=FFFFFFFFFFFFFFFF\n\n"))
	p := make([]byte, 4)
	if _, e := io.ReadFull(os.Stdin, p); e != nil {
		os.Exit(2)
	}
	os.Stdout.Write(bytes.ToUpper(p))
	os.Exit(0)
}

// Full duplex, slot release and framing: see TestFramedSessionEndsWithUploadOpen.
func TestAuthenticationAndBusy(t *testing.T) {
	b := &Backend{Secret: secret, Slot: make(chan struct{}, 1)}
	s := httptest.NewServer(b)
	defer s.Close()
	for _, c := range []struct {
		path, key, protocol string
		status              int
	}{{"/healthz", "", "", 401}, {"/healthz", secret, "", 200}, {"/v1/engine", secret, "", 400}, {"/v1/engine", secret, "engine-5.0.4", 503}} {
		b.Slot <- struct{}{}
		method := "POST"
		if c.path == "/healthz" {
			method = "GET"
		}
		req, _ := http.NewRequest(method, s.URL+c.path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("X-Stems-Protocol", c.protocol)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		<-b.Slot
		if resp.StatusCode != c.status {
			t.Fatalf("%s: %d expected %d", c.path, resp.StatusCode, c.status)
		}
	}
}

func TestWorkerFailurePropagates(t *testing.T) {
	b := &Backend{Secret: secret, Worker: []string{"/bin/sh", "-c", "printf broken; exit 1"}, Slot: make(chan struct{}, 1)}
	s := httptest.NewServer(b)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := Proxy(ctx, s.URL, secret, strings.NewReader(""), &out); err == nil {
		t.Fatal("worker failure was reported as success")
	}
}

func TestDisconnectReleasesSlot(t *testing.T) {
	b := &Backend{Secret: secret, Worker: []string{os.Args[0], "-test.run=TestWorker", "--", "bridge-worker"}, Slot: make(chan struct{}, 1)}
	s := httptest.NewServer(b)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	in, send := io.Pipe()
	out, receive := io.Pipe()
	done := make(chan error, 1)
	go func() { e := Proxy(ctx, s.URL, secret, in, receive); receive.CloseWithError(e); done <- e }()
	greeting := make([]byte, len("session=FFFFFFFFFFFFFFFF\n\n"))
	if _, e := io.ReadFull(out, greeting); e != nil {
		cancel()
		t.Fatal(e)
	}
	defer send.Close()
	cancel()
	out.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not cancel")
	}
	until := time.Now().Add(3 * time.Second)
	for len(b.Slot) != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(b.Slot) != 0 {
		t.Fatal("cancelled worker retained render slot")
	}
}

// Unlike an EOF-sensitive worker, sleep cannot exit just because stdin closes.
func TestDisconnectKillsWorkerIgnoringStdin(t *testing.T) {
	b := &Backend{Secret: secret, Worker: []string{"/bin/sh", "-c", "printf 'session=1\\n\\n'; exec sleep 60"}, Slot: make(chan struct{}, 1)}
	s := httptest.NewServer(b)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, send := io.Pipe()
	defer send.Close()
	out, receive := io.Pipe()
	done := make(chan error, 1)
	go func() { e := Proxy(ctx, s.URL, secret, in, receive); receive.CloseWithError(e); done <- e }()
	greeting := make([]byte, 11)
	if _, e := io.ReadFull(out, greeting); e != nil {
		t.Fatal(e)
	}
	if string(greeting) != "session=1\n\n" {
		t.Fatal(string(greeting))
	}
	cancel()
	out.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not cancel")
	}
	until := time.Now().Add(3 * time.Second)
	for len(b.Slot) != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(b.Slot) != 0 {
		t.Fatal("worker ignoring stdin was not killed")
	}
}
