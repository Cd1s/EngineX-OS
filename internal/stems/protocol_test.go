package stems

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProtocolWorker is re-executed as a worker speaking worker.py's dialect
// with zero latency. STEMS_TEST_MODE=fail exits 1 after draining; truncate
// exits 0 without the closing returnFrames=0.
func TestProtocolWorker(t *testing.T) {
	if os.Args[len(os.Args)-1] != "protocol-worker" {
		return
	}
	in, out := bufio.NewReader(os.Stdin), bufio.NewWriter(os.Stdout)
	send := func(s string) { out.WriteString(s); out.Flush() }
	header := func() map[string]string {
		f := map[string]string{}
		for {
			l, err := in.ReadString('\n')
			if err != nil {
				os.Exit(3)
			}
			if l = strings.TrimSpace(l); l == "" {
				return f
			}
			k, v, _ := strings.Cut(l, "=")
			f[k] = v
		}
	}
	send("session=FFFFFFFFFFFFFFFF\n\n")
	ch, _ := strconv.Atoi(header()["numChannels"])
	send(fmt.Sprintf("preparing=ready\n\npreparing=ready\nnumReturnChannels=%d\n\n", 4*ch))
	for {
		f := header()
		if f["action"] == "done" {
			break
		}
		n, _ := strconv.Atoi(f["numSamples"])
		if _, err := io.ReadFull(in, make([]byte, n*ch*4)); err != nil {
			os.Exit(4)
		}
		out.WriteString(fmt.Sprintf("returnFrames=%d\n\n", n))
		out.Write(bytes.Repeat([]byte{1}, n*4*ch*4))
		out.Flush()
	}
	switch os.Getenv("STEMS_TEST_MODE") {
	case "truncate":
		os.Exit(0)
	case "fail":
		send("returnFrames=0\n\n")
		os.Exit(1)
	}
	send("returnFrames=0\n\n")
	os.Exit(0)
}

func protocolBackend(t *testing.T, mode string) (*Backend, *httptest.Server) {
	t.Setenv("STEMS_TEST_MODE", mode)
	b := &Backend{Secret: secret, Worker: []string{os.Args[0], "-test.run=^TestProtocolWorker$", "--", "protocol-worker"}, Slot: make(chan struct{}, 1)}
	s := httptest.NewServer(b)
	t.Cleanup(s.Close)
	return b, s
}

// holdingEdge forwards like an HTTP/1.1 reverse proxy but, as the deck's
// edge appeared to on 2026-11-03, only ends its reply once the upload ends.
func holdingEdge(t *testing.T, upstream string) (*httptest.Server, chan struct{}) {
	held := make(chan struct{}, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		rc.EnableFullDuplex()
		pr, pw := io.Pipe()
		uploaded := make(chan struct{})
		go func() { _, e := io.Copy(pw, r.Body); pw.CloseWithError(e); close(uploaded) }()
		req, _ := http.NewRequest("POST", upstream+r.URL.Path, pr)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 32<<10)
		for {
			n, e := resp.Body.Read(buf)
			w.Write(buf[:n])
			rc.Flush()
			if e != nil {
				break
			}
		}
		held <- struct{}{}
		<-uploaded
	}))
	t.Cleanup(s.Close)
	return s, held
}

// engineSide plays Engine: init, three blocks, done, then keeps stdin open
// the way Engine may until its child exits. It returns the bytes received.
func engineSide(t *testing.T, endpoint string) ([]byte, error, func()) {
	in, send := io.Pipe()
	var got bytes.Buffer
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	go func() { done <- Proxy(ctx, endpoint, secret, in, &got) }()
	send.Write([]byte("version=1.6.0\nnumChannels=2\nsampleRate=44100\nmaxBlockSize=256\n\n"))
	for i := 0; i < 3; i++ {
		send.Write([]byte("action=block\nnumSamples=256\n\n"))
		send.Write(make([]byte, 256*2*4))
	}
	send.Write([]byte("action=done\n\n"))
	start := time.Now()
	err := <-done
	if time.Since(start) > 5*time.Second {
		t.Fatalf("client waited %v for the end of the reply", time.Since(start))
	}
	return got.Bytes(), err, func() { send.Close(); cancel() }
}

func TestFramedSessionEndsWithUploadOpen(t *testing.T) {
	b, backend := protocolBackend(t, "")
	edge, held := holdingEdge(t, backend.URL)
	got, err, closeUpload := engineSide(t, edge.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The client is already done; the edge has the whole reply yet still waits on the open upload.
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("edge never saw the end of the reply: the test proves nothing")
	}
	greeting := "session=FFFFFFFFFFFFFFFF\n\npreparing=ready\n\npreparing=ready\nnumReturnChannels=8\n\n"
	if !strings.HasPrefix(string(got), greeting) || !strings.HasSuffix(string(got), "returnFrames=0\n\n") {
		t.Fatalf("Engine got %q...", got[:min(len(got), 120)])
	}
	// Packed on the wire; Engine still gets all eight float32 channels.
	if streams, _, last := engineReplies(t, got[len(greeting):], 8, 256); len(streams[7]) != 3*256 || last != 0 {
		t.Fatalf("Engine got %d frames per channel, want %d", len(streams[7]), 3*256)
	}
	closeUpload()
	until := time.Now().Add(3 * time.Second)
	for len(b.Slot) != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(b.Slot) != 0 {
		t.Fatal("render slot leaked")
	}
}

func TestFramedSessionFailures(t *testing.T) {
	for _, mode := range []string{"fail", "truncate"} {
		_, backend := protocolBackend(t, mode)
		_, err, closeUpload := engineSide(t, backend.URL)
		closeUpload()
		if err == nil {
			t.Fatalf("%s: reported as success", mode)
		}
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestReadFramesSizesAndShortWrites(t *testing.T) {
	frame := func(kind byte, p []byte) []byte {
		return append([]byte{kind, byte(len(p) >> 24), byte(len(p) >> 16), byte(len(p) >> 8), byte(len(p))}, p...)
	}
	// A 200 KiB data frame (larger than the read buffer) and the limit itself must pass.
	big := []byte("numReturnChannels=8\n\nreturnFrames=6400\n\n")
	big = append(big, make([]byte, 6400*8*4)...)
	s := &Session{}
	s.FeedIn([]byte("numChannels=2\nsampleRate=44100\nmaxBlockSize=8192\n\naction=block\nnumSamples=6400\n\n"))
	s.FeedIn(make([]byte, 6400*2*4))
	s.FeedIn([]byte("action=done\n\n"))
	stream := append(frame('D', big), frame('D', []byte("returnFrames=0\n\n"))...)
	var got bytes.Buffer
	if err := readFrames(bytes.NewReader(append(stream, frame('E', []byte("ok"))...)), &got, s); err != nil || got.Len() != len(big)+16 {
		t.Fatalf("large frame: %v (%d bytes)", err, got.Len())
	}
	if err := readFrames(bytes.NewReader(frame('D', make([]byte, maxFrame+1))), io.Discard, &Session{}); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if err := readFrames(bytes.NewReader(frame('D', []byte("numChannels"))), shortWriter{}, &Session{}); err != io.ErrShortWrite {
		t.Fatalf("short write: %v", err)
	}
}

func TestSessionRejectsBadStreams(t *testing.T) {
	init := "numChannels=2\nsampleRate=44100\nmaxBlockSize=4\n\n"
	for name, c := range map[string]struct{ in, out string }{
		"short reply":   {init + "action=block\nnumSamples=2\n\n" + strings.Repeat("x", 16) + "action=done\n\n", "numReturnChannels=8\n\nreturnFrames=1\n\n" + strings.Repeat("y", 32) + "returnFrames=0\n\n"},
		"no done":       {init + "action=block\nnumSamples=0\n\n", "numReturnChannels=8\n\nreturnFrames=0\n\n"},
		"mid payload":   {init + "action=block\nnumSamples=1\n\n" + strings.Repeat("x", 8) + "action=done\n\n", "numReturnChannels=8\n\nreturnFrames=1\n\n" + strings.Repeat("y", 31)},
		"block too big": {init + "action=block\nnumSamples=5\n\n", ""},
		"wrong stems":   {init + "action=done\n\n", "numReturnChannels=2\n\nreturnFrames=0\n\n"},
	} {
		s := &Session{}
		s.FeedIn([]byte(c.in))
		s.FeedOut([]byte(c.out))
		if s.Complete() == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err := (&Session{}).Complete(); err != nil {
		t.Fatalf("greeting-only session: %v", err)
	}
	half := &Session{}
	half.FeedIn([]byte("numChannels=2\n"))
	if half.Complete() == nil {
		t.Fatal("half an init header accepted")
	}
	s := &Session{}
	s.FeedIn([]byte(init + "action=block\nnumSamples=2\n\n" + strings.Repeat("x", 16) + "action=done\n\n"))
	// Split across writes the way TCP delivers it.
	for _, p := range []string{"numReturn", "Channels=8\n\nreturnFrames=0\n", "\nreturnFrames=2\n\n" + strings.Repeat("y", 64), "returnFrames=0\n\n"} {
		s.FeedOut([]byte(p))
	}
	if err := s.Complete(); err != nil {
		t.Fatal(err)
	}
}
