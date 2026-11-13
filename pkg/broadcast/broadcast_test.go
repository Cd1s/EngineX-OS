package broadcast

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

func newTestGuard() *resourceguard.Guard {
	return resourceguard.New(resourceguard.Config{Disabled: true})
}

func TestConfigNormalizationAndDefaults(t *testing.T) {
	b := New(newTestGuard())
	defer b.Close()

	// Default configuration
	cfg := Config{}
	if err := b.Configure(cfg); err != nil {
		t.Fatalf("unexpected error configuring defaults: %v", err)
	}

	st := b.Status()
	if st.Protocol != "icecast" {
		t.Errorf("expected default protocol icecast, got %s", st.Protocol)
	}
	if st.Mountpoint != "/live" {
		t.Errorf("expected default mountpoint /live, got %s", st.Mountpoint)
	}
	if st.Username != "source" {
		t.Errorf("expected default username source, got %s", st.Username)
	}
	if st.Format != "mp3" {
		t.Errorf("expected default format mp3, got %s", st.Format)
	}
	if st.Bitrate != 192 {
		t.Errorf("expected default bitrate 192, got %d", st.Bitrate)
	}
	if st.StreamName != "EngineX OS Live Mix" {
		t.Errorf("expected default streamName, got %s", st.StreamName)
	}

	// Mountpoint normalization (missing leading slash)
	cfg2 := Config{
		Mountpoint: "teststream",
		Protocol:   "ICECAST",
		Format:     "MP3",
		Bitrate:    320,
	}
	if err := b.Configure(cfg2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	st2 := b.Status()
	if st2.Mountpoint != "/teststream" {
		t.Errorf("expected /teststream, got %s", st2.Mountpoint)
	}
	if st2.Protocol != "icecast" {
		t.Errorf("expected lowercase icecast, got %s", st2.Protocol)
	}
	if st2.Format != "mp3" {
		t.Errorf("expected lowercase mp3, got %s", st2.Format)
	}
	if st2.Bitrate != 320 {
		t.Errorf("expected bitrate 320, got %d", st2.Bitrate)
	}

	// Validation errors
	if err := b.Configure(Config{Protocol: "unknown"}); err == nil {
		t.Error("expected error for unknown protocol")
	}
	if err := b.Configure(Config{Format: "flac"}); err == nil {
		t.Error("expected error for unaccepted format flac")
	}
	if err := b.Configure(Config{Bitrate: 96}); err == nil {
		t.Error("expected error for unaccepted bitrate 96")
	}
}

func TestStatusMasksPassword(t *testing.T) {
	b := New(newTestGuard())
	defer b.Close()

	if err := b.Configure(Config{Password: "secretpass"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	st := b.Status()
	if !st.PasswordSet {
		t.Errorf("expected PasswordSet to be true")
	}

	if err := b.Configure(Config{Password: ""}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	st = b.Status()
	if st.PasswordSet {
		t.Errorf("expected PasswordSet to be false")
	}
}

func TestIcecastStreaming(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	var (
		handshakeHeaders []string
		framesReceived   int
		mu               sync.Mutex
		done             = make(chan struct{})
	)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			mu.Lock()
			handshakeHeaders = append(handshakeHeaders, strings.TrimSpace(line))
			mu.Unlock()
			if strings.TrimSpace(line) == "" {
				break
			}
		}

		// Reply with HTTP 200 OK
		_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")

		// Read audio frames
		buf := make([]byte, 1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if n >= 4 && buf[0] == 0xFF && buf[1] == 0xFB {
				mu.Lock()
				framesReceived++
				if framesReceived >= 2 {
					select {
					case <-done:
					default:
						close(done)
					}
				}
				mu.Unlock()
			}
		}
	}()

	b := New(newTestGuard())
	defer b.Close()

	cfg := Config{
		Enabled:    true,
		Protocol:   "icecast",
		ServerURL:  "http://" + ln.Addr().String(),
		Mountpoint: "/stream.mp3",
		Username:   "source",
		Password:   "hackme",
		Format:     "mp3",
		Bitrate:    192,
		StreamName: "Live Set",
	}

	if err := b.Configure(cfg); err != nil {
		t.Fatalf("failed to configure broadcaster: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for Icecast stream frames")
	}

	st := b.Status()
	if !st.Streaming {
		t.Errorf("expected Streaming to be true")
	}
	if st.ConnectedAt == "" {
		t.Errorf("expected ConnectedAt to be non-empty")
	}
	if st.BytesSent <= 0 {
		t.Errorf("expected BytesSent > 0, got %d", st.BytesSent)
	}

	mu.Lock()
	headersText := strings.Join(handshakeHeaders, "\n")
	mu.Unlock()

	if !strings.Contains(headersText, "SOURCE /stream.mp3 HTTP/1.0") {
		t.Errorf("missing SOURCE line in handshake:\n%s", headersText)
	}
	expectedAuth := base64.StdEncoding.EncodeToString([]byte("source:hackme"))
	if !strings.Contains(headersText, "Authorization: Basic "+expectedAuth) {
		t.Errorf("missing expected Authorization header in handshake:\n%s", headersText)
	}
	if !strings.Contains(headersText, "Content-Type: audio/mpeg") {
		t.Errorf("missing Content-Type header in handshake:\n%s", headersText)
	}
	if !strings.Contains(headersText, "Ice-Name: Live Set") {
		t.Errorf("missing Ice-Name header in handshake:\n%s", headersText)
	}

	// Turn off streaming cleanly
	if err := b.SetEnabled(false); err != nil {
		t.Fatalf("failed to disable: %v", err)
	}
	st = b.Status()
	if st.Streaming {
		t.Errorf("expected Streaming to be false after SetEnabled(false)")
	}
}

func TestShoutcastStreaming(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	var (
		passwordReceived string
		icyReceived      []string
		framesReceived   int
		mu               sync.Mutex
		done             = make(chan struct{})
	)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		pwd, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		mu.Lock()
		passwordReceived = strings.TrimSpace(pwd)
		mu.Unlock()

		// Reply with OK2
		_, _ = io.WriteString(conn, "OK2\r\n")

		// Read ICY headers
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			mu.Lock()
			icyReceived = append(icyReceived, strings.TrimSpace(line))
			mu.Unlock()
			if strings.TrimSpace(line) == "" {
				break
			}
		}

		// Read audio data
		buf := make([]byte, 1024)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if n >= 4 && buf[0] == 0xFF && buf[1] == 0xFB {
				mu.Lock()
				framesReceived++
				if framesReceived >= 2 {
					select {
					case <-done:
					default:
						close(done)
					}
				}
				mu.Unlock()
			}
		}
	}()

	b := New(newTestGuard())
	defer b.Close()

	cfg := Config{
		Enabled:    true,
		Protocol:   "shoutcast",
		ServerURL:  ln.Addr().String(), // test raw host:port format without scheme
		Password:   "shoutpwd",
		Bitrate:    256,
		StreamName: "Club Radio",
	}

	if err := b.Configure(cfg); err != nil {
		t.Fatalf("failed to configure shoutcast: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for Shoutcast stream frames")
	}

	mu.Lock()
	pwd := passwordReceived
	icyText := strings.Join(icyReceived, "\n")
	mu.Unlock()

	if pwd != "shoutpwd" {
		t.Errorf("expected password 'shoutpwd', got %q", pwd)
	}
	if !strings.Contains(icyText, "icy-name:Club Radio") {
		t.Errorf("missing icy-name header in:\n%s", icyText)
	}
	if !strings.Contains(icyText, "icy-br:256") {
		t.Errorf("missing icy-br:256 header in:\n%s", icyText)
	}

	st := b.Status()
	if !st.Streaming {
		t.Errorf("expected Streaming to be true")
	}
	if st.BytesSent <= 0 {
		t.Errorf("expected BytesSent > 0, got %d", st.BytesSent)
	}
}

func TestIcecastHandshakeReject(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			reader := bufio.NewReader(conn)
			for {
				line, err := reader.ReadString('\n')
				if err != nil || strings.TrimSpace(line) == "" {
					break
				}
			}
			_, _ = io.WriteString(conn, "HTTP/1.0 401 Unauthorized\r\n\r\n")
			conn.Close()
		}
	}()

	b := New(newTestGuard())
	defer b.Close()

	cfg := Config{
		Enabled:   true,
		Protocol:  "icecast",
		ServerURL: "http://" + ln.Addr().String(),
	}

	_ = b.Configure(cfg)
	// Wait a moment for connection attempt
	time.Sleep(200 * time.Millisecond)

	st := b.Status()
	if st.Streaming {
		t.Errorf("should not be streaming when rejected")
	}
	if !strings.Contains(st.LastError, "401") && !strings.Contains(st.LastError, "rejected") {
		t.Errorf("expected reject error in LastError, got: %s", st.LastError)
	}
}

func TestShoutcastHandshakeReject(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "invalid password\r\n")
			conn.Close()
		}
	}()

	b := New(newTestGuard())
	defer b.Close()

	cfg := Config{
		Enabled:   true,
		Protocol:  "shoutcast",
		ServerURL: ln.Addr().String(),
		Password:  "wrong",
	}

	_ = b.Configure(cfg)
	time.Sleep(200 * time.Millisecond)

	st := b.Status()
	if st.Streaming {
		t.Errorf("should not be streaming when rejected")
	}
	if !strings.Contains(st.LastError, "authentication failed") {
		t.Errorf("expected authentication failed in LastError, got: %s", st.LastError)
	}
}

func TestUpdateMetadataIcecast(t *testing.T) {
	var (
		lastPath  string
		lastAuth  string
		mu        sync.Mutex
		callCount int
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		lastPath = r.URL.RequestURI()
		lastAuth = r.Header.Get("Authorization")

		if r.URL.Query().Get("song") == "Fail" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintln(w, "Forbidden song")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	}))
	defer server.Close()

	b := New(newTestGuard())
	defer b.Close()

	b.cfg = Config{
		Protocol:   "icecast",
		ServerURL:  server.URL,
		Mountpoint: "/live.mp3",
		Username:   "admin",
		Password:   "hackme",
	}

	// 1. Both artist and title
	err := b.UpdateMetadata("Daft Punk", "One More Time")
	if err != nil {
		t.Fatalf("UpdateMetadata failed: %v", err)
	}
	st := b.Status()
	if st.CurrentTrack != "Daft Punk - One More Time" {
		t.Errorf("unexpected CurrentTrack: %s", st.CurrentTrack)
	}

	mu.Lock()
	if !strings.Contains(lastPath, "/admin/metadata?mode=updinfo&mount=%2Flive.mp3&song=Daft+Punk+-+One+More+Time") {
		t.Errorf("unexpected query URL: %s", lastPath)
	}
	expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:hackme"))
	if lastAuth != expectedAuth {
		t.Errorf("expected auth %s, got %s", expectedAuth, lastAuth)
	}
	mu.Unlock()

	// 2. Title only
	if err := b.UpdateMetadata("", "Solo Title"); err != nil {
		t.Fatalf("UpdateMetadata title-only failed: %v", err)
	}
	if b.Status().CurrentTrack != "Solo Title" {
		t.Errorf("expected 'Solo Title', got %s", b.Status().CurrentTrack)
	}

	// 3. Artist only
	if err := b.UpdateMetadata("Solo Artist", ""); err != nil {
		t.Fatalf("UpdateMetadata artist-only failed: %v", err)
	}
	if b.Status().CurrentTrack != "Solo Artist" {
		t.Errorf("expected 'Solo Artist', got %s", b.Status().CurrentTrack)
	}

	// 4. Server returns error
	err = b.UpdateMetadata("", "Fail")
	if err == nil {
		t.Errorf("expected error when server returns 403")
	}
	if !strings.Contains(b.Status().LastError, "metadata update failed") {
		t.Errorf("expected metadata error in LastError, got: %s", b.Status().LastError)
	}
}

func TestUpdateMetadataShoutcast(t *testing.T) {
	var lastPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	}))
	defer server.Close()

	b := New(newTestGuard())
	defer b.Close()

	b.cfg = Config{
		Protocol:  "shoutcast",
		ServerURL: server.URL,
		Password:  "mypass",
	}

	err := b.UpdateMetadata("Kraftwerk", "Autobahn")
	if err != nil {
		t.Fatalf("UpdateMetadata failed: %v", err)
	}

	if !strings.Contains(lastPath, "/admin.cgi?mode=updinfo&pass=mypass&song=Kraftwerk+-+Autobahn") {
		t.Errorf("unexpected shoutcast query URL: %s", lastPath)
	}
}

func TestWAVAndOggStreaming(t *testing.T) {
	// Test WAV container header and framing
	lnWAV, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer lnWAV.Close()

	doneWAV := make(chan struct{})
	go func() {
		conn, err := lnWAV.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")

		// Read WAV 44-byte header
		header := make([]byte, 44)
		if _, err := io.ReadFull(conn, header); err == nil {
			if string(header[0:4]) == "RIFF" && string(header[8:12]) == "WAVE" {
				close(doneWAV)
			}
		}
	}()

	bWAV := New(newTestGuard())
	defer bWAV.Close()

	cfgWAV := Config{
		Enabled:   true,
		Protocol:  "icecast",
		Format:    "wav",
		ServerURL: "http://" + lnWAV.Addr().String(),
	}
	if err := bWAV.Configure(cfgWAV); err != nil {
		t.Fatalf("failed to configure wav: %v", err)
	}

	select {
	case <-doneWAV:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for WAV stream header")
	}

	// Ogg: no Vorbis/Opus encoder on any deck. The old code sent an "OggS" page that
	// wrapped raw PCM bytes (undecodable); now nothing is sent and the status says why.
	lnOgg, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer lnOgg.Close()
	oggConnected := make(chan struct{}, 1)
	go func() {
		if conn, err := lnOgg.Accept(); err == nil {
			oggConnected <- struct{}{}
			conn.Close()
		}
	}()

	bOgg := New(newTestGuard())
	defer bOgg.Close()

	cfgOgg := Config{
		Enabled:   true,
		Protocol:  "icecast",
		Format:    "ogg",
		ServerURL: "http://" + lnOgg.Addr().String(),
	}
	if err := bOgg.Configure(cfgOgg); err != nil {
		t.Fatalf("failed to configure ogg: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for bOgg.Status().LastError == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := bOgg.Status(); !strings.Contains(st.LastError, "Ogg") || st.Streaming {
		t.Fatalf("ogg: status %+v, want a clear Ogg encoder error", st)
	}
	select {
	case <-oggConnected:
		t.Fatal("ogg: connected to the server without an encoder")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestInputPathPCM(t *testing.T) {
	tmpDir := t.TempDir()
	pcmFile := filepath.Join(tmpDir, "test.pcm")
	pcmData := make([]byte, 4096)
	for i := range pcmData {
		pcmData[i] = byte(i % 256)
	}
	if err := os.WriteFile(pcmFile, pcmData, 0644); err != nil {
		t.Fatalf("failed to write test pcm: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")

		frame := make([]byte, 1024)
		n, err := conn.Read(frame)
		if err == nil && n >= 68 {
			close(done)
		}
	}()

	b := New(newTestGuard())
	defer b.Close()

	cfg := Config{
		Enabled:   true,
		Protocol:  "icecast",
		ServerURL: "http://" + ln.Addr().String(),
		InputPath: pcmFile,
	}

	if err := b.Configure(cfg); err != nil {
		t.Fatalf("failed to configure input path: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for PCM frame with custom input path")
	}
}

func TestUpdateMetadataLastErrorDoesNotLeakPassword(t *testing.T) {
	b := New(newTestGuard())
	defer b.Close()

	secretPassword := "superSecretPassword123!@#"
	// Unreachable port
	b.cfg = Config{
		Protocol:  "shoutcast",
		ServerURL: "http://127.0.0.1:59999",
		Password:  secretPassword,
	}

	err := b.UpdateMetadata("Artist", "Track")
	if err == nil {
		t.Fatal("expected error updating metadata against unreachable URL")
	}

	st := b.Status()
	if strings.Contains(st.LastError, secretPassword) {
		t.Fatalf("Status().LastError leaked plain password: %s", st.LastError)
	}
	escaped := url.QueryEscape(secretPassword)
	if strings.Contains(st.LastError, escaped) {
		t.Fatalf("Status().LastError leaked escaped password: %s", st.LastError)
	}
	if !strings.Contains(st.LastError, "***") {
		t.Fatalf("expected '***' in sanitized LastError, got: %s", st.LastError)
	}
}

func TestLocalOnlyWithoutServerURL(t *testing.T) {
	pcmFile := filepath.Join(t.TempDir(), "live.pcm")
	pcmData := make([]byte, 1152*4)
	for i := range pcmData {
		pcmData[i] = byte(i%250 + 1)
	}
	if err := os.WriteFile(pcmFile, pcmData, 0644); err != nil {
		t.Fatal(err)
	}

	b := New(newTestGuard())
	defer b.Close()
	if err := b.Configure(Config{InputPath: pcmFile}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetEnabled(true); err != nil {
		t.Fatal(err)
	}

	ch, cancel := b.Subscribe()
	defer cancel()
	select {
	case pcm := <-ch:
		if len(pcm) == 0 || pcm[0] != 1 {
			t.Fatalf("unexpected PCM chunk: %d bytes", len(pcm))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no PCM reached the local listener without a ServerURL")
	}
	st := b.Status()
	if !st.Enabled || !st.Streaming || st.Listeners != 1 {
		t.Errorf("unexpected status: %+v", st)
	}

	if err := b.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	for range ch { // closed on disable; must not hang
	}
	if st := b.Status(); st.Streaming || st.Listeners != 0 {
		t.Errorf("expected stopped, got %+v", st)
	}
}

func TestLocalListenersAndExternalPushTogether(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n")
		buf := make([]byte, 1024)
		if n, err := conn.Read(buf); err == nil && n >= 4 {
			close(got)
		}
	}()

	b := New(newTestGuard())
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, ServerURL: "http://" + ln.Addr().String()}); err != nil {
		t.Fatal(err)
	}
	ch, cancel := b.Subscribe()
	defer cancel()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("external server received nothing")
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("local listener received nothing while pushing externally")
	}
}

func TestLANModeBroadcaster(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer ln.Close()

	b := New(newTestGuard())
	defer b.Close()

	// 1. Normalization
	if err := b.Configure(Config{LANMode: "bad"}); err == nil {
		t.Errorf("expected error for invalid LANMode")
	}

	cfg := Config{
		Enabled:   true,
		LANMode:   " RTP ",
		LANTarget: ln.LocalAddr().String(),
	}
	if err := b.Configure(cfg); err != nil {
		t.Fatalf("failed to configure LANMode RTP: %v", err)
	}

	st := b.Status()
	if st.LANMode != "rtp" {
		t.Errorf("expected normalized lanMode 'rtp', got %q", st.LANMode)
	}
	if st.LANTarget != ln.LocalAddr().String() {
		t.Errorf("expected lanTarget %q, got %q", ln.LocalAddr().String(), st.LANTarget)
	}
	if !st.Streaming {
		t.Errorf("expected Streaming == true when LAN streaming is enabled")
	}

	// Verify RTP packets arrive
	buf := make([]byte, 8192)
	_ = ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := ln.ReadFrom(buf)
	if err != nil {
		t.Fatalf("did not receive LAN RTP packet: %v", err)
	}
	pkt, err := ParseRTPPacket(buf[:n])
	if err != nil {
		t.Fatalf("failed to parse RTP packet: %v", err)
	}
	if pkt.Version != 2 {
		t.Errorf("expected RTP version 2, got %d", pkt.Version)
	}

	// Turn off
	if err := b.Configure(Config{LANMode: "off"}); err != nil {
		t.Fatalf("failed to turn off LANMode: %v", err)
	}
	if b.Status().LANMode != "off" {
		t.Errorf("expected lanMode 'off'")
	}
}

func TestShoutcastLoginUsesStreamID(t *testing.T) {
	for _, c := range []struct{ user, want string }{
		{"source", "pw"}, {"", "pw"}, {"admin", "pw"}, {"2", "pw:#2"}, {"#1", "pw:#1"}, {" #3 ", "pw:#3"},
	} {
		if got := shoutcastLogin(Config{Username: c.user, Password: "pw"}); got != c.want {
			t.Errorf("user %q: got %q want %q", c.user, got, c.want)
		}
	}
}

func TestCustomUsernameNormalized(t *testing.T) {
	b := New(newTestGuard())
	defer b.Close()
	if err := b.Configure(Config{Username: " admin ", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.Username != "admin" || !st.PasswordSet {
		t.Fatalf("status = %+v", st)
	}
	if err := b.Configure(Config{}); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.Username != DefaultUsername {
		t.Fatalf("empty username should default to source, got %q", st.Username)
	}
}

// #50: the feed is 48 kHz (the hook resamples the master bus to it), so the WAV
// header must say 48000 Hz like X-Audio-Format and the LAN path. It said 44100.
func TestWAVHeaderMatchesFeedRate(t *testing.T) {
	hdr := WAVHeader()
	if rate := binary.LittleEndian.Uint32(hdr[24:28]); rate != 48000 || SampleRate != 48000 {
		t.Fatalf("WAV header rate %d, SampleRate %d, want 48000", rate, SampleRate)
	}
	if byteRate := binary.LittleEndian.Uint32(hdr[28:32]); byteRate != 48000*4 {
		t.Fatalf("WAV byte rate %d, want %d", byteRate, 48000*4)
	}
}

// #50: the Engine hook keeps the FIFO open between audio blocks. An open but idle
// writer must not park the capture loop (local listeners then get no silence at
// all); data written by the hook is read as soon as it is there.
func TestReadPCMFromIdleFIFOWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.pcm")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	got := make(chan []byte, 1)
	rd := &pcmReader{f: r}
	go func() { got <- rd.read() }()
	select {
	case pcm := <-got:
		if pcm != nil {
			t.Fatalf("idle writer: got %d bytes, want none", len(pcm))
		}
	case <-time.After(time.Second):
		t.Fatal("read blocked on an idle FIFO writer")
	}
	frame := bytes.Repeat([]byte{1, 2, 3, 4}, 1152)
	if _, err := w.Write(frame); err != nil {
		t.Fatal(err)
	}
	if pcm := rd.read(); !bytes.Equal(pcm, frame) {
		t.Fatalf("got %d bytes, want the written frame", len(pcm))
	}
}

// testFrames returns n s16le stereo frames whose two channels differ (L = i, R = ^i),
// so any byte shift shows up as a frame that breaks the pattern.
func testFrames(start, n int) []byte {
	out := make([]byte, 0, n*4)
	for i := start; i < start+n; i++ {
		out = binary.LittleEndian.AppendUint16(out, uint16(i))
		out = binary.LittleEndian.AppendUint16(out, ^uint16(i))
	}
	return out
}

// BH-GF3-2: a read that ends inside a 4-byte frame must not hand out the partial
// frame (the old reader did: the next chunk was shifted, swapping channels or
// turning into full-scale noise). The rest of the frame arrives with the next read,
// and a new writer after EOF starts on a frame boundary again.
func TestPCMReaderKeepsFrameAlignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.pcm")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stream := testFrames(0, 3000)
	rd := &pcmReader{f: r}
	var got []byte
	for off, i := 0, 0; off < len(stream); i++ {
		n := min([]int{4610, 3, 4605, 1, 2, 4097}[i%6], len(stream)-off)
		if _, err := w.Write(stream[off : off+n]); err != nil {
			t.Fatal(err)
		}
		off += n
		for pcm := rd.read(); pcm != nil; pcm = rd.read() {
			if len(pcm)%4 != 0 {
				t.Fatalf("read returned %d bytes: not whole frames", len(pcm))
			}
			got = append(got, pcm...)
		}
	}
	if !bytes.Equal(got, stream) {
		t.Fatalf("got %d bytes, want the %d written, frame for frame", len(got), len(stream))
	}
	// A writer that dies mid-frame: its stray bytes never prefix the next writer's audio.
	if _, err := w.Write([]byte{0xAA, 0xBB}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	for i := 0; i < 3; i++ {
		if pcm := rd.read(); pcm != nil {
			t.Fatalf("stray partial frame returned: %x", pcm)
		}
	}
	w, err = os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	next := testFrames(5000, 10)
	if _, err := w.Write(next); err != nil {
		t.Fatal(err)
	}
	if pcm := rd.read(); !bytes.Equal(pcm, next) {
		t.Fatalf("after a new writer: got %x, want %x", pcm, next)
	}
}

// A single empty tick while music plays must not splice 24 ms of zeros into the
// stream (a click, and the stream drifts ahead); keep-alive silence only follows a
// pause longer than silenceAfter.
func TestSilenceGate(t *testing.T) {
	var g silenceGate
	t0 := time.Unix(1000, 0)
	if pcm, ok := g.next(nil, t0); !ok || len(pcm) != 1152*4 || !bytes.Equal(pcm, make([]byte, 1152*4)) {
		t.Fatal("nothing played yet: want keep-alive silence")
	}
	frame := testFrames(0, 1152)
	if pcm, ok := g.next(frame, t0); !ok || !bytes.Equal(pcm, frame) {
		t.Fatal("live PCM must pass through")
	}
	if _, ok := g.next(nil, t0.Add(24*time.Millisecond)); ok {
		t.Fatal("a 24 ms gap must publish nothing")
	}
	if _, ok := g.next(nil, t0.Add(silenceAfter)); ok {
		t.Fatal("a gap of silenceAfter must still publish nothing")
	}
	if pcm, ok := g.next(nil, t0.Add(silenceAfter+time.Millisecond)); !ok || !bytes.Equal(pcm, make([]byte, 1152*4)) {
		t.Fatal("a longer pause: want keep-alive silence")
	}
}

// End to end through the capture loop: a writer that pauses 60 ms between frames
// (one or two empty ticks) never gets zeros published between its frames.
func TestShortFeedGapIsNotFilledWithSilence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.pcm")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	b := New(newTestGuard())
	defer b.Close()
	if err := b.Configure(Config{Enabled: true, InputPath: path}); err != nil {
		t.Fatal(err)
	}
	ch, cancel := b.Subscribe() // after Configure: a restart closes earlier subscribers
	defer cancel()
	w, err := os.OpenFile(path, os.O_WRONLY, 0) // waits for the capture loop to open it
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	go func() {
		frame := bytes.Repeat([]byte{0x11, 0x22, 0x33, 0x44}, 1152)
		for i := 0; i < 25; i++ {
			if _, err := w.Write(frame); err != nil {
				return
			}
			time.Sleep(60 * time.Millisecond)
		}
	}()
	var chunks [][]byte
	timeout := time.After(1700 * time.Millisecond)
collect:
	for {
		select {
		case pcm, ok := <-ch:
			if !ok {
				break collect
			}
			chunks = append(chunks, pcm)
		case <-timeout:
			break collect
		}
	}
	first, last := -1, -1
	for i, c := range chunks {
		if !bytes.Equal(c, make([]byte, len(c))) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || last-first < 10 {
		t.Fatalf("too little live PCM published: %d chunks, live %d..%d", len(chunks), first, last)
	}
	for i := first; i <= last; i++ {
		if bytes.Equal(chunks[i], make([]byte, len(chunks[i]))) {
			t.Fatalf("chunk %d of %d..%d is spliced silence", i, first, last)
		}
	}
}

// BH2-A3-7: mount point, login and stream name go onto the lines of the server
// handshake as they are; each must stay on its own line.
func TestStreamSettingsStayOnTheirHandshakeLine(t *testing.T) {
	handshake := func(protocol string, cfg Config) string {
		t.Helper()
		cfg.Protocol = protocol
		if err := normalizeConfig(&cfg); err != nil {
			t.Fatal(err)
		}
		deck, server := net.Pipe()
		defer deck.Close()
		got := make(chan string, 1)
		go func() {
			defer server.Close()
			r := bufio.NewReader(server)
			var all string
			if protocol == "shoutcast" { // the login line, then the answer, then the headers
				line, _ := r.ReadString('\n')
				all = line
				_, _ = io.WriteString(server, "OK2\r\n")
			}
			for {
				line, err := r.ReadString('\n')
				all += line
				if err != nil || line == "\r\n" {
					break
				}
			}
			got <- all
			if protocol == "icecast" { // Shoutcast waits for no answer to its headers
				_, _ = io.WriteString(server, "HTTP/1.0 200 OK\r\n\r\n")
			}
		}()
		b := New(newTestGuard())
		defer b.Close()
		_ = deck.SetDeadline(time.Now().Add(3 * time.Second))
		_ = server.SetDeadline(time.Now().Add(3 * time.Second))
		var err error
		if protocol == "shoutcast" {
			err = b.handshakeShoutcast(deck, cfg)
		} else {
			err = b.handshakeIcecast(deck, cfg)
		}
		if err != nil {
			t.Fatalf("%s handshake: %v", protocol, err)
		}
		select {
		case all := <-got:
			return all
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the server never saw the end of the handshake", protocol)
			return ""
		}
	}
	plain := Config{Mountpoint: "/live", Username: "source", Password: "pw", StreamName: "Friday Night Mix"}
	pasted := Config{Mountpoint: "/li\r\nve", Username: "sou\nrce", Password: "pw\r\n", StreamName: "Friday\r\n Night\x00 Mix\n"}
	for _, protocol := range []string{"icecast", "shoutcast"} {
		want, got := handshake(protocol, plain), handshake(protocol, pasted)
		if got != want {
			t.Errorf("%s: settings pasted with line breaks changed the handshake:\n got %q\nwant %q", protocol, got, want)
		}
	}
}

// BH2-A3-12: a bracketed IPv6 server address without a port got a second pair of brackets.
func TestParseServerTargetIPv6WithoutPort(t *testing.T) {
	for raw, want := range map[string]string{
		"[2001:db8::1]":             "[2001:db8::1]:8000",
		"http://[2001:db8::1]/live": "[2001:db8::1]:8000",
		"[2001:db8::1]:9000":        "[2001:db8::1]:9000",
		"2001:db8::1":               "[2001:db8::1]:8000",
		"radio.example":             "radio.example:8000",
		"radio.example:8443":        "radio.example:8443",
		"192.168.1.20":              "192.168.1.20:8000",
	} {
		if _, got, err := parseServerTarget(raw); err != nil || got != want {
			t.Errorf("parseServerTarget(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

// #234 review R4: a configuration saved before 5.3.0.5 may name the hook's feed under its old
// name; the hook of this release writes only the new one. The built-in feed is this release's
// under either name, and any other path is kept as the user set it.
func TestRename234BuiltinInputUnderTheOldNameIsThisReleasesFeed(t *testing.T) {
	for in, want := range map[string]string{
		"/tmp/sc5000-broadcast.pcm":  DefaultInputPath,
		" /tmp/sc5000-broadcast.pcm": DefaultInputPath,
		"":                           DefaultInputPath,
		"/tmp/my-mix.pcm":            "/tmp/my-mix.pcm",
	} {
		cfg := Config{InputPath: in}
		if err := normalizeConfig(&cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.InputPath != want {
			t.Errorf("%q -> %q, want %q", in, cfg.InputPath, want)
		}
	}
}
