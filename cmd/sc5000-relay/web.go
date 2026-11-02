package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func wantsJSON(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == "application/json" || strings.Contains(r.Header.Get("Accept"), "application/json")
}

// formValues reads an urlencoded/multipart form or a JSON object body.
func formValues(w http.ResponseWriter, r *http.Request) url.Values {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		v := url.Values{}
		for k, x := range m {
			switch t := x.(type) {
			case string:
				v.Set(k, t)
			case float64:
				v.Set(k, strconv.FormatFloat(t, 'f', -1, 64))
			}
		}
		return v
	}
	if ct == "multipart/form-data" {
		_ = r.ParseMultipartForm(256 << 10)
	} else {
		_ = r.ParseForm()
	}
	return r.PostForm
}

func firstOf(v url.Values, keys ...string) string {
	for _, k := range keys {
		if s := strings.TrimSpace(v.Get(k)); s != "" {
			return s
		}
	}
	return ""
}

// ---- phone pages (token-protected, no password, no Origin checks) ----

func (s *server) handleMobileGet(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	token := r.URL.Query().Get("t")
	cfg := s.cfg.get()
	if !s.allowNewSession(r, token) {
		renderPage(w, 429, "errorPage", pageData{Title: "请求过于频繁", Message: "请稍后再试。"})
		return
	}
	sess, ok := s.sessions.open(token, provider, cfg.SpotifyClientID)
	if !ok {
		renderPage(w, 400, "errorPage", pageData{Title: "二维码无效或已过期", Message: "请在碟机上重新生成二维码后再扫码。"})
		return
	}
	if provider == "spotify" {
		if id := r.URL.Query().Get("client_id"); validClientID(id) && id != sess.SpotifyClientID && sess.Status != "done" {
			s.sessions.update(token, func(x *session) { x.SpotifyClientID = id })
			sess.SpotifyClientID = id
		}
		s.renderSpotify(w, r, sess, "")
		return
	}
	renderPage(w, 200, "deezerPage", pageData{Title: "连接 Deezer", Token: token, Done: sess.Status == "done", UserID: cfg.DeezerUserID})
}

func (s *server) handleMobilePost(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	token := r.URL.Query().Get("t")
	v := formValues(w, r)
	if token == "" {
		token = v.Get("t")
	}
	if !s.allowNewSession(r, token) {
		s.mobileResult(w, r, provider, token, 429, errors.New("请求过于频繁，请稍后再试"))
		return
	}
	sess, ok := s.sessions.open(token, provider, s.cfg.get().SpotifyClientID)
	if !ok {
		s.mobileResult(w, r, provider, token, 400, errors.New("二维码无效或已过期，请在碟机上重新生成二维码"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if provider == "deezer" {
		info, err := s.connectDeezer(ctx, firstOf(v, "user", "link", "url", "userId", "id"), firstOf(v, "arl"), sess.Trusted)
		if err == nil {
			s.sessions.update(token, func(x *session) { x.Deezer, x.Status = info, "done" })
		}
		s.mobileResult(w, r, provider, token, 200, err)
		return
	}
	code, redirect, err := parsePastedCode(firstOf(v, "code", "url", "callback"))
	if err == nil {
		err = s.finishSpotify(ctx, sess, code, redirect)
	}
	if err != nil && !wantsJSON(r) {
		s.renderSpotify(w, r, sess, err.Error())
		return
	}
	s.mobileResult(w, r, provider, token, 200, err)
}

// connectDeezer resolves the pasted link/ID and/or validates the ARL. For
// sessions the deck opened with the password (sync), the result is also
// mirrored into the server config used by /api/audio; anyone can mint a
// token, so untrusted sessions only carry the result back to their deck.
func (s *server) connectDeezer(ctx context.Context, user, arl string, sync bool) (*deezerInfo, error) {
	if user == "" && arl == "" {
		return nil, errors.New("请填写 Deezer 链接 / 用户 ID，或 ARL Cookie")
	}
	info := &deezerInfo{}
	var arlUser, arlName string
	if arl != "" {
		ses, err := s.deezer.login(ctx, arl)
		if err != nil {
			return nil, fmt.Errorf("%v（如只想用链接连接，请清空 ARL 输入框）", err)
		}
		info.ARL, arlUser, arlName = arl, ses.UserID, ses.UserName
	}
	if user != "" {
		id, err := s.deezer.resolveUser(ctx, user)
		if err != nil {
			return nil, err
		}
		info.UserID = id
	} else {
		info.UserID = arlUser
	}
	if err := s.deezer.userPlaylistsOK(ctx, info.UserID); err != nil {
		return nil, fmt.Errorf("Deezer 用户 %s 的歌单无法访问：%v", info.UserID, err)
	}
	if sync {
		if err := s.cfg.update(func(c *config) {
			c.DeezerUserID = info.UserID
			if info.ARL != "" {
				c.DeezerARL, c.DeezerARLUserName, c.DeezerARLChecked = info.ARL, arlName, time.Now().UTC()
			}
		}); err != nil {
			return nil, err
		}
	}
	log.Printf("deezer: connected user %s (arl: %v, synced to config: %v)", info.UserID, info.ARL != "", sync)
	return info, nil
}

func (s *server) mobileResult(w http.ResponseWriter, r *http.Request, provider, token string, code int, err error) {
	if err != nil && code == 200 {
		code = 400
	}
	if wantsJSON(r) {
		if err != nil {
			writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		sess, _ := s.sessions.get(token)
		writeJSON(w, 200, map[string]any{"ok": true, "status": sess.Status, "deezer": sess.Deezer, "spotify": sess.SpotifyToken != nil})
		return
	}
	if err != nil {
		renderPage(w, code, "deezerPage", pageData{Title: "连接 Deezer", Token: token, Error: err.Error(), UserID: s.cfg.get().DeezerUserID})
		return
	}
	renderPage(w, 200, "donePage", pageData{Title: "已完成连接", Provider: provider})
}

func (s *server) renderSpotify(w http.ResponseWriter, r *http.Request, sess session, errMsg string) {
	auto := s.spotify.useHTTPS(r.Context(), sess.SpotifyClientID)
	d := pageData{
		Title:         "连接 Spotify",
		Token:         sess.Token,
		Done:          sess.Status == "done",
		Error:         errMsg,
		ClientID:      sess.SpotifyClientID,
		Auto:          auto,
		HTTPSRedirect: s.spotify.httpsRedirect,
		AuthHTTPS:     s.spotify.authorizeURL(sess.SpotifyClientID, s.spotify.httpsRedirect, sess.Token, sess.Verifier),
		AuthLoopback:  s.spotify.authorizeURL(sess.SpotifyClientID, spotifyLoopbackURI, sess.Token, sess.Verifier),
	}
	code := 200
	if errMsg != "" {
		code = 400
	}
	renderPage(w, code, "spotifyPage", d)
}

func (s *server) finishSpotify(ctx context.Context, sess session, code, redirect string) error {
	if sess.Verifier == "" {
		return errors.New("会话缺少 PKCE 信息，请重新打开扫码页面")
	}
	tok, err := s.spotify.exchange(ctx, sess.SpotifyClientID, code, sess.Verifier, redirect)
	if err != nil {
		log.Printf("spotify: exchange failed: %v", err)
		return fmt.Errorf("Spotify 换取令牌失败：%v", err)
	}
	if redirect == s.spotify.httpsRedirect {
		s.spotify.learnHTTPS(sess.SpotifyClientID)
	}
	if !s.sessions.update(sess.Token, func(x *session) { x.SpotifyToken, x.Status = tok, "done" }) {
		return errors.New("会话已过期，请在碟机上重新生成二维码")
	}
	return nil
}

func (s *server) handleSpotifyCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	token := q.Get("state")
	sess, ok := s.sessions.get(token)
	if !ok {
		renderPage(w, 400, "errorPage", pageData{Title: "会话已过期", Message: "请在碟机上重新生成二维码后再扫码。"})
		return
	}
	if e := q.Get("error"); e != "" {
		s.renderSpotify(w, r, sess, "Spotify 拒绝了授权："+e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.finishSpotify(ctx, sess, q.Get("code"), s.spotify.httpsRedirect); err != nil {
		s.renderSpotify(w, r, sess, err.Error())
		return
	}
	renderPage(w, 200, "donePage", pageData{Title: "已完成连接", Provider: "spotify"})
}

// ---- password-protected API ----

func (s *server) configView() map[string]any {
	c := s.cfg.get()
	v := map[string]any{
		"backendUrl":        s.public,
		"deezerUserId":      c.DeezerUserID,
		"deezerArlSet":      c.DeezerARL != "",
		"spotifyClientId":   c.SpotifyClientID,
		"passwordIsDefault": c.PasswordIsDefault,
		"spotifyRedirect":   s.spotify.httpsRedirect,

		"soundcloudClientIdSet": c.SoundCloudClientID != "",
		"soundcloudTokenSet":    c.SoundCloudToken != "",
		"getSongBpmKeySet":      c.GetSongBPMKey != "",
	}
	if c.DeezerARL != "" {
		v["deezerArlMasked"] = maskSecret(c.DeezerARL)
		v["deezerArlUser"] = c.DeezerARLUserName
		v["deezerArlChecked"] = c.DeezerARLChecked
	}
	if !c.PasswordChangedAt.IsZero() {
		v["passwordChangedAt"] = c.PasswordChangedAt
	}
	return v
}

func (s *server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	view := s.configView()
	if r.URL.Query().Get("secrets") == "1" {
		c := s.cfg.get()
		view["deezerArl"] = c.DeezerARL
		view["soundcloudClientId"] = c.SoundCloudClientID
		view["soundcloudToken"] = c.SoundCloudToken
		view["getSongBpmKey"] = c.GetSongBPMKey
	}
	writeJSON(w, 200, map[string]any{"ok": true, "config": view})
}

func (s *server) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	v := formValues(w, r)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var msgs []string
	fail := func(code int, msg string) {
		writeJSON(w, code, map[string]any{"ok": false, "error": msg, "config": s.configView()})
	}
	if v.Get("clearArl") == "1" || v.Get("clearArl") == "true" {
		if err := s.cfg.update(func(c *config) { c.DeezerARL, c.DeezerARLUserName = "", "" }); err != nil {
			fail(500, err.Error())
			return
		}
		msgs = append(msgs, "已清除 ARL")
	}
	if arl := strings.TrimSpace(v.Get("deezerArl")); arl != "" {
		ses, err := s.deezer.login(ctx, arl)
		if err != nil {
			fail(400, err.Error())
			return
		}
		if err := s.cfg.update(func(c *config) {
			c.DeezerARL, c.DeezerARLUserName, c.DeezerARLChecked, c.DeezerUserID = arl, ses.UserName, time.Now().UTC(), ses.UserID
		}); err != nil {
			fail(500, err.Error())
			return
		}
		msgs = append(msgs, "ARL 有效，已登录 Deezer 用户 "+ses.UserID)
	}
	if u := strings.TrimSpace(v.Get("deezerUserId")); u != "" && u != s.cfg.get().DeezerUserID {
		id, err := s.deezer.resolveUser(ctx, u)
		if err != nil {
			fail(400, err.Error())
			return
		}
		if err := s.cfg.update(func(c *config) { c.DeezerUserID = id }); err != nil {
			fail(500, err.Error())
			return
		}
		msgs = append(msgs, "Deezer User ID 已设为 "+id)
	}
	if id := strings.TrimSpace(v.Get("spotifyClientId")); id != "" {
		if !validClientID(id) {
			fail(400, "Spotify Client ID 应为 32 位十六进制")
			return
		}
		if err := s.cfg.update(func(c *config) { c.SpotifyClientID = strings.ToLower(id) }); err != nil {
			fail(500, err.Error())
			return
		}
		msgs = append(msgs, "Spotify Client ID 已保存")
	}
	if v.Get("clearBpm") == "1" {
		if err := s.cfg.update(func(c *config) { c.SoundCloudClientID, c.SoundCloudToken, c.GetSongBPMKey = "", "", "" }); err != nil {
			fail(500, err.Error())
			return
		}
		msgs = append(msgs, "已清除 BPM 服务密钥")
	}
	cid, token, key := strings.TrimSpace(v.Get("soundcloudClientId")), strings.TrimSpace(v.Get("soundcloudToken")), strings.TrimSpace(v.Get("getSongBpmKey"))
	if cid+token+key != "" {
		if err := s.cfg.update(func(c *config) {
			if cid != "" {
				c.SoundCloudClientID = cid
			}
			if token != "" {
				c.SoundCloudToken = token
			}
			if key != "" {
				c.GetSongBPMKey = key
			}
		}); err != nil {
			fail(500, err.Error())
			return
		}
		msgs = append(msgs, "BPM 服务密钥已保存，SC5000 会在一小时内同步（重启 stream-sync 立即同步）")
	}
	if np := v.Get("newPassword"); np != "" {
		if !s.cfg.checkPassword(v.Get("currentPassword")) {
			fail(403, "当前密码不正确")
			return
		}
		if err := s.cfg.setPassword(np); err != nil {
			fail(400, err.Error())
			return
		}
		s.cfg.issueCookie(w, r) // old cookies are now invalid; keep this browser in
		msgs = append(msgs, "密码已修改（其它已登录设备需重新登录，SC5000 需填写新密码）")
		log.Printf("config: password changed from %s", clientIP(r))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "messages": msgs, "config": s.configView()})
}

// ---- web console ----

func (s *server) handlePanel(w http.ResponseWriter, r *http.Request) {
	// Cookie only: password guessing goes through /login and its limiter.
	c, err := r.Cookie(authCookie)
	if err != nil || !s.cfg.validCookie(c.Value) {
		renderPage(w, 200, "loginPage", pageData{Title: "SC5000 Deemix 登录"})
		return
	}
	renderPage(w, 200, "panelPage", pageData{Title: "SC5000 Deemix 控制台", Backend: s.public})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.fails.blocked(ip) {
		renderPage(w, 429, "loginPage", pageData{Title: "SC5000 Deemix 登录", Error: "失败次数过多，请 5 分钟后再试"})
		return
	}
	v := formValues(w, r)
	if !s.cfg.checkPassword(v.Get("password")) {
		s.fails.fail(ip)
		renderPage(w, 401, "loginPage", pageData{Title: "SC5000 Deemix 登录", Error: "密码不正确"})
		return
	}
	s.cfg.issueCookie(w, r)
	http.Redirect(w, r, "./", http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w)
	http.Redirect(w, r, "./", http.StatusSeeOther)
}
