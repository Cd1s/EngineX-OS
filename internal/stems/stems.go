// Package stems tunnels the native Engine 5.0.4 Stems child-process byte stream
// unchanged between the deck (stems-processor) and the renderer (sc5000-stemsd).
package stems

import (
	"bufio"
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol is the Engine child-process dialect carried in X-Stems-Protocol.
const Protocol = "engine-5.0.4"

// FramingHeader asks the renderer to wrap its reply in frames: 'D' + uint32
// big-endian length + Engine bytes, and a closing 'E' + length + "ok" or
// "failed" once the worker has exited. Unlike the X-Stems-Status trailer,
// the closing frame is ordinary body data, so edge proxies pass it on.
// Value 2 also asks for packed replies (pack.go); a renderer that packs
// answers 2, one that only frames answers 1. The relay forwards the value.
const FramingHeader = "X-Stems-Framing"

const maxFrame = 1 << 20

var startupTimeout = 30 * time.Second

// ErrUnreachable: Proxy could not connect and read nothing from Engine, so
// the caller may still serve the session another way.
var ErrUnreachable = errors.New("stems backend unreachable")

// ErrUnavailable: the backend answered 502/503/504 before any reply reached
// Engine. Whether Engine's stdin was touched is for the caller to tell.
var ErrUnavailable = errors.New("stems backend unavailable")

// Secret reads the renderer's internal bearer token (at least 24 characters).
func Secret(path string) (string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return "", e
	}
	s := strings.TrimSpace(string(b))
	if len(s) < 24 {
		return "", errors.New("credential file must contain at least 24 characters")
	}
	return s, nil
}

// Proxy streams in to the backend's /v1/engine and the reply to out. It needs a
// streaming HTTP/1.1 connection: the child speaks first. endpoint is a base URL
// (a path prefix such as https://relay.example/sc5000 is kept).
func Proxy(ctx context.Context, endpoint, secret string, in io.Reader, out io.Writer) error {
	u, e := url.Parse(endpoint)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return errors.New("invalid backend URL")
	}
	callerCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sess := &Session{}
	pipe := newPipeline(out)
	start := time.Now()
	up := &engineReader{r: tap{in, sess.FeedIn}, p: pipe, buf: make([]byte, 64<<10)}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(endpoint, "/")+"/v1/engine", io.NopCloser(up))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Stems-Protocol", Protocol)
	req.Header.Set(FramingHeader, "2")
	req.Close = true
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &idleConn{Conn: c}, nil
	}, Proxy: nil, ForceAttemptHTTP2: false, ResponseHeaderTimeout: startupTimeout, DisableCompression: true}
	defer tr.CloseIdleConnections()
	var startupFired atomic.Bool
	startup := time.AfterFunc(startupTimeout, func() {
		startupFired.Store(true)
		cancel()
	})
	resp, e := tr.RoundTrip(req)
	startup.Stop()
	if e != nil {
		// A failed dial never started the upload, so Engine's stdin is untouched.
		if op := (*net.OpError)(nil); errors.As(e, &op) && op.Op == "dial" {
			return fmt.Errorf("%w: %v", ErrUnreachable, e)
		}
		var ne net.Error
		if callerCtx.Err() == nil && (startupFired.Load() || (errors.As(e, &ne) && ne.Timeout())) {
			return fmt.Errorf("%w: %v", ErrUnavailable, e)
		}
		return fmt.Errorf("backend connection: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout || (resp.StatusCode >= 520 && resp.StatusCode <= 530) {
			// Busy renderer, relay without one, or offline Cloudflare tunnel: nothing reached Engine yet (#41, BH-GF2-7).
			return fmt.Errorf("%w: backend status %d", ErrUnavailable, resp.StatusCode)
		}
		return fmt.Errorf("backend status %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Stems-Protocol") != Protocol {
		return errors.New("backend protocol mismatch")
	}
	sess.Packed = resp.Header.Get(FramingHeader) == "2"
	pipe.decide(sess.Packed)
	if f := resp.Header.Get(FramingHeader); f != "1" && f != "2" {
		// Older renderer or relay: only the trailer says whether it finished.
		_, e = io.Copy(pipe, resp.Body)
		if e == nil && resp.Trailer.Get("X-Stems-Status") != "ok" {
			return errors.New("remote renderer did not finish successfully")
		}
		return cmp.Or(e, pipe.Complete())
	}
	if e = readFrames(resp.Body, pipe, sess); e != nil {
		return e
	}
	if e = pipe.Complete(); e != nil {
		return e
	}
	// Engine's journal keeps child stderr: frames, blocks and wall time show where a slow render spends it.
	fmt.Fprintf(os.Stderr, "remote stems: ok %s, %.1fs\n", sess, time.Since(start).Seconds())
	return nil
}

// readFrames unwraps a framed reply into out. It returns as soon as the
// renderer's closing frame confirms a session that is complete on the wire,
// without waiting for the HTTP response to end.
func readFrames(body io.Reader, out io.Writer, sess *Session) error {
	br := bufio.NewReaderSize(body, 64<<10)
	var head [5]byte
	var buf []byte
	for {
		if _, e := io.ReadFull(br, head[:]); e != nil {
			return fmt.Errorf("renderer stream ended early: %w", e)
		}
		n := binary.BigEndian.Uint32(head[1:])
		if n > maxFrame {
			return errors.New("renderer frame too large")
		}
		if cap(buf) < int(n) {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, e := io.ReadFull(br, buf); e != nil {
			return fmt.Errorf("renderer stream ended early: %w", e)
		}
		switch head[0] {
		case 'D':
			sess.FeedOut(buf)
			if e := sess.Err(); e != nil {
				return e
			}
			if w, e := out.Write(buf); e != nil || w != len(buf) {
				return cmp.Or(e, io.ErrShortWrite)
			}
		case 'E':
			if string(buf) != "ok" {
				return fmt.Errorf("remote renderer %s", buf)
			}
			return sess.Complete()
		default:
			return errors.New("renderer sent an unknown frame")
		}
	}
}

type tap struct {
	r io.Reader
	f func([]byte)
}

func (t tap) Read(p []byte) (int, error) {
	n, e := t.r.Read(p)
	if n > 0 {
		t.f(p[:n])
	}
	return n, e
}

type frameWriter struct{ w io.Writer }

func (f frameWriter) frame(kind byte, p []byte) error {
	b := make([]byte, 5+len(p))
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:], uint32(len(p)))
	copy(b[5:], p)
	_, e := f.w.Write(b)
	return e
}

func (f frameWriter) Write(p []byte) (int, error) {
	for done := 0; done < len(p); {
		n := min(len(p)-done, maxFrame)
		if e := f.frame('D', p[done:done+n]); e != nil {
			return done, e
		}
		done += n
	}
	return len(p), nil
}

// Enforce inactivity bounds even while an HTTP upload body remains open.
type idleConn struct{ net.Conn }

func (c *idleConn) Read(p []byte) (int, error) {
	c.SetReadDeadline(time.Now().Add(10 * time.Minute))
	return c.Conn.Read(p)
}
func (c *idleConn) Write(p []byte) (int, error) {
	c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.Conn.Write(p)
}

var (
	// Provisional until header/upload timings are measured on real decks (#243).
	uploadHeaderRead   = 30 * time.Second
	uploadProgressRead = 30 * time.Minute
	uploadIdleRead     = 120 * time.Second
	uploadDrainRead    = 10 * time.Minute
)

type idleBody struct {
	r          io.Reader
	c          *http.ResponseController
	tracker    UploadTracker
	timer      *time.Timer
	mu         sync.Mutex
	uploadDone bool
	stopped    bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if b.uploadDone {
		b.c.SetReadDeadline(time.Now().Add(uploadDrainRead))
	} else {
		b.c.SetReadDeadline(time.Now().Add(uploadIdleRead))
	}
	b.mu.Unlock()
	n, err := b.r.Read(p)
	if n > 0 {
		initialized := b.tracker.init
		complete := b.tracker.Feed(p[:n])
		b.mu.Lock()
		if !b.stopped {
			if complete {
				b.uploadDone = true
				b.timer.Stop()
				b.c.SetReadDeadline(time.Now().Add(uploadDrainRead))
			} else if !initialized && b.tracker.init {
				// A fixed upload bound cannot be renewed by trickling bytes
				// or tiny blocks; rendering after action=done is separate.
				b.timer.Reset(uploadProgressRead)
			}
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *idleBody) touchDrain() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.uploadDone && !b.stopped {
		b.c.SetReadDeadline(time.Now().Add(uploadDrainRead))
	}
}

func (b *idleBody) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.stopped = true
	b.c.SetReadDeadline(time.Now())
}

// Backend serves /v1/engine. An empty Secret serves LAN callers without a
// credential (a home renderer found by discovery); others are refused. A
// nonempty Secret is required from every caller.
type Backend struct {
	Secret string
	Worker []string
	Slot   chan struct{}
	Log    io.Writer
	Name   string // separator reported by /healthz: hpss (default) or htdemucs
}
type flushWriter struct {
	w    http.ResponseWriter
	body *idleBody
}

func (f *flushWriter) Write(p []byte) (int, error) {
	http.NewResponseController(f.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	f.body.touchDrain()
	n, e := f.w.Write(p)
	if e == nil {
		e = http.NewResponseController(f.w).Flush()
	}
	return n, e
}
func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if b.Secret == "" {
		if !lanIP(ip) {
			http.Error(w, "LAN only", http.StatusForbidden)
			return
		}
	} else if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.Secret)) != 1 {
		// A configured credential applies to every caller, LAN or not.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		name, separator := cmp.Or(b.Name, "hpss"), "spectral-hpss-approximation"
		if name != "hpss" {
			separator = name
		}
		device, _ := os.Hostname()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": Protocol, "separator": separator,
			"backend": name, "device": device, "sessions": cap(b.Slot), "busy": len(b.Slot) >= cap(b.Slot)})
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/engine" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("X-Stems-Protocol") != Protocol {
		http.Error(w, "protocol required", 400)
		return
	}
	select {
	case b.Slot <- struct{}{}:
		defer func() { <-b.Slot }()
	default:
		http.Error(w, "renderer busy", http.StatusServiceUnavailable)
		return
	}
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		http.Error(w, "full duplex required", 500)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Worker[0], b.Worker[1:]...)
	stdin, e := cmd.StdinPipe()
	if e != nil {
		http.Error(w, "worker pipe", 500)
		return
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		stdin.Close()
		http.Error(w, "worker pipe", 500)
		return
	}
	cmd.Stderr = b.Log
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Stems-Protocol", Protocol)
	w.Header().Set("Connection", "close")
	w.Header().Set("Trailer", "X-Stems-Status")
	framing := r.Header.Get(FramingHeader)
	framed := framing == "1" || framing == "2"
	if framed {
		w.Header().Set(FramingHeader, framing)
	}
	if e = cmd.Start(); e != nil {
		stdin.Close()
		http.Error(w, "worker unavailable", 500)
		return
	}
	w.WriteHeader(200)
	http.NewResponseController(w).Flush()
	// A body error must terminate the worker; a normal EOF is how Engine ends a session.
	done := make(chan error, 1)
	body := &idleBody{r: r.Body, c: http.NewResponseController(w), tracker: UploadTracker{Packed: framing == "2"}}
	body.timer = time.AfterFunc(uploadHeaderRead, func() {
		cancel() // Also interrupts a blocked write to worker stdin.
		body.stop()
	})
	defer body.timer.Stop()
	go func() {
		var dst io.Writer = stdin
		if framing == "2" {
			dst = newUnpackWriter(stdin)
		}
		_, err := io.Copy(dst, body)
		stdin.Close()
		if err != nil {
			cancel()
		}
		done <- err
	}()
	var dst io.Writer = &flushWriter{w: w, body: body}
	if framed {
		dst = frameWriter{dst}
	}
	if framing == "2" {
		dst = newPackWriter(dst)
	}
	_, copyErr := io.Copy(dst, stdout)
	if copyErr != nil {
		cancel()
	}
	e = cmd.Wait()
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	status := "failed"
	if e == nil && copyErr == nil {
		status = "ok"
	}
	if framed {
		frameWriter{&flushWriter{w: w}}.frame('E', []byte(status))
	}
	w.Header().Set("X-Stems-Status", status)
	cancel()
	body.stop()
	r.Body.Close()
	<-done
	if e != nil && b.Log != nil {
		fmt.Fprintln(b.Log, "renderer session failed:", e)
	}
}
