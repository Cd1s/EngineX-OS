package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
)

// webdavRig is a deck with the given nas.json and settings.conf (either may be "").
type webdavRig struct {
	s     *Server
	gw    *httptest.Server
	uiDir string
}

func newWebDAVRig(t *testing.T, nas, settings string) *webdavRig {
	t.Helper()
	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	if nas != "" {
		if err := os.WriteFile(filepath.Join(uiDir, "nas.json"), []byte(nas), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if settings != "" {
		if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	gw := httptest.NewServer(s)
	t.Cleanup(gw.Close)
	return &webdavRig{s: s, gw: gw, uiDir: uiDir}
}

func (r *webdavRig) status(t *testing.T) uiSettings {
	t.Helper()
	var v uiSettings
	tidalGet(t, r.gw.URL, "/v1/enginex/ui", 200, &v)
	return v
}

// change posts one add, edit or remove as the deck does.
func (r *webdavRig) change(t *testing.T, c webdavChange, want int) []webdavView {
	t.Helper()
	body, _ := json.Marshal(c)
	resp, err := http.Post(r.gw.URL+"/v1/enginex/webdav", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %q: HTTP %d, want %d: %s", c.Action, c.URL, resp.StatusCode, want, data)
	}
	var out struct{ Servers []webdavView }
	_ = json.Unmarshal(data, &out)
	return out.Servers
}

func (r *webdavRig) file(t *testing.T) nasFile {
	t.Helper()
	var f nasFile
	b, err := os.ReadFile(filepath.Join(r.uiDir, "nas.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func webdavNames(v []webdavView) []string {
	var out []string
	for _, s := range v {
		out = append(out, s.Name)
	}
	return out
}

const releasedWebDAV = `{"protocol":"webdav","url":"https://dav.example:8443/Music/","username":"dj","password":"wd-secret-1","enabled":true}`

// A deck that set WebDAV up under the released [NAS] route gets the WebDAV source
// with no re-entry: the server in nas.json's slot is the first WebDAV server, the
// switch starts on, and nothing reports it as the NAS.
func TestWebDAVReleasedSetupIsTheFirstServer(t *testing.T) {
	r := newWebDAVRig(t, releasedWebDAV, "lang=en\nsources=deezer\nknown=tidal,netease,spotify,deezer,soundcloud,beatport,applemusic\n")
	v := r.status(t)
	if !slices.Contains(v.Sources, webdavScope) || !slices.Contains(v.LoggedIn, webdavScope) {
		t.Fatalf("released WebDAV setup is not the WebDAV source: sources=%v loggedIn=%v", v.Sources, v.LoggedIn)
	}
	if slices.Contains(v.Sources, nasScope) || v.NASSetUp {
		t.Fatalf("a WebDAV server shows as the NAS: sources=%v nasSetUp=%v", v.Sources, v.NASSetUp)
	}
	want := webdavView{ID: webdavRoot("https://dav.example:8443/Music/"), Name: "dav.example:8443", URL: "https://dav.example:8443/Music/", Username: "dj", HasPassword: true}
	if len(v.WebDAV) != 1 || v.WebDAV[0] != want {
		t.Fatalf("servers = %+v, want [%+v]", v.WebDAV, want)
	}
	if code, _ := uiPost(t, r.gw.URL, "scope=webdav"); code != http.StatusOK {
		t.Fatalf("scope=webdav: HTTP %d", code)
	}
	raw := tidalGet(t, r.gw.URL, "/v1/enginex/ui", 200, nil)
	list := tidalGet(t, r.gw.URL, "/v1/enginex/webdav", 200, nil)
	if bytes.Contains(raw, []byte("wd-secret-1")) || bytes.Contains(list, []byte("wd-secret-1")) {
		t.Fatal("a WebDAV password reached the deck's status or list")
	}
}

// The WebDAV row and the NAS row in Services, both on/off combinations, and the
// other switches still work while NAS, WebDAV or rekordbox is in the status list.
func TestWebDAVAndNASSwitches(t *testing.T) {
	smb := `{"protocol":"smb","url":"smb://nas.example/music","username":"dj","password":"smb-secret","enabled":true}`
	r := newWebDAVRig(t, smb, "sources=deezer\nscope=\nomni_source=0\n")
	v := r.status(t)
	if slices.Contains(v.Sources, webdavScope) {
		t.Fatalf("WebDAV is on with no server set up: %v", v.Sources)
	}
	if !v.NASSetUp || !slices.Contains(v.Sources, nasScope) {
		t.Fatalf("SMB NAS missing: nasSetUp=%v sources=%v", v.NASSetUp, v.Sources)
	}
	// WebDAV on with no server yet: listed (tapping it asks for a server), not usable.
	if code, v := uiPost(t, r.gw.URL, "webdav=1"); code != 200 || !slices.Contains(v.Sources, webdavScope) || slices.Contains(v.LoggedIn, webdavScope) {
		t.Fatalf("webdav=1: %d sources=%v loggedIn=%v", code, v.Sources, v.LoggedIn)
	}
	if code, _ := uiPost(t, r.gw.URL, "scope=webdav"); code != http.StatusConflict {
		t.Fatalf("scope=webdav with no server: HTTP %d, want 409", code)
	}
	r.change(t, webdavChange{Action: "add", URL: "http://dav.example/music", Username: "dj", Password: "wd-pw"}, 200)
	for _, c := range []struct{ webdav, nas string }{{"1", "1"}, {"1", "0"}, {"0", "1"}, {"0", "0"}, {"1", "1"}} {
		if code, _ := uiPost(t, r.gw.URL, "webdav="+c.webdav); code != 200 {
			t.Fatalf("webdav=%s: HTTP %d", c.webdav, code)
		}
		if code, _ := uiPost(t, r.gw.URL, "nas="+c.nas); code != 200 {
			t.Fatalf("nas=%s: HTTP %d", c.nas, code)
		}
		v := r.status(t)
		if got := slices.Contains(v.Sources, webdavScope); got != (c.webdav == "1") {
			t.Fatalf("webdav=%s nas=%s: WebDAV listed=%v", c.webdav, c.nas, got)
		}
		if got := slices.Contains(v.Sources, nasScope); got != (c.nas == "1") {
			t.Fatalf("webdav=%s nas=%s: NAS listed=%v", c.webdav, c.nas, got)
		}
		if got := len(r.s.webdavServers()) > 0; got != (c.webdav == "1") {
			t.Fatalf("webdav=%s: servers in use=%v", c.webdav, got)
		}
		if got := r.s.ownerSMBSelfHosted() != nil; got != (c.nas == "1") {
			t.Fatalf("nas=%s: NAS provider=%v", c.nas, got)
		}
		wantNAS, wantWD := http.StatusOK, http.StatusOK
		if c.nas == "0" {
			wantNAS = http.StatusConflict
		}
		if c.webdav == "0" {
			wantWD = http.StatusConflict
		}
		if code, _ := uiPost(t, r.gw.URL, "scope=nas"); code != wantNAS {
			t.Fatalf("nas=%s: scope=nas HTTP %d, want %d", c.nas, code, wantNAS)
		}
		if code, _ := uiPost(t, r.gw.URL, "scope=webdav"); code != wantWD {
			t.Fatalf("webdav=%s: scope=webdav HTTP %d, want %d", c.webdav, code, wantWD)
		}
	}
	// The NAS switch is the console's "enabled" in nas.json; the WebDAV list stays.
	if f := r.file(t); !f.Enabled || f.Protocol != "smb" || len(f.WebDAV) != 1 {
		t.Fatalf("nas.json after the switches: %+v", f.NASConfig)
	}
	// Turning WebDAV off while it is the selected source leaves no source selected.
	uiPost(t, r.gw.URL, "scope=webdav")
	if _, v := uiPost(t, r.gw.URL, "webdav=0"); v.Scope != "" {
		t.Fatalf("scope after webdav=0: %q", v.Scope)
	}
	// The deck sends its whole list back when a service is flipped: rekordbox, NAS
	// and WebDAV in it are not refused and are not saved as services.
	if code, v := uiPost(t, r.gw.URL, "sources=deezer,spotify,nas,webdav,rekordbox"); code != 200 || !slices.Contains(v.Sources, "spotify") {
		t.Fatalf("sources with status-only entries: %d %v", code, v.Sources)
	}
	raw, _ := os.ReadFile(filepath.Join(r.uiDir, "settings.conf"))
	if line := settingsLine(string(raw), "sources"); line != "spotify,deezer" {
		t.Fatalf("saved sources = %q", line)
	}
	if line := settingsLine(string(raw), "webdav"); line != "0" {
		t.Fatalf("saved webdav = %q", line)
	}
	// No NAS set up: nothing to turn on.
	r2 := newWebDAVRig(t, "", "")
	if code, _ := uiPost(t, r2.gw.URL, "nas=1"); code != http.StatusConflict {
		t.Fatalf("nas=1 with no NAS: HTTP %d, want 409", code)
	}
	if code, _ := uiPost(t, r2.gw.URL, "webdav=2"); code != http.StatusBadRequest {
		t.Fatalf("webdav=2: HTTP %d, want 400", code)
	}
}

func settingsLine(text, key string) string {
	for _, l := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v
		}
	}
	return "<none>"
}

// Several servers: added, edited and removed; passwords only in nas.json (0600).
func TestWebDAVServersAddEditRemove(t *testing.T) {
	r := newWebDAVRig(t, "", "webdav=1\n")
	r.change(t, webdavChange{Action: "add", Name: "Studio", URL: "https://dav.example/music/", Username: "dj", Password: "pw-a"}, 200)
	list := r.change(t, webdavChange{Action: "add", URL: "http://other.example:8080/dj", Username: "dj2", Password: "pw-b"}, 200)
	if got := webdavNames(list); !slices.Equal(got, []string{"Studio", "other.example:8080"}) {
		t.Fatalf("names = %v", got)
	}
	a, b := list[0], list[1]
	// The same server again (case of the host, a trailing slash, the default port) is one server.
	r.change(t, webdavChange{Action: "add", URL: "https://DAV.example:443/music", Username: "x"}, http.StatusConflict)
	for _, bad := range []string{"smb://dav.example/music", "ftp://dav.example/", "dav.example/music", "https://user:pw@dav.example/", ""} {
		r.change(t, webdavChange{Action: "add", URL: bad}, http.StatusBadRequest)
	}
	r.change(t, webdavChange{Action: "edit", ID: "0123456789abcdef0123456789abcdef", URL: "https://x.example/"}, http.StatusNotFound)
	// Edit: a new name, an empty password keeps the saved one.
	list = r.change(t, webdavChange{Action: "edit", ID: b.ID, Name: "Club", URL: b.URL, Username: "dj2"}, 200)
	if list[1].Name != "Club" || list[1].ID != b.ID || !list[1].HasPassword {
		t.Fatalf("after edit: %+v", list[1])
	}
	if f := r.file(t); f.WebDAV[1].Password != "pw-b" {
		t.Fatal("an edit without a password dropped the saved one")
	}
	// Edit the address: another server, so another ID; it may not become a duplicate.
	r.change(t, webdavChange{Action: "edit", ID: b.ID, URL: a.URL}, http.StatusConflict)
	list = r.change(t, webdavChange{Action: "edit", ID: b.ID, Name: "Club", URL: "http://other.example:8080/dj2", Username: "dj2"}, 200)
	if list[1].ID == b.ID || list[1].ID != webdavRoot("http://other.example:8080/dj2") {
		t.Fatalf("moved server kept its ID: %+v", list[1])
	}
	// Remove.
	list = r.change(t, webdavChange{Action: "remove", ID: a.ID}, 200)
	if got := webdavNames(list); !slices.Equal(got, []string{"Club"}) {
		t.Fatalf("after remove: %v", got)
	}
	r.change(t, webdavChange{Action: "remove", ID: a.ID}, http.StatusNotFound)
	info, err := os.Stat(filepath.Join(r.uiDir, "nas.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("nas.json mode: %v %v", info, err)
	}
	if f := r.file(t); f.Protocol != "" || len(f.WebDAV) != 1 || f.WebDAV[0].Password != "pw-b" {
		t.Fatalf("nas.json: %+v", f)
	}
	list = r.change(t, webdavChange{Action: "remove", ID: list[0].ID}, 200)
	if len(list) != 0 {
		t.Fatalf("servers left: %v", list)
	}
	if _, err := os.Stat(filepath.Join(r.uiDir, "nas.json")); !os.IsNotExist(err) {
		t.Fatalf("empty nas.json left behind: %v", err)
	}
}

// The console's network source (the slot) and the WebDAV list are one place: a
// slot WebDAV server is edited and removed in place; when SMB takes the slot the
// WebDAV server moves to the list with its ID; the console cannot add a server twice.
func TestWebDAVConsoleSlotAndList(t *testing.T) {
	r := newWebDAVRig(t, releasedWebDAV, "")
	slot := webdavRoot("https://dav.example:8443/Music/")
	list := r.change(t, webdavChange{Action: "edit", ID: slot, Name: "Home", URL: "https://dav.example:8443/Music/", Username: "dj"}, 200)
	if f := r.file(t); f.Protocol != webdavScope || f.Name != "Home" || f.Password != "wd-secret-1" || len(f.WebDAV) != 0 || list[0].Name != "Home" {
		t.Fatalf("slot edit: %+v %+v", f, list)
	}
	r.change(t, webdavChange{Action: "add", URL: "http://club.example/dav", Password: "pw-c"}, 200)
	post := func(body string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		r.s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/api/nas", strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("console POST %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	post(`{"protocol":"webdav","url":"http://club.example/dav/","enabled":true}`, http.StatusConflict)
	post(`{"protocol":"smb","url":"smb://nas.example/music","username":"dj","password":"smb-pw","enabled":true}`, 200)
	v := r.status(t)
	if !v.NASSetUp || !slices.Contains(v.Sources, nasScope) {
		t.Fatalf("SMB not the NAS after the console saved it: %+v", v.Sources)
	}
	if len(v.WebDAV) != 2 || v.WebDAV[0].ID != slot || v.WebDAV[0].Name != "Home" || v.WebDAV[1].Name != "club.example" {
		t.Fatalf("WebDAV servers after SMB took the slot: %+v", v.WebDAV)
	}
	if f := r.file(t); f.WebDAV[0].Password != "wd-secret-1" || f.Password != "smb-pw" {
		t.Fatal("a password was lost when the slot changed")
	}
	w := httptest.NewRecorder()
	r.s.handleNASConfig(w, httptest.NewRequest(http.MethodDelete, "/api/nas", nil))
	if w.Code != 200 {
		t.Fatalf("console DELETE: %d", w.Code)
	}
	if f := r.file(t); f.Protocol != "" || len(f.WebDAV) != 2 {
		t.Fatalf("console DELETE touched the WebDAV list: %+v", f)
	}
	// A slot WebDAV server removed on the deck empties the slot only.
	r3 := newWebDAVRig(t, releasedWebDAV, "")
	r3.change(t, webdavChange{Action: "add", URL: "http://club.example/dav"}, 200)
	r3.change(t, webdavChange{Action: "remove", ID: slot}, 200)
	if f := r3.file(t); f.Protocol != "" || f.URL != "" || f.Password != "" || len(f.WebDAV) != 1 {
		t.Fatalf("slot removal: %+v", f)
	}
}

// The phone page: the QR code holds a one-time address (no password), the page
// lists the servers without passwords, a change is saved once and spends the token.
func TestWebDAVPhonePage(t *testing.T) {
	r := newWebDAVRig(t, releasedWebDAV, "")
	start := func() loginFlow {
		t.Helper()
		resp, err := http.Post(r.gw.URL+"/v1/enginex/login?provider=webdav", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var f loginFlow
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&f) != nil {
			t.Fatalf("login start: HTTP %d", resp.StatusCode)
		}
		return f
	}
	f := start()
	u, err := url.Parse(f.Target)
	if err != nil || u.Path != "/m/webdav" || u.Query().Get("t") == "" || strings.Contains(f.Target, "wd-secret-1") {
		t.Fatalf("QR target %q", f.Target)
	}
	page := func(want int) string {
		t.Helper()
		resp, err := http.Get(r.gw.URL + u.RequestURI())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("phone page: HTTP %d, want %d", resp.StatusCode, want)
		}
		return string(b)
	}
	body := page(200)
	if !strings.Contains(body, "dav.example:8443") || !strings.Contains(body, `name="action" value="add"`) || strings.Contains(body, "wd-secret-1") {
		t.Fatalf("phone page content: %.400s", body)
	}
	form := func(vals url.Values) string {
		t.Helper()
		resp, err := http.PostForm(r.gw.URL+u.RequestURI(), vals)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	status := func() string {
		t.Helper()
		var s loginFlow
		tidalGet(t, r.gw.URL, "/v1/enginex/login?provider=webdav", 200, &s)
		return s.Status
	}
	// A bad address keeps the token for another try.
	if got := form(url.Values{"action": {"add"}, "url": {"ftp://x.example/"}}); !strings.Contains(got, tr(0, "wdBadURL")) || status() != "waiting" {
		t.Fatalf("bad address: status=%s page=%s", status(), got[max(0, len(got)-300):])
	}
	got := form(url.Values{"action": {"add"}, "name": {"Phone"}, "url": {"https://phone.example/dav"}, "user": {"me"}, "password": {"phone-pw"}})
	if !strings.Contains(got, tr(0, "wdSaved")) || status() != "done" {
		t.Fatalf("phone add: %.300s status=%s", got, status())
	}
	page(http.StatusForbidden) // spent
	if v := r.status(t); !slices.Equal(webdavNames(v.WebDAV), []string{"dav.example:8443", "Phone"}) {
		t.Fatalf("servers after the phone add: %v", webdavNames(v.WebDAV))
	}
	if fl := r.file(t); fl.WebDAV[0].Password != "phone-pw" {
		t.Fatal("phone password not saved in nas.json")
	}
	// Edit and remove the same way as on the deck.
	f = start()
	u, _ = url.Parse(f.Target)
	phone := webdavRoot("https://phone.example/dav")
	form(url.Values{"action": {"edit"}, "id": {phone}, "name": {"Phone 2"}, "url": {"https://phone.example/dav"}, "user": {"me"}})
	if fl := r.file(t); fl.WebDAV[0].Name != "Phone 2" || fl.WebDAV[0].Password != "phone-pw" {
		t.Fatalf("phone edit: %+v", fl.WebDAV)
	}
	f = start()
	u, _ = url.Parse(f.Target)
	form(url.Values{"action": {"remove"}, "id": {webdavRoot("https://dav.example:8443/Music/")}})
	if v := r.status(t); !slices.Equal(webdavNames(v.WebDAV), []string{"Phone 2"}) {
		t.Fatalf("servers after the phone remove: %v", webdavNames(v.WebDAV))
	}
	// The icon the page shows is served.
	tidalGet(t, r.gw.URL, "/icons/webdav", 200, nil)
}
