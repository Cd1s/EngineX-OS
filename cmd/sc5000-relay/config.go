package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	defaultPassword     = "change-me-in-config"
	defaultDeezerUserID = "6850245583"
	authCookie          = "deemix_auth"
	authCookieTTL       = 30 * 24 * time.Hour
)

type config struct {
	PasswordHash      string    `json:"passwordHash"`
	PasswordIsDefault bool      `json:"passwordIsDefault"`
	PasswordChangedAt time.Time `json:"passwordChangedAt,omitzero"`
	CookieSecret      string    `json:"cookieSecret"`
	DeezerARL         string    `json:"deezerArl,omitempty"`
	DeezerARLUserName string    `json:"deezerArlUserName,omitempty"`
	DeezerARLChecked  time.Time `json:"deezerArlChecked,omitzero"`
	DeezerUserID      string    `json:"deezerUserId"`
	SpotifyClientID   string    `json:"spotifyClientId"`

	SoundCloudClientID string `json:"soundcloudClientId,omitempty"`
	SoundCloudSecret   string `json:"soundcloudClientSecret,omitempty"`
	SoundCloudToken    string `json:"soundcloudToken,omitempty"`
	GetSongBPMKey      string `json:"getSongBpmKey,omitempty"`
}

// configStore persists config.json (0600) and verifies the access password.
type configStore struct {
	mu   sync.Mutex
	path string
	c    config
	// sha256 of the last password that matched PasswordHash, so polling
	// clients do not pay bcrypt on every request.
	good [32]byte
	hasG bool
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func loadConfig(path string) (*configStore, error) {
	cs := &configStore{path: path}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &cs.c); err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	changed := false
	if cs.c.PasswordHash == "" {
		h, _ := bcrypt.GenerateFromPassword([]byte(defaultPassword), bcrypt.DefaultCost)
		cs.c.PasswordHash, cs.c.PasswordIsDefault = string(h), true
		changed = true
	}
	if cs.c.CookieSecret == "" {
		cs.c.CookieSecret = randomHex(32)
		changed = true
	}
	if cs.c.DeezerUserID == "" {
		cs.c.DeezerUserID = defaultDeezerUserID
		changed = true
	}
	if !validClientID(cs.c.SpotifyClientID) {
		cs.c.SpotifyClientID = defaultSpotifyClientID
		changed = true
	}
	if changed {
		if err := cs.saveLocked(); err != nil {
			return nil, err
		}
	}
	return cs, nil
}

func (cs *configStore) saveLocked() error {
	b, _ := json.MarshalIndent(cs.c, "", "  ")
	return writeFileAtomic(cs.path, b, 0o600)
}

func (cs *configStore) get() config {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.c
}

// update applies fn to a copy and publishes it only once it is on disk.
func (cs *configStore) update(fn func(*config)) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	next := cs.c
	fn(&next)
	b, _ := json.MarshalIndent(next, "", "  ")
	if err := writeFileAtomic(cs.path, b, 0o600); err != nil {
		return fmt.Errorf("保存配置失败：%v", err)
	}
	cs.c = next
	return nil
}

func (cs *configStore) checkPassword(pw string) bool {
	if pw == "" || len(pw) > 256 {
		return false
	}
	sum := sha256.Sum256([]byte(pw))
	cs.mu.Lock()
	hash, good, hasG := cs.c.PasswordHash, cs.good, cs.hasG
	cs.mu.Unlock()
	if hasG && subtle.ConstantTimeCompare(sum[:], good[:]) == 1 {
		return true
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) != nil {
		return false
	}
	cs.mu.Lock()
	if cs.c.PasswordHash == hash {
		cs.good, cs.hasG = sum, true
	}
	cs.mu.Unlock()
	return true
}

func (cs *configStore) setPassword(pw string) error {
	if len(pw) < 8 || len(pw) > 128 {
		return errors.New("新密码长度需为 8~128 位")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	err = cs.update(func(c *config) {
		c.PasswordHash = string(h)
		c.PasswordIsDefault = pw == defaultPassword
		c.PasswordChangedAt = time.Now().UTC()
	})
	if err == nil {
		cs.mu.Lock()
		cs.hasG = false
		cs.mu.Unlock()
	}
	return err
}

// Cookie value: <expiry-unix>.<hmac(secret, expiry|passwordHash)>, so a
// password change logs every browser out.
func (cs *configStore) cookieMAC(exp string) string {
	c := cs.get()
	m := hmac.New(sha256.New, []byte(c.CookieSecret))
	m.Write([]byte(exp + "|" + c.PasswordHash))
	return hex.EncodeToString(m.Sum(nil))
}

func (cs *configStore) issueCookie(w http.ResponseWriter, r *http.Request) {
	exp := strconv.FormatInt(time.Now().Add(authCookieTTL).Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: authCookie, Value: exp + "." + cs.cookieMAC(exp), Path: "/",
		MaxAge: int(authCookieTTL / time.Second), HttpOnly: true,
		Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: authCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

func (cs *configStore) validCookie(v string) bool {
	exp, mac, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	t, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > t {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(cs.cookieMAC(exp)))
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// passwordFromRequest returns the password presented by an API client.
func passwordFromRequest(r *http.Request) string {
	if v := r.Header.Get("X-Deemix-Password"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	if v := r.URL.Query().Get("key"); v != "" {
		return v
	}
	return r.URL.Query().Get("password")
}

// failLimiter counts events per client in a sliding window: password
// failures (10 per 5 minutes) and new public sessions (30 per minute).
type failLimiter struct {
	mu     sync.Mutex
	m      map[string][]time.Time
	window time.Duration
	max    int
}

const (
	failWindow = 5 * time.Minute
	failMax    = 10
)

func newLimiter(window time.Duration, max int) *failLimiter {
	return &failLimiter{m: map[string][]time.Time{}, window: window, max: max}
}

func (l *failLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= l.max
}

func (l *failLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 10000 {
		l.m = map[string][]time.Time{}
	}
	l.m[key] = append(l.prune(key), time.Now())
}

func (l *failLimiter) prune(key string) []time.Time {
	ts := l.m[key]
	i := 0
	for i < len(ts) && time.Since(ts[i]) > l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.m, key)
	}
	return ts
}

// clientIP trusts X-Forwarded-For only from private/tailnet peers (the
// mini-router edge proxy); the right-most entry is the one it appended.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsPrivate() || ip.IsLoopback() || cgnat.Contains(ip)) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return v
		}
	}
	return host
}

var _, cgnat, _ = net.ParseCIDR("100.64.0.0/10")

// trustedCaller reports whether an optional password on a public endpoint
// (deck session calls) is valid; wrong guesses count against the limiter.
func (s *server) trustedCaller(r *http.Request) bool {
	pw := passwordFromRequest(r)
	if pw == "" {
		return false
	}
	ip := clientIP(r)
	if s.fails.blocked(ip) {
		return false
	}
	if !s.cfg.checkPassword(pw) {
		s.fails.fail(ip)
		return false
	}
	return true
}

// allowNewSession limits sessions an unauthenticated client may create.
func (s *server) allowNewSession(r *http.Request, token string) bool {
	if s.sessions.exists(token) {
		return true
	}
	ip := clientIP(r)
	if s.newSessions.blocked(ip) {
		return false
	}
	s.newSessions.fail(ip)
	return true
}

func (s *server) authorized(r *http.Request) bool {
	if pw := passwordFromRequest(r); pw != "" {
		return s.cfg.checkPassword(pw)
	}
	if c, err := r.Cookie(authCookie); err == nil {
		return s.cfg.validCookie(c.Value)
	}
	return false
}

func (s *server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.fails.blocked(ip) {
			writeJSON(w, 429, map[string]any{"error": "too many failed password attempts, retry later"})
			return
		}
		if !s.authorized(r) {
			s.fails.fail(ip)
			log.Printf("auth: rejected %s %s from %s", r.Method, r.URL.Path, ip)
			writeJSON(w, 401, map[string]any{"error": "unauthorized: invalid deemix password"})
			return
		}
		h(w, r)
	}
}
