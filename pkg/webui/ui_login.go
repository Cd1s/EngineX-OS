package webui

// Login from the deck's own screen. The deck shows a QR code: NetEase's goes to
// its app directly; the others have no such flow, so theirs opens the cloud
// relay's page (see relay.go), or without a relay a page on this deck
// (phone_pages.go). Either is guarded by a one-time token shown only on the deck.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
	qrcode "github.com/skip2/go-qrcode"
)

const (
	// Long enough for Spotify's email code and new-device page: at 10 minutes the
	// deck put up a new QR code meanwhile, and the pasted callback was refused (#48).
	loginLifetime           = 30 * time.Minute
	spotifyLoopbackRedirect = "http://127.0.0.1:8080/oauth/spotify/callback"
)

type loginFlow struct {
	Provider string `json:"provider"`
	Status   string `json:"status"` // waiting, scanned, done, expired
	token    string
	key      string // NetEase unikey
	Target   string `json:"target"` // what the QR code encodes
	session  string
	viaRelay bool
	expires  time.Time
	// spotifyState is the last authorization the phone page offered, for a pasted
	// address that lost its state.
	spotifyState string
}

// DefaultSpotifyClientID is this deck's registered public PKCE client.
const DefaultSpotifyClientID = "<redacted-spotify-client-id>"

// deezerOwner is the linked profile and, if given, the login cookie.
type deezerOwner struct{ UserID, ARL string }

func (s *Server) deezerOwnerPath() string { return filepath.Join(s.cfg.StateDir, "deezer-owner.json") }
func (s *Server) spotifyClientPath() string {
	return filepath.Join(s.cfg.StateDir, "spotify-client-id")
}

// spotifyClientID prefers the configured ID, then a saved one, then the default.
func (s *Server) spotifyClientID() string {
	if s.cfg.SpotifyClientID != "" {
		return s.cfg.SpotifyClientID
	}
	b, _ := os.ReadFile(s.spotifyClientPath())
	if id := strings.TrimSpace(string(b)); id != "" {
		return id
	}
	return DefaultSpotifyClientID
}

func (s *Server) savedDeezerOwner() deezerOwner {
	var v deezerOwner
	if b, err := os.ReadFile(s.deezerOwnerPath()); err == nil {
		json.Unmarshal(b, &v)
	}
	return v
}

// soundCloudOwner is the linked profile and the credentials that reach it.
type soundCloudOwner struct {
	UserID                            int64
	Username, ClientID, Secret, Token string
}

func (s *Server) soundCloudOwnerPath() string {
	return filepath.Join(s.cfg.StateDir, "soundcloud-owner.json")
}

// ownerSoundCloudProvider combines the linked profile with the app's credentials,
// wherever they were entered. All of them share one token store.
func (s *Server) ownerSoundCloudProvider() *providers.SoundCloud {
	var owner soundCloudOwner
	if b, err := os.ReadFile(s.soundCloudOwnerPath()); err == nil {
		json.Unmarshal(b, &owner)
	}
	if s.cfg.BPM != nil {
		keys := s.cfg.BPM.Config()
		if owner.ClientID == "" {
			owner.ClientID = keys.SoundCloudClientID
		}
		if owner.Secret == "" {
			owner.Secret = keys.SoundCloudSecret
		}
		if owner.Token == "" {
			owner.Token = keys.SoundCloudToken
		}
	}
	return s.soundCloud(&providers.SoundCloud{ClientID: owner.ClientID, Secret: owner.Secret, Token: owner.Token, Auth: s.soundCloudAuth, UserID: owner.UserID, Username: owner.Username})
}

// soundCloud points p at SoundCloud; tests serve all of it from one fake server.
func (s *Server) soundCloud(p *providers.SoundCloud) *providers.SoundCloud {
	p.BaseURL, p.WebURL, p.SiteURL, p.HTTP = s.cfg.SoundCloudURL, s.cfg.SoundCloudURL, s.cfg.SoundCloudURL, s.cfg.HTTP
	return p
}

// ownerSoundCloud returns the owner's SoundCloud access. Without any credentials
// it still reaches public profiles and search.
func (s *Server) ownerSoundCloud() *providers.SoundCloud {
	return s.ownerSoundCloudProvider()
}

// linkSoundCloudAccount saves an account login the relay completed. The tokens live
// with the app's in soundcloud-token.json, where they get renewed.
func (s *Server) linkSoundCloudAccount(session *Session, userID int64, username, token, refresh string, expiry time.Time) error {
	if userID == 0 || token == "" {
		return errRelayIncomplete
	}
	if err := s.soundCloudAuth.SetUser(token, refresh, expiry); err != nil {
		return err
	}
	current := s.ownerSoundCloudProvider()
	if err := providers.SaveJSON(s.soundCloudOwnerPath(), soundCloudOwner{userID, username, current.ClientID, current.Secret, ""}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := session.p.(*providers.SoundCloud); ok {
		p.ClientID, p.Secret, p.Token, p.UserID, p.Username = current.ClientID, current.Secret, "", userID, username
	}
	return nil
}

// linkSoundCloud checks and saves what the phone page or the relay collected. Binding
// a public profile or a pasted token replaces any account login.
func (s *Server) linkSoundCloud(ctx context.Context, session *Session, user, clientID, secret, token string) error {
	check := s.ownerSoundCloudProvider()
	check.Auth = &providers.SoundCloudAuth{TokenURL: s.cfg.SoundCloudTokenURL, Path: filepath.Join(s.cfg.StateDir, "soundcloud-token-check.json"), HTTP: s.cfg.HTTP}
	defer os.Remove(check.Auth.Path)
	if clientID != "" {
		check.ClientID = clientID
	}
	if secret != "" {
		check.Secret = secret
	}
	if token != "" {
		check.Token, check.UserID, check.Username = token, 0, ""
	}
	if user != "" {
		if err := check.ResolveUser(ctx, user); err != nil {
			return err
		}
	} else if err := check.Identify(ctx); err != nil {
		return err
	} else if check.UserID == 0 {
		if _, err := check.Search(ctx, "test"); err != nil {
			return err
		}
	}
	if err := providers.SaveJSON(s.soundCloudOwnerPath(), soundCloudOwner{check.UserID, check.Username, check.ClientID, check.Secret, check.Token}); err != nil {
		return err
	}
	if err := s.soundCloudAuth.SetUser("", "", time.Time{}); err != nil {
		return err
	}
	if s.cfg.BPM != nil {
		keys := s.cfg.BPM.Config()
		keys.SoundCloudClientID, keys.SoundCloudSecret, keys.SoundCloudToken = check.ClientID, check.Secret, check.Token
		if err := s.cfg.BPM.SaveConfig(keys); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := session.p.(*providers.SoundCloud); ok {
		p.ClientID, p.Secret, p.Token, p.UserID, p.Username = check.ClientID, check.Secret, check.Token, check.UserID, check.Username
	}
	return nil
}

// ownerBeatport returns the owner's Beatport login, if there is one.
func (s *Server) ownerBeatport() *providers.Beatport {
	if s.beatport.Ready() {
		return s.beatport
	}
	return nil
}

// linkBeatport checks a login before it replaces the one in p. Only the token is
// kept, never the password.
func (s *Server) linkBeatport(ctx context.Context, p *providers.Beatport, user, password, token string) error {
	check := &providers.Beatport{BaseURL: s.cfg.BeatportURL, HTTP: s.cfg.HTTP}
	var err error
	if token != "" {
		var t providers.BeatportToken
		if t, err = providers.ParseBeatportToken(token); err == nil {
			err = check.SetToken(t)
		}
	} else {
		err = check.Login(ctx, user, password)
	}
	if err == nil {
		_, err = check.Playlists(ctx)
	}
	if err != nil {
		return err
	}
	return p.SetToken(check.Token())
}

// ownerSpotify returns the owner's Spotify login, if there is one.
func (s *Server) ownerSpotify() *providers.Spotify {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ownerDeleted["spotify"] {
		return nil
	}
	var p providers.Provider
	for _, v := range s.sessions {
		if !v.Guest && v.Provider == "spotify" {
			p = v.p
		}
	}
	if p == nil {
		// Kept so that lookups share one token and its refreshes.
		made, err := s.makeProvider("spotify", false, "")
		if err != nil || !ready(made) {
			return nil
		}
		id := providers.RandomID()
		s.sessions[id] = &Session{ID: id, Provider: "spotify", Ready: true, p: made, tracks: map[string]providers.Track{}, expires: time.Now().Add(8 * time.Hour)}
		p = made
	}
	if sp, ok := p.(*providers.Spotify); ok && ready(sp) {
		return sp
	}
	return nil
}

// neteaseFor picks the login a session may fetch audio with. The owner's cookie
// serves the owner's sessions; a guest only ever uses a login of their own.
func (s *Server) neteaseFor(session string, guest bool) *providers.Netease {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p providers.Provider
	if guest {
		if v := s.sessions[session]; v != nil {
			p = v.p
		}
	} else {
		for _, v := range s.sessions {
			if !v.Guest && v.Provider == "netease" {
				p = v.p
			}
		}
		if p == nil && !s.ownerDeleted["netease"] {
			p, _ = s.makeProvider("netease", false, "")
		}
	}
	if n, ok := p.(*providers.Netease); ok && ready(n) {
		return n
	}
	return nil
}

func lanHost() string {
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
				return n.IP.String()
			}
		}
	}
	return "sc5000.lan"
}

// ownerSession returns the single owner session of a service, creating it if needed.
func (s *Server) ownerSession(provider string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	delete(s.ownerDeleted, provider)
	for _, v := range s.sessions {
		if !v.Guest && v.Provider == provider {
			return v, nil
		}
	}
	if len(s.sessions) >= 16 {
		return nil, errors.New("too many sessions")
	}
	p, err := s.makeProvider(provider, false, "")
	if err != nil {
		return nil, err
	}
	v := &Session{ID: providers.RandomID(), Provider: provider, Ready: ready(p), p: p, tracks: map[string]providers.Track{}, expires: time.Now().Add(8 * time.Hour)}
	s.sessions[v.ID] = v
	return v, nil
}

func (s *Server) loginStart(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	if !slices.Contains(loginServices, provider) {
		fail(w, 400, errors.New("unsupported provider"))
		return
	}
	session, err := s.ownerSession(provider)
	if err != nil {
		fail(w, 500, err)
		return
	}
	flow := &loginFlow{Provider: provider, Status: "waiting", token: providers.RandomID(), session: session.ID, expires: time.Now().Add(loginLifetime)}
	if n, ok := session.p.(*providers.Netease); ok {
		q, err := n.BeginQR(r.Context())
		if err != nil {
			fail(w, 502, err)
			return
		}
		flow.key, flow.Target = q.Key, q.URL
	} else if base, _ := s.relaySettings(); base != "" {
		// The relay opens a session by itself when a phone arrives first, so a
		// failed registration here does not stop the QR code from working.
		flow.viaRelay = true
		s.relayRegister(r.Context(), flow)
		flow.Target = s.relayPage(flow)
	} else {
		s.uiMu.Lock()
		lang := s.loadUI().Language
		s.uiMu.Unlock()
		flow.Target = "http://" + lanHost() + ":8080/m/" + provider + "?t=" + url.QueryEscape(flow.token) + "&lang=" + url.QueryEscape(lang)
	}
	s.mu.Lock()
	s.logins[provider] = flow
	s.mu.Unlock()
	respond(w, flow)
}

func (s *Server) loginFlow(provider string) *loginFlow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins[provider]
}

// loginStatus is polled by the deck while the QR code is on screen.
func (s *Server) loginStatus(w http.ResponseWriter, r *http.Request) {
	flow := s.loginFlow(r.URL.Query().Get("provider"))
	if flow == nil {
		fail(w, 404, errors.New("no login in progress"))
		return
	}
	s.mu.Lock()
	session := s.sessions[flow.session]
	status, key, token, viaRelay := flow.Status, flow.key, flow.token, flow.viaRelay
	expired := time.Now().After(flow.expires)
	s.mu.Unlock()
	switch {
	case session == nil || expired:
		status = "expired"
	case key != "":
		code, err := session.p.(*providers.Netease).PollQR(r.Context(), key)
		if err != nil {
			log.Printf("stream-sync: NetEase QR check failed: %v", err)
			fail(w, 502, err)
			return
		}
		previous := status
		status = map[int]string{800: "expired", 801: "waiting", 802: "scanned", 803: "done"}[code]
		if status == "" {
			status = "waiting"
		}
		if status != previous {
			log.Printf("stream-sync: NetEase QR code %d (%s)", code, status)
		}
	case viaRelay && status != "done" && status != "linked":
		status = s.relayStatus(r.Context(), session, token, status)
	case ready(session.p):
		// A phone page finished. A deck already logged in reaches this too, which
		// is why the deck only offers the QR code after asking for a new login.
		if status == "linked" {
			status = "done"
		}
	}
	s.mu.Lock()
	flow.Status = status
	if status == "done" && session != nil {
		session.Ready = true
		flow.token = ""
	}
	s.mu.Unlock()
	respond(w, flow)
}

func (s *Server) loginQR(w http.ResponseWriter, r *http.Request) {
	flow := s.loginFlow(r.URL.Query().Get("provider"))
	if flow == nil {
		fail(w, 404, errors.New("no login in progress"))
		return
	}
	png, err := qrcode.Encode(flow.Target, qrcode.Medium, 640)
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

// phoneFlow checks the one-time token from the QR code.
func (s *Server) phoneFlow(w http.ResponseWriter, r *http.Request) *loginFlow {
	flow := s.loginFlow(r.PathValue("provider"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if flow == nil || flow.token == "" || time.Now().After(flow.expires) || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(flow.token)) != 1 {
		lang := phoneLang(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(403)
		phoneMessage(w, r, nil, tr(lang, "expired"), tr(lang, "expiredHint"))
		return nil
	}
	return flow
}

func (s *Server) phoneGet(w http.ResponseWriter, r *http.Request) {
	flow := s.phoneFlow(w, r)
	if flow == nil {
		return
	}
	lang, action := phoneLang(r), phoneAction(r, flow)
	switch flow.Provider {
	case "deezer":
		phonePage(w, r, deezerPhonePage(lang, action))
	case "soundcloud":
		phonePage(w, r, soundCloudPhonePage(lang, action))
	case "beatport":
		phonePage(w, r, beatportPhonePage(lang, action))
	case "spotify":
		session := s.sessionOf(flow)
		sp, ok := session.p.(*providers.Spotify)
		if !ok {
			fail(w, 500, errors.New("invalid login session"))
			return
		}
		sp.ClientID = s.spotifyClientID()
		state := providers.RandomID()
		auth, verifier := sp.BeginPKCE(state, spotifyLoopbackRedirect)
		s.mu.Lock()
		s.prune()
		s.pending[state] = pending{session.ID, verifier, spotifyLoopbackRedirect, time.Now().Add(loginLifetime)}
		flow.spotifyState = state
		s.mu.Unlock()
		phonePage(w, r, spotifyPhonePage(lang, action, auth))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) sessionOf(flow *loginFlow) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[flow.session]
}

func (s *Server) saveDeezerOwner(p *providers.Deezer, owner deezerOwner) error {
	if err := providers.SaveJSON(s.deezerOwnerPath(), owner); err != nil {
		return err
	}
	s.mu.Lock()
	p.UserID, p.ARL = owner.UserID, owner.ARL
	s.mu.Unlock()
	return nil
}

func (s *Server) finishPhoneLogin(w http.ResponseWriter, r *http.Request, flow *loginFlow) {
	s.mu.Lock()
	flow.Status, flow.token = "linked", ""
	s.mu.Unlock()
	lang := phoneLang(r)
	phoneMessage(w, r, nil, tr(lang, "done"), tr(lang, "doneHint"))
}

// spotifyPaste finds the code and state in what was pasted: the whole address,
// one without "http://", or the address amid other copied text.
func spotifyPaste(in string) (code, state string) {
	for _, f := range strings.Fields(in) {
		if _, query, ok := strings.Cut(f, "?"); ok || strings.HasPrefix(f, "code=") {
			if !ok {
				query = f
			}
			query, _, _ = strings.Cut(query, "#")
			if q, err := url.ParseQuery(query); err == nil && q.Get("code") != "" {
				return q.Get("code"), q.Get("state")
			}
		}
	}
	return "", ""
}

func (s *Server) phonePost(w http.ResponseWriter, r *http.Request) {
	flow := s.phoneFlow(w, r)
	if flow == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		fail(w, 400, errors.New("invalid form"))
		return
	}
	session := s.sessionOf(flow)
	if session == nil {
		fail(w, 400, errors.New("login session is gone"))
		return
	}
	lang := phoneLang(r)
	refuse := func(title, hint string) { phoneMessage(w, r, flow, tr(lang, title), hint) }
	switch p := session.p.(type) {
	case *providers.Deezer:
		// The relay URL and password are not settable here: holding the QR
		// token is not owning the deck (#8). The console and deck UI set them.
		if strings.TrimSpace(r.Form.Get("user")+r.Form.Get("arl")) == "" {
			refuse("badForm", "")
			return
		}
		check := &providers.Deezer{BaseURL: s.cfg.DeezerURL, GatewayURL: s.cfg.DeezerGatewayURL, HTTP: s.cfg.HTTP}
		arl := strings.TrimSpace(r.Form.Get("arl"))
		var user string
		var err error
		if arl != "" {
			// The ARL itself is the check: it also serves Deemix downloads, which
			// need no public playlists.
			if user, err = check.IdentifyARL(r.Context(), arl); err != nil {
				refuse("deezerBadARL", "")
				return
			}
		} else {
			if user, err = check.ResolveUser(r.Context(), r.Form.Get("user")); err != nil {
				refuse("deezerBadUser", "")
				return
			}
			check.UserID = user
			if _, err = check.Playlists(r.Context()); err != nil {
				refuse("deezerNoLists", "")
				return
			}
		}
		// Linking the same profile again by its link keeps the saved login.
		if old := s.savedDeezerOwner(); arl == "" && old.UserID == user {
			arl = old.ARL
		}
		if err = s.saveDeezerOwner(p, deezerOwner{user, arl}); err != nil {
			fail(w, 500, errors.New("could not save the Deezer login"))
			return
		}
		s.finishPhoneLogin(w, r, flow)
	case *providers.SoundCloud:
		user, token := strings.TrimSpace(r.Form.Get("user")), strings.TrimSpace(r.Form.Get("token"))
		clientID, secret := strings.TrimSpace(r.Form.Get("clientId")), strings.TrimSpace(r.Form.Get("clientSecret"))
		if user+token+clientID == "" {
			refuse("badForm", "")
			return
		}
		if err := s.linkSoundCloud(r.Context(), session, user, clientID, secret, token); err != nil {
			log.Printf("stream-sync: SoundCloud link failed: %v", err)
			refuse("scFailed", tr(lang, "scFailedHint"))
			return
		}
		s.finishPhoneLogin(w, r, flow)
	case *providers.Beatport:
		if err := s.linkBeatport(r.Context(), p, strings.TrimSpace(r.Form.Get("user")), r.Form.Get("password"), strings.TrimSpace(r.Form.Get("token"))); err != nil {
			refuse("bpFailed", tr(lang, "bpFailedHint"))
			return
		}
		s.finishPhoneLogin(w, r, flow)
	case *providers.Spotify:
		code, state := spotifyPaste(r.Form.Get("callback"))
		if code == "" {
			refuse("spBadPaste", tr(lang, "spHelp"))
			return
		}
		s.mu.Lock()
		if state == "" {
			state = flow.spotifyState
		}
		pend, ok := s.pending[state]
		delete(s.pending, state)
		s.mu.Unlock()
		if !ok || pend.session != session.ID {
			refuse("spOtherLogin", "")
			return
		}
		if err := p.CompletePKCE(r.Context(), code, pend.verifier, pend.redirect); err != nil {
			log.Printf("stream-sync: Spotify refused the login: %v", err)
			refuse("spRefused", "")
			return
		}
		s.finishPhoneLogin(w, r, flow)
	default:
		http.NotFound(w, r)
	}
}
