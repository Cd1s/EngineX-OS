package main

// SoundCloud account login for the phone page. Like Spotify's, it can finish by a
// pasted loopback address, because the registered app redirects to 127.0.0.1.
// The tokens only pass through here on their way to the deck.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const soundCloudLoopback = "http://127.0.0.1:8765/callback"

// Overridden in tests.
var (
	soundCloudAuthorizeURL = "https://secure.soundcloud.com/authorize"
	soundCloudTokenURL     = "https://secure.soundcloud.com/oauth/token"
	soundCloudAPIURL       = "https://api.soundcloud.com"
)

func soundCloudAuthorize(clientID, redirect, state, verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "state": {state},
	}
	return soundCloudAuthorizeURL + "?" + q.Encode()
}

// soundCloudLogin exchanges the code and finds out whose account it is.
func soundCloudLogin(ctx context.Context, clientID, secret, code, verifier, redirect string) (*soundCloudInfo, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "code": {code}}
	var tok struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
	}
	// The token endpoint takes the app's credentials as Basic auth; the form is the
	// documented alternative for this grant.
	status, err := soundCloudPost(ctx, form, clientID, secret, &tok)
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusBadRequest) && tok.Access == "" {
		form.Set("client_id", clientID)
		form.Set("client_secret", secret)
		status, err = soundCloudPost(ctx, form, "", "", &tok)
	}
	if err != nil {
		return nil, errors.New("无法连接 SoundCloud，请重试")
	}
	if status != http.StatusOK || tok.Access == "" {
		return nil, fmt.Errorf("SoundCloud 拒绝了这次授权（HTTP %d），授权码只能用一次，请重新打开登录", status)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", soundCloudAPIURL+"/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "OAuth "+tok.Access)
	req.Header.Set("Accept", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.New("无法读取 SoundCloud 账号，请重试")
	}
	defer resp.Body.Close()
	var me struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&me); err != nil || resp.StatusCode != http.StatusOK || me.ID == 0 {
		return nil, errors.New("无法读取 SoundCloud 账号，请重试")
	}
	if tok.Expires <= 0 {
		tok.Expires = 3600
	}
	return &soundCloudInfo{UserID: me.ID, Username: me.Username, UserToken: tok.Access, RefreshToken: tok.Refresh, Expiry: time.Now().Add(time.Duration(tok.Expires) * time.Second)}, nil
}

func soundCloudPost(ctx context.Context, form url.Values, id, secret string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", soundCloudTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if id != "" {
		req.SetBasicAuth(id, secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// soundCloudCode reads a pasted callback address. A state that is not this QR code's
// means the address belongs to another login, so it is refused.
func soundCloudCode(pasted, token string) (string, string, error) {
	code, redirect, err := parsePastedCode(pasted)
	if err != nil {
		return "", "", err
	}
	if u, perr := url.Parse(strings.TrimSpace(pasted)); perr == nil {
		if state := u.Query().Get("state"); state != "" && state != token {
			return "", "", errors.New("这个地址属于另一次登录，请重新打开登录")
		}
	}
	return code, redirect, nil
}

func (s *server) finishSoundCloud(ctx context.Context, sess session, token, code, redirect string) error {
	c := s.cfg.get()
	if c.SoundCloudClientID == "" || c.SoundCloudSecret == "" {
		return errors.New("后台还没有配置 SoundCloud 应用")
	}
	info, err := soundCloudLogin(ctx, c.SoundCloudClientID, c.SoundCloudSecret, code, sess.Verifier, redirect)
	if err != nil {
		return err
	}
	s.sessions.update(token, func(x *session) { x.SoundCloud, x.Status = info, "done" })
	return nil
}

func (s *server) soundCloudPageData(sess session, token, message string) pageData {
	d := pageData{Title: "连接 SoundCloud", Token: token, Done: sess.Status == "done", Error: message}
	if id := s.cfg.get().SoundCloudClientID; id != "" {
		d.AuthLoopback = soundCloudAuthorize(id, soundCloudLoopback, token, sess.Verifier)
	}
	return d
}

// handleSoundCloudCallback serves an app whose redirect points at this service.
func (s *server) handleSoundCloudCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	token := q.Get("state")
	sess, ok := s.sessions.get(token)
	if !ok || sess.Provider != "soundcloud" {
		renderPage(w, 400, "errorPage", pageData{Title: "会话已过期", Message: "请在碟机上重新生成二维码。"})
		return
	}
	if e := q.Get("error"); e != "" {
		renderPage(w, 400, "soundcloudPage", s.soundCloudPageData(sess, token, "SoundCloud 拒绝了授权："+e))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.finishSoundCloud(ctx, sess, token, q.Get("code"), s.public+"/oauth/soundcloud/callback"); err != nil {
		renderPage(w, 400, "soundcloudPage", s.soundCloudPageData(sess, token, err.Error()))
		return
	}
	renderPage(w, 200, "donePage", pageData{Title: "已完成连接", Provider: "soundcloud"})
}
