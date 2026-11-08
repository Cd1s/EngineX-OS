package broadcast

import (
	"bufio"
	"encoding/base64"
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

	// Test OGG container page
	lnOgg, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer lnOgg.Close()

	doneOgg := make(chan struct{})
	go func() {
		conn, err := lnOgg.Accept()
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

		pageBuf := make([]byte, 128)
		n, err := conn.Read(pageBuf)
		if err == nil && n >= 4 {
			if string(pageBuf[0:4]) == "OggS" {
				close(doneOgg)
			}
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

	select {
	case <-doneOgg:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for Ogg stream page")
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
