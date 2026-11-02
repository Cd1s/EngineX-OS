package webui

// Login from the deck's own screen. The deck shows a QR code: NetEase's goes to
// its app directly; Spotify and Deezer have no such flow, so theirs opens a small
// page on this deck, guarded by a one-time token shown only on the deck's screen.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	target   string // what the QR code encodes
	session  string
	expires  time.Time
}

var deezerUser = regexp.MustCompile(`(?:^|/profile/)(\d{1,20})(?:[/?#]|$)`)

func (s *Server) deezerOwnerPath() string { return filepath.Join(s.cfg.StateDir, "deezer-owner.json") }
func (s *Server) spotifyClientPath() string {
	return filepath.Join(s.cfg.StateDir, "spotify-client-id")
}

// spotifyClientID prefers the configured ID, then the one saved from the phone page.
func (s *Server) spotifyClientID() string {
	if s.cfg.SpotifyClientID != "" {
		return s.cfg.SpotifyClientID
	}
	b, _ := os.ReadFile(s.spotifyClientPath())
	return strings.TrimSpace(string(b))
}

// savedDeezerUser is the public profile the owner linked on the phone page.
func (s *Server) savedDeezerUser() string {
	b, err := os.ReadFile(s.deezerOwnerPath())
	if err != nil {
		return ""
	}
	var v struct{ UserID string }
	json.Unmarshal(b, &v)
	return v.UserID
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
	if !slices.Contains(uiServices, provider) {
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
		flow.key, flow.target = q.Key, q.URL
	} else {
		flow.target = "http://" + lanHost() + ":8080/m/" + provider + "?t=" + flow.token
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
	status, key := flow.Status, flow.key
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
	png, err := qrcode.Encode(flow.target, qrcode.Medium, 640)
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
		phonePage(w, `<h2>连接 Deezer</h2><p>Deezer 已不再向新应用开放登录授权，这里连接的是公开资料页：填写后可在碟机上浏览你的公开歌单。</p><form method="post" action="`+html.EscapeString(action)+`"><input name="user" placeholder="https://www.deezer.com/profile/数字 或用户 ID" required><button>连接</button></form>`)
	case "spotify":
		if s.spotifyClientID() == "" {
			phonePage(w, `<h2>连接 Spotify</h2><p>Spotify 要求每个设备使用自己注册的应用。请到 developer.spotify.com 创建应用，Redirect URI 填写 <code>`+spotifyLoopbackRedirect+`</code>，再把 Client ID 填到这里。</p><form method="post" action="`+html.EscapeString(action)+`"><input name="client" placeholder="Client ID" required autocomplete="off"><button>下一步</button></form>`)
			return
		}
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
		m := deezerUser.FindStringSubmatch(strings.TrimSpace(r.Form.Get("user")))
		if m == nil {
			phonePage(w, "<h2>没有识别出用户 ID</h2>"+retry)
			return
		}
		check := &providers.Deezer{BaseURL: s.cfg.DeezerURL, UserID: m[1], HTTP: s.cfg.HTTP}
		if _, err := check.Playlists(r.Context()); err != nil {
			phonePage(w, "<h2>无法读取这个资料页的公开歌单</h2>"+retry)
			return
		}
		if err := providers.SaveJSON(s.deezerOwnerPath(), struct{ UserID string }{m[1]}); err != nil {
			fail(w, 500, err)
			return
		}
		s.mu.Lock()
		p.UserID = m[1]
		s.mu.Unlock()
		s.finishPhoneLogin(w, flow)
	case *providers.Spotify:
		if client := strings.TrimSpace(r.Form.Get("client")); client != "" {
			if err := os.WriteFile(s.spotifyClientPath(), []byte(client+"\n"), 0o600); err != nil {
				fail(w, 500, err)
				return
			}
			http.Redirect(w, r, "/m/spotify?t="+url.QueryEscape(flow.token), http.StatusSeeOther)
			return
		}
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
