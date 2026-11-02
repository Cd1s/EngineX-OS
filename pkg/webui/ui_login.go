package webui

// Login from the deck's own screen. The deck shows a QR code: NetEase's goes to
// its app directly; Spotify and Deezer have no such flow, so theirs opens the
// cloud relay's page (see relay.go), or without a relay a small page on this deck.
// Either is guarded by a one-time token shown only on the deck's screen.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
	qrcode "github.com/skip2/go-qrcode"
)

const (
	loginLifetime           = 10 * time.Minute
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
	UserID                    int64
	Username, ClientID, Token string
}

func (s *Server) soundCloudOwnerPath() string {
	return filepath.Join(s.cfg.StateDir, "soundcloud-owner.json")
}

// ownerSoundCloudProvider combines the linked profile with any credentials that
// were entered for BPM lookups.
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
		if owner.Token == "" {
			owner.Token = keys.SoundCloudToken
		}
	}
	return &providers.SoundCloud{BaseURL: s.cfg.SoundCloudURL, HTTP: s.cfg.HTTP, ClientID: owner.ClientID, Token: owner.Token, UserID: owner.UserID, Username: owner.Username}
}

// ownerSoundCloud returns the owner's SoundCloud access, if there is any.
func (s *Server) ownerSoundCloud() *providers.SoundCloud {
	if p := s.ownerSoundCloudProvider(); ready(p) {
		return p
	}
	return nil
}

// linkSoundCloud checks and saves what the phone page or the relay collected.
func (s *Server) linkSoundCloud(ctx context.Context, session *Session, user, clientID, token string) error {
	current := s.ownerSoundCloudProvider()
	check := &providers.SoundCloud{BaseURL: s.cfg.SoundCloudURL, HTTP: s.cfg.HTTP, ClientID: current.ClientID, Token: current.Token, UserID: current.UserID, Username: current.Username}
	if clientID != "" {
		check.ClientID = clientID
	}
	if token != "" {
		check.Token, check.UserID, check.Username = token, 0, ""
	}
	if !ready(check) {
		return errors.New("soundcloud: a Client ID or OAuth token is required")
	}
	if user != "" {
		if err := check.ResolveUser(ctx, user); err != nil {
			return err
		}
	} else if err := check.Identify(ctx); err != nil {
		return err
	} else if _, err := check.Search(ctx, "test"); err != nil && check.UserID == 0 {
		return err
	}
	if err := providers.SaveJSON(s.soundCloudOwnerPath(), soundCloudOwner{check.UserID, check.Username, check.ClientID, check.Token}); err != nil {
		return err
	}
	if s.cfg.BPM != nil {
		keys := s.cfg.BPM.Config()
		keys.SoundCloudClientID, keys.SoundCloudToken = check.ClientID, check.Token
		if err := s.cfg.BPM.SaveConfig(keys); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := session.p.(*providers.SoundCloud); ok {
		p.ClientID, p.Token, p.UserID, p.Username = check.ClientID, check.Token, check.UserID, check.Username
	}
	return nil
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
		flow.Target = "http://" + lanHost() + ":8080/m/" + provider + "?t=" + flow.token
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
			fail(w, 502, err)
			return
		}
		status = map[int]string{800: "expired", 801: "waiting", 802: "scanned", 803: "done"}[code]
		if status == "" {
			status = "waiting"
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

func phonePage(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>SC5000</title><style>body{background:#111;color:#eee;font:18px system-ui,sans-serif;padding:24px;max-width:560px;margin:auto}input,button,a.b{display:block;width:100%;box-sizing:border-box;font:inherit;padding:14px;margin:12px 0;border-radius:10px;border:1px solid #444;background:#1c1c1c;color:#eee;text-align:center;text-decoration:none}button,a.b{background:#2a6;border:0;color:#fff}small{color:#aaa}</style>` + body))
}

// phoneFlow checks the one-time token from the QR code.
func (s *Server) phoneFlow(w http.ResponseWriter, r *http.Request) *loginFlow {
	flow := s.loginFlow(r.PathValue("provider"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if flow == nil || flow.token == "" || time.Now().After(flow.expires) || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(flow.token)) != 1 {
		w.WriteHeader(403)
		phonePage(w, "<h2>二维码已失效</h2><p>请在碟机屏幕上重新打开二维码。</p>")
		return nil
	}
	return flow
}

func (s *Server) phoneGet(w http.ResponseWriter, r *http.Request) {
	flow := s.phoneFlow(w, r)
	if flow == nil {
		return
	}
	action := "/m/" + flow.Provider + "?t=" + url.QueryEscape(flow.token)
	switch flow.Provider {
	case "deezer":
		phonePage(w, `<h2>连接 Deezer</h2><p>填写资料页或分享链接后，可在碟机上浏览你的公开歌单；也可以填写 arl Cookie，碟机会用它确认账号。云端登录服务的地址和密码也可以在这里改，只想改这两项时其余留空。</p><form method="post" action="`+html.EscapeString(action)+`"><input name="user" placeholder="分享链接、资料页地址或用户 ID"><input name="arl" placeholder="arl Cookie（可选）" autocomplete="off"><input name="relayUrl" value="`+html.EscapeString(s.relayStatusView().RelayURL)+`" placeholder="云端登录服务地址" autocomplete="off"><input name="deemixPassword" type="password" placeholder="云端服务新密码（留空不改）" autocomplete="off"><button>连接</button></form>`)
	case "soundcloud":
		phonePage(w, `<h2>连接 SoundCloud</h2><p>主页用来浏览喜欢的曲目和歌单。至少填写 Client ID 或 OAuth Token 之一；填 OAuth Token 还可以看私密内容。只播放账号本身可以完整播放的内容。</p><form method="post" action="`+html.EscapeString(action)+`"><input name="user" placeholder="主页链接或用户名（可选）" autocomplete="off"><input name="clientId" placeholder="Client ID" autocomplete="off"><input name="token" type="password" placeholder="OAuth Token（可选）" autocomplete="off"><button>连接</button></form>`)
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
		s.mu.Unlock()
		phonePage(w, `<h2>连接 Spotify</h2><p>1. 打开授权页并同意。</p><a class="b" target="_blank" rel="noreferrer" href="`+html.EscapeString(auth)+`">打开 Spotify 授权</a><p>2. 同意后手机会显示“无法打开页面”，这是正常的。复制地址栏里以 127.0.0.1 开头的完整地址，粘贴到下面。</p><form method="post" action="`+html.EscapeString(action)+`"><input name="callback" placeholder="http://127.0.0.1:8080/oauth/spotify/callback?code=..." required autocomplete="off"><button>完成登录</button></form>`)
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

func (s *Server) finishPhoneLogin(w http.ResponseWriter, flow *loginFlow) {
	s.mu.Lock()
	flow.Status, flow.token = "linked", ""
	s.mu.Unlock()
	phonePage(w, "<h2>已完成</h2><p>可以回到碟机了，屏幕上的二维码会自动关闭。</p>")
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
	retry := `<p><a class="b" href="/m/` + flow.Provider + `?t=` + url.QueryEscape(flow.token) + `">重试</a></p>`
	switch p := session.p.(type) {
	case *providers.Deezer:
		relay, password := strings.TrimSpace(r.Form.Get("relayUrl")), r.Form.Get("deemixPassword")
		if relay != "" && !validRelayURL(relay) {
			phonePage(w, "<h2>地址必须是 https，或内网 IP 的 http</h2>"+retry)
			return
		}
		if err := s.updateRelayConfig(optional(relay), optional(password)); err != nil {
			fail(w, 500, errors.New("could not save the relay settings"))
			return
		}
		if strings.TrimSpace(r.Form.Get("user")+r.Form.Get("arl")) == "" {
			s.finishPhoneLogin(w, flow)
			return
		}
		check := &providers.Deezer{BaseURL: s.cfg.DeezerURL, GatewayURL: s.cfg.DeezerGatewayURL, HTTP: s.cfg.HTTP}
		arl := strings.TrimSpace(r.Form.Get("arl"))
		var user string
		var err error
		if arl != "" {
			if user, err = check.IdentifyARL(r.Context(), arl); err != nil {
				phonePage(w, "<h2>这个 arl 无效或已过期</h2>"+retry)
				return
			}
		} else if user, err = check.ResolveUser(r.Context(), r.Form.Get("user")); err != nil {
			phonePage(w, "<h2>没有识别出用户 ID</h2>"+retry)
			return
		}
		check.UserID = user
		if _, err = check.Playlists(r.Context()); err != nil {
			phonePage(w, "<h2>无法读取这个资料页的公开歌单</h2>"+retry)
			return
		}
		// Linking the same profile again by its link keeps the saved login.
		if old := s.savedDeezerOwner(); arl == "" && old.UserID == user {
			arl = old.ARL
		}
		if err = s.saveDeezerOwner(p, deezerOwner{user, arl}); err != nil {
			fail(w, 500, errors.New("could not save the Deezer login"))
			return
		}
		s.finishPhoneLogin(w, flow)
	case *providers.SoundCloud:
		if err := s.linkSoundCloud(r.Context(), session, strings.TrimSpace(r.Form.Get("user")), strings.TrimSpace(r.Form.Get("clientId")), strings.TrimSpace(r.Form.Get("token"))); err != nil {
			phonePage(w, "<h2>无法连接 SoundCloud</h2><p>请检查主页和凭据。</p>"+retry)
			return
		}
		s.finishPhoneLogin(w, flow)
	case *providers.Spotify:
		u, err := url.Parse(strings.TrimSpace(r.Form.Get("callback")))
		if err != nil {
			phonePage(w, "<h2>地址不正确</h2>"+retry)
			return
		}
		s.mu.Lock()
		pend, ok := s.pending[u.Query().Get("state")]
		delete(s.pending, u.Query().Get("state"))
		s.mu.Unlock()
		if !ok || pend.session != session.ID || u.Query().Get("code") == "" {
			phonePage(w, "<h2>地址不正确或已过期</h2>"+retry)
			return
		}
		if err := p.CompletePKCE(r.Context(), u.Query().Get("code"), pend.verifier, pend.redirect); err != nil {
			phonePage(w, "<h2>Spotify 拒绝了这次登录</h2>"+retry)
			return
		}
		s.finishPhoneLogin(w, flow)
	default:
		http.NotFound(w, r)
	}
}
