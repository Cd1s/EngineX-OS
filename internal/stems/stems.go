// Package stems tunnels the native Engine 5.0.4 Stems child-process byte stream
// unchanged between the deck (stems-processor) and the renderer (sc5000-stemsd).
package stems

import (
	"context"
	"crypto/subtle"
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
	"time"
)

// Protocol is the Engine child-process dialect carried in X-Stems-Protocol.
const Protocol = "engine-5.0.4"

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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(endpoint, "/")+"/v1/engine", io.NopCloser(in))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Stems-Protocol", Protocol)
	req.Close = true
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &idleConn{Conn: c}, nil
	}, Proxy: nil, ForceAttemptHTTP2: false, ResponseHeaderTimeout: 30 * time.Second, DisableCompression: true}
	defer tr.CloseIdleConnections()
	startup := time.AfterFunc(30*time.Second, cancel)
	resp, e := tr.RoundTrip(req)
	startup.Stop()
	if e != nil {
		return fmt.Errorf("backend connection: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend status %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Stems-Protocol") != Protocol {
		return errors.New("backend protocol mismatch")
	}
	_, e = io.Copy(out, resp.Body)
	if e == nil && resp.Trailer.Get("X-Stems-Status") != "ok" {
		return errors.New("remote renderer did not finish successfully")
	}
	return e
}

// Enforce inactivity bounds even while an HTTP upload body remains open.
type idleConn struct{ net.Conn }

func (c *idleConn) Read(p []byte) (int, error) {
	c.SetReadDeadline(time.Now().Add(120 * time.Second))
	return c.Conn.Read(p)
}
func (c *idleConn) Write(p []byte) (int, error) {
	c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.Conn.Write(p)
}

type idleBody struct {
	r       io.Reader
	c       *http.ResponseController
	mu      sync.Mutex
	stopped bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	b.c.SetReadDeadline(time.Now().Add(120 * time.Second))
	b.mu.Unlock()
	return b.r.Read(p)
}

func (b *idleBody) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	b.c.SetReadDeadline(time.Now())
}

type Backend struct {
	Secret string
	Worker []string
	Slot   chan struct{}
	Log    io.Writer
}
type flushWriter struct {
	w http.ResponseWriter
}

func (f *flushWriter) Write(p []byte) (int, error) {
	http.NewResponseController(f.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	n, e := f.w.Write(p)
	if e == nil {
		e = http.NewResponseController(f.w).Flush()
	}
	return n, e
}
func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := "Bearer " + b.Secret
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"protocol":"engine-5.0.4","separator":"spectral-hpss-approximation"}`)
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
	if e = cmd.Start(); e != nil {
		stdin.Close()
		http.Error(w, "worker unavailable", 500)
		return
	}
	w.WriteHeader(200)
	http.NewResponseController(w).Flush()
	// A body error must terminate the worker; a normal EOF is how Engine ends a session.
	done := make(chan error, 1)
	body := &idleBody{r: r.Body, c: http.NewResponseController(w)}
	go func() {
		_, err := io.Copy(stdin, body)
		stdin.Close()
		if err != nil {
			cancel()
		}
		done <- err
	}()
	_, copyErr := io.Copy(&flushWriter{w: w}, stdout)
	if copyErr != nil {
		cancel()
	}
	e = cmd.Wait()
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if e == nil && copyErr == nil {
		w.Header().Set("X-Stems-Status", "ok")
	} else {
		w.Header().Set("X-Stems-Status", "failed")
	}
	cancel()
	body.stop()
	r.Body.Close()
	<-done
	if e != nil && b.Log != nil {
		fmt.Fprintln(b.Log, "renderer session failed:", e)
	}
}
