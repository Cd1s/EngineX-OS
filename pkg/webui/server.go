// Package webui provides the optional touch console without modifying Engine's
// deck UI. OAuth/cookie state is scoped to a single owner or guest session.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/skip2/go-qrcode"
)

//go:embed index.html
var assets embed.FS

type Session struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Guest    bool   `json:"guest"`
	Ready    bool   `json:"ready"`
	p        providers.Provider
	qr       providers.QRLogin
	tracks   map[string]providers.Track
	expires  time.Time
}
type pending struct {
	session, verifier, redirect string
	expires                     time.Time
}
type Config struct {
	AccessToken                                                           string
	PublicURL                                                             string
	SpotifyRedirectURL                                                    string
	SpotifyClientID, SpotifyTokenPath, SpotifyBaseURL, SpotifyAccountsURL string
	NeteaseURL, StateDir, DeezerURL, DeezerGatewayURL                     string
	SoundCloudURL, SoundCloudTokenURL                                     string
	BeatportURL                                                           string // tests; empty is Beatport's API
	AppleMusicURL, AppleMusicWebURL                                       string // tests; empty is Apple's API and web player
	HTTP                                                                  *http.Client
	GitHubURL, VersionFile                                                string // tests; defaults are api.github.com and versionFile
	// UIDir holds the deck UI settings; empty means the on-device default.
	UIDir string
	// RelayURL is the cloud login service; it passes credentials and never audio.
	// Empty keeps phone logins on the LAN, where the phone must reach this deck.
	RelayURL string
	// TidalAPIURL and TidalAuthURL override TIDAL's servers in tests.
	TidalAPIURL, TidalAuthURL string
	// BPM looks up tempo and key for browsed tracks; nil turns that off.
	BPM *providers.BPMStore
	// MediaRoot is where drives mount for OmniSource (#62); empty means /media.
	MediaRoot string
	// RestartEngine replaces "systemctl restart engine.service" in tests.
	RestartEngine func() error
	// ProlinkExport selects the optional read-only CDJ export; zero uses the
	// mounted USB and standard ports. It stays inert until explicitly enabled.
	ProlinkExport prolink.ExportConfig
	// Factory is useful for embedded integrations and deterministic tests.
	Factory func(provider string, guest bool, userID string) (providers.Provider, error)
}
type Server struct {
	cfg           Config
	jobs          *Jobs
	mu            sync.Mutex
	sessions      map[string]*Session
	pending       map[string]pending
	mux           *http.ServeMux
	tidal         *tidalGateway
	uiMu          sync.Mutex
	stemsMu       sync.Mutex // streaming-stems.json
	prolinkExport *prolink.Exporter
	closed        chan struct{}
	closeOnce     sync.Once

	updateMu    sync.Mutex
	updateCache updateStatus

	// Owner logins ended in the console; the deck must not restore them.
	ownerDeleted map[string]bool
	logins       map[string]*loginFlow

	soundCloudAuth *providers.SoundCloudAuth
	// beatport is the owner's account, shared because each refresh rotates its token.
	beatport *providers.Beatport
	shadow   *shadowState // #49 shadow decks

	automixMu sync.Mutex
	automix   *AutomixManager

	authMu    sync.Mutex
	authFails map[string]*authStrike // wrong access codes per client network
}

func New(cfg Config, jobs *Jobs) *Server {
	if cfg.AccessToken == "" {
		cfg.AccessToken = GenerateAccessCode()
	}
	s := &Server{cfg: cfg, jobs: jobs, sessions: map[string]*Session{}, pending: map[string]pending{}, mux: http.NewServeMux(), ownerDeleted: map[string]bool{}, logins: map[string]*loginFlow{}, authFails: map[string]*authStrike{}}
	s.shadow = newShadowState()
	exportCfg := cfg.ProlinkExport
	if exportCfg.MediaCache == nil {
		cacheRoot := cfg.StateDir
		if cacheRoot == "" {
			cacheRoot = s.uiDir()
		}
		exportCfg.MediaCache = &prolink.MediaCache{Dir: filepath.Join(cacheRoot, "prolink-media"), Enabled: func() bool { return s.loadUI().ProlinkShare }}
	}
	if exportCfg.Observe == nil {
		exportCfg.Observe = func(d prolink.Device) {
			if d.Number < 1 || d.Number > 6 {
				return
			}
			s.shadow.observe(d)
			prolinkMu.Lock()
			defer prolinkMu.Unlock()
			for key, old := range postedDevices[s] {
				if time.Since(old.LastSeen) > 10*time.Second {
					delete(postedDevices[s], key)
				}
			}
			if postedDevices[s] == nil {
				postedDevices[s] = make(map[string]prolink.Device)
			}
			key := deviceKey(d)
			postedDevices[s][key] = mergeDevice(postedDevices[s][key], d)
		}
	}
	if exportCfg.Peers == nil {
		exportCfg.Peers = func() []net.IP {
			var ips []net.IP
			for _, d := range prolink.BuildSnapshot(s.mergedDevices()).Devices {
				if ip := net.ParseIP(d.Address); ip != nil {
					ips = append(ips, ip)
				}
			}
			return ips
		}
	}
	s.prolinkExport = prolink.NewExporter(exportCfg)
	s.closed = make(chan struct{})
	go s.retryExport(10 * time.Second)
	if s.loadUI().ProlinkExport {
		// A missing drive or occupied port must not prevent the normal console
		// from starting. The export status reports whether it actually started.
		if err := s.prolinkExport.SetEnabled(true); err != nil {
			log.Printf("stream-sync: experimental Pro DJ Link export not started: %v", err)
		}
	}
	if cfg.StateDir != "" {
		os.MkdirAll(cfg.StateDir, 0o700)
	}
	s.mux.HandleFunc("GET /", s.index)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("GET /oauth/spotify/callback", s.callback)
	s.mux.HandleFunc("GET /api/sessions", s.listSessions)
	s.mux.HandleFunc("GET /m/{provider}", s.phoneGet)
	s.mux.HandleFunc("GET /icons/{id}", s.serviceIcon)
	s.mux.HandleFunc("POST /m/{provider}", s.phonePost)
	s.mux.HandleFunc("GET /api/ui", s.uiStatus)
	s.mux.HandleFunc("GET /api/update", s.update)
	s.mux.HandleFunc("POST /api/ui", s.uiUpdate)
	s.mux.HandleFunc("GET /api/console-qr", s.consoleQR)
	s.mux.HandleFunc("POST /api/sessions", s.createSession)
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	s.mux.HandleFunc("POST /api/sessions/{id}/login", s.beginLogin)
	s.mux.HandleFunc("POST /api/sessions/{id}/poll", s.pollLogin)
	s.mux.HandleFunc("GET /api/sessions/{id}/qr", s.qr)
	s.mux.HandleFunc("GET /api/sessions/{id}/playlists", s.playlists)
	s.mux.HandleFunc("GET /api/sessions/{id}/tracks", s.tracks)
	s.mux.HandleFunc("POST /api/jobs", s.submit)
	s.mux.HandleFunc("GET /api/jobs", s.listJobs)
	s.mux.HandleFunc("GET /api/jobs/{id}/waveform", s.waveform)
	s.mux.HandleFunc("GET /api/bpm", s.bpmConfigGet)
	s.mux.HandleFunc("POST /api/bpm", s.bpmConfigPost)
	s.mux.HandleFunc("GET /api/relay", s.relayConfigGet)
	s.mux.HandleFunc("GET /api/spotify", s.spotifyClientGet)
	s.mux.HandleFunc("POST /api/spotify", s.spotifyClientPost)
	s.mux.HandleFunc("GET /spotify-connect", s.spotifyConnectGet)
	s.mux.HandleFunc("POST /spotify-connect", s.spotifyConnectPost)
	s.mux.HandleFunc("POST /api/relay", s.relayConfigPost)
	s.RegisterBroadcastRoutes()
	s.mux.HandleFunc("GET /api/prolink", s.prolinkGet)
	s.mux.HandleFunc("GET /api/prolink/diag", s.prolinkDiagGet)
	s.mux.HandleFunc("POST /api/prolink", s.prolinkPost)
	s.mux.HandleFunc("GET /api/drives", s.dropDrivesGet)
	s.mux.HandleFunc("GET /peer/decks", s.peerDecksGet)
	s.mux.HandleFunc("GET /peer/decks/{n}/audio", s.peerAudioGet)
	s.mux.HandleFunc("GET /peer/decks/{n}/perf", s.peerPerfGet)
	s.mux.HandleFunc("POST /api/drop", s.drop)
	s.mux.HandleFunc("POST /api/relocate", s.relocate)
	s.mux.HandleFunc("GET /api/bluetooth", s.handleBluetooth)
	s.mux.HandleFunc("POST /api/bluetooth", s.handleBluetooth)
	s.mux.HandleFunc("GET /v1/sc5000/bluetooth", s.handleBluetooth)
	s.mux.HandleFunc("POST /v1/sc5000/bluetooth", s.handleBluetooth)
	s.mux.HandleFunc("GET /api/nas", s.handleNASConfig)
	s.mux.HandleFunc("POST /api/nas", s.handleNASConfig)
	s.mux.HandleFunc("POST /api/nas/test", s.handleNASTest)
	s.mux.HandleFunc("GET /v1/sc5000/nas", s.handleNASConfig)
	s.mux.HandleFunc("POST /v1/sc5000/nas", s.handleNASConfig)
	s.mux.HandleFunc("POST /v1/sc5000/nas/test", s.handleNASTest)
	s.mux.HandleFunc("GET /api/mods", s.handleMods)
	s.mux.HandleFunc("POST /api/mods", s.handleMods)
	s.mux.HandleFunc("GET /v1/sc5000/mods", s.handleMods)
	s.mux.HandleFunc("POST /v1/sc5000/mods", s.handleMods)
	s.mux.HandleFunc("POST /v1/sc5000/mods/upload", s.handleModsUpload)
	s.mux.HandleFunc("GET /api/hotcues", s.handleHotCues)
	s.mux.HandleFunc("POST /api/hotcues", s.handleHotCues)
	s.mux.HandleFunc("GET /v1/sc5000/hotcues", s.handleHotCues)
	s.mux.HandleFunc("POST /v1/sc5000/hotcues", s.handleHotCues)
	s.mux.HandleFunc("GET /api/ssh", s.handleSSH)
	s.mux.HandleFunc("POST /api/ssh", s.handleSSH)
	s.mux.HandleFunc("GET /v1/sc5000/ssh", s.handleSSH)
	s.mux.HandleFunc("POST /v1/sc5000/ssh", s.handleSSH)
	s.mux.HandleFunc("GET /api/tailscale", s.handleTailscale)
	s.mux.HandleFunc("POST /api/tailscale", s.handleTailscale)
	s.mux.HandleFunc("GET /v1/sc5000/tailscale", s.handleTailscale)
	s.mux.HandleFunc("POST /v1/sc5000/tailscale", s.handleTailscale)
	go s.tailscaleResume() // restarts the daemon after a reboot when it was left on (#130)
	go s.modsResume()      // enabled mods' daemons do not survive a reboot on their own
	s.mux.HandleFunc("GET /api/mounts", s.handleMounts)
	s.mux.HandleFunc("POST /api/mounts", s.handleMounts)
	s.mux.HandleFunc("DELETE /api/mounts", s.handleMounts)
	s.mux.HandleFunc("DELETE /api/mounts/{id}", s.handleMounts)
	s.mux.HandleFunc("GET /v1/sc5000/mounts", s.handleMounts)
	s.mux.HandleFunc("POST /v1/sc5000/mounts", s.handleMounts)
	s.mux.HandleFunc("DELETE /v1/sc5000/mounts", s.handleMounts)
	s.mux.HandleFunc("DELETE /v1/sc5000/mounts/{id}", s.handleMounts)
	s.mux.HandleFunc("GET /api/automix", s.handleAutomix)
	s.mux.HandleFunc("POST /api/automix", s.handleAutomix)
	s.mux.HandleFunc("GET /v1/sc5000/automix", s.handleAutomix)
	s.mux.HandleFunc("POST /v1/sc5000/automix", s.handleAutomix)
	s.mux.HandleFunc("POST /api/sampler/loop-to-pad", s.handleSamplerLoopToPad)
	s.mux.HandleFunc("POST /v1/sc5000/sampler/loop-to-pad", s.handleSamplerLoopToPad)
	s.automix = newAutomixManager(s.automixConfigPath())
	s.soundCloudAuth = &providers.SoundCloudAuth{TokenURL: cfg.SoundCloudTokenURL, Path: filepath.Join(cfg.StateDir, "soundcloud-token.json"), HTTP: cfg.HTTP}
	if cfg.BPM != nil {
		cfg.BPM.SoundCloudAuth = s.soundCloudAuth
		cfg.BPM.Spotify = s.ownerSpotify
	}
	s.beatport = &providers.Beatport{BaseURL: cfg.BeatportURL, HTTP: cfg.HTTP, Path: filepath.Join(cfg.StateDir, "beatport-owner.json")}
	s.tidal = newTidalGateway(s)
	if jobs != nil {
		jobs.mu.Lock()
		jobs.netease = s.neteaseFor
		jobs.neteaseDirect = s.neteaseDirect
		jobs.deezerReady = func() bool { return s.savedDeezerOwner().ARL != "" }
		jobs.soundcloud = s.ownerSoundCloud
		jobs.beatport = s.ownerBeatport
		jobs.spotify = s.ownerSpotify
		jobs.bpm = cfg.BPM
		if jobs.p.Deemix != nil {
			jobs.p.Deemix.PreferMP3 = func() bool { return s.quality() == quality320k }
		}
		jobs.mu.Unlock()
	}
	return s
}

// retryExport starts a saved-ON export once a Rekordbox USB is mounted (#63).
func (s *Server) retryExport(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			s.tryExport()
		}
	}
}

func (s *Server) tryExport() {
	s.uiMu.Lock()
	on := s.loadUI().ProlinkExport
	s.uiMu.Unlock()
	if !on {
		return
	}
	if s.prolinkExport.Status().Enabled {
		if err := s.prolinkExport.Rebind(); err != nil {
			log.Printf("stream-sync: Pro DJ Link export rebind: %v", err)
		}
	} else if s.prolinkExport.SetEnabled(true) == nil {
		log.Printf("stream-sync: Pro DJ Link export started")
	}
}

// Close releases optional network exports, including active dbserver clients.
func (s *Server) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
	if s.tidal != nil {
		s.tidal.saveTracks() // the IDs Engine still lists, for the next run (#73)
	}
	if s.prolinkExport != nil {
		s.prolinkExport.Close()
	}
	prolinkMu.Lock()
	delete(postedDevices, s)
	prolinkMu.Unlock()
}

// DefaultStateDir keeps logins on the deck's writable partition; its home is read-only.
func DefaultStateDir(home string) string {
	if _, err := os.Stat("/data/sc5000-custom"); err == nil {
		return "/data/sc5000-custom/state"
	}
	return filepath.Join(home, ".config/stream-sync")
}
func (s *Server) AccessToken() string { return s.cfg.AccessToken }

// LoadAccessToken keeps the console access code in <dir>/access-token (0600), so a
// restart does not log every browser out (#14). created reports a new code.
func LoadAccessToken(dir string) (token string, created bool, err error) {
	path := filepath.Join(dir, "access-token")
	if b, err := os.ReadFile(path); err == nil {
		// A long code from older releases is too long to type on a phone: replace it.
		if t := strings.TrimSpace(string(b)); t != "" && !legacyAccessCode.MatchString(t) {
			return t, false, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	token = GenerateAccessCode()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return token, true, nil
}

func (s *Server) consoleQR(w http.ResponseWriter, r *http.Request) {
	target := s.cfg.PublicURL
	if target == "" {
		target = "http://sc5000.lan:8080"
	}
	png, err := qrcode.Encode(target, qrcode.Medium, 320)
	if err != nil {
		fail(w, 400, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

// legacyAccessCode matches codes from older releases: providers.RandomID (43 base64url) or 32 hex.
var legacyAccessCode = regexp.MustCompile(`^([A-Za-z0-9_-]{43}|[0-9a-f]{32})$`)

// accessCodeAlphabet leaves out 0/O and 1/I, which read alike on the deck screen.
const accessCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// GenerateAccessCode returns a random XXXX-XXXX code: 32^8 (40 bits) choices.
func GenerateAccessCode() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = accessCodeAlphabet[b[i]%32] // 256 % 32 == 0: no modulo bias
	}
	return string(b[:4]) + "-" + string(b[4:])
}

// normalizeAccessCode lets "k7m9 p4x2" and "K7M9-P4X2" match the same code.
func normalizeAccessCode(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
}

const (
	authMaxFails = 5
	authWindow   = time.Minute
	authLockout  = time.Minute
)

type authStrike struct {
	n            int
	first, until time.Time
}

// authKey groups IPv6 clients by /64, so rotating privacy addresses do not reset the count.
func authKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String()
	}
	return host
}

// tryCode checks code against the access code. authMaxFails wrong codes from one
// client within authWindow lock it out for authLockout, even with the right code.
func (s *Server) tryCode(r *http.Request, code string) (ok, locked bool) {
	if code == "" { // no credential sent is not a guess
		return false, false
	}
	key, now := authKey(r), time.Now()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	f := s.authFails[key]
	if f != nil && now.Before(f.until) {
		return false, true
	}
	if subtle.ConstantTimeCompare([]byte(normalizeAccessCode(code)), []byte(normalizeAccessCode(s.cfg.AccessToken))) == 1 {
		delete(s.authFails, key)
		return true, false
	}
	if f == nil || now.Sub(f.first) > authWindow {
		if len(s.authFails) >= 4096 { // bound memory against address-spraying clients
			for k, v := range s.authFails {
				if now.Sub(v.first) > authWindow && now.After(v.until) {
					delete(s.authFails, k)
				}
			}
		}
		f = &authStrike{first: now}
		s.authFails[key] = f
	}
	if f.n++; f.n >= authMaxFails {
		f.until = now.Add(authLockout)
		return false, true
	}
	return false, false
}

// authorized checks the bearer header or console cookie. A wrong cookie is
// cleared so an open tab with a stale code stops counting toward the lockout.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) (ok, locked bool) {
	v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	cookie := false
	if c, err := r.Cookie("stream_sync"); err == nil && v == "" {
		v, cookie = c.Value, true
	}
	if ok, locked = s.tryCode(r, v); !ok && cookie {
		http.SetCookie(w, &http.Cookie{Name: "stream_sync", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	}
	return ok, locked
}

var errAuthLocked = errors.New("too many wrong access codes, try again in a minute")

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	if isTidal(r.URL.Path) {
		s.tidal.ServeHTTP(w, r)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		// Phone pages carry a one-time token instead of a cookie, and iOS web
		// views send "Origin: null". The console cookie is SameSite=Strict, so a
		// null origin cannot ride on it either.
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" && !strings.HasPrefix(r.URL.Path, "/m/") && r.URL.Path != "/spotify-connect" {
			u, e := url.Parse(origin)
			if e != nil || u.Host != r.Host {
				fail(w, 403, errors.New("same-origin request required"))
				return
			}
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if ok, locked := s.authorized(w, r); locked {
			fail(w, http.StatusTooManyRequests, errAuthLocked)
			return
		} else if !ok {
			fail(w, 401, errors.New("console login required"))
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
func input(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	d.DisallowUnknownFields()
	err := d.Decode(v)
	if err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("extra JSON")
		}
	}
	if err != nil {
		fail(w, 400, errors.New("invalid JSON request"))
		return false
	}
	return true
}

const consoleLoginPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Stream Sync</title><style>body{background:#111;color:#eee;font:16px/1.5 system-ui,sans-serif;margin:0;padding:24px}main{max-width:460px;margin:12vh auto}h1{font-size:28px}label{display:block;font-weight:600}input,button{box-sizing:border-box;width:100%;padding:14px;margin-top:10px;border-radius:6px;font:inherit}input{background:#222;color:#fff;border:1px solid #555}button{background:#287e60;color:white;border:0;font-weight:600;cursor:pointer}p{color:#bbb}</style><main><h1>Stream Sync</h1><form method="post" action="/login"><label for="token" id="label">Console Access Code</label><input id="token" name="token" type="text" placeholder="XXXX-XXXX" autocomplete="off" autocapitalize="characters" spellcheck="false" required><button id="submit">Sign in</button></form><p id="hint">The Console Access Code is shown on the deck screen under Settings → About.</p></main><script>const l=(navigator.language||'en').toLowerCase();const t=l.startsWith('zh')?(l.includes('hant')||/-(tw|hk|mo)/.test(l)?['控制台存取碼','登入','控制台存取碼顯示在碟機螢幕的「設定 → 關於」中。']:['控制台访问码','登录','控制台访问码显示在碟机屏幕的「设置 → 关于」中。']):null;if(t){document.documentElement.lang=l;document.getElementById('label').textContent=t[0];document.getElementById('submit').textContent=t[1];document.getElementById('hint').textContent=t[2]}</script></html>`

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if ok, _ := s.authorized(w, r); !ok {
		io.WriteString(w, consoleLoginPage)
		return
	}
	b, _ := assets.ReadFile("index.html")
	w.Write(b)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		fail(w, 400, errors.New("invalid login"))
		return
	}
	if ok, locked := s.tryCode(r, r.Form.Get("token")); locked {
		fail(w, http.StatusTooManyRequests, errAuthLocked)
		return
	} else if !ok {
		fail(w, 401, errors.New("invalid access code"))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "stream_sync", Value: s.cfg.AccessToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (s *Server) makeProvider(name string, guest bool, user string) (providers.Provider, error) {
	if s.cfg.Factory != nil {
		return s.cfg.Factory(name, guest, user)
	}
	switch name {
	case "deezer":
		arl := ""
		if user == "" && !guest {
			owner := s.savedDeezerOwner()
			user, arl = owner.UserID, owner.ARL
		}
		return &providers.Deezer{BaseURL: s.cfg.DeezerURL, GatewayURL: s.cfg.DeezerGatewayURL, UserID: user, ARL: arl, HTTP: s.cfg.HTTP}, nil
	case "soundcloud":
		if guest {
			return s.soundCloud(&providers.SoundCloud{Auth: s.soundCloudAuth}), nil
		}
		return s.ownerSoundCloudProvider(), nil
	case "beatport":
		if guest {
			return &providers.Beatport{BaseURL: s.cfg.BeatportURL, HTTP: s.cfg.HTTP}, nil
		}
		return s.beatport, nil
	case "netease":
		n := &providers.Netease{BaseURL: s.cfg.NeteaseURL, HTTP: s.cfg.HTTP}
		if !guest {
			n.CookiePath = filepath.Join(s.cfg.StateDir, "netease-owner.json")
			if b, e := os.ReadFile(n.CookiePath); e == nil {
				var v struct {
					Cookie string
					UserID int64
				}
				if json.Unmarshal(b, &v) == nil {
					n.Cookie = v.Cookie
					n.UserID = v.UserID
				}
			}
		}
		return n, nil
	case "applemusic":
		p := &providers.AppleMusic{BaseURL: s.cfg.AppleMusicURL, WebURL: s.cfg.AppleMusicWebURL, HTTP: s.cfg.HTTP}
		if !guest {
			p.OwnerPath = filepath.Join(s.cfg.StateDir, "applemusic-owner.json")
			if err := p.Load(); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
		return p, nil
	case "spotify":
		p := &providers.Spotify{ClientID: s.spotifyClientID(), HTTP: s.cfg.HTTP, BaseURL: s.cfg.SpotifyBaseURL, AccountsURL: s.cfg.SpotifyAccountsURL}
		if !guest {
			p.TokenPath = s.cfg.SpotifyTokenPath
			if s.cfg.StateDir != "" {
				p.LinksPath = filepath.Join(s.cfg.StateDir, "spotify-links.json")
			}
			if p.TokenPath == "" && s.cfg.StateDir != "" {
				p.TokenPath = filepath.Join(s.cfg.StateDir, "spotify-token.json")
			}
			if p.TokenPath != "" {
				if err := p.LoadToken(); err != nil && !os.IsNotExist(err) {
					return nil, err
				}
				// A token minted by another app (the old built-in ID) is not used:
				// each owner logs in again with their own Client ID.
				if id := s.spotifyClientID(); p.ClientID != id {
					p.Token, p.ClientID = providers.Token{}, id
				}
			}
		}
		return p, nil
	}
	return nil, errors.New("unsupported provider")
}
func (s *Server) prune() {
	now := time.Now()
	for id, v := range s.sessions {
		if v.Guest && now.After(v.expires) {
			delete(s.sessions, id)
		}
	}
	for k, v := range s.pending {
		if now.After(v.expires) {
			delete(s.pending, k)
		}
	}
}
func (s *Server) session(r *http.Request) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	v := s.sessions[r.PathValue("id")]
	if v == nil {
		return nil, errors.New("session expired or missing")
	}
	return v, nil
}
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	out := make([]Session, 0, len(s.sessions))
	for _, v := range s.sessions {
		out = append(out, *v)
	}
	respond(w, out)
}
func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Provider string `json:"provider"`
		Guest    bool   `json:"guest"`
		UserID   string `json:"userId"`
	}
	if !input(w, r, &v) {
		return
	}
	if !slices.Contains(loginServices, v.Provider) {
		fail(w, 400, errors.New("unsupported provider"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if len(s.sessions) >= 16 {
		fail(w, 429, errors.New("too many sessions"))
		return
	}
	// One owner per service prevents concurrent credential replacement; guests
	// are independent and cannot reuse the owner's cookie/token file.
	if !v.Guest {
		delete(s.ownerDeleted, v.Provider)
		for _, old := range s.sessions {
			if !old.Guest && old.Provider == v.Provider {
				respond(w, old)
				return
			}
		}
	}
	p, err := s.makeProvider(v.Provider, v.Guest, v.UserID)
	if err != nil {
		fail(w, 400, err)
		return
	}
	session := &Session{ID: providers.RandomID(), Provider: v.Provider, Guest: v.Guest, p: p, tracks: map[string]providers.Track{}, expires: time.Now().Add(8 * time.Hour), Ready: v.Provider == "deezer"}
	if sp, ok := p.(*providers.Spotify); ok {
		session.Ready = sp.HasToken()
	}
	if np, ok := p.(*providers.Netease); ok {
		session.Ready = np.LoggedIn()
	}
	if bp, ok := p.(*providers.Beatport); ok {
		session.Ready = bp.Ready()
	}
	s.sessions[session.ID] = session
	respond(w, session)
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	if v := s.sessions[id]; v != nil && !v.Guest {
		s.ownerDeleted[v.Provider] = true
	}
	delete(s.sessions, id)
	for state, v := range s.pending {
		if v.session == id {
			delete(s.pending, state)
		}
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) beginLogin(w http.ResponseWriter, r *http.Request) {
	v, err := s.session(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	switch p := v.p.(type) {
	case *providers.Netease:
		q, e := p.BeginQR(r.Context())
		if e != nil {
			fail(w, 502, e)
			return
		}
		s.mu.Lock()
		v.qr = q
		s.mu.Unlock()
		respond(w, q)
	case *providers.Spotify:
		if p.ClientID == "" {
			fail(w, 400, errors.New("configure --spotify-client-id"))
			return
		}
		redirect := s.cfg.SpotifyRedirectURL
		if redirect == "" {
			redirect = strings.TrimRight(s.cfg.PublicURL, "/") + "/oauth/spotify/callback"
		}
		state := providers.RandomID()
		auth, verifier := p.BeginPKCE(state, redirect)
		s.mu.Lock()
		s.prune()
		for k, pending := range s.pending {
			if pending.session == v.ID {
				delete(s.pending, k)
			}
		}
		s.pending[state] = pending{v.ID, verifier, redirect, time.Now().Add(10 * time.Minute)}
		v.qr = providers.QRLogin{URL: auth}
		s.mu.Unlock()
		respond(w, map[string]string{"url": auth})
	default:
		fail(w, 400, errors.New("public Deezer playlists need no login"))
	}
}
func (s *Server) pollLogin(w http.ResponseWriter, r *http.Request) {
	v, err := s.session(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	p, ok := v.p.(*providers.Netease)
	if !ok {
		fail(w, 400, errors.New("use Spotify callback"))
		return
	}
	s.mu.Lock()
	key := v.qr.Key
	s.mu.Unlock()
	if key == "" {
		fail(w, 400, errors.New("start QR login first"))
		return
	}
	code, err := p.PollQR(r.Context(), key)
	if err != nil {
		fail(w, 502, err)
		return
	}
	s.mu.Lock()
	v.Ready = code == 803
	s.mu.Unlock()
	respond(w, map[string]int{"code": code})
}
func (s *Server) qr(w http.ResponseWriter, r *http.Request) {
	v, err := s.session(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	s.mu.Lock()
	text := v.qr.URL
	s.mu.Unlock()
	if text == "" {
		fail(w, 400, errors.New("start login first"))
		return
	}
	png, err := qrcode.Encode(text, qrcode.Medium, 320)
	if err != nil {
		fail(w, 400, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	s.mu.Lock()
	s.prune()
	p, ok := s.pending[state]
	delete(s.pending, state)
	v := s.sessions[p.session]
	s.mu.Unlock()
	if !ok || v == nil || r.URL.Query().Get("code") == "" {
		fail(w, 400, errors.New("invalid or expired OAuth state"))
		return
	}
	sp, ok := v.p.(*providers.Spotify)
	if !ok {
		fail(w, 400, errors.New("invalid login session"))
		return
	}
	if err := sp.CompletePKCE(r.Context(), r.URL.Query().Get("code"), p.verifier, p.redirect); err != nil {
		fail(w, 502, err)
		return
	}
	s.mu.Lock()
	v.Ready = true
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, "<meta name=viewport content=width=device-width><p>Spotify 已登录，可以回到 SC5000 控制台浏览歌单。</p>")
}
func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	v, err := s.session(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	p, err := v.p.Playlists(r.Context())
	if err != nil {
		fail(w, 502, err)
		return
	}
	respond(w, p)
}
func (s *Server) tracks(w http.ResponseWriter, r *http.Request) {
	v, err := s.session(r)
	if err != nil {
		fail(w, 404, err)
		return
	}
	id := r.URL.Query().Get("playlist")
	if id == "" {
		fail(w, 400, errors.New("playlist ID required"))
		return
	}
	tracks, err := v.p.Tracks(r.Context(), id)
	if err != nil {
		fail(w, 502, err)
		return
	}
	if len(tracks) > 20000 {
		fail(w, 400, errors.New("playlist too large"))
		return
	}
	s.mu.Lock()
	if v.tracks == nil {
		v.tracks = make(map[string]providers.Track, len(tracks))
	}
	for _, t := range tracks {
		v.tracks[t.ID] = t
	}
	s.mu.Unlock()
	respond(w, tracks)
}
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string `json:"sessionId"`
		TrackID   string `json:"trackId"`
	}
	if !input(w, r, &in) {
		return
	}
	s.mu.Lock()
	s.prune()
	v := s.sessions[in.SessionID]
	var t providers.Track
	guest := false
	if v != nil {
		t = v.tracks[in.TrackID]
		guest = v.Guest
	}
	s.mu.Unlock()
	if v == nil || t.ID == "" {
		fail(w, 400, errors.New("browse the track in an active session first"))
		return
	}
	if s.jobs == nil {
		fail(w, 503, errors.New("worker unavailable"))
		return
	}
	job, err := s.jobs.Submit(v.ID, t, guest)
	if err != nil {
		fail(w, 429, err)
		return
	}
	respond(w, job)
}
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		respond(w, []Job{})
		return
	}
	respond(w, map[string]any{"jobs": s.jobs.List(), "resourceGuard": s.jobs.p.Guard.LastReport()})
}
func (s *Server) waveform(w http.ResponseWriter, r *http.Request) {
	if s.jobs == nil {
		http.NotFound(w, r)
		return
	}
	res := s.jobs.Result(r.PathValue("id"))
	if res == nil {
		http.NotFound(w, r)
		return
	}
	respond(w, map[string]any{"analysis": res, "rgb": res.Waveform, "overview": res.Overview})
}

// ShutdownContext supplies a finite deadline for graceful HTTP shutdown.
func ShutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}
