package webui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteLoadSettingLoopbackOnly(t *testing.T) {
	ui := t.TempDir()
	s := New(Config{AccessToken: "code", UIDir: ui, StateDir: t.TempDir()}, nil)
	do := func(method, target, addr, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.RemoteAddr = addr
		if addr != "127.0.0.1:1" {
			r.Header.Set("Authorization", "Bearer code")
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/v1/sc5000/ui", "127.0.0.1:1", ""); !strings.Contains(w.Body.String(), `"remoteLoad":false`) {
		t.Fatalf("default: %s", w.Body.String())
	}
	if w := do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", ""); w.Code != 200 {
		t.Fatalf("loopback set = %d: %s", w.Code, w.Body.String())
	}
	conf, _ := os.ReadFile(filepath.Join(ui, "settings.conf"))
	if !strings.Contains(string(conf), "remote_load=1") || !s.remoteLoadEnabled() {
		t.Fatalf("not saved: %q", conf)
	}
	if w := do("POST", "/v1/sc5000/ui?remoteLoad=maybe", "127.0.0.1:1", ""); w.Code != 400 {
		t.Fatalf("maybe = %d", w.Code)
	}
	if w := do("POST", "/api/ui?remoteLoad=0", "192.0.2.1:1", ""); w.Code != 403 {
		t.Fatalf("external query = %d", w.Code)
	}
	if w := do("POST", "/api/ui", "192.0.2.1:1", `{"remoteLoad":0}`); w.Code != 403 {
		t.Fatalf("external json = %d", w.Code)
	}
	if !s.remoteLoadEnabled() {
		t.Fatal("external request changed the setting")
	}
	if w := do("POST", "/v1/sc5000/ui?remoteLoad=0", "127.0.0.1:1", ""); w.Code != 200 || s.remoteLoadEnabled() {
		t.Fatalf("loopback off = %d", w.Code)
	}
}

func remoteTestLoader(now *time.Time, decks ...remoteDeck) *remoteLoader {
	l := newRemoteLoader(func() time.Time { return *now })
	l.setBoard("JP21")
	if decks != nil {
		l.report(decks, nil, true)
	}
	return l
}

func TestRemoteLoaderChecks(t *testing.T) {
	now := time.Unix(1000, 0)
	idle := []remoteDeck{{Deck: 1, Local: true}, {Deck: 2, Playing: true}, {Deck: 3, Local: true, Playing: true}, {Deck: 4, Local: true}}
	l := remoteTestLoader(&now, idle...)
	check := func(name string, deck int, enabled bool, want int) {
		t.Helper()
		_, code, err := l.submit(deck, "a.mp3", "STICK", "a.mp3", "/x/a.mp3", enabled)
		if code != want || (want >= 400) != (err != nil) {
			t.Errorf("%s: code %d err %v, want %d", name, code, err, want)
		}
	}
	check("off", 1, false, 403)
	check("deck 0", 0, true, 400)
	check("deck 5", 5, true, 400)
	check("locked", 2, true, 409)
	if _, _, err := l.submit(2, "a.mp3", "STICK", "a.mp3", "/x/a.mp3", true); err == nil || err.Error() != "deck locked" {
		t.Errorf("locked+playing deck: %v, want deck locked first", err)
	}
	if _, _, err := l.submit(3, "a.mp3", "STICK", "a.mp3", "/x/a.mp3", true); err == nil || err.Error() != "deck playing" {
		t.Errorf("playing deck: %v, want deck playing", err)
	}
	check("playing", 3, true, 409)
	check("ok", 1, true, 202)
	check("open request", 1, true, 409)
	if r, _, _ := l.submit(4, "a.mp3", "STICK", "a.mp3", "/x/a.mp3", true); r == nil || r.Status != "pending" {
		t.Errorf("submit = %+v", r)
	}
	now = now.Add(3 * time.Second)
	check("stale", 4, true, 503)
	if _, code, _ := remoteTestLoader(&now).submit(1, "a", "", "a", "", true); code != 503 {
		t.Errorf("no state = %d", code)
	}
}

func TestRemoteLoaderLifecycle(t *testing.T) {
	now := time.Unix(1000, 0)
	idle := []remoteDeck{{Deck: 1, Local: true}, {Deck: 2, Local: true}}
	l := remoteTestLoader(&now, idle...)
	a, _, _ := l.submit(1, "a.mp3", "STICK", "a.mp3", "/x/a.mp3", true)
	got := l.report(idle, nil, true)
	if got == nil || got.ID != a.ID || got.Status != "loading" || got.File != "/x/a.mp3" {
		t.Fatalf("report = %+v", got)
	}
	if again := l.report(idle, nil, true); again != nil {
		t.Fatalf("handed out twice: %+v", again)
	}
	l.report(idle, []remoteLoadUpdate{{ID: "nope", Status: "loaded"}, {ID: a.ID, Status: "pending"}}, true)
	if r, _ := l.get(a.ID, true); r.Status != "loading" {
		t.Fatalf("bad update applied: %+v", r)
	}
	l.report(idle, []remoteLoadUpdate{{ID: a.ID, Status: "loaded"}}, true)
	now = now.Add(time.Minute)
	if r, _ := l.get(a.ID, true); r.Status != "loaded" {
		t.Fatalf("loaded did not stick: %+v", r)
	}
	// pending expires after 10 s
	l.report(idle, nil, true)
	b, _, _ := l.submit(2, "b.mp3", "STICK", "b.mp3", "/x/b.mp3", true)
	now = now.Add(11 * time.Second)
	if r, _ := l.get(b.ID, true); r.Status != "expired" {
		t.Fatalf("pending after 11 s: %+v", r)
	}
	// loading fails after 8 s
	l.report(idle, nil, true)
	c, _, _ := l.submit(2, "c.mp3", "STICK", "c.mp3", "/x/c.mp3", true)
	l.report(idle, nil, true)
	now = now.Add(9 * time.Second)
	if r, _ := l.get(c.ID, true); r.Status != "failed" || r.Reason != "no confirmation" {
		t.Fatalf("loading after 9 s: %+v", r)
	}
	// switched off rejects pending
	l.report(idle, nil, true)
	d, _, _ := l.submit(2, "d.mp3", "STICK", "d.mp3", "/x/d.mp3", true)
	if got := l.report(idle, nil, false); got != nil {
		t.Fatalf("handed out while off: %+v", got)
	}
	if r, _ := l.get(d.ID, false); r.Status != "rejected" || r.Reason != "switched off" {
		t.Fatalf("off: %+v", r)
	}
	// only the last 20 are kept
	for i := 0; i < 25; i++ {
		l.report(idle, nil, true)
		r, _, err := l.submit(1, "e.mp3", "STICK", "e.mp3", "/x/e.mp3", true)
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		l.report(idle, nil, true)
		l.report(idle, []remoteLoadUpdate{{ID: r.ID, Status: "loaded"}}, true)
	}
	if _, _, reqs, _, _ := l.snapshot(true); len(reqs) != 20 {
		t.Fatalf("kept %d requests", len(reqs))
	}
}

// A slow load the deck still waits for stays "loading" past the 8 s backstop
// and its late "loaded" counts; the next request waits instead of being
// handed to a deck that would answer "busy" (review of 1a6edad8).
func TestRemoteLoaderKeepsTheActiveRequestOpen(t *testing.T) {
	now := time.Unix(1000, 0)
	idle := []remoteDeck{{Deck: 1, Local: true}, {Deck: 2, Local: true}}
	l := remoteTestLoader(&now, idle...)
	a, _, _ := l.submit(1, "long.flac", "STICK", "long.flac", "/x/long.flac", true)
	if got := l.reportLock(idle, nil, "", nil, true); got == nil || got.ID != a.ID {
		t.Fatalf("report = %+v", got)
	}
	var b *remoteLoadRequest
	for i := 0; i < 30; i++ { // 15 s of polls naming a as active
		now = now.Add(500 * time.Millisecond)
		if i == 10 {
			var code int
			var err error
			if b, code, err = l.submit(2, "b.mp3", "STICK", "b.mp3", "/x/b.mp3", true); err != nil {
				t.Fatalf("submit b = %d %v", code, err)
			}
		}
		if got := l.reportLock(idle, nil, a.ID, nil, true); got != nil {
			t.Fatalf("handed out %+v while %s is loading", got, a.ID)
		}
	}
	if r, _ := l.get(a.ID, true); r.Status != "loading" {
		t.Fatalf("active request after 15 s: %+v", r)
	}
	// a's late result counts, and b, which waited within its TTL, goes out next.
	next := l.reportLock(idle, nil, "", []remoteLoadUpdate{{ID: a.ID, Status: "loaded"}}, true)
	if r, _ := l.get(a.ID, true); r.Status != "loaded" {
		t.Fatalf("late loaded: %+v", r)
	}
	if next == nil || next.ID != b.ID {
		t.Fatalf("next = %+v, want b", next)
	}
	l.reportLock(idle, nil, "", []remoteLoadUpdate{{ID: b.ID, Status: "loaded"}}, true)
	// Once the deck stops naming it, the backstop counts from the last mention.
	c, _, _ := l.submit(1, "c.mp3", "STICK", "c.mp3", "/x/c.mp3", true)
	l.reportLock(idle, nil, "", nil, true)
	l.reportLock(idle, nil, c.ID, nil, true)
	now = now.Add(7 * time.Second)
	if r, _ := l.get(c.ID, true); r.Status != "loading" {
		t.Fatalf("7 s after the last mention: %+v", r)
	}
	now = now.Add(2 * time.Second)
	if r, _ := l.get(c.ID, true); r.Status != "failed" || r.Reason != "no confirmation" {
		t.Fatalf("9 s after the last mention: %+v", r)
	}
}

func TestResolveRemoteFile(t *testing.T) {
	root := t.TempDir()
	devs := map[string]string{}
	for name, dev := range map[string]string{"STICK": "/dev/sda1", "SD": "/dev/mmcblk1p1", "INTERNAL": "/dev/mmcblk0p5", "NAS": "//nas/share"} {
		if err := os.MkdirAll(filepath.Join(root, name, "Music"), 0o755); err != nil {
			t.Fatal(err)
		}
		devs[filepath.Join(root, name)] = dev
	}
	old := remoteMountDevices
	remoteMountDevices = func() map[string]string { return devs }
	defer func() { remoteMountDevices = old }()
	put := func(drive, rel string) {
		if err := os.WriteFile(filepath.Join(root, drive, rel), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: root}, nil)
	const name = "Music/Ä Bär #1?.mp3"
	put("STICK", name)
	put("INTERNAL", "Music/only-internal.mp3")
	put("NAS", "Music/only-nas.mp3")
	put("STICK", "Music/notes.txt")
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "STICK", "Music", "out.mp3")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for _, n := range []string{"outside.mp3", "a.mp3"} {
		if err := os.WriteFile(filepath.Join(outside, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "outside.mp3"), filepath.Join(root, "STICK", "Music", "escape.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "STICK", "Music", "ext")); err != nil {
		t.Fatal(err)
	}

	file, drive, _, _, code, err := s.resolveRemoteFile(name, "")
	if err != nil || drive != "STICK" || code != 200 || !strings.HasSuffix(file, "Bär #1?.mp3") {
		t.Fatalf("resolve = %q %q %d %v", file, drive, code, err)
	}
	for _, c := range []struct {
		rel  string
		want int
	}{
		{"Music/only-internal.mp3", 404}, {"Music/only-nas.mp3", 404}, {"Music/notes.txt", 404},
		{"Music/out.mp3", 404}, {"Music/escape.mp3", 404}, {"Music/ext/a.mp3", 404}, {"Music/missing.mp3", 404},
		{"../x.mp3", 400}, {"/etc/passwd", 400}, {"Music/../../x.mp3", 400}, {"", 400},
	} {
		if _, _, _, _, code, err := s.resolveRemoteFile(c.rel, ""); code != c.want || err == nil {
			t.Errorf("%q = %d %v, want %d", c.rel, code, err, c.want)
		}
	}
	put("SD", name)
	_, _, _, cands, code, err := s.resolveRemoteFile(name, "")
	if code != 409 || err == nil || strings.Join(cands, ",") != "SD,STICK" {
		t.Fatalf("ambiguous = %v %d %v", cands, code, err)
	}
	if _, drive, _, _, code, err := s.resolveRemoteFile(name, "SD"); err != nil || drive != "SD" || code != 200 {
		t.Fatalf("SD = %q %d %v", drive, code, err)
	}
	if _, _, _, _, code, _ := s.resolveRemoteFile(name, "INTERNAL"); code != 404 {
		t.Fatalf("INTERNAL = %d", code)
	}
}

func TestRemoteSide(t *testing.T) {
	for deck, want := range map[int]string{1: "left", 2: "right", 3: "left", 4: "right", 5: ""} {
		if got := remoteSide("SCLIVE4", deck); got != want {
			t.Errorf("SCLIVE4 deck %d = %q, want %q", deck, got, want)
		}
		if got := remoteSide("JP21", deck); got != want {
			t.Errorf("JP21 deck %d = %q, want %q", deck, got, want)
		}
	}
	if remoteSide("JP07", 1) != "" {
		t.Error("JP07 must have no side")
	}
}

func remoteRouteServer(t *testing.T) (*Server, func(method, target, addr, body string, hdr ...string) *httptest.ResponseRecorder) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "STICK", "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "STICK", "Music", "a.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := remoteMountDevices
	remoteMountDevices = func() map[string]string { return map[string]string{filepath.Join(root, "STICK"): "/dev/sda1"} }
	t.Cleanup(func() { remoteMountDevices = old })
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: root}, nil)
	do := func(method, target, addr, body string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.RemoteAddr = addr
		if !strings.HasPrefix(addr, "127.") {
			r.Header.Set("Authorization", "Bearer code")
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	return s, do
}

func TestRemoteLoadRoutesAuth(t *testing.T) {
	s, _ := remoteRouteServer(t)
	for _, m := range []string{"GET", "POST"} {
		r := httptest.NewRequest(m, "/api/remote-load", strings.NewReader(`{}`))
		r.RemoteAddr = "192.0.2.1:1"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s without code: %d", m, w.Code)
		}
	}
	_, do := remoteRouteServer(t)
	if w := do("GET", "/api/remote-load", "192.0.2.1:1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":1,"path":"Music/a.mp3"}`); w.Code != 403 {
		t.Fatalf("POST switch off: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{bad`); w.Code != 400 {
		t.Fatalf("POST bad json: %d", w.Code)
	}
	if w := do("GET", "/api/remote-load/nope", "192.0.2.1:1", ""); w.Code != 404 {
		t.Fatalf("GET unknown id: %d", w.Code)
	}
}

func TestRemoteLoadRoutesRefuseTunnel(t *testing.T) {
	_, do := remoteRouteServer(t)
	if w := do("GET", "/api/remote-load", "192.0.2.1:1", "", "Cf-Connecting-Ip", "1.2.3.4"); w.Code != 403 {
		t.Fatalf("GET: %d", w.Code)
	}
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{}`, "Cf-Ray", "abc"); w.Code != 403 {
		t.Fatalf("POST: %d", w.Code)
	}
	if w := do("POST", "/v1/sc5000/remote-load", "127.0.0.1:1", `{}`, "Cf-Connecting-Ip", "1.2.3.4"); w.Code != 403 {
		t.Fatalf("internal: %d", w.Code)
	}
	// Every header a tunnel edge adds (relayedFromOutside), not only Cloudflare's.
	for _, h := range []string{"Cdn-Loop", "Tailscale-User-Login"} {
		if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":1,"path":"Music/a.mp3"}`, h, "x"); w.Code != 403 {
			t.Fatalf("POST via %s: %d %s", h, w.Code, w.Body.String())
		}
	}
}

func TestRemoteLoadInternalLoopbackOnly(t *testing.T) {
	_, do := remoteRouteServer(t)
	if w := do("POST", "/v1/sc5000/remote-load", "192.0.2.1:1", `{}`); w.Code != 403 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestRemoteLoadEndToEnd(t *testing.T) {
	s, do := remoteRouteServer(t)
	if w := do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", ""); w.Code != 200 {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	report := `{"decks":[{"deck":1,"local":true,"loaded":true,"playing":true},{"deck":2,"local":true},{"deck":3,"local":true},{"deck":4,"local":true}]}`
	w := do("POST", "/v1/sc5000/remote-load", "127.0.0.1:1", report)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"request":null`) {
		t.Fatalf("idle poll: %d %s", w.Code, w.Body.String())
	}
	w = do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":3,"path":"Music/a.mp3"}`)
	var acc struct{ ID, Status string }
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &acc) != nil || acc.ID == "" || acc.Status != "pending" {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	w = do("POST", "/v1/sc5000/remote-load", "127.0.0.1:1", `{}`)
	var poll struct {
		Request *struct {
			ID   string
			Deck int
			File string
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &poll) != nil || poll.Request == nil {
		t.Fatalf("poll: %d %s", w.Code, w.Body.String())
	}
	if poll.Request.ID != acc.ID || poll.Request.Deck != 3 || poll.Request.File != filepath.Join(s.mediaRoot(), "STICK", "Music", "a.mp3") {
		t.Fatalf("poll request: %+v", poll.Request)
	}
	w = do("POST", "/v1/sc5000/remote-load", "127.0.0.1:1", `{"updates":[{"id":"`+acc.ID+`","status":"loaded"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"request":null`) {
		t.Fatalf("update poll: %d %s", w.Code, w.Body.String())
	}
	w = do("GET", "/api/remote-load/"+acc.ID, "192.0.2.1:1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"loaded"`) || strings.Contains(w.Body.String(), "/STICK/") {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	w = do("GET", "/api/remote-load", "192.0.2.1:1", "")
	if !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), acc.ID) || !strings.Contains(w.Body.String(), `"stateAgeMs"`) {
		t.Fatalf("list: %s", w.Body.String())
	}
	w = do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":1,"path":"Music/a.mp3"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "deck playing") {
		t.Fatalf("playing: %d %s", w.Code, w.Body.String())
	}
}

func TestRemoteLoadSwitchRefusedThroughTunnel(t *testing.T) {
	s, do := remoteRouteServer(t)
	for _, c := range []struct{ target, body, hdr string }{
		{"/v1/sc5000/ui?remoteLoad=1", "", "Cf-Ray"},
		{"/v1/sc5000/ui?remoteLoad=1", "", "Cf-Connecting-Ip"},
		{"/v1/sc5000/ui", `{"remoteLoad":1}`, "Cf-Ray"},
		{"/v1/sc5000/ui?remoteLoad=1", "", "Cdn-Loop"},
		{"/v1/sc5000/ui?remoteLoad=1", "", "Tailscale-User-Login"},
	} {
		w := do("POST", c.target, "127.0.0.1:1", c.body, c.hdr, "x", "Content-Type", "application/json")
		if w.Code != 403 || s.remoteLoadEnabled() {
			t.Fatalf("%s %s via %s: %d %s", c.target, c.body, c.hdr, w.Code, w.Body.String())
		}
	}
	// The console API cannot switch it either, with the access code.
	if w := do("POST", "/api/ui?remoteLoad=1", "192.0.2.1:1", ""); w.Code != 403 || s.remoteLoadEnabled() {
		t.Fatalf("/api/ui: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", ""); w.Code != 200 || !s.remoteLoadEnabled() {
		t.Fatalf("loopback: %d", w.Code)
	}
}

// remotePollRequest posts a QML poll and returns the request it hands out.
func remotePollRequest(t *testing.T, do func(method, target, addr, body string, hdr ...string) *httptest.ResponseRecorder, body string) map[string]any {
	t.Helper()
	w := do("POST", "/v1/sc5000/remote-load", "127.0.0.1:1", body)
	var poll struct{ Request map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &poll) != nil {
		t.Fatalf("poll: %d %s", w.Code, w.Body.String())
	}
	return poll.Request
}

func TestRemoteLoadOneLoadAtATime(t *testing.T) {
	_, do := remoteRouteServer(t)
	do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", "")
	decks := `{"decks":[{"deck":1,"local":true},{"deck":2,"local":true}]}`
	remotePollRequest(t, do, decks)
	ids := map[float64]string{}
	for _, d := range []int{1, 2} {
		w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":`+string(rune('0'+d))+`,"path":"Music/a.mp3"}`)
		var acc struct{ ID string }
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &acc) != nil {
			t.Fatalf("submit deck %d: %d %s", d, w.Code, w.Body.String())
		}
		ids[float64(d)] = acc.ID
	}
	first := remotePollRequest(t, do, decks)
	if first == nil || first["deck"] != float64(1) || first["id"] != ids[1] {
		t.Fatalf("first poll: %v", first)
	}
	for i := 0; i < 3; i++ {
		if got := remotePollRequest(t, do, decks); got != nil {
			t.Fatalf("handed out while deck 1 loads: %v", got)
		}
	}
	second := remotePollRequest(t, do, `{"decks":[{"deck":1,"local":true,"loaded":true},{"deck":2,"local":true}],"updates":[{"id":"`+ids[1]+`","status":"loaded"}]}`)
	if second == nil || second["deck"] != float64(2) || second["id"] != ids[2] {
		t.Fatalf("after deck 1 loaded: %v", second)
	}
}

func TestRemoteLoadPollSendsDriveRelativePath(t *testing.T) {
	_, do := remoteRouteServer(t)
	do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", "")
	remotePollRequest(t, do, `{"decks":[{"deck":1,"local":true}]}`)
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":1,"path":"Music//./a.mp3"}`); w.Code != 202 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	got := remotePollRequest(t, do, `{}`)
	if got == nil || got["path"] != "Music/a.mp3" {
		t.Fatalf("poll request: %v", got)
	}
}

func TestRemoteLoadDeckChecksBeforeFileLookup(t *testing.T) {
	_, do := remoteRouteServer(t)
	do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", "")
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":9,"path":"missing.mp3"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "deck must be 1-4") {
		t.Fatalf("deck 9 + missing file: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":1,"path":"missing.mp3"}`); w.Code != 503 {
		t.Fatalf("no state + missing file: %d %s", w.Code, w.Body.String())
	}
	remotePollRequest(t, do, `{"decks":[{"deck":1,"local":true,"loaded":true,"playing":true},{"deck":2},{"deck":3,"local":true}]}`)
	for body, want := range map[string]string{
		`{"deck":1,"path":"missing.mp3"}`: "deck playing",
		`{"deck":2,"path":"missing.mp3"}`: "deck locked",
	} {
		if w := do("POST", "/api/remote-load", "192.0.2.1:1", body); w.Code != 409 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":3,"path":"Music/a.mp3"}`); w.Code != 202 {
		t.Fatalf("deck 3: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":3,"path":"missing.mp3"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "open request") {
		t.Fatalf("open request + missing file: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":4,"path":"missing.mp3"}`); w.Code != 503 {
		t.Fatalf("deck without state: %d %s", w.Code, w.Body.String())
	}
}

// The QML poll reports Engine's stock "Lock Playing Deck" preference; GET shows
// it, null while unknown.
func TestRemoteLoadReportsLockPlayingDeck(t *testing.T) {
	_, do := remoteRouteServer(t)
	do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", "")
	lock := func() any {
		t.Helper()
		w := do("GET", "/api/remote-load", "192.0.2.1:1", "")
		var out map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("GET: %d %s", w.Code, w.Body.String())
		}
		v, ok := out["lockPlayingDeck"]
		if !ok {
			t.Fatalf("no lockPlayingDeck: %s", w.Body.String())
		}
		return v
	}
	if v := lock(); v != nil {
		t.Fatalf("before any poll: %v", v)
	}
	remotePollRequest(t, do, `{"decks":[{"deck":1,"local":true}],"lockPlayingDeck":false}`)
	if v := lock(); v != false {
		t.Fatalf("off: %v", v)
	}
	remotePollRequest(t, do, `{"decks":[{"deck":1,"local":true}],"lockPlayingDeck":true}`)
	if v := lock(); v != true {
		t.Fatalf("on: %v", v)
	}
	remotePollRequest(t, do, `{"decks":[{"deck":1,"local":true}],"lockPlayingDeck":null}`)
	if v := lock(); v != nil {
		t.Fatalf("unknown: %v", v)
	}
}

// The drive-relative path comes from the file lookup itself, not from a second
// scan of the drives that may no longer see the drive.
func TestRemoteLoadDriveRelativePathFromLookup(t *testing.T) {
	s, do := remoteRouteServer(t)
	do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", "")
	remotePollRequest(t, do, `{"decks":[{"deck":1,"local":true}]}`)
	devs := map[string]string{filepath.Join(s.mediaRoot(), "STICK"): "/dev/sda1"}
	scans := 0
	remoteMountDevices = func() map[string]string {
		scans++
		if scans > 1 {
			return map[string]string{} // the drive is gone by the second scan
		}
		return devs
	}
	if w := do("POST", "/api/remote-load", "192.0.2.1:1", `{"deck":1,"path":"Music/a.mp3"}`); w.Code != 202 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	if got := remotePollRequest(t, do, `{}`); got == nil || got["path"] != "Music/a.mp3" {
		t.Fatalf("poll request: %v", got)
	}
}

// The board is read once when the server starts, not on every poll.
func TestRemoteLoadBoardReadOnce(t *testing.T) {
	old := deviceCompatiblePath
	deviceCompatiblePath = filepath.Join(t.TempDir(), "compatible")
	t.Cleanup(func() { deviceCompatiblePath = old })
	if err := os.WriteFile(deviceCompatiblePath, []byte("inmusic,jp21\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, do := remoteRouteServer(t)
	if err := os.WriteFile(deviceCompatiblePath, []byte("inmusic,jp07\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	do("POST", "/v1/sc5000/ui?remoteLoad=1", "127.0.0.1:1", "")
	remotePollRequest(t, do, `{"decks":[{"deck":1,"local":true},{"deck":2,"local":true}]}`)
	w := do("GET", "/api/remote-load", "192.0.2.1:1", "")
	var out struct {
		Board string
		Decks []remoteDeck
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if out.Board != "SCLIVE4" || len(out.Decks) != 2 || out.Decks[0].Side != "left" || out.Decks[1].Side != "right" {
		t.Fatalf("board read again after start: %s", w.Body.String())
	}
}
