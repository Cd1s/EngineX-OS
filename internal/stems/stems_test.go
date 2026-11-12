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
	// Non-loopback private/CGNAT LAN decks can probe /healthz and render without the relay token.
	for addr, want := range map[string]int{
		"127.0.0.1:5000":     401,
		"203.0.113.5:5000":   401,
		"192.168.1.224:5000": 200,
		"192.168.1.238:5000": 200,
		"100.64.1.2:5000":    200,
	} {
		req := httptest.NewRequest("GET", "/healthz", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		b.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("LAN direct %s: got %d, want %d", addr, w.Code, want)
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

func TestSlowRenderAfterDoneDoesNotTimeoutIdleUpload(t *testing.T) {
	prev := uploadIdleRead
	uploadIdleRead = 80 * time.Millisecond
	t.Cleanup(func() { uploadIdleRead = prev })

	// Worker reads init + 1 non-empty block + action=done, then takes 250ms
	// (> uploadIdleRead) to finish draining while Engine keeps its upload open.
	worker := []string{"/bin/sh", "-c", `
printf 'session=FFFFFFFFFFFFFFFF\n\n'
while IFS= read -r l && [ -n "$l" ]; do :; done
printf 'preparing=ready\n\npreparing=ready\nnumReturnChannels=4\n\n'
while IFS= read -r l && [ -n "$l" ]; do :; done
dd bs=16 count=1 >/dev/null 2>&1
while IFS= read -r l; do
  [ "$l" = "action=done" ] && break
done
while IFS= read -r l && [ -n "$l" ]; do :; done
sleep 0.25
printf 'returnFrames=4\n\n'
dd if=/dev/zero bs=64 count=1 2>/dev/null
printf 'returnFrames=0\n\n'
`}
	b := &Backend{Secret: secret, Worker: worker, Slot: make(chan struct{}, 1)}
	s := httptest.NewServer(b)
	defer s.Close()

	in, send := io.Pipe()
	defer send.Close()
	var out bytes.Buffer
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- Proxy(ctx, s.URL, secret, in, &out) }()

	blk := append([]byte("numChannels=1\nsampleRate=44100\nmaxBlockSize=4\n\naction=block\nnumSamples=4\n\n"), make([]byte, 4*4)...)
	send.Write(append(blk, []byte("action=done\n\n")...))
	// Deliberately leave send open (no EOF), matching Engine waiting on child stdout.
	if err := <-done; err != nil {
		t.Fatalf("Proxy killed slow render after action=done: %v", err)
	}
}

func TestLocalRenderRejectsNaNSampleRate(t *testing.T) {
	var out bytes.Buffer
	in := append([]byte("numChannels=2\nsampleRate=NaN\nmaxBlockSize=4\n\naction=block\nnumSamples=1\n\n"), make([]byte, 8)...)
	in = append(in, []byte("action=done\n\n")...)
	if err := LocalRender(bytes.NewReader(in), &out); err == nil {
		t.Fatal("LocalRender accepted sampleRate=NaN without error")
	}
}
