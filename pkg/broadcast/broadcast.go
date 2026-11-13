package broadcast

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

// Default settings
const (
	DefaultProtocol   = "icecast"
	DefaultMountpoint = "/live"
	DefaultUsername   = "source"
	DefaultFormat     = "wav"
	DefaultStreamName = "EngineX OS Live Mix"
	DefaultInputPath  = "/tmp/enginex-broadcast.pcm"
)

// SampleRate is the rate of the PCM feed: the Engine hook (#50) resamples the
// master bus to 48 kHz s16le stereo, matching X-Audio-Format and the LAN path.
const SampleRate = 48000

// wavKbps is the bitrate of the WAV stream (48 kHz x 16 bit x 2 channels), the
// only output: no deck image ships an MP3 or Ogg encoder.
const wavKbps = SampleRate * 16 * 2 / 1000

// shoutcastUnsupported is why a saved Shoutcast target is not pushed to: SHOUTcast
// servers take MP3 or AAC, never WAV. The local /live.wav stream is not affected.
const shoutcastUnsupported = "Shoutcast is not supported: its servers take only MP3/AAC, not WAV. Save an Icecast server as the target; /live.wav still works"

// Config specifies live stream parameters.
type Config struct {
	Enabled    bool   `json:"enabled"`
	Protocol   string `json:"protocol"`            // "icecast" (default); "shoutcast" is kept for old settings only (its servers take MP3/AAC, not WAV)
	ServerURL  string `json:"serverUrl"`           // e.g. "http://192.168.1.10:8000" or "192.168.1.10:8000"
	Mountpoint string `json:"mountpoint"`          // e.g. "/live.mp3" (default "/live")
	Username   string `json:"username"`            // default "source"
	Password   string `json:"password,omitempty"`  // omitted in JSON output
	Format     string `json:"format"`              // always "wav": old settings that say mp3 or ogg load as wav
	StreamName string `json:"streamName"`          // default "EngineX OS Live Mix"
	InputPath  string `json:"inputPath,omitempty"` // optional PCM file/FIFO path (default "/tmp/enginex-broadcast.pcm")
	LANMode    string `json:"lanMode,omitempty"`   // "off" (default), "rtp", "teleport"
	LANTarget  string `json:"lanTarget,omitempty"` // e.g. "239.255.0.1:5004" or "192.168.1.50:9998"
}

// pushes reports whether the WAV stream is sent to an external server. A Shoutcast
// target is kept from old settings but not pushed to: its servers take MP3/AAC, not WAV.
func (c Config) pushes() bool { return c.ServerURL != "" && c.Protocol != "shoutcast" }

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
	StreamName   string `json:"streamName"`
	CurrentTrack string `json:"currentTrack"`
	BytesSent    int64  `json:"bytesSent"`
	Listeners    int    `json:"listeners"` // clients on the local /live.wav fan-out
	ConnectedAt  string `json:"connectedAt,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	LANMode      string `json:"lanMode,omitempty"`
	LANTarget    string `json:"lanTarget,omitempty"`
	// DriftPPM is the measured rate offset of the Engine feed against the system
	// clock, in parts per million (positive: the audio clock runs fast). The live
	// stream is resampled by it; absent until it has been measured.
	DriftPPM float64 `json:"driftPpm,omitempty"`
}

// Broadcaster manages background audio streaming to Icecast on CPU3.
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
	driftPPB  atomic.Int64 // measured feed rate offset, parts per billion; 0 until measured
	drift     float64      // last measured offset, kept across restarts of the capture loop
	driftSet  bool

	hub hub

	lanStreamer *LANStreamer
	lanErr      string // why there is no lanStreamer although LAN streaming is on

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
			StreamName: DefaultStreamName,
			InputPath:  DefaultInputPath,
			LANMode:    "off",
		},
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// oneLine drops control characters. Mount point, login and stream name are
// written as they are onto lines of the Icecast handshake, so each has
// to stay on its line: a pasted value that brought a line break along cut the
// handshake in two (BH2-A3-7).
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// normalizeConfig validates and applies defaults to a Config.
func normalizeConfig(cfg *Config) error {
	cfg.Mountpoint, cfg.Username, cfg.Password, cfg.StreamName = oneLine(cfg.Mountpoint), oneLine(cfg.Username), oneLine(cfg.Password), oneLine(cfg.StreamName)
	cfg.Protocol = strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if cfg.Protocol == "" {
		cfg.Protocol = DefaultProtocol
	}
	if cfg.Protocol != "icecast" && cfg.Protocol != "shoutcast" {
		return fmt.Errorf("invalid protocol: %q (must be 'icecast' or 'shoutcast')", cfg.Protocol)
	}

	cfg.Format = DefaultFormat // saved settings and older clients may still say mp3 or ogg

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

	// The LAN stream counts as streaming while it has a socket, and says why when it
	// has none. Capture alone (the deck's own /live stream) counts only when nothing
	// is meant to be sent anywhere: with LAN streaming on it said "streaming" while
	// the target did not resolve or the network was not up yet, and the console
	// hid the reason behind "connected".
	lanOn := b.cfg.LANMode != "" && b.cfg.LANMode != "off"
	lanUp, lastError := false, b.lastError
	if b.cfg.ServerURL != "" && b.cfg.Protocol == "shoutcast" {
		if lastError != "" {
			lastError += "; "
		}
		lastError += shoutcastUnsupported
	}
	lanErr, sent := b.lanErr, b.bytesSent.Load()
	if b.lanStreamer != nil {
		lanUp, lanErr = b.lanStreamer.Connected(), b.lanStreamer.Err()
		sent += b.lanStreamer.BytesSent() // BH2-A3-10: only the external server's bytes were counted
	}
	if lanErr != "" {
		if lastError != "" {
			lastError += "; "
		}
		lastError += "LAN stream: " + lanErr
	}
	return Status{
		Enabled:      b.cfg.Enabled,
		Streaming:    b.streaming || lanUp || (b.capturing && b.cfg.ServerURL == "" && !lanOn),
		Protocol:     b.cfg.Protocol,
		ServerURL:    b.cfg.ServerURL,
		Mountpoint:   b.cfg.Mountpoint,
		Username:     b.cfg.Username,
		PasswordSet:  b.cfg.Password != "",
		Format:       b.cfg.Format,
		StreamName:   b.cfg.StreamName,
		CurrentTrack: b.currentTrack,
		BytesSent:    sent,
		Listeners:    b.hub.count(),
		ConnectedAt:  b.connectedAt,
		LastError:    lastError,
		LANMode:      b.cfg.LANMode,
		LANTarget:    b.cfg.LANTarget,
		DriftPPM:     float64(b.driftPPB.Load()) / 1000,
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
	b.hub.reopen()
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.wg.Add(1)

	if b.cfg.LANMode != "" && b.cfg.LANMode != "off" && b.cfg.LANTarget == "" {
		b.lanErr = "no target set" // on, and nowhere to send: say so instead of "connecting"
	} else if b.cfg.LANMode != "" && b.cfg.LANMode != "off" {
		streamer, err := NewLANStreamer(LANConfig{
			Mode:       b.cfg.LANMode,
			Target:     b.cfg.LANTarget,
			SampleRate: 48000,
			Channels:   2,
		})
		// BH2-A3-6: both errors were dropped, and Status said "streaming" with no
		// error while nothing was sent. A target that does not resolve is reported;
		// a socket that cannot be opened yet is reported and retried by Push.
		// b.streaming is the external server only; the LAN socket is asked in Status.
		if err != nil {
			b.lanErr = err.Error()
		} else {
			b.lanStreamer = streamer
			_ = streamer.Start() // the reason is kept in streamer.Err()
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
	b.lanErr = ""
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
		// No port. An IPv6 literal comes in brackets, which JoinHostPort adds itself
		// (BH2-A3-12: "[2001:db8::1]" became "[[2001:db8::1]]:8000" and never connected).
		host = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
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

// captureTick is the keep-alive cadence and the longest wait for PCM: 1152 frames
// (one MPEG frame) at 48 kHz = 24 ms.
const captureTick = 1152 * time.Second / SampleRate

// workerLoop reads the PCM feed as it arrives, puts it on the system clock (see
// pacer), fans it out to local listeners and, when a ServerURL is set, feeds the
// external Icecast pusher. It is paced by the feed itself, so the guard's
// cooperative throttle is not used here: its sleeps would only add jitter.
func (b *Broadcaster) workerLoop(ctx context.Context, _ *resourceguard.Throttle) {
	b.mu.Lock()
	cfg := b.cfg
	b.capturing = true
	seed, seeded := b.drift, b.driftSet
	b.mu.Unlock()
	start := time.Now()
	p := pacer{base: start, tr: newDriftTracker(seed, seeded)}
	defer func() {
		b.mu.Lock()
		b.capturing = false
		if p.tr.locked {
			b.drift, b.driftSet = p.tr.offset, true
		}
		b.mu.Unlock()
	}()

	feed := make(chan []byte, 80) // about 0.8 s of ~10 ms chunks, as 32 was with 24 ms ones
	if cfg.pushes() {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.pushLoop(ctx, cfg, feed)
		}()
	}

	var input pcmReader
	defer func() {
		if input.f != nil {
			input.f.Close()
		}
	}()
	for ctx.Err() == nil {
		pass := time.Now()
		if input.f == nil { // the hook may create the PCM source after the stream is enabled
			if f, err := os.OpenFile(cfg.InputPath, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				input = pcmReader{f: f}
				if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeNamedPipe != 0 {
					input.fifo = true // the live feed: wait for its writes, correct its drift
					p.correct = true
					growPipe(f)
				}
			}
		}
		wait := idleReadWait
		if input.fifo {
			wait = captureTick
		}
		pcm := input.readFor(wait)
		if out := p.step(pcm, time.Since(start)); out != nil {
			b.hub.publish(out)

			b.mu.Lock()
			ls := b.lanStreamer
			b.mu.Unlock()
			if ls != nil {
				_ = ls.Push(out)
			}

			select {
			case feed <- out:
			default: // external server is slow or down: drop rather than stall the capture
			}
		}
		if p.tr.locked {
			b.driftPPB.Store(int64(math.Round(p.tr.offset * 1e9)))
		}
		if pcm != nil && input.fifo {
			continue // the next write of the feed paces the loop
		}
		// Nothing to wait on (no source or no writer yet, or a regular file): one pass per tick.
		if rest := captureTick - time.Since(pass); rest > 0 {
			timer := time.NewTimer(rest)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

// silenceAfter is how long the PCM feed may pause before keep-alive silence is sent.
// The hook writes about every 10 ms; a single empty 24 ms tick is jitter, and
// splicing zeros into playing audio clicks and makes the stream drift ahead.
const silenceAfter = 100 * time.Millisecond

// silenceGate decides what one capture tick publishes.
type silenceGate struct{ last time.Time }

// next returns the bytes to publish and whether to publish at all: live PCM as is,
// nothing during a short gap, and one frame of silence once the feed has been quiet
// for longer than silenceAfter (so local players stay connected while nothing plays).
func (g *silenceGate) next(pcm []byte, now time.Time) ([]byte, bool) {
	if pcm != nil {
		g.last = now
		return pcm, true
	}
	if !g.last.IsZero() && now.Sub(g.last) <= silenceAfter {
		return nil, false
	}
	return make([]byte, 1152*4), true
}

// pcmReader reads the s16le stereo PCM feed and only ever returns whole 4-byte
// frames: a read that ends inside a frame keeps the 1-3 extra bytes for the next
// read, so a channel or byte shift can never reach the listeners. The remainder
// is dropped when the writer goes away (EOF), since a new writer starts on a frame.
type pcmReader struct {
	f     *os.File
	fifo  bool // a FIFO: reads can wait for the writer
	carry [3]byte
	n     int
}

// idleReadWait is how long read waits on an open but idle FIFO writer.
const idleReadWait = 5 * time.Millisecond

// read returns what is available (up to two MPEG frames' worth, so a late tick
// catches up), looping regular files; nil when no whole frame is available. A FIFO
// whose writer is connected but idle must not park the capture loop: the read
// gives up after a few milliseconds.
func (p *pcmReader) read() []byte { return p.readFor(idleReadWait) }

// readFor is read with a chosen wait: a FIFO read returns as soon as the writer has
// written something, or with nothing after wait.
func (p *pcmReader) readFor(wait time.Duration) []byte {
	if p.f == nil {
		return nil
	}
	buf := make([]byte, p.n+2*1152*4)
	copy(buf, p.carry[:p.n])
	_ = p.f.SetReadDeadline(time.Now().Add(wait)) // no-op error on regular files
	m, err := p.f.Read(buf[p.n:])
	if err == io.EOF {
		p.n = 0
		_, _ = p.f.Seek(0, 0)
		m, _ = p.f.Read(buf)
	}
	total := p.n + max(m, 0)
	whole := total &^ 3
	p.n = copy(p.carry[:], buf[whole:total])
	if whole == 0 {
		return nil
	}
	return buf[:whole]
}

// pushLoop keeps the external Icecast connection alive, reconnecting with
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
	if err = b.handshakeIcecast(conn, cfg); err != nil {
		return false, err
	}
	_ = conn.SetDeadline(time.Time{})
	handshook = true

	b.mu.Lock()
	b.streaming = true
	b.connectedAt = time.Now().UTC().Format(time.RFC3339)
	b.lastError = ""
	b.mu.Unlock()

	return true, b.streamFrames(ctx, conn, feed)
}

// writeConn writes with a 5s deadline so a stalled server cannot block the session forever.
func writeConn(conn net.Conn, p []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Write(p)
	return err
}

// streamFrames sends the WAV stream until ctx ends or a write fails.
func (b *Broadcaster) streamFrames(ctx context.Context, conn net.Conn, feed <-chan []byte) error {
	hdr := makeWAVHeader()
	if err := writeConn(conn, hdr); err != nil {
		return err
	}
	b.bytesSent.Add(int64(len(hdr)))

	// Drop PCM queued while we were connecting so the stream starts live.
	for drained := false; !drained; {
		select {
		case <-feed:
		default:
			drained = true
		}
	}

	for {
		var pcm []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pcm = <-feed:
		}
		frame := nextWAVFrame(pcm)
		if err := writeConn(conn, frame); err != nil {
			return err
		}
		b.bytesSent.Add(int64(len(frame)))
	}
}

// handshakeIcecast performs the HTTP SOURCE handshake with Icecast v2.
func (b *Broadcaster) handshakeIcecast(conn net.Conn, cfg Config) error {
	auth := base64.StdEncoding.EncodeToString([]byte(cfg.Username + ":" + cfg.Password))
	req := fmt.Sprintf(
		"SOURCE %s HTTP/1.0\r\n"+
			"Authorization: Basic %s\r\n"+
			"Content-Type: audio/wav\r\n"+
			"Ice-Name: %s\r\n"+
			"Ice-Bitrate: %d\r\n"+
			"Ice-Audio-Info: ice-samplerate=48000;ice-bitrate=%d;ice-channels=2\r\n"+
			"\r\n",
		cfg.Mountpoint, auth, cfg.StreamName, wavKbps, wavKbps,
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

// nextWAVFrame returns the captured PCM, or 1152 samples of silence.
func nextWAVFrame(pcm []byte) []byte {
	if len(pcm) > 0 {
		return pcm
	}
	return make([]byte, 1152*4) // 1152 samples * 2 channels * 2 bytes/sample
}

// UpdateMetadata pushes track metadata changes to the Icecast server.
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

	if !cfg.pushes() {
		return nil
	}

	scheme, hostPort, err := parseServerTarget(cfg.ServerURL)
	if err != nil {
		b.mu.Lock()
		b.lastError = sanitizeError(err, cfg.Password)
		b.mu.Unlock()
		return err
	}

	metaURL := fmt.Sprintf("%s://%s/admin/metadata?mode=updinfo&mount=%s&song=%s",
		scheme, hostPort, url.QueryEscape(cfg.Mountpoint), url.QueryEscape(song))
	req, err := http.NewRequest("GET", metaURL, nil)
	if err != nil {
		b.mu.Lock()
		b.lastError = sanitizeError(err, cfg.Password)
		b.mu.Unlock()
		return err
	}

	req.SetBasicAuth(cfg.Username, cfg.Password)
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

// WAVHeader is the streaming WAV header (48 kHz, stereo, 16-bit) that precedes raw PCM on /live.wav.
func WAVHeader() []byte { return makeWAVHeader() }

// makeWAVHeader builds a standard 44-byte RIFF/WAVE header for streaming.
func makeWAVHeader() []byte {
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], 0x7FFFFFFF) // stream/unknown size
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)           // PCM header size
	binary.LittleEndian.PutUint16(hdr[20:22], 1)            // PCM
	binary.LittleEndian.PutUint16(hdr[22:24], 2)            // 2 channels
	binary.LittleEndian.PutUint32(hdr[24:28], SampleRate)   // 48 kHz
	binary.LittleEndian.PutUint32(hdr[28:32], SampleRate*4) // byte rate
	binary.LittleEndian.PutUint16(hdr[32:34], 4)            // block align
	binary.LittleEndian.PutUint16(hdr[34:36], 16)           // 16 bits
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], 0x7FFFFFFF) // stream size
	return hdr
}
