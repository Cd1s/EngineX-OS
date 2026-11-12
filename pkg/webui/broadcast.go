package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/broadcast"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

var (
	broadcasters              sync.Map // map[*Server]*broadcast.Broadcaster
	broadcasterMu             sync.Mutex
	broadcastRoutesRegistered sync.Map // map[*Server]bool
	usbAudioManagers          sync.Map // map[*Server]*broadcast.USBAudioManager
	usbAudioMu                sync.Mutex
)

// usbAudioManager returns the lazily initialized USBAudioManager for this server.
func (s *Server) usbAudioManager() *broadcast.USBAudioManager {
	if v, ok := usbAudioManagers.Load(s); ok {
		return v.(*broadcast.USBAudioManager)
	}

	usbAudioMu.Lock()
	defer usbAudioMu.Unlock()
	if v, ok := usbAudioManagers.Load(s); ok {
		return v.(*broadcast.USBAudioManager)
	}

	configPath := filepath.Join(s.cfg.StateDir, "usb-audio.json")
	mgr := broadcast.NewUSBAudioManager(configPath, "", "")
	usbAudioManagers.Store(s, mgr)
	return mgr
}

// broadcaster returns the lazily initialized Broadcaster for this server.
func (s *Server) broadcaster() *broadcast.Broadcaster {
	if v, ok := broadcasters.Load(s); ok {
		return v.(*broadcast.Broadcaster)
	}

	broadcasterMu.Lock()
	defer broadcasterMu.Unlock()
	if v, ok := broadcasters.Load(s); ok {
		return v.(*broadcast.Broadcaster)
	}

	var guard *resourceguard.Guard
	if s.jobs != nil && s.jobs.p.Guard != nil {
		guard = s.jobs.p.Guard
	}
	b := broadcast.New(guard)

	// Always load the saved settings so a later SetEnabled(true) keeps ServerURL and password.
	_ = b.Configure(s.loadBroadcastConfig())

	broadcasters.Store(s, b)
	return b
}

func (s *Server) broadcastConfigPath() string {
	return filepath.Join(s.cfg.StateDir, "broadcast-config.json")
}

func (s *Server) loadBroadcastConfig() broadcast.Config {
	var cfg broadcast.Config
	if b, err := os.ReadFile(s.broadcastConfigPath()); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	return cfg
}

func broadcastHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(raw)
	}
	return strings.ToLower(u.Host)
}

// broadcastGet handles GET /api/broadcast.
func (s *Server) broadcastGet(w http.ResponseWriter, r *http.Request) {
	b := s.broadcaster()
	respond(w, b.Status())
}

type broadcastPostInput struct {
	Enabled    *bool   `json:"enabled"`
	Protocol   *string `json:"protocol"`
	ServerURL  *string `json:"serverUrl"`
	Mountpoint *string `json:"mountpoint"`
	Username   *string `json:"username"`
	Password   *string `json:"password"`
	Format     *string `json:"format"`
	Bitrate    *int    `json:"bitrate"`
	StreamName *string `json:"streamName"`
	InputPath  *string `json:"inputPath"`
	LANMode    *string `json:"lanMode"`
	LANTarget  *string `json:"lanTarget"`
}

// broadcastPost handles POST /api/broadcast.
func (s *Server) broadcastPost(w http.ResponseWriter, r *http.Request) {
	b := s.broadcaster()
	s.uiMu.Lock()
	defer s.uiMu.Unlock()

	cfg := s.loadBroadcastConfig()

	// 1. Handle query parameter ?enabled=1|0 or true|false, lanMode, lanTarget
	if q := r.URL.Query().Get("enabled"); q != "" {
		if q == "1" || strings.EqualFold(q, "true") {
			cfg.Enabled = true
		} else if q == "0" || strings.EqualFold(q, "false") {
			cfg.Enabled = false
		}
	}
	if q := r.URL.Query().Get("lanMode"); q != "" {
		cfg.LANMode = q
	}
	if q := r.URL.Query().Get("lanTarget"); q != "" {
		cfg.LANTarget = q
	}

	// 2. Handle optional JSON body
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
		if err != nil {
			fail(w, 400, errors.New("request body too large"))
			return
		}
		if len(bytes.TrimSpace(bodyBytes)) > 0 {
			var in broadcastPostInput
			d := json.NewDecoder(bytes.NewReader(bodyBytes))
			d.DisallowUnknownFields()
			if err := d.Decode(&in); err != nil {
				fail(w, 400, errors.New("invalid JSON request"))
				return
			}

			if in.Enabled != nil {
				cfg.Enabled = *in.Enabled
			}
			if in.ServerURL != nil {
				newHost := broadcastHost(*in.ServerURL)
				oldHost := broadcastHost(cfg.ServerURL)
				if newHost != oldHost && in.Password == nil {
					cfg.Password = ""
				}
				cfg.ServerURL = *in.ServerURL
			}
			if in.Password != nil {
				cfg.Password = *in.Password
			}
			if in.Protocol != nil {
				cfg.Protocol = *in.Protocol
			}
			if in.Mountpoint != nil {
				cfg.Mountpoint = *in.Mountpoint
			}
			if in.Username != nil {
				cfg.Username = *in.Username
			}
			if in.Format != nil {
				cfg.Format = *in.Format
			}
			if in.Bitrate != nil {
				cfg.Bitrate = *in.Bitrate
			}
			if in.StreamName != nil {
				cfg.StreamName = *in.StreamName
			}
			if in.InputPath != nil {
				cfg.InputPath = *in.InputPath
			}
			if in.LANMode != nil {
				cfg.LANMode = *in.LANMode
			}
			if in.LANTarget != nil {
				cfg.LANTarget = *in.LANTarget
			}
		}
	}

	// Validate first: a rejected config must not be persisted and reloaded at next boot.
	if err := b.Configure(cfg); err != nil {
		fail(w, 400, err)
		return
	}
	if err := providers.SaveJSON(s.broadcastConfigPath(), cfg); err != nil {
		fail(w, 500, errors.New("could not save broadcast settings"))
		return
	}
	respond(w, b.Status())
}

// broadcastMetadataPost handles POST /api/broadcast/metadata.
func (s *Server) broadcastMetadataPost(w http.ResponseWriter, r *http.Request) {
	b := s.broadcaster()

	artist := r.URL.Query().Get("artist")
	title := r.URL.Query().Get("title")

	if r.Body != nil {
		bodyBytes, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
		if len(bytes.TrimSpace(bodyBytes)) > 0 {
			var in struct {
				Artist string `json:"artist"`
				Title  string `json:"title"`
				Deck   int    `json:"deck"`
			}
			if err := json.Unmarshal(bodyBytes, &in); err == nil {
				if in.Artist != "" && artist == "" {
					artist = in.Artist
				}
				if in.Title != "" && title == "" {
					title = in.Title
				}
			}
		}
	}

	_ = b.UpdateMetadata(artist, title)
	respond(w, b.Status())
}

// BroadcastEnabled reports whether broadcast streaming is enabled.
func (s *Server) BroadcastEnabled() bool {
	return s.broadcaster().Status().Enabled
}

// SetBroadcastEnabled toggles broadcast streaming and persists the preference.
func (s *Server) SetBroadcastEnabled(on bool) error {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()

	cfg := s.loadBroadcastConfig()
	cfg.Enabled = on
	if err := providers.SaveJSON(s.broadcastConfigPath(), cfg); err != nil {
		return err
	}
	return s.broadcaster().SetEnabled(on)
}

// RegisterBroadcastRoutes registers broadcast routes on the server's mux.
func (s *Server) RegisterBroadcastRoutes() {
	if _, loaded := broadcastRoutesRegistered.LoadOrStore(s, true); loaded {
		return
	}
	s.mux.HandleFunc("GET /api/broadcast", s.broadcastGet)
	s.mux.HandleFunc("POST /api/broadcast", s.broadcastPost)
	s.mux.HandleFunc("POST /api/broadcast/metadata", s.broadcastMetadataPost)
	s.mux.HandleFunc("GET /api/usb-audio", s.handleUSBAudio)
	s.mux.HandleFunc("POST /api/usb-audio", s.handleUSBAudio)
	s.mux.HandleFunc("GET /v1/sc5000/usb-audio", s.handleUSBAudio)
	s.mux.HandleFunc("POST /v1/sc5000/usb-audio", s.handleUSBAudio)
	// Public on purpose (like the phone pages): OBS/VLC/browsers open these without a console cookie.
	s.mux.HandleFunc("GET /live.mp3", s.liveMP3)
	s.mux.HandleFunc("GET /live.wav", s.liveWAV)
}

// handleUSBAudio handles GET and POST requests for /v1/sc5000/usb-audio and /api/usb-audio.
func (s *Server) handleUSBAudio(w http.ResponseWriter, r *http.Request) {
	mgr := s.usbAudioManager()

	if r.Method == http.MethodPost {
		s.uiMu.Lock()
		defer s.uiMu.Unlock()

		cfg := mgr.GetConfig()

		if q := r.URL.Query().Get("gadgetUac2"); q != "" {
			if q == "1" || strings.EqualFold(q, "true") {
				cfg.GadgetUAC2 = true
			} else if q == "0" || strings.EqualFold(q, "false") {
				cfg.GadgetUAC2 = false
			}
		}
		if q := r.URL.Query().Get("hostDacMode"); q != "" {
			cfg.HostDACMode = q
		}
		if q := r.URL.Query().Get("hostDacCard"); q != "" {
			cfg.HostDACCard = q
		}
		if q := r.URL.Query().Get("sampleRate"); q != "" {
			if sr, err := strconv.Atoi(q); err == nil {
				cfg.SampleRate = sr
			}
		}

		if r.Body != nil {
			bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
			if err != nil {
				fail(w, 400, errors.New("request body too large"))
				return
			}
			if len(bytes.TrimSpace(bodyBytes)) > 0 {
				var in struct {
					HostDACMode *string `json:"hostDacMode"`
					HostDACCard *string `json:"hostDacCard"`
					SampleRate  *int    `json:"sampleRate"`
					GadgetUAC2  *bool   `json:"gadgetUac2"`
				}
				d := json.NewDecoder(bytes.NewReader(bodyBytes))
				d.DisallowUnknownFields()
				if err := d.Decode(&in); err != nil {
					fail(w, 400, errors.New("invalid JSON request"))
					return
				}
				if in.HostDACMode != nil {
					cfg.HostDACMode = *in.HostDACMode
				}
				if in.HostDACCard != nil {
					cfg.HostDACCard = *in.HostDACCard
				}
				if in.SampleRate != nil {
					cfg.SampleRate = *in.SampleRate
				}
				if in.GadgetUAC2 != nil {
					cfg.GadgetUAC2 = *in.GadgetUAC2
				}
			}
		}

		if err := mgr.Apply(cfg); err != nil {
			fail(w, 400, err)
			return
		}
	} else if r.Method != http.MethodGet {
		fail(w, 405, errors.New("method not allowed"))
		return
	}

	cfg := mgr.GetConfig()
	dacs, _ := mgr.DetectDACs()
	if dacs == nil {
		dacs = []broadcast.USBDAC{}
	}

	respond(w, map[string]any{
		"hostDacMode": cfg.HostDACMode,
		"hostDacCard": cfg.HostDACCard,
		"sampleRate":  cfg.SampleRate,
		"gadgetUac2":  cfg.GadgetUAC2,
		"config":      cfg,
		"dacs":        dacs,
	})
}

// liveSubscribe starts a local listener, or answers 503 when Live Stream is off.
func (s *Server) liveSubscribe(w http.ResponseWriter, contentType string) (<-chan []byte, func(), bool) {
	b := s.broadcaster()
	st := b.Status()
	if !st.Enabled {
		http.Error(w, "Live Stream is off: set Live Stream to ON in the deck control centre", http.StatusServiceUnavailable)
		return nil, nil, false
	}
	// The server-wide WriteTimeout would cut a live stream after two minutes.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	liveHeaders(w.Header(), contentType, st)
	ch, cancel := b.Subscribe()
	return ch, cancel, true
}

func liveHeaders(h http.Header, contentType string, st broadcast.Status) {
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Audio-Format", "48000Hz-16bit-2ch-PCM")
	h.Set("icy-name", st.StreamName)
	if st.CurrentTrack != "" {
		h.Set("X-EngineX-Track", strings.Map(func(r rune) rune {
			if r < 0x20 || r > 0x7e {
				return '?'
			}
			return r
		}, st.CurrentTrack))
	}
}

// liveStallTimeout is how long one write to a /live.wav listener may block. A
// network outage up to this long is ridden out: meanwhile the hub trims the
// listener's queue back to its cushion, so when the network returns the stream
// resumes with bounded latency. r3 closed the listener after 10 s; the FIN/RST
// was lost in the outage, so OBS stayed ESTABLISHED with no data and played
// silence until the source was restarted.
const liveStallTimeout = 60 * time.Second

// liveKeepAlive probes an idle /live.wav connection, so a peer that went away
// for good is noticed and the connection closed.
var liveKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 4}

// liveSendBuffer caps the kernel send buffer of a /live.wav connection, so a Wi-Fi
// stall backs audio up in the listener queue (which is trimmed) instead of the socket.
const liveSendBuffer = 16 << 10

// liveBufferMS reads ?buffer=<ms> (0..2000); missing or invalid means the default, tuned for a wired network.
func liveBufferMS(r *http.Request) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("buffer")))
	if err != nil || v < 0 || v > broadcast.MaxBufferMS {
		return broadcast.DefaultBufferMS
	}
	return v
}

// liveWAV streams 48000 Hz 16-bit stereo audio of the deck's live mix.
// Supports ?format=pcm for raw audio/L16;rate=48000;channels=2 stream with zero header overhead,
// and ?buffer=<ms> (default 120): the listener starts with that much recent audio as a
// cushion against gaps in delivery, and is trimmed back to it when it falls further behind.
func (s *Server) liveWAV(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	contentType := "audio/wav"
	if format == "pcm" {
		contentType = "audio/L16;rate=48000;channels=2"
	}
	b := s.broadcaster()
	if !b.Status().Enabled {
		http.Error(w, "Live Stream is off: set Live Stream to ON in the deck control centre", http.StatusServiceUnavailable)
		return
	}
	l, cancel := b.SubscribeLive(liveBufferMS(r))
	defer cancel()
	liveHeaders(w.Header(), contentType, b.Status())

	write, done := liveWriter(w)
	defer done()
	if format != "pcm" {
		if write(broadcast.WAVHeader()) != nil {
			return
		}
	}
	for {
		pcm, ok := l.Next(r.Context())
		if !ok || write(pcm) != nil {
			return
		}
	}
}

// liveWriter takes over the connection to cap its send buffer, sending the status
// and headers itself (a close-delimited body, as Icecast-style players expect). Where
// that is not possible it streams through w as before.
func liveWriter(w http.ResponseWriter) (write func([]byte) error, done func()) {
	rc := http.NewResponseController(w)
	conn, _, err := rc.Hijack()
	if err != nil {
		fl, _ := w.(http.Flusher)
		return func(p []byte) error {
			// Per write, as on the hijacked path: not the server's two-minute
			// WriteTimeout for the whole response.
			_ = rc.SetWriteDeadline(time.Now().Add(liveStallTimeout))
			if _, err := w.Write(p); err != nil {
				return err
			}
			if fl != nil {
				fl.Flush()
			}
			return nil
		}, func() {}
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(liveSendBuffer)
		_ = tc.SetKeepAliveConfig(liveKeepAlive)
	}
	h := w.Header().Clone()
	h.Set("Connection", "close")
	var head bytes.Buffer
	head.WriteString("HTTP/1.1 200 OK\r\n")
	_ = h.Write(&head)
	head.WriteString("\r\n")
	headSent := false
	return func(p []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(liveStallTimeout))
		if !headSent {
			headSent = true
			p = append(head.Bytes(), p...)
		}
		_, err := conn.Write(p)
		return err
	}, func() { _ = conn.Close() }
}

// mp3Encoder finds an external encoder that reads s16le 48 kHz stereo on stdin and writes MP3 to stdout.
// shortcut: no MP3 encoder is built into the binary; use lame/ffmpeg when the deck has one, upgrade to a pure-Go encoder if it never does.
func mp3Encoder(bitrate int) *exec.Cmd {
	br := strconv.Itoa(bitrate)
	if p, err := exec.LookPath("lame"); err == nil {
		return exec.Command(p, "-r", "-s", "48", "--signed", "--little-endian", "--bitwidth", "16", "-m", "s", "-b", br, "--silent", "-", "-")
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return exec.Command(p, "-loglevel", "quiet", "-fflags", "+nobuffer", "-f", "s16le", "-ar", "48000", "-ac", "2", "-i", "pipe:0", "-f", "mp3", "-b:a", br+"k", "-flush_packets", "1", "pipe:1")
	}
	return nil
}

// liveMP3 streams the deck's live mix as MP3 through an external encoder.
func (s *Server) liveMP3(w http.ResponseWriter, r *http.Request) {
	cmd := mp3Encoder(s.broadcaster().Status().Bitrate)
	if cmd == nil {
		http.Error(w, "no MP3 encoder (lame or ffmpeg) on this deck: use /live.wav", http.StatusServiceUnavailable)
		return
	}
	ch, cancel, ok := s.liveSubscribe(w, "audio/mpeg")
	if !ok {
		return
	}
	defer cancel()

	in, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "encoder start failed", http.StatusInternalServerError)
		return
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "encoder start failed", http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, "encoder start failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	go func() {
		defer in.Close()
		for {
			select {
			case <-r.Context().Done():
				return
			case pcm, open := <-ch:
				if !open {
					return
				}
				if _, err := in.Write(pcm); err != nil {
					return
				}
			}
		}
	}()

	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := out.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
