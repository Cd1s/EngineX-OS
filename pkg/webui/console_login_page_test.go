package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #344: signing in with a wrong code showed the API's raw JSON in the browser, because the page was a plain form
// post to /login. The page now posts with fetch and shows a readable message in its languages; /login keeps
// answering JSON for other clients.
func TestConsoleLoginPageShowsReadableErrorsAndTheAPIKeepsJSON(t *testing.T) {
	s := New(Config{AccessToken: "K7M9-P4X2", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	serve := func(method, target, body, ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		r.RemoteAddr = ip + ":1234"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	page := serve("GET", "/", "", "192.0.2.20").Body.String()
	for _, want := range []string{`<form id="login" method="post" action="/login">`, `<p id="loginError" role="alert" hidden></p>`,
		`addEventListener('submit'`, `ev.preventDefault()`, `fetch('/login',`} {
		if !strings.Contains(page, want) {
			t.Errorf("sign-in page lacks %s", want)
		}
	}
	for _, kind := range []string{"wrong", "locked", "busy", "failed"} {
		m := regexp.MustCompile(kind + `:\[((?:'(?:[^'\\]|\\.)*',?){4})\]`).FindStringSubmatch(page)
		if m == nil {
			t.Errorf("no four-language message for %q", kind)
			continue
		}
		for i, text := range regexp.MustCompile(`'((?:[^'\\]|\\.)*)'`).FindAllStringSubmatch(m[1], -1) {
			if strings.TrimSpace(text[1]) == "" || strings.Contains(text[1], "{") {
				t.Errorf("%s message %d is empty or JSON: %q", kind, i, text[1])
			}
		}
	}

	wrong := serve("POST", "/login", url.Values{"token": {"WRONG-CODE"}}.Encode(), "192.0.2.21")
	var body map[string]string
	if wrong.Code != http.StatusUnauthorized || !strings.HasPrefix(wrong.Header().Get("Content-Type"), "application/json") ||
		json.Unmarshal(wrong.Body.Bytes(), &body) != nil || body["error"] != "invalid access code" {
		t.Fatalf("wrong code: %d %q %s, want the API's JSON 401", wrong.Code, wrong.Header().Get("Content-Type"), wrong.Body.String())
	}
	var last *httptest.ResponseRecorder
	for i := 0; i < authMaxFails; i++ {
		last = serve("POST", "/login", url.Values{"token": {"WRONG-CODE"}}.Encode(), "192.0.2.22")
	}
	if last.Code != http.StatusTooManyRequests || json.Unmarshal(last.Body.Bytes(), &body) != nil || body["error"] != errAuthLocked.Error() {
		t.Fatalf("lockout: %d %s, want the API's JSON 429", last.Code, last.Body.String())
	}
}

// The page's own script, run in node with a stand-in DOM and fetch: every refusal /login gives, and a deck that
// cannot be reached, shows its message in English, Simplified and Traditional Chinese and Spanish; a sign-in that
// is accepted opens the console.
func TestConsoleLoginScriptShowsTheMessageForEachRefusal(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	start, end := strings.Index(consoleLoginPage, "<script>"), strings.LastIndex(consoleLoginPage, "</script>")
	if start < 0 || end < start {
		t.Fatal("no script in the sign-in page")
	}
	dir := t.TempDir()
	scriptPath, driverPath := filepath.Join(dir, "page.js"), filepath.Join(dir, "driver.js")
	if err := os.WriteFile(scriptPath, []byte(consoleLoginPage[start+len("<script>"):end]), 0o644); err != nil {
		t.Fatal(err)
	}
	const driver = `
const vm = require('node:vm');
const fs = require('node:fs');
const script = fs.readFileSync(process.argv[2], 'utf8');
const cases = [['wrong', {status: 401}], ['locked', {status: 429}], ['busy', {status: 503}], ['busy', {status: 500}],
  ['busy', null], ['failed', {status: 400}], ['ok', {status: 0, type: 'opaqueredirect'}]];
(async () => {
  const out = [];
  for (const lang of ['en-US', 'zh-CN', 'zh-TW', 'es-ES']) {
    const els = {};
    const el = id => els[id] || (els[id] = {id, textContent: '', hidden: true, disabled: false, value: '', listeners: {},
      addEventListener(type, f) { this.listeners[type] = f; }});
    el('token').value = 'WRONG-CODE';
    let next = null, replaced = null;
    const calls = [];
    const ctx = vm.createContext({
      navigator: {language: lang}, document: {getElementById: el, documentElement: {lang: 'en'}},
      location: {replace(u) { replaced = u; }}, URLSearchParams, setInterval() {}, console,
      fetch: async (u, o) => {
        if (u === '/boot-status') return {ok: false, status: 404};
        calls.push({url: u, method: o && o.method, body: String(o && o.body), redirect: o && o.redirect});
        if (next === null) throw new TypeError('Failed to fetch');
        return {ok: next.status >= 200 && next.status < 300, status: next.status, type: next.type || 'basic', json: async () => ({error: 'x'})};
      },
    });
    vm.runInContext(script, ctx);
    for (const [kind, response] of cases) {
      next = response; replaced = null; calls.length = 0;
      let prevented = false;
      el('loginError').hidden = true; el('loginError').textContent = '';
      await el('login').listeners.submit({preventDefault() { prevented = true; }});
      out.push({lang, kind, prevented, calls: calls.slice(), replaced, shown: el('loginError').hidden ? '' : el('loginError').textContent,
        disabled: el('submit').disabled});
    }
  }
  console.log(JSON.stringify(out));
})().catch(e => { console.error(String(e && e.stack || e)); process.exit(1); });
`
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, driverPath, scriptPath)
	cmd.Stderr = new(strings.Builder)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(stdout)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the page script did not handle a sign-in: %v\n%s", err, cmd.Stderr)
	}
	var runs []struct {
		Lang, Kind, Replaced, Shown string
		Prevented, Disabled         bool
		Calls                       []struct{ URL, Method, Body, Redirect string }
	}
	if err := json.Unmarshal(raw, &runs); err != nil || len(runs) != 28 {
		t.Fatalf("driver output (%v): %s", err, raw)
	}
	want := map[string][4]string{ // a phrase of each message, per language
		"wrong":  {"access code is not right", "访问码不正确", "存取碼不正確", "no es correcto"},
		"locked": {"Too many wrong codes", "错误次数过多", "錯誤次數過多", "Demasiados códigos incorrectos"},
		"busy":   {"busy or cannot be reached", "正忙或无法连接", "忙碌中或無法連線", "ocupado o no responde"},
		"failed": {"Sign-in failed", "登录失败", "登入失敗", "No se pudo iniciar sesión"},
	}
	lang := map[string]int{"en-US": 0, "zh-CN": 1, "zh-TW": 2, "es-ES": 3}
	for _, r := range runs {
		name := r.Lang + "/" + r.Kind
		if !r.Prevented || len(r.Calls) != 1 || r.Calls[0].URL != "/login" || r.Calls[0].Method != "POST" ||
			r.Calls[0].Body != "token=WRONG-CODE" || r.Calls[0].Redirect != "manual" {
			t.Errorf("%s: the form was not sent by the page: prevented=%v calls=%+v", name, r.Prevented, r.Calls)
		}
		if r.Disabled {
			t.Errorf("%s: the Sign in button stays disabled", name)
		}
		if r.Kind == "ok" {
			if r.Replaced != "/" || r.Shown != "" {
				t.Errorf("%s: accepted sign-in: replaced=%q shown=%q", name, r.Replaced, r.Shown)
			}
			continue
		}
		if phrase := want[r.Kind][lang[r.Lang]]; !strings.Contains(r.Shown, phrase) || r.Replaced != "" {
			t.Errorf("%s: shows %q, want a message with %q", name, r.Shown, phrase)
		}
	}
}
