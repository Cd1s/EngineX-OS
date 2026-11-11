// Command sc5000-relay (enginex-relay) relays Spotify/Deezer/SoundCloud/Beatport
// QR login credentials to EngineX OS decks and is the single public entry point
// for remote Stems rendering.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defaultSpotifyClientID is the public Spotify Desktop identity; an explicitly
// configured operator app still takes precedence.
const defaultSpotifyClientID = "65b708073fc0480ea92a077233ca87bd"

type server struct {
	publicMu  sync.RWMutex
	public    string // public base URL, no trailing slash
	tunnelURL string // Quick Tunnel URL when running in domain-free mode
	cfg       *configStore
	fails     *failLimiter
	// newSessions caps sessions created by unauthenticated callers.
	newSessions *failLimiter
	sessions    *sessionStore
	decks       *deckStore
	spotify     *spotifyAuth
	deezer      *deezerClient
	browser     *deezerBrowser
	stems       *stemsProxy
	beatportURL string // tests; empty is Beatport's API
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func defaultRelayStateDir() string {
	if v := strings.TrimSpace(os.Getenv("ENGINEX_RELAY_STATE")); v != "" {
		return v
	}
	if _, err := os.Stat("/var/lib/enginex-relay"); err == nil {
		return "/var/lib/enginex-relay"
	}
	return "/var/lib/sc5000-relay"
}

func main() {
	listen := flag.String("listen", envOrDefault("ENGINEX_RELAY_LISTEN", "0.0.0.0:6595"), "listen address")
	public := flag.String("public", envOrDefault("ENGINEX_RELAY_PUBLIC", ""), "public base URL of your self-hosted enginex-relay (empty or 'auto' starts a domain-free Cloudflare Quick Tunnel)")
	stateDir := flag.String("state", defaultRelayStateDir(), "state directory (config.json, sessions)")
	stemsTarget := flag.String("stems-target", envOrDefault("ENGINEX_RELAY_STEMS_TARGET", "http://127.0.0.1:6596"), "local enginex-stemsd base URL (/v1/engine is appended)")
	stemsToken := flag.String("stems-token-file", envOrDefault("ENGINEX_RELAY_STEMS_TOKEN_FILE", defaultStemsTokenFile()), "enginex-stemsd internal token; missing file sends none")
	proxies := flag.String("trusted-proxies", envOrDefault("ENGINEX_RELAY_TRUSTED_PROXIES", ""), "comma-separated proxy IPs/CIDRs whose X-Forwarded-For is trusted; empty trusts all private, loopback and tailnet peers")
	flag.Parse()
	if strings.Contains(*public, "relay.example.com") {
		log.Fatal("-public must be this relay's real public URL or 'auto' (relay.example.com is a placeholder)")
	}
	tp, err := parseTrustedProxies(*proxies)
	if err != nil {
		log.Fatal(err)
	}
	trustedProxies = tp

	for _, d := range []string{*stateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			log.Fatal(err)
		}
	}
	cfg, err := loadConfig(filepath.Join(*stateDir, "config.json"))
	if err != nil {
		log.Fatal(err)
	}
	target, err := parseStemsTarget(*stemsTarget)
	if err != nil {
		log.Fatal(err)
	}
	autoTunnel := strings.TrimSpace(*public) == "" || strings.EqualFold(strings.TrimSpace(*public), "auto")
	initialPublic := strings.TrimRight(strings.TrimSpace(*public), "/")
	if autoTunnel {
		initialPublic = ""
	}
	s := &server{
		public:      initialPublic,
		cfg:         cfg,
		fails:       newLimiter(failWindow, failMax),
		newSessions: newLimiter(time.Minute, 30),
		sessions:    newSessionStore(filepath.Join(*stateDir, "sessions.json")),
		decks:       newDeckStore(filepath.Join(*stateDir, "decks.json")),
		deezer:      newDeezerClient(),
		browser:     newDeezerBrowser("http://127.0.0.1:9222"),
		stems:       newStemsProxy(target, *stemsToken),
	}
	s.spotify = newSpotifyAuth(filepath.Join(*stateDir, "spotify-https-ok.json"), s.public+"/oauth/spotify/callback")

	tunnelCtx, tunnelCancel := context.WithCancel(context.Background())
	defer tunnelCancel()
	if autoTunnel {
		go func() {
			u, err := startQuickTunnel(tunnelCtx, *listen, *stateDir, func(url string) {
				s.setPublicURL(url)
				log.Printf("enginex-relay (sc5000-relay) quick tunnel active: %s", url)
			})
			if err != nil && tunnelCtx.Err() == nil {
				log.Printf("enginex-relay (sc5000-relay) quick tunnel error: %v", err)
				return
			}
			if u != "" {
				s.setPublicURL(u)
			}
		}()
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go s.sessions.janitor()
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		tunnelCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.browser.close()
		_ = srv.Shutdown(ctx)
	}()
	pubLabel := s.publicURL()
	if pubLabel == "" {
		pubLabel = "auto-quick-tunnel"
	}
	log.Printf("enginex-relay (sc5000-relay) listening on %s (public %s)", *listen, pubLabel)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	s.browser.close()
	s.sessions.save()
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	// Deck <-> phone QR login, protected by the high-entropy session token.
	mux.HandleFunc("POST /api/relay/session", s.handleSessionCreate)
	mux.HandleFunc("GET /api/relay/session", s.handleSessionPoll)
	mux.HandleFunc("GET /m/deezer/frame", s.handleDeezerBrowserFrame)
	mux.HandleFunc("POST /m/deezer/act", s.handleDeezerBrowserAct)
	mux.HandleFunc("GET /m/{provider}", s.handleMobileGet)
	mux.HandleFunc("POST /m/{provider}", s.handleMobilePost)
	mux.HandleFunc("GET /oauth/spotify/callback", s.handleSpotifyCallback)
	mux.HandleFunc("GET /oauth/soundcloud/callback", s.handleSoundCloudCallback)
	// All legacy audio endpoints are permanently disabled, regardless of method.
	for _, p := range []string{"/api/audio", "/api/download-stream", "/api/download", "/api/queue"} {
		mux.HandleFunc(p, audioGone)
		mux.HandleFunc(p+"/", audioGone)
	}
	// Remote Stems: console password in, local daemon token out.
	mux.HandleFunc("POST /v1/engine", s.requireStemsAuth(func(w http.ResponseWriter, r *http.Request) { s.stems.engine(w, r) }))
	mux.HandleFunc("GET /healthz/stems", s.requireStemsAuth(func(w http.ResponseWriter, r *http.Request) { s.stems.health(w, r) }))
	mux.HandleFunc("POST /api/relay/deck", s.requireStemsAuth(s.handleDeckPost))
	mux.HandleFunc("GET /api/config", s.requireAuth(s.handleConfigGet))
	mux.HandleFunc("POST /api/config", s.requireAuth(s.handleConfigPost))
	// Web console.
	mux.HandleFunc("GET /{$}", s.handlePanel)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	return withLog(stripPrefix(mux))
}

// stripPrefix serves /sc5000/... with the same handlers as /...
func stripPrefix(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/sc5000" || strings.HasPrefix(p, "/sc5000/") {
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(p, "/sc5000")
			if r2.URL.Path == "" {
				r2.URL.Path = "/"
			}
			r2.URL.RawPath = ""
			h.ServeHTTP(w, r2)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func withLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		// Query strings carry passwords, tokens and ARL cookies: path only.
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond), clientIP(r))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func validProvider(p string) bool {
	return p == "spotify" || p == "deezer" || p == "soundcloud" || p == "beatport"
}

func (s *server) mobileURL(provider, token string) string {
	return s.publicURL() + "/m/" + provider + "?t=" + token
}

func (s *server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token           string `json:"token"`
		Provider        string `json:"provider"`
		SpotifyClientID string `json:"spotifyClientId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	if !validToken(req.Token) || !validProvider(req.Provider) {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "token (>=16 url-safe chars) and provider spotify|deezer required"})
		return
	}
	clientID := req.SpotifyClientID
	if !validClientID(clientID) {
		clientID = s.cfg.get().SpotifyClientID
	}
	trusted := s.trustedCaller(r)
	if !trusted && !s.allowNewSession(r, req.Token) {
		writeJSON(w, 429, map[string]any{"ok": false, "error": "too many new sessions"})
		return
	}
	if !s.sessions.register(req.Token, req.Provider, clientID, trusted) {
		writeJSON(w, 503, map[string]any{"ok": false, "error": "too many sessions"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "url": s.mobileURL(req.Provider, req.Token)})
}

func (s *server) handleSessionPoll(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	trusted := s.trustedCaller(r)
	if !trusted && !s.allowNewSession(r, token) {
		writeJSON(w, 429, map[string]any{"status": "expired", "error": "too many new sessions"})
		return
	}
	writeJSON(w, 200, s.sessions.poll(token, s.cfg.get().SpotifyClientID, trusted))
}

const audioGoneMessage = "audio download runs directly on SC5000; clawbot is credential relay only"

func audioGone(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, map[string]string{"error": audioGoneMessage})
}
