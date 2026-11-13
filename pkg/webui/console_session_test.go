package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// After signing in, the browser's cookie is a sign-in token of its own, not the access code:
// requests made after the sign-in no longer carry the code. A restart keeps the sign-in, a
// made-up token is refused, and a cookie from before sign-in sessions is replaced.
func TestConsoleCookieIsASessionNotTheCode(t *testing.T) {
	const code = "ABCD-EFGH"
	state := t.TempDir()
	s := New(Config{AccessToken: code, UIDir: t.TempDir(), StateDir: state}, nil)
	defer s.Close()
	do := func(s *Server, method, target string, body string, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.10:5555"
		if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "stream_sync", Value: cookie})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	cookieOf := func(w *httptest.ResponseRecorder) string {
		for _, c := range w.Result().Cookies() {
			if c.Name == "stream_sync" {
				return c.Value
			}
		}
		return ""
	}
	norm := func(v string) string { return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(v)) }

	w := do(s, "POST", "/login", url.Values{"token": {code}}.Encode(), "")
	session := cookieOf(w)
	if w.Code != http.StatusSeeOther || session == "" {
		t.Fatalf("sign-in: %d, cookie %q", w.Code, session)
	}
	if strings.Contains(norm(session), norm(code)) {
		t.Fatalf("the console cookie carries the access code: %q", session)
	}
	if w := do(s, "GET", "/api/ui", "", session); w.Code != http.StatusOK {
		t.Fatalf("signed-in request: %d", w.Code)
	}
	again := cookieOf(do(s, "POST", "/login", url.Values{"token": {code}}.Encode(), ""))
	if again == "" || again == session {
		t.Fatalf("a second sign-in got %q, want a token of its own", again)
	}

	restarted := New(Config{AccessToken: code, UIDir: t.TempDir(), StateDir: state}, nil)
	defer restarted.Close()
	if w := do(restarted, "GET", "/api/ui", "", session); w.Code != http.StatusOK {
		t.Fatalf("sign-in after a restart: %d", w.Code)
	}
	forged := "s." + strings.Repeat("0", 64)
	if w := do(restarted, "GET", "/api/ui", "", forged); w.Code != http.StatusUnauthorized {
		t.Fatalf("made-up session token: %d, want 401", w.Code)
	}

	w = do(restarted, "GET", "/api/ui", "", code)
	if w.Code != http.StatusOK {
		t.Fatalf("cookie from before sessions: %d", w.Code)
	}
	if c := cookieOf(w); c == "" || strings.Contains(norm(c), norm(code)) {
		t.Fatalf("the old cookie was not replaced by a session: %q", c)
	}
}
