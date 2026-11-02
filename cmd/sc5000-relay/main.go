// Command sc5000-relay relays Spotify/Deezer QR login credentials to SC5000.
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
	"syscall"
	"time"
)

const defaultSpotifyClientID = "<redacted-spotify-client-id>"

type server struct {
	public string // public base URL, no trailing slash
	cfg    *configStore
	fails  *failLimiter
	// newSessions caps sessions created by unauthenticated callers.
	newSessions *failLimiter
	sessions    *sessionStore
	spotify     *spotifyAuth
	deezer      *deezerClient
	browser     *deezerBrowser
}

func main() {
	listen := flag.String("listen", "0.0.0.0:6595", "listen address")
	public := flag.String("public", "https://relay.example.com", "public base URL")
	stateDir := flag.String("state", "/var/lib/sc5000-relay", "state directory (config.json, sessions)")
	flag.Parse()

	for _, d := range []string{*stateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			log.Fatal(err)
		}
	}
	cfg, err := loadConfig(filepath.Join(*stateDir, "config.json"))
	if err != nil {
		log.Fatal(err)
	}
	s := &server{
		public:      strings.TrimRight(*public, "/"),
		cfg:         cfg,
		fails:       newLimiter(failWindow, failMax),
		newSessions: newLimiter(time.Minute, 30),
		sessions:    newSessionStore(filepath.Join(*stateDir, "sessions.json")),
		deezer:      newDeezerClient(),
		browser:     newDeezerBrowser("http://127.0.0.1:9222"),
	}
	s.spotify = newSpotifyAuth(filepath.Join(*stateDir, "spotify-https-ok.json"), s.public+"/oauth/spotify/callback")

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
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.browser.close()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("sc5000-relay listening on %s (public %s)", *listen, s.public)
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

func validProvider(p string) bool { return p == "spotify" || p == "deezer" || p == "soundcloud" }

func (s *server) mobileURL(provider, token string) string {
	return s.public + "/m/" + provider + "?t=" + token
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

const audioGoneMessage = "audio download runs directly on SC5000; relay-host is credential relay only"

func audioGone(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, map[string]string{"error": audioGoneMessage})
}
