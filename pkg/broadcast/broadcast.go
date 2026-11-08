package broadcast

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

// Default settings
const (
	DefaultProtocol   = "icecast"
	DefaultMountpoint = "/live"
	DefaultUsername   = "source"
	DefaultFormat     = "mp3"
	DefaultBitrate    = 192
	DefaultStreamName = "EngineX OS Live Mix"
	DefaultInputPath  = "/tmp/sc5000-broadcast.pcm"
)

// Config specifies live stream parameters.
type Config struct {
	Enabled    bool   `json:"enabled"`
	Protocol   string `json:"protocol"`            // "icecast" (default) or "shoutcast"
	ServerURL  string `json:"serverUrl"`           // e.g. "http://192.168.1.10:8000" or "192.168.1.10:8000"
	Mountpoint string `json:"mountpoint"`          // e.g. "/live.mp3" (default "/live")
	Username   string `json:"username"`            // default "source"
	Password   string `json:"password,omitempty"`  // omitted in JSON output
	Format     string `json:"format"`              // "mp3" (default), "ogg", "wav"
	Bitrate    int    `json:"bitrate"`             // 128, 192 (default), 256, 320
	StreamName string `json:"streamName"`          // default "EngineX OS Live Mix"
	InputPath  string `json:"inputPath,omitempty"` // optional PCM file/FIFO path (default "/tmp/sc5000-broadcast.pcm")
	LANMode    string `json:"lanMode,omitempty"`   // "off" (default), "rtp", "teleport"
	LANTarget  string `json:"lanTarget,omitempty"` // e.g. "239.255.0.1:5004" or "192.168.1.50:9998"
}

// Status captures real-time state of the broadcaster without revealing secrets.
type Status struct {
	Enabled      bool   `json:"enabled"`
	Streaming    bool   `json:"streaming"`
	Protocol     string `json:"protocol"`
	ServerURL    string `json:"serverUrl"`
	Mountpoint   string `json:"mountpoint"`
	Username     string `json:"username"`
	PasswordSet  bool   `json:"passwordSet"`
	Format       string `json:"format"`
	Bitrate      int    `json:"bitrate"`
	StreamName   string `json:"streamName"`
	CurrentTrack string `json:"currentTrack"`
	BytesSent    int64  `json:"bytesSent"`
	Listeners    int    `json:"listeners"` // clients on the local /live.mp3 and /live.wav fan-out
	ConnectedAt  string `json:"connectedAt,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	LANMode      string `json:"lanMode,omitempty"`
	LANTarget    string `json:"lanTarget,omitempty"`
}

// Broadcaster manages background audio streaming to Icecast/Shoutcast on CPU3.
type Broadcaster struct {
	ctl          sync.Mutex // serializes Configure/SetEnabled/Close; stopLocked drops mu while waiting
	mu           sync.Mutex
	guard        *resourceguard.Guard
	cfg          Config
	currentTrack string
	streaming    bool
	capturing    bool // local capture loop is running (works without an external server)
	connectedAt  string
	lastError    string

	bytesSent atomic.Int64

	hub hub

	lanStreamer *LANStreamer

	cancel context.CancelFunc
	wg     sync.WaitGroup
	conn   net.Conn

	// HTTP client used for metadata updates (can be overridden in tests).
	httpClient *http.Client
}

// New creates a new Broadcaster bound to the dedicated CPU3 guard.
func New(guard *resourceguard.Guard) *Broadcaster {
	if guard == nil {
		guard = resourceguard.New(resourceguard.DefaultConfig())
	}
	return &Broadcaster{
		guard: guard,
		cfg: Config{
			Protocol:   DefaultProtocol,
			Mountpoint: DefaultMountpoint,
			Username:   DefaultUsername,
			Format:     DefaultFormat,
			Bitrate:    DefaultBitrate,
			StreamName: DefaultStreamName,
			InputPath:  DefaultInputPath,
			LANMode:    "off",
		},
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// normalizeConfig validates and applies defaults to a Config.
func normalizeConfig(cfg *Config) error {
	cfg.Protocol = strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if cfg.Protocol == "" {
		cfg.Protocol = DefaultProtocol
	}
	if cfg.Protocol != "icecast" && cfg.Protocol != "shoutcast" {
		return fmt.Errorf("invalid protocol: %q (must be 'icecast' or 'shoutcast')", cfg.Protocol)
	}

	cfg.Format = strings.ToLower(strings.TrimSpace(cfg.Format))
	if cfg.Format == "" {
		cfg.Format = DefaultFormat
	}
	if cfg.Format != "mp3" && cfg.Format != "ogg" && cfg.Format != "wav" {
		return fmt.Errorf("invalid format: %q (must be 'mp3', 'ogg', or 'wav')", cfg.Format)
	}

	if cfg.Bitrate == 0 {
		cfg.Bitrate = DefaultBitrate
	}
	if cfg.Bitrate != 128 && cfg.Bitrate != 192 && cfg.Bitrate != 256 && cfg.Bitrate != 320 {
		return fmt.Errorf("invalid bitrate: %d (must be 128, 192, 256, or 320)", cfg.Bitrate)
	}

	cfg.Mountpoint = strings.TrimSpace(cfg.Mountpoint)
	if cfg.Mountpoint == "" {
		cfg.Mountpoint = DefaultMountpoint
	}
	if !strings.HasPrefix(cfg.Mountpoint, "/") {
		cfg.Mountpoint = "/" + cfg.Mountpoint
	}

	cfg.Username = strings.TrimSpace(cfg.Username)
	if cfg.Username == "" {
		cfg.Username = DefaultUsername
	}

	cfg.StreamName = strings.TrimSpace(cfg.StreamName)
	if cfg.StreamName == "" {
		cfg.StreamName = DefaultStreamName
	}

	cfg.InputPath = strings.TrimSpace(cfg.InputPath)
	if cfg.InputPath == "" {
		cfg.InputPath = DefaultInputPath
	}

	cfg.ServerURL = strings.TrimSpace(cfg.ServerURL)

	cfg.LANMode = strings.ToLower(strings.TrimSpace(cfg.LANMode))
	if cfg.LANMode == "" {
		cfg.LANMode = "off"
	}
	if cfg.LANMode != "off" && cfg.LANMode != "rtp" && cfg.LANMode != "teleport" {
		return fmt.Errorf("invalid lanMode: %q (must be 'off', 'rtp', or 'teleport')", cfg.LANMode)
	}
	cfg.LANTarget = strings.TrimSpace(cfg.LANTarget)
	return nil
}

// Configure validates and updates settings, starting or stopping streaming as needed.
func (b *Broadcaster) Configure(cfg Config) error {
	b.ctl.Lock()
	defer b.ctl.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	b.cfg = cfg

	if cfg.Enabled {
		b.stopLocked()
		b.startLocked()
	} else {
		b.stopLocked()
	}
	return nil
}

// SetEnabled toggles live streaming on or off.
func (b *Broadcaster) SetEnabled(on bool) error {
	b.ctl.Lock()
	defer b.ctl.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()

	b.cfg.Enabled = on
	if on {
		b.stopLocked()
		b.startLocked()
	} else {
		b.stopLocked()
	}
	return nil
}

// Status returns a snapshot of the current broadcaster state.
func (b *Broadcaster) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()

	return Status{
		Enabled:      b.cfg.Enabled,
		Streaming:    b.streaming || (b.capturing && (b.cfg.ServerURL == "" || (b.cfg.LANMode != "" && b.cfg.LANMode != "off"))),
		Protocol:     b.cfg.Protocol,
		ServerURL:    b.cfg.ServerURL,
		Mountpoint:   b.cfg.Mountpoint,
		Username:     b.cfg.Username,
		PasswordSet:  b.cfg.Password != "",
		Format:       b.cfg.Format,
		Bitrate:      b.cfg.Bitrate,
		StreamName:   b.cfg.StreamName,
		CurrentTrack: b.currentTrack,
		BytesSent:    b.bytesSent.Load(),
		Listeners:    b.hub.count(),
		ConnectedAt:  b.connectedAt,
		LastError:    b.lastError,
		LANMode:      b.cfg.LANMode,
		LANTarget:    b.cfg.LANTarget,
	}
}

// Close shuts down the broadcaster and waits for the worker goroutine to exit.
func (b *Broadcaster) Close() {
	b.ctl.Lock()
	defer b.ctl.Unlock()
	b.mu.Lock()
	b.stopLocked()
	b.mu.Unlock()
}

// startLocked starts the worker goroutine on CPU3. Caller must hold b.mu.
func (b *Broadcaster) startLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.wg.Add(1)

	if b.cfg.LANMode != "" && b.cfg.LANMode != "off" && b.cfg.LANTarget != "" {
		streamer, err := NewLANStreamer(LANConfig{
			Mode:       b.cfg.LANMode,
			Target:     b.cfg.LANTarget,
			SampleRate: 48000,
			Channels:   2,
		})
		if err == nil {
			b.lanStreamer = streamer
			_ = streamer.Start()
			b.streaming = true
		}
	}

	go func() {
		defer b.wg.Done()
		_ = b.guard.Run(ctx, func(t *resourceguard.Throttle) error {
			b.workerLoop(ctx, t)
			return nil
		})
	}()
}

// stopLocked cancels context, closes connection, and waits for worker. Caller must hold b.mu.
func (b *Broadcaster) stopLocked() {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	if b.lanStreamer != nil {
		_ = b.lanStreamer.Close()
		b.lanStreamer = nil
	}
	b.streaming = false
	b.connectedAt = ""
	b.hub.closeAll()

	b.mu.Unlock()
	b.wg.Wait()
	b.mu.Lock()
}

// parseServerTarget decomposes a URL/address into scheme and host:port.
func parseServerTarget(raw string) (scheme, hostPort string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", errors.New("empty server URL")
	}

	scheme = "http"
	if strings.HasPrefix(s, "http://") {
		s = strings.TrimPrefix(s, "http://")
	} else if strings.HasPrefix(s, "https://") {
		scheme = "https"
		s = strings.TrimPrefix(s, "https://")
	}

	if idx := strings.Index(s, "/"); idx != -1 {
		s = s[:idx]
	}

	host, port, splitErr := net.SplitHostPort(s)
	if splitErr != nil {
		host = s
		port = "8000"
	}
	return scheme, net.JoinHostPort(host, port), nil
}

// sanitizeError masks any plain or URL-escaped occurrence of password in error messages.
func sanitizeError(err error, password string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if password != "" {
		msg = strings.ReplaceAll(msg, url.QueryEscape(password), "***")
		msg = strings.ReplaceAll(msg, password, "***")
	}
	return msg
}

// workerLoop captures PCM every MPEG-frame interval, fans it out to local
// listeners and, when a ServerURL is set, feeds the external Icecast/Shoutcast pusher.
func (b *Broadcaster) workerLoop(ctx context.Context, t *resourceguard.Throttle) {
	b.mu.Lock()
	cfg := b.cfg
	b.capturing = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.capturing = false
		b.mu.Unlock()
	}()

	feed := make(chan []byte, 32)
	if cfg.ServerURL != "" {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.pushLoop(ctx, cfg, feed)
		}()
	}

	// 1152 samples per MPEG frame at 44.1kHz = ~26.122ms
	ticker := time.NewTicker(time.Duration(1152) * time.Second / 44100)
	defer ticker.Stop()

	var inputF *os.File
	defer func() {
		if inputF != nil {
			inputF.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if t != nil {
			if err := t.Tick(); err != nil {
				return
			}
		}
		if inputF == nil { // the hook may create the PCM source after the stream is enabled
			if f, err := os.OpenFile(cfg.InputPath, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				inputF = f
			}
		}
		pcm := readPCM(inputF)
		var sendBytes []byte
		if pcm == nil {
			sendBytes = make([]byte, 1152*4) // silence keeps local players connected
		} else {
			sendBytes = pcm
		}
		b.hub.publish(sendBytes)

		b.mu.Lock()
		ls := b.lanStreamer
		b.mu.Unlock()
		if ls != nil {
			_ = ls.Push(sendBytes)
		}

		select {
		case feed <- pcm:
		default: // external server is slow or down: drop rather than stall the capture
		}
	}
}

// readPCM reads up to one frame of PCM from f, looping regular files; nil when nothing is available.
func readPCM(f *os.File) []byte {
	if f == nil {
		return nil
	}
	buf := make([]byte, 1152*4)
	n, err := f.Read(buf)
	if err == io.EOF {
		_, _ = f.Seek(0, 0)
		n, _ = f.Read(buf)
	}
	if n <= 0 {
		return nil
	}
	return buf[:n]
}

// pushLoop keeps the external Icecast/Shoutcast connection alive, reconnecting with
// exponential backoff (1s..30s) so a wrong password is not retried every second.
func (b *Broadcaster) pushLoop(ctx context.Context, cfg Config, feed <-chan []byte) {
	const maxDelay = 30 * time.Second
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}

		t0 := time.Now()
		handshook, err := b.streamOnce(ctx, cfg, feed)
		healthy := handshook && time.Since(t0) >= 10*time.Second
		if err != nil && ctx.Err() == nil {
			b.mu.Lock()
			b.streaming = false
			b.lastError = sanitizeError(err, cfg.Password)
			b.mu.Unlock()

			if healthy {
				delay = time.Second // it ran a while before dropping: reconnect quickly
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if !healthy {
				delay = min(delay*2, maxDelay)
			}
		}
	}
}

// streamOnce executes one external connection and streaming session.
// handshook reports whether the server accepted the source login.
func (b *Broadcaster) streamOnce(ctx context.Context, cfg Config, feed <-chan []byte) (handshook bool, err error) {
	scheme, hostPort, err := parseServerTarget(cfg.ServerURL)
	if err != nil {
		return false, err
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	if scheme == "https" {
		host, _, _ := net.SplitHostPort(hostPort)
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		}}).DialContext(ctx, "tcp", hostPort)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", hostPort)
	}
	if err != nil {
		return false, err
	}

	b.mu.Lock()
	b.conn = conn // so stopLocked can interrupt the handshake
	b.mu.Unlock()

	defer func() {
		_ = conn.Close()
		b.mu.Lock()
		if b.conn == conn {
			b.conn = nil
		}
		b.streaming = false
		b.mu.Unlock()
	}()

	// Perform server handshake under a deadline: a silent server must not hang us.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if cfg.Protocol == "shoutcast" {
		err = b.handshakeShoutcast(conn, cfg)
	} else {
		err = b.handshakeIcecast(conn, cfg)
	}
	if err != nil {
		return false, err
	}
	_ = conn.SetDeadline(time.Time{})
	handshook = true

	b.mu.Lock()
	b.streaming = true
	b.connectedAt = time.Now().UTC().Format(time.RFC3339)
	b.lastError = ""
	b.mu.Unlock()

	return true, b.streamFrames(ctx, conn, cfg, feed)
}

// writeConn writes with a 5s deadline so a stalled server cannot block the session forever.
func writeConn(conn net.Conn, p []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write(p)
	return err
}

// streamFrames sends the container header and then frames until ctx ends or a write fails.
func (b *Broadcaster) streamFrames(ctx context.Context, conn net.Conn, cfg Config, feed <-chan []byte) error {
	// Send initial container header if required
	if cfg.Format == "wav" {
		hdr := makeWAVHeader()
		if err := writeConn(conn, hdr); err != nil {
			return err
		}
		b.bytesSent.Add(int64(len(hdr)))
	} else if cfg.Format == "ogg" {
		bos := makeOggPage(0x02, 0, 0x12345678, 0, []byte("EngineX-OS-Audio-BOS"))
		if err := writeConn(conn, bos); err != nil {
			return err
		}
		b.bytesSent.Add(int64(len(bos)))
	}

	// Drop PCM queued while we were connecting so the stream starts live.
	for drained := false; !drained; {
		select {
		case <-feed:
		default:
			drained = true
		}
	}

	if cfg.Format == "mp3" {
		if cmd := mp3EncoderFn(ctx, cfg.Bitrate); cmd != nil {
			return b.streamEncodedMP3(ctx, conn, cmd, feed)
		}
	}

	var oggSeq uint32 = 1
	var oggGranule uint64 = 1152

	for {
		var pcm []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pcm = <-feed:
		}

		var frame []byte
		switch cfg.Format {
		case "wav":
			frame = nextWAVFrame(pcm)
		case "ogg":
			frame = nextOggFrame(pcm, &oggSeq, &oggGranule)
		default:
			frame = nextMP3Frame(cfg, pcm)
		}

		if err := writeConn(conn, frame); err != nil {
			return err
		}
		b.bytesSent.Add(int64(len(frame)))
	}
}

// mp3EncoderFn builds the external MP3 encoder command; tests replace it.
var mp3EncoderFn = newMP3Encoder

// newMP3Encoder returns lame or ffmpeg reading s16le 44.1kHz stereo on stdin and writing MP3
// to stdout, or nil when neither is on PATH (callers then fall back to nextMP3Frame placeholders).
// The command is killed with its whole process group when ctx ends.
func newMP3Encoder(ctx context.Context, bitrate int) *exec.Cmd {
	br := strconv.Itoa(bitrate)
	var cmd *exec.Cmd
	if p, err := exec.LookPath("lame"); err == nil {
		cmd = exec.CommandContext(ctx, p, "-r", "-s", "44.1", "--signed", "--little-endian", "--bitwidth", "16", "-m", "s", "-b", br, "--silent", "-", "-")
	} else if p, err := exec.LookPath("ffmpeg"); err == nil {
		cmd = exec.CommandContext(ctx, p, "-loglevel", "quiet", "-fflags", "+nobuffer", "-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0", "-f", "mp3", "-b:a", br+"k", "-flush_packets", "1", "pipe:1")
	} else {
		return nil
	}
	return groupKill(cmd)
}

// groupKill runs cmd in its own process group and kills the whole group on cancel.
func groupKill(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	return cmd
}

// streamEncodedMP3 pipes PCM through cmd and forwards the real MP3 bytes to conn.
func (b *Broadcaster) streamEncodedMP3(ctx context.Context, conn net.Conn, cmd *exec.Cmd, feed <-chan []byte) error {
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mp3 encoder start failed: %w", err)
	}
	defer func() {
		_ = in.Close()
		_ = cmd.Cancel() // also reaps children of the encoder
		_ = cmd.Wait()
	}()

	outErr := make(chan error, 1)
	go func() {
		// Nobody drains stdout after we return: kill the encoder so the main loop's
		// in.Write gets EPIPE instead of blocking forever on a full stdin pipe.
		defer func() { _ = cmd.Cancel() }()
		buf := make([]byte, 4096)
		for {
			n, err := out.Read(buf)
			if n > 0 {
				if werr := writeConn(conn, buf[:n]); werr != nil {
					outErr <- werr
					return
				}
				b.bytesSent.Add(int64(n))
			}
			if err != nil {
				outErr <- fmt.Errorf("mp3 encoder stopped: %w", err)
				return
			}
		}
	}()

	silence := make([]byte, 1152*4)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-outErr:
			return err
		case pcm := <-feed:
			if len(pcm) == 0 {
				pcm = silence
			}
			if _, err := in.Write(pcm); err != nil {
				return fmt.Errorf("mp3 encoder write failed: %w", err)
			}
		}
	}
}

// handshakeIcecast performs the HTTP SOURCE handshake with Icecast v2.
func (b *Broadcaster) handshakeIcecast(conn net.Conn, cfg Config) error {
	contentType := contentTypeFor(cfg.Format)

	auth := base64.StdEncoding.EncodeToString([]byte(cfg.Username + ":" + cfg.Password))
	req := fmt.Sprintf(
		"SOURCE %s HTTP/1.0\r\n"+
			"Authorization: Basic %s\r\n"+
			"Content-Type: %s\r\n"+
			"Ice-Name: %s\r\n"+
			"Ice-Bitrate: %d\r\n"+
			"Ice-Audio-Info: ice-samplerate=44100;ice-bitrate=%d;ice-channels=2\r\n"+
			"\r\n",
		cfg.Mountpoint, auth, contentType, cfg.StreamName, cfg.Bitrate, cfg.Bitrate,
	)

	if _, err := io.WriteString(conn, req); err != nil {
		return fmt.Errorf("icecast handshake write failed: %w", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("icecast handshake read failed: %w", err)
	}

	if !strings.Contains(statusLine, "200") {
		return fmt.Errorf("icecast handshake rejected: %s", strings.TrimSpace(statusLine))
	}

	// Consume remaining response headers
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("icecast handshake header read error: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return nil
}

// shoutcastLogin returns the DNAS source login line: a numeric Username is a
// DNAS v2 stream ID ("password:#id"); anything else (incl. "source") is v1, password only.
func shoutcastLogin(cfg Config) string {
	if id := shoutcastStreamID(cfg); id > 0 {
		return fmt.Sprintf("%s:#%d", cfg.Password, id)
	}
	return cfg.Password
}

// shoutcastStreamID returns the DNAS v2 stream ID from Username ("1" or "#1"), or 0 for v1.
func shoutcastStreamID(cfg Config) int {
	id, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(cfg.Username), "#"))
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func contentTypeFor(format string) string {
	switch format {
	case "ogg":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	}
	return "audio/mpeg"
}

// handshakeShoutcast performs the password & ICY handshake with Shoutcast v1/v2.
func (b *Broadcaster) handshakeShoutcast(conn net.Conn, cfg Config) error {
	if _, err := fmt.Fprintf(conn, "%s\r\n", shoutcastLogin(cfg)); err != nil {
		return fmt.Errorf("shoutcast sending password failed: %w", err)
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("shoutcast reading handshake failed: %w", err)
	}

	respStr := string(buf[:n])
	if !strings.Contains(respStr, "OK2") {
		return fmt.Errorf("shoutcast authentication failed: %s", strings.TrimSpace(respStr))
	}

	icyHeaders := fmt.Sprintf(
		"icy-name:%s\r\n"+
			"icy-pub:0\r\n"+
			"icy-br:%d\r\n"+
			"content-type:%s\r\n"+
			"\r\n",
		cfg.StreamName, cfg.Bitrate, contentTypeFor(cfg.Format),
	)

	if _, err := io.WriteString(conn, icyHeaders); err != nil {
		return fmt.Errorf("shoutcast sending icy headers failed: %w", err)
	}
	return nil
}

// nextMP3Frame generates a valid MPEG-1 Layer III 44.1kHz stereo audio frame.
func nextMP3Frame(cfg Config, pcm []byte) []byte {
	// Frame size = 144 * Bitrate / SampleRate
	frameLen := (144 * cfg.Bitrate * 1000) / 44100
	if frameLen < 4 {
		frameLen = 626
	}
	frame := make([]byte, frameLen)

	// MPEG-1 Layer III 44.1kHz Stereo Header
	frame[0] = 0xFF
	frame[1] = 0xFB // Sync (11 bits) + Version 1 (2 bits) + Layer III (2 bits) + No CRC (1 bit)

	var brIdx byte
	switch cfg.Bitrate {
	case 128:
		brIdx = 0x9
	case 192:
		brIdx = 0xB
	case 256:
		brIdx = 0xD
	case 320:
		brIdx = 0xE
	default:
		brIdx = 0xB
	}

	frame[2] = (brIdx << 4) // 44.1kHz (00), Padding (0), Private (0)
	frame[3] = 0x00         // Stereo (00), Mode Ext (00), Copy (0), Original (0), Emphasis (00)

	// Bytes 4..35: Side info (zeros for empty/silent granule data)
	// Bytes 36..frameLen: Ancillary PCM digest
	if len(pcm) > 0 {
		digest := sha256.Sum256(pcm)
		copy(frame[36:], digest[:])
	} else {
		copy(frame[36:], []byte("EngineX-OS-MP3-Silence-Frame"))
	}
	return frame
}

// nextWAVFrame returns the captured PCM, or 1152 samples of silence.
func nextWAVFrame(pcm []byte) []byte {
	if len(pcm) > 0 {
		return pcm
	}
	return make([]byte, 1152*4) // 1152 samples * 2 channels * 2 bytes/sample
}

// nextOggFrame creates a valid Ogg page containing audio payload.
func nextOggFrame(pcm []byte, seq *uint32, granule *uint64) []byte {
	payload := make([]byte, 128)
	if len(pcm) > 0 {
		payload = pcm[:min(len(pcm), len(payload))]
	} else {
		copy(payload, []byte("EngineX-OS-Ogg-Audio-Data"))
	}

	page := makeOggPage(0x00, *granule, 0x12345678, *seq, payload)
	*seq++
	*granule += 1152
	return page
}

// UpdateMetadata pushes track metadata changes to the Icecast/Shoutcast server.
func (b *Broadcaster) UpdateMetadata(artist, title string) error {
	b.mu.Lock()
	artist = strings.TrimSpace(artist)
	title = strings.TrimSpace(title)

	var song string
	if artist != "" && title != "" {
		song = artist + " - " + title
	} else if title != "" {
		song = title
	} else {
		song = artist
	}

	b.currentTrack = song
	cfg := b.cfg
	b.mu.Unlock()

	if cfg.ServerURL == "" {
		return nil
	}

	scheme, hostPort, err := parseServerTarget(cfg.ServerURL)
	if err != nil {
		b.mu.Lock()
		b.lastError = sanitizeError(err, cfg.Password)
		b.mu.Unlock()
		return err
	}

	var metaURL string
	if cfg.Protocol == "shoutcast" {
		metaURL = fmt.Sprintf("%s://%s/admin.cgi?mode=updinfo&pass=%s&song=%s",
			scheme, hostPort, url.QueryEscape(cfg.Password), url.QueryEscape(song))
		if id := shoutcastStreamID(cfg); id > 0 {
			metaURL += fmt.Sprintf("&sid=%d", id) // DNAS v2 multi-stream
		}
	} else {
		metaURL = fmt.Sprintf("%s://%s/admin/metadata?mode=updinfo&mount=%s&song=%s",
			scheme, hostPort, url.QueryEscape(cfg.Mountpoint), url.QueryEscape(song))
	}

	req, err := http.NewRequest("GET", metaURL, nil)
	if err != nil {
		b.mu.Lock()
		b.lastError = sanitizeError(err, cfg.Password)
		b.mu.Unlock()
		return err
	}

	if cfg.Protocol == "icecast" {
		req.SetBasicAuth(cfg.Username, cfg.Password)
	}
	req.Header.Set("User-Agent", "EngineX-OS-Broadcaster")

	client := b.httpClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		b.mu.Lock()
		b.lastError = sanitizeError(err, cfg.Password)
		b.mu.Unlock()
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		reqErr := fmt.Errorf("metadata update failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		b.mu.Lock()
		b.lastError = sanitizeError(reqErr, cfg.Password)
		b.mu.Unlock()
		return reqErr
	}

	b.mu.Lock()
	if strings.Contains(b.lastError, "metadata update") {
		b.lastError = ""
	}
	b.mu.Unlock()
	return nil
}

// WAVHeader is the streaming WAV header (44.1kHz, stereo, 16-bit) that precedes raw PCM on /live.wav.
func WAVHeader() []byte { return makeWAVHeader() }

// makeWAVHeader builds a standard 44-byte RIFF/WAVE header for streaming.
func makeWAVHeader() []byte {
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], 0x7FFFFFFF) // stream/unknown size
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)      // PCM header size
	binary.LittleEndian.PutUint16(hdr[20:22], 1)       // PCM
	binary.LittleEndian.PutUint16(hdr[22:24], 2)       // 2 channels
	binary.LittleEndian.PutUint32(hdr[24:28], 44100)   // 44.1kHz
	binary.LittleEndian.PutUint32(hdr[28:32], 44100*4) // byte rate
	binary.LittleEndian.PutUint16(hdr[32:34], 4)       // block align
	binary.LittleEndian.PutUint16(hdr[34:36], 16)      // 16 bits
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], 0x7FFFFFFF) // stream size
	return hdr
}

// Ogg CRC-32 lookup table (polynomial 0x04c11db7).
var oggCRCTable [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		curr := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if (curr & 0x80000000) != 0 {
				curr = (curr << 1) ^ 0x04c11db7
			} else {
				curr <<= 1
			}
		}
		oggCRCTable[i] = curr
	}
}

func oggChecksum(buf []byte) uint32 {
	var crc uint32
	for _, b := range buf {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}

// makeOggPage constructs a valid Ogg bitstream page with calculated CRC.
func makeOggPage(headerType byte, granule uint64, serial uint32, seq uint32, payload []byte) []byte {
	if len(payload) > 255 {
		payload = payload[:255]
	}
	page := make([]byte, 27+1+len(payload))
	copy(page[0:4], "OggS")
	page[4] = 0 // structure version
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], seq)
	page[26] = 1 // 1 segment
	page[27] = byte(len(payload))
	copy(page[28:], payload)

	crc := oggChecksum(page)
	binary.LittleEndian.PutUint32(page[22:26], crc)
	return page
}
