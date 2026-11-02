package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"regexp"
	"sync"
	"time"
)

const (
	sessionTTL   = 15 * time.Minute
	tombstoneTTL = time.Hour
	maxSessions  = 20000
)

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,256}$`)

func validToken(t string) bool { return tokenRe.MatchString(t) }

type spotifyToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int       `json:"expires_in"`
	Expiry       time.Time `json:"expiry"`
	ClientID     string    `json:"client_id"`
}

type deezerInfo struct {
	UserID string `json:"userId"`
	ARL    string `json:"arl,omitempty"`
}

// soundCloudInfo is passed to the deck as entered; the deck checks it against the API.
type soundCloudInfo struct {
	User     string `json:"user,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	Secret   string `json:"clientSecret,omitempty"`
	Token    string `json:"token,omitempty"`

	// Set by an account login, which also reaches private lists.
	UserID       int64     `json:"userId,omitempty"`
	Username     string    `json:"username,omitempty"`
	UserToken    string    `json:"userToken,omitempty"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
}

type session struct {
	Token           string          `json:"token"`
	Provider        string          `json:"provider"`
	Status          string          `json:"status"` // waiting|scanned|done
	SpotifyClientID string          `json:"spotifyClientId"`
	Verifier        string          `json:"verifier,omitempty"`
	SpotifyToken    *spotifyToken   `json:"spotifyToken,omitempty"`
	Deezer          *deezerInfo     `json:"deezer,omitempty"`
	SoundCloud      *soundCloudInfo `json:"soundcloud,omitempty"`
	Expires         time.Time       `json:"expires"`
	// Trusted: the deck registered or polled it with the access password, so
	// results from the phone may also update the server-wide config.
	Trusted bool `json:"trusted,omitempty"`
}

func (s *session) expired(now time.Time) bool { return now.After(s.Expires) }

type pollResponse struct {
	Status       string          `json:"status"`
	Provider     string          `json:"provider,omitempty"`
	SpotifyToken *spotifyToken   `json:"spotifyToken,omitempty"`
	Deezer       *deezerInfo     `json:"deezer,omitempty"`
	SoundCloud   *soundCloudInfo `json:"soundcloud,omitempty"`
}

// sessionStore keeps sessions in memory and mirrors them to disk so a
// service restart does not invalidate QR codes already on the deck screen.
type sessionStore struct {
	mu    sync.Mutex
	path  string
	m     map[string]*session
	dirty bool
}

func newSessionStore(path string) *sessionStore {
	st := &sessionStore{path: path, m: map[string]*session{}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &st.m); err != nil {
			log.Printf("sessions: ignoring unreadable %s: %v", path, err)
			st.m = map[string]*session{}
		}
	}
	return st
}

// saveLocked marks the store dirty; the janitor flushes at most every 2s so
// public endpoints cannot force a full rewrite per request.
func (st *sessionStore) saveLocked() { st.dirty = true }

func (st *sessionStore) flushLocked() {
	if !st.dirty {
		return
	}
	st.dirty = false
	b, err := json.Marshal(st.m)
	if err != nil {
		return
	}
	if err := writeFileAtomic(st.path, b, 0o600); err != nil {
		log.Printf("sessions: save: %v", err)
	}
}

func (st *sessionStore) save() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.flushLocked()
}

func (st *sessionStore) exists(token string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.m[token] != nil
}

func newSession(token, provider, clientID string) *session {
	return &session{Token: token, Provider: provider, Status: "waiting", SpotifyClientID: clientID, Expires: time.Now().Add(sessionTTL)}
}

// register creates or refreshes the session the deck is about to show as QR.
func (st *sessionStore) register(token, provider, clientID string, trusted bool) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.m[token]
	if s == nil && len(st.m) >= maxSessions {
		return false
	}
	if s == nil || s.expired(time.Now()) || s.Provider != provider {
		s = newSession(token, provider, clientID)
		st.m[token] = s
	} else {
		s.Expires = time.Now().Add(sessionTTL)
		if s.Status != "done" && s.SpotifyClientID != clientID {
			s.SpotifyClientID = clientID
			s.Verifier = ""
		}
	}
	s.Trusted = s.Trusted || trusted
	st.saveLocked()
	return true
}

// poll never deletes: retries may read the same result twice.
func (st *sessionStore) poll(token, clientID string, trusted bool) pollResponse {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.m[token]
	if s == nil {
		if !validToken(token) || len(st.m) >= maxSessions {
			return pollResponse{Status: "expired"}
		}
		// Deck registered before a restart we have no record of, or never
		// reached us: keep its QR usable.
		s = newSession(token, "", clientID)
		st.m[token] = s
		st.saveLocked()
	}
	if s.expired(time.Now()) {
		return pollResponse{Status: "expired", Provider: s.Provider}
	}
	if trusted && !s.Trusted {
		s.Trusted = true
		st.saveLocked()
	}
	return pollResponse{Status: s.Status, Provider: s.Provider, SpotifyToken: s.SpotifyToken, Deezer: s.Deezer, SoundCloud: s.SoundCloud}
}

// open returns a copy of the session for a phone page, creating it on demand
// (stateless fallback), moving waiting -> scanned and, for Spotify, fixing
// the PKCE verifier so reloading the page keeps an issued code valid.
func (st *sessionStore) open(token, provider, clientID string) (session, bool) {
	if !validToken(token) || !validProvider(provider) {
		return session{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.m[token]
	if s == nil && len(st.m) >= maxSessions {
		return session{}, false
	}
	if s != nil && s.expired(time.Now()) {
		// An old QR must not come back to life; the deck makes a new token.
		return session{}, false
	}
	if s == nil || (s.Provider != "" && s.Provider != provider && s.Status != "done") {
		trusted := s != nil && s.Trusted
		s = newSession(token, provider, clientID)
		s.Trusted = trusted
		st.m[token] = s
	}
	if s.Provider == "" {
		s.Provider = provider
	}
	if s.Status == "waiting" {
		s.Status = "scanned"
	}
	if (provider == "spotify" || provider == "soundcloud") && s.Verifier == "" {
		s.Verifier = randomVerifier()
	}
	st.saveLocked()
	return *s, true
}

func (st *sessionStore) get(token string) (session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.m[token]
	if s == nil || s.expired(time.Now()) {
		return session{}, false
	}
	return *s, true
}

// update applies fn to a live session and persists it.
func (st *sessionStore) update(token string, fn func(*session)) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.m[token]
	if s == nil || s.expired(time.Now()) {
		return false
	}
	fn(s)
	st.saveLocked()
	return true
}

func (st *sessionStore) janitor() {
	for i := 0; ; i++ {
		time.Sleep(2 * time.Second)
		st.mu.Lock()
		if i%30 != 0 {
			st.flushLocked()
			st.mu.Unlock()
			continue
		}
		now := time.Now()
		changed := false
		for k, s := range st.m {
			if now.After(s.Expires.Add(tombstoneTTL)) {
				delete(st.m, k)
				changed = true
			} else if s.expired(now) && (s.SpotifyToken != nil || s.Deezer != nil || s.SoundCloud != nil || s.Verifier != "") {
				// Expired sessions only answer "expired"; drop credentials early.
				s.SpotifyToken, s.Deezer, s.SoundCloud, s.Verifier = nil, nil, nil, ""
				changed = true
			}
		}
		if changed {
			st.saveLocked()
		}
		st.flushLocked()
		st.mu.Unlock()
	}
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// completeDeezer commits the account only while its QR is live. The config
// callback runs under the same lock, so expiry/provider changes/trust upgrades
// cannot race global config synchronization or publish a partial success.
func (st *sessionStore) completeDeezer(token, userID, arl string, syncConfig func(bool) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.m[token]
	if s == nil || s.expired(time.Now()) || s.Provider != "deezer" || s.Status == "done" {
		return errors.New("Deezer 会话已过期或已完成，请重新扫码")
	}
	if err := syncConfig(s.Trusted); err != nil {
		return err
	}
	s.Deezer, s.Status = &deezerInfo{UserID: userID, ARL: arl}, "done"
	st.saveLocked()
	// Successful login credentials must survive an immediate service restart.
	st.flushLocked()
	return nil
}
