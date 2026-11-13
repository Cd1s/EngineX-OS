package main

// Public entry point for remote Stems rendering. A deck authenticates with the
// console password; the relay forwards the full-duplex Engine byte stream to
// the local sc5000-stemsd and presents that daemon's own internal token, which
// therefore never leaves this host.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

const (
	stemsProtocol      = "engine-5.0.4"
	stemsStatusTrailer = "X-Stems-Status"
	stemsTokenFile     = "/etc/sc5000-stems/token"
	// stemsFramingHeader matches internal/stems.FramingHeader: the deck asks
	// sc5000-stemsd for an in-body closing frame, so it need not see the trailer.
	stemsFramingHeader = "X-Stems-Framing"
	// stemsCredential is the LoadCredential= name in the systemd drop-in.
	stemsCredential = "stems-token"
	stemsIdleWrite  = 30 * time.Second
)

var (
	stemsIdleRead  = 120 * time.Second
	stemsDrainRead = 10 * time.Minute
)

type stemsProxy struct {
	target    *url.URL // base URL of sc5000-stemsd, no trailing slash
	tokenFile string
	transport http.RoundTripper
}

func newStemsProxy(target *url.URL, tokenFile string) *stemsProxy {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &stemsProxy{target: target, tokenFile: tokenFile, transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
	}}
}

// parseStemsTarget accepts http://host:port, host:port, and either with the
// /v1/engine path that -stems-target examples sometimes include.
func parseStemsTarget(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid -stems-target %q", raw)
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1/engine")
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u, nil
}

// defaultStemsTokenFile prefers the systemd credential (the relay runs as a
// DynamicUser and cannot read the root-only file itself).
func defaultStemsTokenFile() string {
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		p := filepath.Join(dir, stemsCredential)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return stemsTokenFile
}

// token re-reads the file on each session so a rotated token applies without
// a restart. A missing file means the daemon needs no token.
func (p *stemsProxy) token() (string, error) {
	if p.tokenFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(p.tokenFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (p *stemsProxy) url(path string) string {
	u := *p.target
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String()
}

// stemsPassword reads the console password from headers only: audio sessions
// must not put it in URLs, where proxies log it.
func stemsPassword(r *http.Request) string {
	if v := r.Header.Get("X-Deemix-Password"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

func (s *server) requireStemsAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.fails.blocked(ip) {
			writeJSON(w, 429, map[string]any{"ok": false, "error": "too many failed password attempts, retry later"})
			return
		}
		if !s.cfg.checkPassword(stemsPassword(r)) {
			s.fails.fail(ip)
			log.Printf("auth: rejected %s %s from %s", r.Method, r.URL.Path, ip)
			writeJSON(w, 401, map[string]any{"ok": false, "error": "unauthorized: invalid relay password"})
			return
		}
		if s.cfg.get().PasswordIsDefault {
			writeJSON(w, 403, map[string]any{"ok": false, "error": "change the default console password before using Stems"})
			return
		}
		if s.stems == nil {
			writeJSON(w, 503, map[string]any{"ok": false, "error": "stems backend not configured"})
			return
		}
		h(w, r)
	}
}

func (p *stemsProxy) health(w http.ResponseWriter, r *http.Request) {
	token, err := p.token()
	if err != nil {
		log.Printf("stems: token: %v", err)
		writeJSON(w, 502, map[string]any{"ok": false, "error": "stems backend credential unreadable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", p.url("/healthz"), nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		log.Printf("stems: health: %v", err)
		writeJSON(w, 502, map[string]any{"ok": false, "error": "stems backend unreachable"})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		writeJSON(w, 502, map[string]any{"ok": false, "error": fmt.Sprintf("stems backend status %d", resp.StatusCode)})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// engine relays one render session. The Engine child speaks first and the
// deck keeps uploading while stems come back, so both directions stream at
// once; X-Stems-Status travels as a trailer once the worker has finished.
func (p *stemsProxy) engine(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Stems-Protocol") != stemsProtocol {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "X-Stems-Protocol " + stemsProtocol + " required"})
		return
	}
	token, err := p.token()
	if err != nil {
		log.Printf("stems: token: %v", err)
		writeJSON(w, 502, map[string]any{"ok": false, "error": "stems backend credential unreadable"})
		return
	}
	// One session per connection. Ending a session cuts the upload short with
	// a read deadline, which would otherwise poison a reused keep-alive
	// connection for the next request.
	w.Header().Set("Connection", "close")
	rc := http.NewResponseController(w)
	if err := rc.EnableFullDuplex(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": "full duplex unavailable"})
		return
	}
	body := &stemsBody{
		r:       r.Body,
		rc:      rc,
		tracker: stems.UploadTracker{Packed: r.Header.Get(stemsFramingHeader) == "2"},
		closed:  make(chan struct{}),
	}
	// Runs last: unblock any pending upload read and wait for the transport
	// to release the body before the server discards the connection.
	defer body.finish()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	out, _ := http.NewRequestWithContext(ctx, "POST", p.url("/v1/engine"), body)
	out.Header.Set("Content-Type", "application/octet-stream")
	out.Header.Set("X-Stems-Protocol", stemsProtocol)
	if v := r.Header.Get(stemsFramingHeader); v != "" {
		out.Header.Set(stemsFramingHeader, v)
	}
	if token != "" {
		out.Header.Set("Authorization", "Bearer "+token)
	}
	out.Close = true
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		log.Printf("stems: backend: %v", err)
		writeJSON(w, 502, map[string]any{"ok": false, "error": "stems backend unreachable"})
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 200 && resp.Header.Get("X-Stems-Protocol") != stemsProtocol:
		writeJSON(w, 502, map[string]any{"ok": false, "error": "stems backend protocol mismatch"})
		return
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		// The deck's password was fine; the relay's internal token was not.
		log.Printf("stems: backend rejected the internal token (%d)", resp.StatusCode)
		writeJSON(w, 502, map[string]any{"ok": false, "error": "stems backend rejected the relay"})
		return
	case resp.StatusCode == 400 || resp.StatusCode == 503:
		writeJSON(w, resp.StatusCode, map[string]any{"ok": false, "error": fmt.Sprintf("stems backend status %d", resp.StatusCode)})
		return
	case resp.StatusCode != 200:
		writeJSON(w, 502, map[string]any{"ok": false, "error": fmt.Sprintf("stems backend status %d", resp.StatusCode)})
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Stems-Protocol", stemsProtocol)
	h.Set("Cache-Control", "no-store")
	// Tell any buffering edge proxy (nginx) to pass bytes straight through.
	h.Set("X-Accel-Buffering", "no")
	h.Set("Trailer", stemsStatusTrailer)
	if v := resp.Header.Get(stemsFramingHeader); v != "" {
		h.Set(stemsFramingHeader, v)
	}
	w.WriteHeader(200)
	_ = rc.Flush()

	_, copyErr := io.Copy(&stemsFlushWriter{w: w, rc: rc, body: body}, resp.Body)
	status := resp.Trailer.Get(stemsStatusTrailer)
	if copyErr != nil || status == "" {
		status = "failed"
	}
	if status != "ok" {
		log.Printf("stems: session ended %s (copy: %v)", status, copyErr)
	}
	_ = rc.SetWriteDeadline(time.Now().Add(stemsIdleWrite))
	h.Set(stemsStatusTrailer, status)
}

// stemsBody is the upload side. Each read extends an idle deadline so a
// vanished deck cannot hold the renderer slot; finish ends it for good.
type stemsBody struct {
	r          io.Reader
	rc         *http.ResponseController
	tracker    stems.UploadTracker
	mu         sync.Mutex
	stopped    bool
	eof        bool
	uploadDone bool
	once       sync.Once
	closed     chan struct{}
}

func (b *stemsBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if b.uploadDone || b.eof {
		_ = b.rc.SetReadDeadline(time.Now().Add(stemsDrainRead))
	} else {
		_ = b.rc.SetReadDeadline(time.Now().Add(stemsIdleRead))
	}
	b.mu.Unlock()
	n, err := b.r.Read(p)
	doneNow := n > 0 && b.tracker.Feed(p[:n])
	if err == io.EOF || doneNow {
		// The upload is complete and the server now watches the connection
		// for a hang-up under a bounded drain window refreshed by writes.
		b.mu.Lock()
		if err == io.EOF {
			b.eof = true
		}
		if doneNow {
			b.uploadDone = true
		}
		if !b.stopped {
			_ = b.rc.SetReadDeadline(time.Now().Add(stemsDrainRead))
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *stemsBody) touchDrain() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if (b.uploadDone || b.eof) && !b.stopped {
		_ = b.rc.SetReadDeadline(time.Now().Add(stemsDrainRead))
	}
}

// Close is called by the transport once it stops sending the upload.
func (b *stemsBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func (b *stemsBody) finish() {
	b.mu.Lock()
	b.stopped = true
	if !b.eof {
		_ = b.rc.SetReadDeadline(time.Now())
	}
	b.mu.Unlock()
	select {
	case <-b.closed:
	case <-time.After(5 * time.Second):
		log.Printf("stems: upload copier did not stop in time")
	}
}

type stemsFlushWriter struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	body *stemsBody
}

func (f *stemsFlushWriter) Write(p []byte) (int, error) {
	_ = f.rc.SetWriteDeadline(time.Now().Add(stemsIdleWrite))
	f.body.touchDrain()
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
	}
	return n, err
}
