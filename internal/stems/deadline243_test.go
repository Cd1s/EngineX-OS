package stems

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const init243 = "numChannels=1\nsampleRate=44100\nmaxBlockSize=1048576\n\n"

func deadline243Limits(t *testing.T) {
	t.Helper()
	header, progress, idle := uploadHeaderRead, uploadProgressRead, uploadIdleRead
	uploadHeaderRead, uploadProgressRead, uploadIdleRead = 150*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { uploadHeaderRead, uploadProgressRead, uploadIdleRead = header, progress, idle })
}

func deadline243Connection(t *testing.T, worker []string, framing string) (net.Conn, io.WriteCloser, *http.Response, <-chan struct{}) {
	t.Helper()
	b := &Backend{Secret: secret, Worker: worker, Slot: make(chan struct{}, 1)}
	finished := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.ServeHTTP(w, r)
		close(finished)
	}))
	t.Cleanup(s.Close)
	c, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "POST /v1/engine HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nX-Stems-Protocol: %s\r\n%s: %s\r\nTransfer-Encoding: chunked\r\n\r\n", secret, Protocol, FramingHeader, framing)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Returning from ServeHTTP proves cmd.Wait and the upload goroutine finished;
	// the deferred slot release must already have run as well.
	t.Cleanup(func() {
		c.Close()
		select {
		case <-finished:
			if len(b.Slot) != 0 {
				t.Error("handler returned with its slot held")
			}
		case <-time.After(3 * time.Second):
			t.Error("handler retained its worker or upload goroutine")
		}
	})
	return c, httputil.NewChunkedWriter(c), resp, finished
}

func TestDeadline243ReleasesUnfinishedUploads(t *testing.T) {
	deadline243Limits(t)
	for _, tc := range []struct {
		name, initial, drip string
		header              bool
	}{
		{"stalled-header", "numChannels=", "", true},
		{"trickling-header", "numChannels=", "1", true},
		{"trickling-block-header", init243 + "action=", "b", false},
		{"trickling-payload", init243 + "action=block\nnumSamples=1048576\n\n", "\x00", false},
		{"empty-blocks", init243, "action=block\nnumSamples=0\n\n", false},
		{"tiny-complete-blocks", init243, "action=block\nnumSamples=1\n\n\x00\x00\x00\x00", false},
		{"worker-backpressure", init243 + "action=block\nnumSamples=1048576\n\n" + strings.Repeat("\x00", 128<<10), "", false},
		{"disconnect", "numChannels=", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// exec preserves this PID; stdin closure alone cannot stop sleep.
			worker := []string{"/bin/sh", "-c", "printf 'pid=%s\\n\\n' \"$$\"; exec sleep 60"}
			c, upload, resp, finished := deadline243Connection(t, worker, "")
			greeting := bufio.NewReader(resp.Body)
			line, err := greeting.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid=")))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if _, err := io.WriteString(upload, tc.initial); err != nil {
				t.Fatal(err)
			}
			if tc.name == "disconnect" {
				c.Close()
			}
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			timeout := time.NewTimer(2 * time.Second)
			defer timeout.Stop()
			for {
				select {
				case <-finished:
					if tc.header && time.Since(start) >= 400*time.Millisecond {
						t.Error("incomplete initial header waited for the upload idle bound")
					}
					if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
						t.Errorf("worker still exists after slot release: %v", err)
					}
					return
				case <-ticker.C:
					if tc.drip != "" {
						io.WriteString(upload, tc.drip)
					}
				case <-timeout.C:
					t.Fatal("unfinished upload retained the renderer slot and worker")
				}
			}
		})
	}
}

func TestDeadline243HealthySlowUploads(t *testing.T) {
	deadline243Limits(t)
	uploadProgressRead = 3 * time.Second
	for _, framing := range []string{"1", "2"} {
		t.Run(framing, func(t *testing.T) {
			worker := []string{os.Args[0], "-test.run=^TestProtocolWorker$", "--", "protocol-worker"}
			_, upload, resp, _ := deadline243Connection(t, worker, framing)
			sess := &Session{Packed: framing == "2"}
			sess.FeedIn([]byte(init243))
			if _, err := io.WriteString(upload, init243); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 4; i++ {
				header := "action=block\nnumSamples=4\n\n"
				// Split payloads keep a healthy upload alive beyond the initial
				// header and idle windows, but inside the total upload bound.
				sess.FeedIn([]byte(header + strings.Repeat("\x00", 16)))
				io.WriteString(upload, header)
				n := 16
				if framing == "2" {
					n = 8
				}
				for j := 0; j < 4; j++ {
					time.Sleep(60 * time.Millisecond)
					if _, err := io.WriteString(upload, strings.Repeat("\x00", n/4)); err != nil {
						t.Fatal(err)
					}
				}
			}
			sess.FeedIn([]byte("action=done\n\n"))
			io.WriteString(upload, "action=done\n\n")
			// Keep the request open while waiting for the final frame, like Engine.
			if err := readFrames(resp.Body, io.Discard, sess); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeadline243SlowRenderAfterDone(t *testing.T) {
	deadline243Limits(t)
	uploadIdleRead = 80 * time.Millisecond
	worker := []string{"/bin/sh", "-c", `
printf 'session=FFFFFFFFFFFFFFFF\n\n'
while IFS= read -r l; do
  if [ "$l" = "action=done" ]; then
    IFS= read -r l
    break
  fi
done
sleep 0.8
printf 'numReturnChannels=4\n\nreturnFrames=0\n\n'
`}
	_, upload, resp, _ := deadline243Connection(t, worker, "1")
	in := init243 + "action=done\n\n"
	if _, err := io.WriteString(upload, in); err != nil {
		t.Fatal(err)
	}
	sess := &Session{}
	sess.FeedIn([]byte(in))
	if err := readFrames(resp.Body, io.Discard, sess); err != nil {
		t.Fatalf("slow render exceeded upload deadlines after done: %v", err)
	}
}
