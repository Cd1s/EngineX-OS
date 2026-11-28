package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/broadcast"
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
	return newWebDAVRigAt(t, uiDir, stateDir, dlDir)
}

func newWebDAVRigAt(t *testing.T, uiDir, stateDir, dlDir string) *webdavRig {
	t.Helper()
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

// changeKind posts one add, edit or remove of a kind's server as the deck does.
func (r *webdavRig) changeKind(t *testing.T, kind string, c serverChange, want int) []serverView {
	t.Helper()
	body, _ := json.Marshal(c)
	resp, err := http.Post(r.gw.URL+"/v1/enginex/servers/"+kind, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s %q: HTTP %d, want %d: %s", kind, c.Action, c.URL, resp.StatusCode, want, data)
	}
	var out struct{ Servers []serverView }
	_ = json.Unmarshal(data, &out)
	return out.Servers
}

func (r *webdavRig) change(t *testing.T, c serverChange, want int) []serverView {
	t.Helper()
	return r.changeKind(t, webdavScope, c, want)
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

func webdavNames(v []serverView) []string {
	var out []string
	for _, s := range v {
		out = append(out, s.Name)
	}
	return out
}

// serverSources are the Sources IDs of servers in a status (WebDAV and NAS).
func serverSources(v uiSettings) []string {
	var out []string
	for _, id := range v.Sources {
		if _, _, ok := serverScopeOf(id); ok || id == nasScope {
			out = append(out, id)
		}
	}
	return out
}

const releasedWebDAV = `{"protocol":"webdav","url":"https://dav.example:8443/Music/","username":"dj","password":"wd-secret-1","enabled":true}`
const releasedSMB = `{"protocol":"smb","url":"smb://nas.example/music","username":"dj","password":"smb-secret-1","enabled":true}`

// A deck that set WebDAV or SMB up on a released version gets server 1 of that kind
// with no re-entry: the server stays in nas.json's slot, its kind starts on, and it
// is its own entry in Sources under its host name.
func TestServersReleasedSetupIsServerOne(t *testing.T) {
	r := newWebDAVRig(t, releasedWebDAV, "lang=en\nsources=deezer\nknown=tidal,netease,spotify,deezer,soundcloud,beatport,applemusic\n")
	v := r.status(t)
	id := serverRoot(kindWebDAV, "https://dav.example:8443/Music/")
	want := serverView{ID: id, Name: "dav.example:8443", URL: "https://dav.example:8443/Music/", Username: "dj", HasPassword: true, Scope: "webdav:" + id}
	if len(v.WebDAV) != 1 || v.WebDAV[0] != want || !v.WebDAVOn {
		t.Fatalf("servers = %+v on=%v, want [%+v]", v.WebDAV, v.WebDAVOn, want)
	}
	if !slices.Equal(serverSources(v), []string{want.Scope}) || !slices.Contains(v.LoggedIn, want.Scope) {
		t.Fatalf("released WebDAV server is not its own source: sources=%v loggedIn=%v", v.Sources, v.LoggedIn)
	}
	if slices.Contains(v.Sources, webdavScope) || slices.Contains(v.Sources, nasScope) || v.NASOn {
		t.Fatalf("a kind or NAS listed for a WebDAV server: %v nasOn=%v", v.Sources, v.NASOn)
	}
	if code, _ := uiPost(t, r.gw.URL, "scope="+want.Scope); code != http.StatusOK {
		t.Fatalf("scope=%s: HTTP %d", want.Scope, code)
	}
	raw := tidalGet(t, r.gw.URL, "/v1/enginex/ui", 200, nil)
	list := tidalGet(t, r.gw.URL, "/v1/enginex/servers/webdav", 200, nil)
	if bytes.Contains(raw, []byte("wd-secret-1")) || bytes.Contains(list, []byte("wd-secret-1")) {
		t.Fatal("a WebDAV password reached the deck's status or list")
	}

	r = newWebDAVRig(t, releasedSMB, "")
	v = r.status(t)
	nas := serverView{ID: serverRoot(kindNAS, "smb://nas.example/music"), Name: "nas.example", URL: "smb://nas.example/music", Username: "dj", HasPassword: true, Scope: nasScope}
	if len(v.NAS) != 1 || v.NAS[0] != nas || !v.NASOn || !slices.Equal(serverSources(v), []string{nasScope}) {
		t.Fatalf("released NAS: servers=%+v on=%v sources=%v", v.NAS, v.NASOn, v.Sources)
	}
	if bytes.Contains(tidalGet(t, r.gw.URL, "/v1/enginex/servers/nas", 200, nil), []byte("smb-secret-1")) {
		t.Fatal("a NAS password reached the deck's list")
	}
}

// The WebDAV and NAS rows in Services: each turns all servers of its kind on or off,
// both on/off combinations, and the other switches still work while server entries,
// NAS or rekordbox are in the status list.
func TestServersSwitches(t *testing.T) {
	r := newWebDAVRig(t, releasedSMB, "sources=deezer\nscope=\nomni_source=0\n")
	v := r.status(t)
	if v.WebDAVOn || !v.NASOn {
		t.Fatalf("defaults: webdavOn=%v nasOn=%v", v.WebDAVOn, v.NASOn)
	}
	// WebDAV on with no server yet: on, nothing to list.
	if code, v := uiPost(t, r.gw.URL, "webdav=1"); code != 200 || !v.WebDAVOn || len(serverSources(v)) != 1 {
		t.Fatalf("webdav=1: %d on=%v sources=%v", code, v.WebDAVOn, v.Sources)
	}
	wd := r.change(t, serverChange{Action: "add", Name: "Studio", URL: "http://dav.example/music", Username: "dj", Password: "wd-pw"}, 200)
	nas := r.changeKind(t, nasScope, serverChange{Action: "add", Name: "Club NAS", URL: "smb://club.example/sets", Username: "dj", Password: "smb-pw"}, 200)
	if len(wd) != 1 || len(nas) != 2 || nas[1].Scope != "nas:"+nas[1].ID {
		t.Fatalf("servers: webdav=%+v nas=%+v", wd, nas)
	}
	for _, c := range []struct{ webdav, nas string }{{"1", "1"}, {"1", "0"}, {"0", "1"}, {"0", "0"}, {"1", "1"}} {
		uiPost(t, r.gw.URL, "webdav="+c.webdav)
		uiPost(t, r.gw.URL, "nas="+c.nas)
		v := r.status(t)
		var want []string
		if c.nas == "1" {
			want = append(want, nas[1].Scope, nasScope) // the slot's NAS is added after the servers
		}
		if c.webdav == "1" {
			want = append([]string{wd[0].Scope}, want...)
		}
		got := serverSources(v)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) || v.WebDAVOn != (c.webdav == "1") || v.NASOn != (c.nas == "1") {
			t.Fatalf("webdav=%s nas=%s: sources=%v on=%v/%v", c.webdav, c.nas, got, v.WebDAVOn, v.NASOn)
		}
		if got := len(r.s.servers(kindWebDAV)) > 0; got != (c.webdav == "1") {
			t.Fatalf("webdav=%s: servers in use=%v", c.webdav, got)
		}
		if got := r.s.ownerSMBSelfHosted() != nil; got != (c.nas == "1") {
			t.Fatalf("nas=%s: NAS slot provider=%v", c.nas, got)
		}
		for _, scope := range []string{wd[0].Scope, nas[1].Scope, nasScope} {
			on := c.nas == "1"
			if scope == wd[0].Scope {
				on = c.webdav == "1"
			}
			want := map[bool]int{true: http.StatusOK, false: http.StatusConflict}[on]
			if code, _ := uiPost(t, r.gw.URL, "scope="+scope); code != want {
				t.Fatalf("webdav=%s nas=%s: scope=%s HTTP %d, want %d", c.webdav, c.nas, scope, code, want)
			}
		}
	}
	// The NAS row is not the console's "enabled": nas.json keeps it.
	if f := r.file(t); !f.Enabled || f.Protocol != "smb" || len(f.SMB) != 1 || len(f.WebDAV) != 1 {
		t.Fatalf("nas.json after the switches: %+v", f)
	}
	// Turning a kind off while one of its servers is selected leaves no source selected.
	uiPost(t, r.gw.URL, "scope="+nas[1].Scope)
	if _, v := uiPost(t, r.gw.URL, "nas=0"); v.Scope != "" {
		t.Fatalf("scope after nas=0: %q", v.Scope)
	}
	uiPost(t, r.gw.URL, "scope="+wd[0].Scope)
	if _, v := uiPost(t, r.gw.URL, "webdav=0"); v.Scope != "" {
		t.Fatalf("scope after webdav=0: %q", v.Scope)
	}
	// The deck sends its whole list back when a service is flipped: server entries,
	// NAS and rekordbox in it are not refused and not saved as services.
	uiPost(t, r.gw.URL, "webdav=1")
	uiPost(t, r.gw.URL, "nas=1")
	if code, v := uiPost(t, r.gw.URL, "sources=deezer,spotify,nas,webdav,rekordbox,"+wd[0].Scope+","+nas[1].Scope); code != 200 || !slices.Contains(v.Sources, "spotify") {
		t.Fatalf("sources with status-only entries: %d %v", code, v.Sources)
	}
	raw, _ := os.ReadFile(filepath.Join(r.uiDir, "settings.conf"))
	for key, want := range map[string]string{"sources": "spotify,deezer", "webdav": "1", "nas": "1"} {
		if line := settingsLine(string(raw), key); line != want {
			t.Fatalf("saved %s = %q, want %q", key, line, want)
		}
	}
	// No server of a kind yet: turning it on is allowed (the panel opens to add one).
	r2 := newWebDAVRig(t, "", "")
	if code, v := uiPost(t, r2.gw.URL, "nas=1"); code != 200 || !v.NASOn || len(serverSources(v)) != 0 {
		t.Fatalf("nas=1 with no NAS: %d on=%v %v", code, v.NASOn, v.Sources)
	}
	if code, _ := uiPost(t, r2.gw.URL, "scope=nas"); code != http.StatusConflict {
		t.Fatalf("scope=nas with no NAS: HTTP %d", code)
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

// Several servers of each kind: added, edited and removed; a name is asked (empty:
// the host); passwords only in nas.json (0600).
func TestServersAddEditRemove(t *testing.T) {
	cases := []struct {
		kind              string
		a, b, aAgain, bTo string
		bad               []string
	}{
		{webdavScope, "https://dav.example/music/", "http://other.example:8080/dj", "https://DAV.example:443/music", "http://other.example:8080/dj2",
			[]string{"smb://dav.example/music", "ftp://dav.example/", "dav.example/music", "https://user:pw@dav.example/", ""}},
		{nasScope, "smb://nas.example/music/", "smb://other.example:4450/dj", "smb://NAS.example:445/music", "smb://other.example:4450/dj2",
			[]string{"https://nas.example/music", "nas.example/music", "smb://user:pw@nas.example/music", ""}},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			r := newWebDAVRig(t, "", c.kind+"=1\n")
			r.changeKind(t, c.kind, serverChange{Action: "add", Name: "Studio", URL: c.a, Username: "dj", Password: "pw-a"}, 200)
			list := r.changeKind(t, c.kind, serverChange{Action: "add", URL: c.b, Username: "dj2", Password: "pw-b"}, 200)
			if got := webdavNames(list); !slices.Equal(got, []string{"Studio", serverHost(c.b)}) {
				t.Fatalf("names = %v", got)
			}
			a, b := list[0], list[1]
			r.changeKind(t, c.kind, serverChange{Action: "add", URL: c.aAgain, Username: "x"}, http.StatusConflict)
			for _, bad := range c.bad {
				r.changeKind(t, c.kind, serverChange{Action: "add", URL: bad}, http.StatusBadRequest)
			}
			r.changeKind(t, c.kind, serverChange{Action: "edit", ID: "0123456789abcdef0123456789abcdef", URL: c.a}, http.StatusNotFound)
			list = r.changeKind(t, c.kind, serverChange{Action: "edit", ID: b.ID, Name: "Club", URL: b.URL, Username: "dj2"}, 200)
			if list[1].Name != "Club" || list[1].ID != b.ID || !list[1].HasPassword {
				t.Fatalf("after edit: %+v", list[1])
			}
			saved := r.file(t)
			if (*saved.list(map[string]serverKind{webdavScope: kindWebDAV, nasScope: kindNAS}[c.kind]))[1].Password != "pw-b" {
				t.Fatal("an edit without a password dropped the saved one")
			}
			r.changeKind(t, c.kind, serverChange{Action: "edit", ID: b.ID, URL: a.URL}, http.StatusConflict)
			list = r.changeKind(t, c.kind, serverChange{Action: "edit", ID: b.ID, Name: "Club", URL: c.bTo, Username: "dj2"}, 200)
			if list[1].ID == b.ID || list[1].Scope != c.kind+":"+list[1].ID {
				t.Fatalf("moved server kept its ID or scope: %+v", list[1])
			}
			list = r.changeKind(t, c.kind, serverChange{Action: "remove", ID: a.ID}, 200)
			if got := webdavNames(list); !slices.Equal(got, []string{"Club"}) {
				t.Fatalf("after remove: %v", got)
			}
			r.changeKind(t, c.kind, serverChange{Action: "remove", ID: a.ID}, http.StatusNotFound)
			info, err := os.Stat(filepath.Join(r.uiDir, "nas.json"))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("nas.json mode: %v %v", info, err)
			}
			r.changeKind(t, c.kind, serverChange{Action: "remove", ID: list[0].ID}, 200)
			if _, err := os.Stat(filepath.Join(r.uiDir, "nas.json")); !os.IsNotExist(err) {
				t.Fatalf("empty nas.json left behind: %v", err)
			}
		})
	}
	// The same address is one server across both lists only within a kind: a WebDAV
	// and an SMB server are different servers.
	r := newWebDAVRig(t, "", "")
	r.change(t, serverChange{Action: "add", URL: "http://host.example/music"}, 200)
	r.changeKind(t, nasScope, serverChange{Action: "add", URL: "smb://host.example/music"}, 200)
	r.changeKind(t, "cloud", serverChange{Action: "add", URL: "smb://x.example/y"}, http.StatusNotFound)
}

// The console's network source (the slot) and the lists are one place: a slot
// server is edited and removed in place; when another kind takes the slot, the
// WebDAV or SMB server there moves to its list with its ID and name; the console
// cannot add a listed server again.
func TestServersConsoleSlotAndList(t *testing.T) {
	r := newWebDAVRig(t, releasedWebDAV, "")
	slot := serverRoot(kindWebDAV, "https://dav.example:8443/Music/")
	list := r.change(t, serverChange{Action: "edit", ID: slot, Name: "Home", URL: "https://dav.example:8443/Music/", Username: "dj"}, 200)
	if f := r.file(t); f.Protocol != webdavScope || f.Name != "Home" || f.Password != "wd-secret-1" || len(f.WebDAV) != 0 || list[0].Name != "Home" {
		t.Fatalf("slot edit: %+v %+v", f, list)
	}
	r.change(t, serverChange{Action: "add", URL: "http://club.example/dav", Password: "pw-c"}, 200)
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
	if !v.NASOn || !slices.Contains(v.Sources, nasScope) || len(v.NAS) != 1 || v.NAS[0].Scope != nasScope {
		t.Fatalf("SMB not NAS server 1 after the console saved it: %v %+v", v.Sources, v.NAS)
	}
	if len(v.WebDAV) != 2 || v.WebDAV[0].ID != slot || v.WebDAV[0].Name != "Home" || v.WebDAV[1].Name != "club.example" {
		t.Fatalf("WebDAV servers after SMB took the slot: %+v", v.WebDAV)
	}
	if f := r.file(t); f.WebDAV[0].Password != "wd-secret-1" || f.Password != "smb-pw" {
		t.Fatal("a password was lost when the slot changed")
	}
	// The slot's SMB server named on the deck, then moved to the SMB list when the
	// console puts Subsonic in the slot: same ID and name, now "nas:<id>".
	nas := v.NAS[0].ID // the server the console put in the slot (round 3: a new identity, not its address's)
	r.changeKind(t, nasScope, serverChange{Action: "edit", ID: nas, Name: "Studio NAS", URL: "smb://nas.example/music", Username: "dj"}, 200)
	post(`{"protocol":"smb","url":"smb://nas.example/music/","enabled":true}`, 200) // the same server again: stays in the slot
	r.changeKind(t, nasScope, serverChange{Action: "add", URL: "smb://club.example/sets"}, 200)
	post(`{"protocol":"smb","url":"smb://club.example/sets","enabled":true}`, http.StatusConflict)
	post(`{"protocol":"subsonic","url":"http://sub.example:4533","enabled":true}`, 200)
	v = r.status(t)
	if len(v.NAS) != 2 || v.NAS[0].ID != nas || v.NAS[0].Name != "Studio NAS" || v.NAS[0].Scope != "nas:"+nas {
		t.Fatalf("NAS servers after Subsonic took the slot: %+v", v.NAS)
	}
	w := httptest.NewRecorder()
	r.s.handleNASConfig(w, httptest.NewRequest(http.MethodDelete, "/api/nas", nil))
	if f := r.file(t); w.Code != 200 || f.Protocol != "" || len(f.WebDAV) != 2 || len(f.SMB) != 2 {
		t.Fatalf("console DELETE touched the lists: %d %+v", w.Code, f)
	}
	// A slot server removed on the deck empties the slot only.
	r3 := newWebDAVRig(t, releasedSMB, "")
	r3.changeKind(t, nasScope, serverChange{Action: "add", URL: "smb://club.example/sets"}, 200)
	r3.changeKind(t, nasScope, serverChange{Action: "remove", ID: serverRoot(kindNAS, "smb://nas.example/music")}, 200)
	if f := r3.file(t); f.Protocol != "" || f.URL != "" || f.Password != "" || len(f.SMB) != 1 {
		t.Fatalf("slot removal: %+v", f)
	}
}

// The phone pages (WebDAV and NAS): the QR code holds a one-time address (no
// password), the page lists the servers without passwords, a change is saved once
// and spends the token.
func TestServersPhonePage(t *testing.T) {
	for _, c := range []struct{ kind, cfg, host, secret, addURL, badURL, badText string }{
		{webdavScope, releasedWebDAV, "dav.example:8443", "wd-secret-1", "https://phone.example/dav", "ftp://x.example/", "wdBadURL"},
		{nasScope, releasedSMB, "nas.example", "smb-secret-1", "smb://phone.example/share", "http://x.example/", "nasBadURL"},
	} {
		t.Run(c.kind, func(t *testing.T) {
			r := newWebDAVRig(t, c.cfg, "")
			start := func() *url.URL {
				t.Helper()
				resp, err := http.Post(r.gw.URL+"/v1/enginex/login?provider="+c.kind, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				var f loginFlow
				if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&f) != nil {
					t.Fatalf("login start: HTTP %d", resp.StatusCode)
				}
				u, err := url.Parse(f.Target)
				if err != nil || u.Path != "/m/"+c.kind || u.Query().Get("t") == "" || strings.Contains(f.Target, c.secret) {
					t.Fatalf("QR target %q", f.Target)
				}
				return u
			}
			u := start()
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
			if !strings.Contains(body, c.host) || !strings.Contains(body, `name="action" value="add"`) || strings.Contains(body, c.secret) {
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
				tidalGet(t, r.gw.URL, "/v1/enginex/login?provider="+c.kind, 200, &s)
				return s.Status
			}
			if got := form(url.Values{"action": {"add"}, "url": {c.badURL}}); !strings.Contains(got, tr(0, c.badText)) || status() != "waiting" {
				t.Fatalf("bad address: status=%s page=%s", status(), got[max(0, len(got)-300):])
			}
			got := form(url.Values{"action": {"add"}, "name": {"Phone"}, "url": {c.addURL}, "user": {"me"}, "password": {"phone-pw"}})
			if !strings.Contains(got, tr(0, "wdSaved")) || status() != "done" {
				t.Fatalf("phone add: %.300s status=%s", got, status())
			}
			page(http.StatusForbidden) // spent
			k, _ := kindOf(c.kind)
			views := serverViews(k, r.file(t))
			if !slices.Equal(webdavNames(views), []string{c.host, "Phone"}) {
				t.Fatalf("servers after the phone add: %v", webdavNames(views))
			}
			if fl := r.file(t); (*fl.list(k))[0].Password != "phone-pw" {
				t.Fatal("phone password not saved in nas.json")
			}
			u = start()
			phone := views[1].ID // the server the phone added (round 3: its own identity)
			form(url.Values{"action": {"edit"}, "id": {phone}, "name": {"Phone 2"}, "url": {c.addURL}, "user": {"me"}})
			if fl := r.file(t); (*fl.list(k))[0].Name != "Phone 2" || (*fl.list(k))[0].Password != "phone-pw" {
				t.Fatalf("phone edit: %+v", *fl.list(k))
			}
			u = start()
			form(url.Values{"action": {"remove"}, "id": {views[0].ID}})
			if got := webdavNames(serverViews(k, r.file(t))); !slices.Equal(got, []string{"Phone 2"}) {
				t.Fatalf("servers after the phone remove: %v", got)
			}
			tidalGet(t, r.gw.URL, "/icons/"+c.kind, 200, nil)
		})
	}
}

// Round 3 (review of the WebDAV branch, item 1): a server moved to another address
// with the password field left empty kept the old password, in all four places
// (WebDAV and SMB, the slot and the list), and the console card kept the slot's
// password and token the same way. A new address never takes the old credentials;
// an empty field keeps them only for the same address.
func TestServersNewAddressNeverTakesTheOldPassword(t *testing.T) {
	const marker = "old-pass-r3-marker"
	type place struct {
		name, kind, fixture, from, to string
	}
	slotWebDAV := fmt.Sprintf(`{"protocol":"webdav","url":"https://dav.example/Music/","username":"dj","password":%q,"enabled":true}`, marker)
	slotSMB := fmt.Sprintf(`{"protocol":"smb","url":"smb://nas.example/music","username":"dj","password":%q,"enabled":true}`, marker)
	listWebDAV := fmt.Sprintf(`{"webdav":[{"url":"https://club.example/dav/","username":"dj","password":%q,"enabled":true}]}`, marker)
	listSMB := fmt.Sprintf(`{"smb":[{"url":"smb://club.example/sets","username":"dj","password":%q,"enabled":true}]}`, marker)
	for _, c := range []place{
		{"webdav slot", webdavScope, slotWebDAV, "https://dav.example/Music/", "https://other.example/Music/"},
		{"smb slot", nasScope, slotSMB, "smb://nas.example/music", "smb://other.example/music"},
		{"webdav list", webdavScope, listWebDAV, "https://club.example/dav/", "https://other.example/dav/"},
		{"smb list", nasScope, listSMB, "smb://club.example/sets", "smb://other.example/sets"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newWebDAVRig(t, c.fixture, "")
			k, _ := kindOf(c.kind)
			id := serverViews(k, r.file(t))[0].ID
			// The same address with the field empty keeps the password.
			list := r.changeKind(t, c.kind, serverChange{Action: "edit", ID: id, Name: "Same", URL: c.from, Username: "dj"}, 200)
			if list[0].ID != id || !list[0].HasPassword {
				t.Fatalf("the same address lost its password or identity: %+v", list)
			}
			// Another address with the field empty: no password, and none anywhere in nas.json.
			list = r.changeKind(t, c.kind, serverChange{Action: "edit", ID: id, Name: "Moved", URL: c.to, Username: "dj"}, 200)
			if len(list) != 1 || list[0].HasPassword {
				t.Fatalf("the server moved to %s still has a password: %+v", c.to, list)
			}
			raw, err := os.ReadFile(filepath.Join(r.uiDir, "nas.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), marker) {
				t.Fatalf("the old password stays in nas.json for the new address: %s", raw)
			}
			if list[0].ID == id {
				t.Fatalf("the server moved to another address kept its identity %s", id)
			}
		})
	}
	t.Run("console card", func(t *testing.T) {
		const token = "old-token-r3-marker"
		fixture := fmt.Sprintf(`{"protocol":"plex","url":"http://plex.example:32400","username":"dj","password":%q,"token":%q,"enabled":true}`, marker, token)
		r := newWebDAVRig(t, fixture, "")
		post := func(body string) {
			t.Helper()
			w := httptest.NewRecorder()
			r.s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/api/nas", strings.NewReader(body)))
			if w.Code != http.StatusOK {
				t.Fatalf("console POST %s: %d %s", body, w.Code, w.Body.String())
			}
		}
		post(`{"protocol":"plex","url":"http://plex.example:32400/","username":"dj","password":"***","token":"***","enabled":true}`)
		if f := r.file(t); f.Password != marker || f.Token != token {
			t.Fatalf("the same server lost its password or token: %+v", f.NASConfig)
		}
		post(`{"protocol":"plex","url":"http://other.example:32400","username":"dj","password":"","token":"","enabled":true}`)
		f := r.file(t)
		if f.Password != "" || f.Token != "" {
			t.Fatalf("the console's new address kept the old credentials: password %v, token %v", f.Password != "", f.Token != "")
		}
	})
}

// Round 4 (round-10 re-check, item 3) and its class: every editor that keeps a saved
// credential keeps it only for the same address.
//   - The console Mounts card: on 7a1424de a mount moved to another host with its
//     password field left empty kept the old password, for WebDAV and SMB alike, and the
//     WebDAV mount copied it into nas.json. Now another host, port or remote path takes
//     no password along (SFTP too); the same address with the field empty or "***" keeps
//     it.
//   - The NAS card's test of a server: on 7a1424de a test of another address was sent the
//     saved password and token.
//   - The broadcast settings: a changed scheme counts as another address too.
func TestEditorsNeverTakeASavedCredentialToANewAddress(t *testing.T) {
	const marker = "mount-pass-r4-marker"
	for _, proto := range []string{"webdav", "smb", "sftp"} {
		t.Run("mounts "+proto, func(t *testing.T) {
			r := newWebDAVRigAt(t, t.TempDir(), t.TempDir(), t.TempDir())
			mountPoint := filepath.Join(t.TempDir(), "Share")
			post := func(body map[string]any) {
				t.Helper()
				b, _ := json.Marshal(body)
				w := httptest.NewRecorder()
				r.s.handleMounts(w, httptest.NewRequest(http.MethodPost, "/api/mounts", bytes.NewReader(b)))
				if w.Code != http.StatusOK {
					t.Fatalf("mounts POST %v: %d %s", body, w.Code, w.Body.String())
				}
			}
			password := func() string {
				for _, m := range r.s.loadMounts() {
					if m.ID == "m1" {
						return m.Password
					}
				}
				t.Fatal("the mount is gone")
				return ""
			}
			mount := func(change map[string]any) map[string]any {
				m := map[string]any{"id": "m1", "name": "Share", "protocol": proto, "host": "nas.example", "remotePath": "/music", "username": "dj", "password": "", "enabled": false, "mountPoint": mountPoint}
				for k, v := range change {
					m[k] = v
				}
				return m
			}
			post(mount(map[string]any{"password": marker}))
			for _, keep := range []map[string]any{{}, {"password": "***"}, {"remotePath": "music/"}, {"host": "NAS.example"}} {
				post(mount(keep))
				if password() != marker {
					t.Fatalf("the same address (%v) lost its password", keep)
				}
			}
			for _, moved := range []map[string]any{{"host": "other.example"}, {"port": 8443}, {"remotePath": "/other"}, {"host": "other.example", "password": "***"}} {
				post(mount(map[string]any{"password": marker})) // back at the first address with its password
				post(mount(moved))
				if got := password(); got != "" {
					t.Fatalf("the mount moved (%v) kept the old password", moved)
				}
				if raw, err := os.ReadFile(filepath.Join(r.uiDir, "nas.json")); err == nil && strings.Contains(string(raw), marker) {
					t.Fatalf("the mount moved (%v): nas.json holds the old password", moved)
				}
			}
		})
	}
	for _, protocol := range []string{"webdav", "plex"} {
		t.Run("nas test "+protocol, func(t *testing.T) {
			var mu sync.Mutex
			var sent []string
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				sent = append(sent, r.Header.Get("Authorization")+" "+r.Header.Get("X-Plex-Token")+" "+r.URL.RawQuery)
				mu.Unlock()
				http.Error(w, "no", http.StatusUnauthorized)
			}))
			defer other.Close()
			saved := fmt.Sprintf(`{"protocol":%q,"url":"http://nas.example/music/","username":"dj","password":%q,"token":%q,"enabled":true}`, protocol, marker, marker)
			r := newWebDAVRig(t, saved, "")
			body := fmt.Sprintf(`{"protocol":%q,"url":%q,"username":"dj","password":"","token":"***","enabled":true}`, protocol, other.URL+"/music/")
			w := httptest.NewRecorder()
			r.s.handleNASTest(w, httptest.NewRequest(http.MethodPost, "/api/nas/test", strings.NewReader(body)))
			mu.Lock()
			defer mu.Unlock()
			if len(sent) == 0 {
				t.Fatalf("the test never reached the other server (HTTP %d %s)", w.Code, w.Body.String())
			}
			for _, s := range sent {
				if strings.Contains(s, marker) || strings.Contains(s, "Basic "+base64.StdEncoding.EncodeToString([]byte("dj:"+marker))) {
					t.Fatalf("the test of another server was sent the saved credential: %q", s)
				}
			}
		})
	}
	t.Run("broadcast scheme", func(t *testing.T) {
		s, _ := newTestServer(t)
		post := func(payload map[string]any) broadcast.Status {
			t.Helper()
			body, _ := json.Marshal(payload)
			resp := doAuthRequest(t, s, "POST", "/api/broadcast", body)
			defer resp.Body.Close()
			var st broadcast.Status
			if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&st) != nil {
				t.Fatalf("broadcast POST: HTTP %d", resp.StatusCode)
			}
			return st
		}
		if st := post(map[string]any{"protocol": "icecast", "serverUrl": "http://cast.example:8000", "mountpoint": "/club", "password": marker}); !st.PasswordSet {
			t.Fatal("the password was not saved")
		}
		if st := post(map[string]any{"serverUrl": "https://cast.example:8000"}); st.PasswordSet {
			t.Fatal("the broadcast password stayed for another scheme on the same host and port")
		}
	})
}
