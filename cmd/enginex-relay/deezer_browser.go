package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const deezerBrowserIdle = 5 * time.Minute
const deezerLoginURL = "https://www.deezer.com/login"

// One browser-level connection owns one ephemeral context. CDP commands and
// HTTP operations are serialized so tokens can never share a page or cookies.
type deezerBrowser struct {
	mu       sync.Mutex
	endpoint string
	idle     time.Duration
	active   *deezerBrowserSession
	closed   bool
}

type deezerBrowserSession struct {
	token, contextID, sessionID string
	rpc                         *deezerCDP
	expires                     time.Time
	timer                       *time.Timer
}

type deezerCDP struct {
	conn   *websocket.Conn
	nextID int
}

func newDeezerBrowser(endpoint string) *deezerBrowser {
	return &deezerBrowser{endpoint: endpoint, idle: deezerBrowserIdle}
}

// The caller holds the browser mutex; events may be interleaved with replies.
func (c *deezerCDP) call(ctx context.Context, sessionID, method string, params, out any) error {
	deadline := time.Now().Add(12 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.conn.SetWriteDeadline(deadline)
	c.conn.SetReadDeadline(deadline)
	c.nextID++
	id := c.nextID
	req := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		req["sessionId"] = sessionID
	}
	if err := c.conn.WriteJSON(req); err != nil {
		return err
	}
	for {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := c.conn.ReadJSON(&reply); err != nil {
			return err
		}
		if reply.ID != id {
			continue
		}
		// Do not propagate arbitrary CDP messages, which may contain account data.
		if reply.Error != nil {
			return fmt.Errorf("CDP %s failed (%d)", method, reply.Error.Code)
		}
		if out != nil {
			return json.Unmarshal(reply.Result, out)
		}
		return nil
	}
}

func (b *deezerBrowser) startLocked(ctx context.Context, sess session) error {
	req, err := http.NewRequestWithContext(ctx, "GET", b.endpoint+"/json/version", nil)
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	var version struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&version)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || version.URL == "" {
		return errors.New("Chrome CDP unavailable")
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, version.URL, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(8 << 20)
	a := &deezerBrowserSession{token: sess.Token, rpc: &deezerCDP{conn: conn}}
	b.active = a
	success := false
	defer func() {
		if !success {
			b.disposeLocked()
		}
	}()
	var bc struct {
		ID string `json:"browserContextId"`
	}
	if err = a.rpc.call(ctx, "", "Target.createBrowserContext", map[string]any{"disposeOnDetach": true}, &bc); err != nil {
		return err
	}
	a.contextID = bc.ID
	if a.contextID == "" {
		return errors.New("missing browser context")
	}
	var target struct {
		ID string `json:"targetId"`
	}
	// Apply the mobile viewport before the first navigation.
	if err = a.rpc.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": a.contextID}, &target); err != nil {
		return err
	}
	var attached struct {
		ID string `json:"sessionId"`
	}
	if err = a.rpc.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.ID, "flatten": true}, &attached); err != nil {
		return err
	}
	a.sessionID = attached.ID
	if a.sessionID == "" {
		return errors.New("missing page session")
	}
	if err = a.rpc.call(ctx, a.sessionID, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 680, "deviceScaleFactor": 1, "mobile": true}, nil); err != nil {
		return err
	}
	if err = a.rpc.call(ctx, a.sessionID, "Page.navigate", map[string]any{"url": deezerLoginURL}, nil); err != nil {
		return err
	}
	b.touchLocked(sess.Expires)
	success = true
	return nil
}

func (b *deezerBrowser) touchLocked(sessionExpires time.Time) {
	a := b.active
	if a.timer != nil {
		a.timer.Stop()
	}
	a.expires = time.Now().Add(b.idle)
	if sessionExpires.Before(a.expires) {
		a.expires = sessionExpires
	}
	a.timer = time.AfterFunc(time.Until(a.expires), func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		// A previously fired timer must not destroy a refreshed or replaced session.
		if b.active == a && !time.Now().Before(a.expires) {
			b.disposeLocked()
		}
	})
}

func (b *deezerBrowser) disposeLocked() {
	a := b.active
	if a == nil {
		return
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	if a.contextID != "" {
		// Cleanup must survive request cancellation. disposeOnDetach also removes
		// the context on connection loss, service termination or failed disposal.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.rpc.call(ctx, "", "Target.disposeBrowserContext", map[string]any{"browserContextId": a.contextID}, nil)
		cancel()
	}
	a.rpc.conn.Close()
	b.active = nil
}

func (b *deezerBrowser) closeToken(token string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != nil && b.active.token == token {
		b.disposeLocked()
	}
}

func (b *deezerBrowser) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.disposeLocked()
}

type deezerBrowserAction struct {
	Type string  `json:"type"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	DY   float64 `json:"dy"`
	Text string  `json:"text"`
	Key  string  `json:"key"`
}

func (a deezerBrowserAction) valid() bool {
	finite := func(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
	switch a.Type {
	case "start", "stop":
		return true
	case "click":
		return finite(a.X) && finite(a.Y) && a.X >= 0 && a.X < 390 && a.Y >= 0 && a.Y < 680
	case "wheel":
		return finite(a.DY) && a.DY != 0 && math.Abs(a.DY) <= 2000
	case "text":
		return a.Text != "" && len(a.Text) <= 4096
	case "key":
		return a.Key == "Enter" || a.Key == "Tab" || a.Key == "Backspace"
	}
	return false
}

func (a *deezerBrowserSession) act(ctx context.Context, v deezerBrowserAction) error {
	call := func(method string, p any) error { return a.rpc.call(ctx, a.sessionID, method, p, nil) }
	switch v.Type {
	case "click":
		for _, typ := range []string{"mousePressed", "mouseReleased"} {
			if err := call("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": v.X, "y": v.Y, "button": "left", "clickCount": 1}); err != nil {
				return err
			}
		}
	case "wheel":
		return call("Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": 195, "y": 340, "deltaX": 0, "deltaY": v.DY})
	case "text":
		return call("Input.insertText", map[string]any{"text": v.Text})
	case "key":
		code := map[string]int{"Enter": 13, "Tab": 9, "Backspace": 8}[v.Key]
		for _, typ := range []string{"keyDown", "keyUp"} {
			p := map[string]any{"type": typ, "key": v.Key, "code": v.Key, "windowsVirtualKeyCode": code}
			if v.Key == "Enter" && typ == "keyDown" {
				p["text"] = "\r"
			}
			if err := call("Input.dispatchKeyEvent", p); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkLogin never returns an ARL to the phone. The isolated page's cookies are
// verified by Deezer itself before completing the deck's relay session.
func (s *server) checkBrowserLogin(ctx context.Context, token string, a *deezerBrowserSession) (bool, error) {
	var result struct {
		Cookies []struct{ Name, Value, Domain string } `json:"cookies"`
	}
	if err := a.rpc.call(ctx, a.sessionID, "Network.getCookies", map[string]any{"urls": []string{"https://www.deezer.com", "https://deezer.com"}}, &result); err != nil {
		return false, err
	}
	for _, c := range result.Cookies {
		domain := strings.ToLower(strings.TrimPrefix(c.Domain, "."))
		if c.Name != "arl" || !validARL(c.Value) || !(domain == "deezer.com" || strings.HasSuffix(domain, ".deezer.com")) {
			continue
		}
		id, name, err := s.deezer.arlUser(ctx, c.Value)
		if err != nil || id == "" || id == "0" {
			continue
		}
		// Drop browser credentials before publishing success, including if the
		// token expired while the upstream identity request was running.
		s.browser.disposeLocked()
		return true, s.sessions.completeDeezer(token, id, c.Value, func(trusted bool) error {
			if !trusted {
				return nil
			}
			return s.cfg.update(func(cfg *config) {
				cfg.DeezerARL, cfg.DeezerUserID = c.Value, id
				cfg.DeezerARLUserName, cfg.DeezerARLChecked = name, time.Now().UTC()
			})
		})
	}
	return false, nil
}

func (s *server) handleDeezerBrowserFrame(w http.ResponseWriter, r *http.Request) {
	s.handleDeezerBrowser(w, r, true)
}
func (s *server) handleDeezerBrowserAct(w http.ResponseWriter, r *http.Request) {
	s.handleDeezerBrowser(w, r, false)
}

func (s *server) handleDeezerBrowser(w http.ResponseWriter, r *http.Request, frame bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	release := func() {}
	respond := func(code int, value any) { release(); writeJSON(w, code, value) }
	fail := func(code int, msg string) { respond(code, map[string]any{"ok": false, "error": msg}) }
	token := r.URL.Query().Get("t")
	sess, ok := s.sessions.get(token)
	if !validToken(token) || !ok || sess.Provider != "deezer" {
		fail(400, "二维码无效或已过期，请重新扫码")
		return
	}
	if s.browser == nil {
		fail(503, "云端登录暂不可用")
		return
	}
	var action deezerBrowserAction
	if !frame {
		// Requiring JSON prevents cross-site HTML forms from injecting input.
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			fail(415, "请使用 JSON")
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		if err := dec.Decode(&action); err != nil || !action.valid() {
			fail(400, "无效的浏览器操作")
			return
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			fail(400, "无效的浏览器操作")
			return
		}
	}
	b := s.browser
	b.mu.Lock()
	locked := true
	release = func() {
		if locked {
			locked = false
			b.mu.Unlock()
		}
	}
	defer release()
	if b.closed {
		fail(503, "云端登录暂不可用")
		return
	}
	// Re-read after waiting for another request and retire invalid owners.
	if b.active != nil {
		owner, live := s.sessions.get(b.active.token)
		if !live || owner.Provider != "deezer" || owner.Status == "done" || !time.Now().Before(b.active.expires) {
			b.disposeLocked()
		}
	}
	sess, ok = s.sessions.get(token)
	if !ok || sess.Provider != "deezer" {
		fail(400, "二维码已过期，请重新扫码")
		return
	}
	if sess.Status == "done" {
		respond(200, map[string]any{"ok": true, "status": "done"})
		return
	}
	if b.active != nil && b.active.token != token {
		fail(409, "已有其他登录窗口正在使用，请稍后再试")
		return
	}
	if !frame && action.Type == "stop" {
		b.disposeLocked()
		respond(200, map[string]any{"ok": true, "status": "stopped"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if b.active == nil {
		// Direct frame requests can start a session; polling an expired window
		// receives 410, so it cannot silently keep recreating browser contexts.
		if frame && r.URL.Query().Get("poll") == "1" {
			fail(410, "登录窗口已关闭或五分钟无操作，请重新启动")
			return
		}
		if !frame && action.Type != "start" {
			fail(410, "请先启动云端登录窗口")
			return
		}
		if err := b.startLocked(ctx, sess); err != nil {
			fail(502, "无法启动云端登录窗口，请稍后重试")
			return
		}
	}
	a := b.active
	if !frame {
		if err := a.act(ctx, action); err != nil {
			b.disposeLocked()
			fail(502, "浏览器操作失败，请重新启动窗口")
			return
		}
		b.touchLocked(sess.Expires)
	}
	done, err := s.checkBrowserLogin(ctx, token, a)
	if err != nil {
		b.disposeLocked()
		fail(502, "登录检查失败，请重试")
		return
	}
	if done {
		respond(200, map[string]any{"ok": true, "status": "done"})
		return
	}
	if !frame {
		respond(200, map[string]any{"ok": true, "status": "active"})
		return
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := a.rpc.call(ctx, a.sessionID, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 75, "captureBeyondViewport": false}, &shot); err != nil {
		b.disposeLocked()
		fail(502, "获取登录画面失败，请重新启动窗口")
		return
	}
	data, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil || len(data) < 3 || data[0] != 0xff || data[1] != 0xd8 {
		b.disposeLocked()
		fail(502, "登录画面无效，请重新启动窗口")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	// A slow phone must not delay another action or the idle cleanup timer.
	release()
	w.Write(data)
}
