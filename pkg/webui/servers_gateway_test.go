package webui

import (
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// fakeDAV is a WebDAV server with the same folders as every other one here:
// /music/intro.mp3 and /music/House/{a,b}.mp3. Its audio says which server it is.
type fakeDAV struct {
	srv   *httptest.Server
	audio []byte
	finds atomic.Int32 // PROPFIND requests
	gets  atomic.Int32
}

func newFakeDAV(t *testing.T, audio []byte, hold <-chan struct{}) *fakeDAV {
	t.Helper()
	d := &fakeDAV{audio: audio}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			d.finds.Add(1)
			if hold != nil { // a slow server
				select {
				case <-hold:
				case <-r.Context().Done():
					return
				}
			}
			var body string
			switch strings.TrimSuffix(r.URL.Path, "/") {
			case "/music":
				body = davXML("/music/", "/music/House/", "/music/intro.mp3")
			case "/music/House":
				body = davXML("/music/House/", "/music/House/a.mp3", "/music/House/b.mp3")
			default:
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			io.WriteString(w, body)
		case http.MethodGet:
			d.gets.Add(1)
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(d.audio)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func davXML(hrefs ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:">`)
	for _, h := range hrefs {
		kind := ""
		if strings.HasSuffix(h, "/") {
			kind = "<d:collection/>"
		}
		b.WriteString(`<d:response><d:href>` + h + `</d:href><d:propstat><d:prop><d:resourcetype>` + kind + `</d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
	}
	b.WriteString(`</d:multistatus>`)
	return b.String()
}

type gwItem = map[string]any

// open selects a Sources entry and returns its lists by title.
func (r *webdavRig) open(t *testing.T, scope string) map[string]string {
	t.Helper()
	if code, _ := uiPost(t, r.gw.URL, "scope="+scope); code != http.StatusOK {
		t.Fatalf("scope=%s: HTTP %d", scope, code)
	}
	return r.lists(t)
}

func (r *webdavRig) lists(t *testing.T) map[string]string {
	t.Helper()
	var page struct{ Items []gwItem }
	tidalGet(t, r.gw.URL, "/v1/users/1/playlists?limit=100&offset=0", 200, &page)
	out := map[string]string{}
	for _, it := range page.Items {
		out[fmt.Sprint(it["title"])] = fmt.Sprint(it["uuid"])
	}
	return out
}

func (r *webdavRig) tracks(t *testing.T, uuid string) map[string]gwItem {
	t.Helper()
	var page struct{ Items []gwItem }
	tidalGet(t, r.gw.URL, "/v1/playlists/"+uuid+"/tracks?limit=100&offset=0", 200, &page)
	out := map[string]gwItem{}
	for _, it := range page.Items {
		out[fmt.Sprint(it["title"])] = it
	}
	return out
}

func rowID(it gwItem) int64 { return int64(it["id"].(float64)) }

func refOf(g *tidalGateway, id int64) trackRef {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tracks[id]
}

func rowAlbum(it gwItem) string {
	if al, ok := it["album"].(map[string]any); ok {
		return fmt.Sprint(al["title"])
	}
	return ""
}

// stream plays a row as the deck does and returns the status and the audio.
func (r *webdavRig) stream(t *testing.T, id int64) (int, []byte) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", r.gw.URL, id))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	resp, err = http.Get(fmt.Sprintf("%s/v1/stream/%d.mp3", r.gw.URL, id))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (r *webdavRig) search(t *testing.T, query string) []gwItem {
	t.Helper()
	var res struct{ Tracks struct{ Items []gwItem } }
	tidalGet(t, r.gw.URL, "/v1/search?query="+url.QueryEscape(query)+"&limit=50&offset=0", 200, &res)
	return res.Tracks.Items
}

func webdavTitles(items []gwItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, fmt.Sprint(it["title"])+"@"+rowAlbum(it))
	}
	slices.Sort(out)
	return out
}

func scopeOf(views []serverView, name string) string {
	for _, v := range views {
		if v.Name == name {
			return v.Scope
		}
	}
	return ""
}

// Two WebDAV servers with the same folders never mix (#335's root identity per
// server): each is its own entry in Sources under its name, their rows differ, each
// plays its own audio, and their downloads, tempo and Stems entries stay apart.
func TestServersTwoWebDAVWithTheSameFoldersNeverMix(t *testing.T) {
	a, b := newFakeDAV(t, testMP3(120), nil), newFakeDAV(t, testMP3(150), nil)
	r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=0\nwebdav=1\n")
	r.change(t, serverChange{Action: "add", Name: "Studio", URL: a.srv.URL + "/music/"}, 200)
	views := r.change(t, serverChange{Action: "add", Name: "Club", URL: b.srv.URL + "/music/"}, 200)
	studio, club := scopeOf(views, "Studio"), scopeOf(views, "Club")
	if v := r.status(t); !slices.Equal(serverSources(v), []string{studio, club}) {
		t.Fatalf("Sources = %v, want one entry per server [%s %s]", serverSources(v), studio, club)
	}
	la := r.open(t, studio)
	if la["Root"] == "" || la["House"] == "" {
		t.Fatalf("Studio's lists: %v", la)
	}
	ia := r.tracks(t, la["House"])["a"]
	ib := r.tracks(t, r.open(t, club)["House"])["a"]
	if ia == nil || ib == nil || rowID(ia) == rowID(ib) {
		t.Fatalf("same path on two servers is one row: %v %v", ia, ib)
	}
	if rowAlbum(ia) != "Studio" || rowAlbum(ib) != "Club" {
		t.Fatalf("rows do not name their server: %q %q", rowAlbum(ia), rowAlbum(ib))
	}
	if code, got := r.stream(t, rowID(ia)); code != 200 || len(got) != len(a.audio) {
		t.Fatalf("Studio's row: HTTP %d, %d bytes, want %d", code, len(got), len(a.audio))
	}
	if code, got := r.stream(t, rowID(ib)); code != 200 || len(got) != len(b.audio) {
		t.Fatalf("Club's row: HTTP %d, %d bytes, want %d (the other server's file?)", code, len(got), len(b.audio))
	}
	if a.gets.Load() != 1 || b.gets.Load() != 1 {
		t.Fatalf("downloads: Studio %d, Club %d", a.gets.Load(), b.gets.Load())
	}
	refA, refB := refOf(r.s.tidal, rowID(ia)), refOf(r.s.tidal, rowID(ib))
	if refA.track.ID == refB.track.ID || !strings.HasPrefix(refA.track.ID, serverRoot(kindWebDAV, a.srv.URL+"/music/")+":") {
		t.Fatalf("track IDs: %q %q", refA.track.ID, refB.track.ID)
	}
	// Each row's Stems entry joins its own server's Stems list.
	for _, ref := range []trackRef{refA, refB} {
		if err := r.s.addStemmedAt(rowScope(ref.track), stemmedTrack{Track: ref.track, Session: ref.session, Path: "/x/" + ref.track.ID}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.s.stemmed(studio)) != 1 || len(r.s.stemmed(club)) != 1 {
		t.Fatalf("Stems lists per server: %d %d", len(r.s.stemmed(studio)), len(r.s.stemmed(club)))
	}
	if lists := r.open(t, studio); lists["Stems"] == "" {
		t.Fatal("Studio's entry has no Stems list")
	}
}

// Removing a server removes its Sources entry, rows, lists, Stems entries and search
// results; moving a server to another address does the same for its old rows. The
// other server keeps playing.
func TestServersRemovingAWebDAVServerRemovesItsRows(t *testing.T) {
	a, b := newFakeDAV(t, testMP3(120), nil), newFakeDAV(t, testMP3(150), nil)
	r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=1\nwebdav=1\n")
	r.change(t, serverChange{Action: "add", Name: "Studio", URL: a.srv.URL + "/music/"}, 200)
	views := r.change(t, serverChange{Action: "add", Name: "Club", URL: b.srv.URL + "/music/"}, 200)
	studio, club := scopeOf(views, "Studio"), scopeOf(views, "Club")
	ia := r.tracks(t, r.open(t, studio)["House"])["a"]
	ib := r.tracks(t, r.open(t, club)["House"])["a"]
	refA, refB := refOf(r.s.tidal, rowID(ia)), refOf(r.s.tidal, rowID(ib))
	for _, ref := range []trackRef{refA, refB} {
		if err := r.s.addStemmedAt(rowScope(ref.track), stemmedTrack{Track: ref.track, Session: ref.session, Path: "/x/" + ref.track.ID}, 0); err != nil {
			t.Fatal(err)
		}
	}
	// From Club's entry: its own search, and OmniSource's for the other servers.
	if got := webdavTitles(r.search(t, "a")); !slices.Equal(got, []string{"a@Club", "a@Studio"}) {
		t.Fatalf("search before the removal: %v", got)
	}
	r.change(t, serverChange{Action: "remove", ID: refA.track.ID[:32]}, 200)
	if code, _ := r.stream(t, rowID(ia)); code != http.StatusNotFound {
		t.Fatalf("a removed server's row: HTTP %d, want 404", code)
	}
	if code, got := r.stream(t, rowID(ib)); code != 200 || len(got) != len(b.audio) {
		t.Fatalf("the other server's row after the removal: HTTP %d", code)
	}
	if v := r.status(t); !slices.Equal(serverSources(v), []string{club}) {
		t.Fatalf("Sources after the removal: %v", serverSources(v))
	}
	if code, _ := uiPost(t, r.gw.URL, "scope="+studio); code != http.StatusConflict {
		t.Fatalf("scope of a removed server: HTTP %d, want 409", code)
	}
	if got := webdavTitles(r.search(t, "a")); !slices.Equal(got, []string{"a@Club"}) {
		t.Fatalf("search after the removal: %v", got)
	}
	if len(r.s.stemmed(studio)) != 0 || len(r.s.stemmed(club)) != 1 {
		t.Fatalf("Stems after the removal: %d %d", len(r.s.stemmed(studio)), len(r.s.stemmed(club)))
	}
	r.s.tidal.saveTracks()
	saved, _ := os.ReadFile(filepath.Join(r.s.cfg.StateDir, "gateway-tracks.json"))
	if strings.Contains(string(saved), serverRoot(kindWebDAV, a.srv.URL+"/music/")) {
		t.Fatal("gateway-tracks.json keeps rows of the removed server")
	}
	// Moving Club to another address: its old rows go too, the new entry lists.
	r.change(t, serverChange{Action: "edit", ID: refB.track.ID[:32], Name: "Club", URL: b.srv.URL + "/music/House/"}, 200)
	if code, _ := r.stream(t, rowID(ib)); code != http.StatusNotFound {
		t.Fatalf("a moved server's old row: HTTP %d, want 404", code)
	}
	v := r.status(t)
	if got := serverSources(v); len(got) != 1 || got[0] == club {
		t.Fatalf("Sources after the move: %v", got)
	}
	if got := r.open(t, serverSources(v)[0]); got["Root"] == "" {
		t.Fatalf("the moved server is not listed: %v", got)
	}
}

// A released deck: WebDAV in nas.json's slot, OmniSource's [NAS] rows of it in
// gateway-tracks.json and its download in the index (no NAS root recorded). After
// the upgrade the old row answers 404 once (maintainer 2026-11-27); the server is
// WebDAV server 1, its own entry in Sources, with no re-entry, and its rows play.
func TestServersWebDAVUpgradeFromAReleasedNASSetup(t *testing.T) {
	d := newFakeDAV(t, testMP3(120), nil)
	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := `{"protocol":"webdav","url":"` + d.srv.URL + `/music/","username":"dj","password":"wd-pw","enabled":true}`
	if err := os.WriteFile(filepath.Join(uiDir, "nas.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=deezer\nscope=deezer\nomni_source=1\nknown=tidal,netease,spotify,deezer,soundcloud,beatport,applemusic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := providers.Track{ID: "/music/House/a.mp3", Title: "a", Album: "/music/House/", Provider: webdavScope}
	writeReleasedState(t, stateDir, dlDir, webdavScope, old, "a.mp3", d.audio)
	r := newWebDAVRigAt(t, uiDir, stateDir, dlDir)
	if code, _ := r.stream(t, 1); code != http.StatusNotFound {
		t.Fatalf("the released [NAS] WebDAV row after the upgrade: HTTP %d, want 404", code)
	}
	v := r.status(t)
	if len(v.WebDAV) != 1 || !slices.Equal(serverSources(v), []string{v.WebDAV[0].Scope}) {
		t.Fatalf("no WebDAV entry after the upgrade: %v %+v", v.Sources, v.WebDAV)
	}
	row := r.tracks(t, r.open(t, v.WebDAV[0].Scope)["House"])["a"]
	if row == nil {
		t.Fatalf("the server's House list has no row a")
	}
	if code, got := r.stream(t, rowID(row)); code != 200 || len(got) != len(d.audio) {
		t.Fatalf("the server's row: HTTP %d", code)
	}
	if d.gets.Load() != 1 {
		t.Fatalf("downloads after the upgrade: %d, want 1 (the released entry is not used)", d.gets.Load())
	}
}

// A WebDAV server shows once: its own entry in Sources, not again in OmniSource's
// [NAS] lists (a released deck's slot) or as an [NAS: name] share (a WebDAV mount).
func TestServersWebDAVIsNotListedTwice(t *testing.T) {
	d := newFakeDAV(t, testMP3(120), nil)
	cfg := `{"protocol":"webdav","url":"` + d.srv.URL + `/music/","enabled":true}`
	r := newWebDAVRig(t, cfg, "sources=\nscope=\nomni_source=1\n")
	u, _ := url.Parse(d.srv.URL)
	mounts := []RemoteMount{{ID: "m1", Name: "Share", Protocol: "webdav", Host: u.Host, RemotePath: "/music/", MountPoint: t.TempDir(), Enabled: true}}
	if err := providers.SaveJSON(filepath.Join(r.s.cfg.StateDir, "mounts.json"), mounts); err != nil {
		t.Fatal(err)
	}
	if got := r.s.loadMounts(); len(got) != 1 {
		t.Fatalf("mount fixture not read: %+v", got)
	}
	var page struct{ Items []gwItem }
	tidalGet(t, r.gw.URL, "/v1/users/1/playlists?limit=100&offset=0", 200, &page)
	for _, it := range page.Items {
		if title := fmt.Sprint(it["title"]); strings.HasPrefix(title, "[NAS") {
			t.Fatalf("the WebDAV server is listed again in OmniSource: %q", title)
		}
	}
	v := r.status(t)
	if got := serverSources(v); len(got) != 1 || got[0] != v.WebDAV[0].Scope {
		t.Fatalf("Sources = %v, want the one WebDAV entry", got)
	}
	if got := r.open(t, v.WebDAV[0].Scope); got["House"] == "" {
		t.Fatalf("the WebDAV entry lists %v", got)
	}
}

// Search: OmniSource's search asks every enabled WebDAV and NAS server at once,
// each in the NAS search's time budget, so a slow server holds up nobody; a
// disabled server and a kind switched off are not searched; a server's own entry
// searches that server only. Every result says its server and plays from it.
func TestServersSearchEveryEnabledServerWithinTheBudget(t *testing.T) {
	hold := make(chan struct{})
	a, b := newFakeDAV(t, testMP3(120), nil), newFakeDAV(t, testMP3(150), nil)
	slow, off := newFakeDAV(t, testMP3(90), hold), newFakeDAV(t, testMP3(80), nil)
	t.Cleanup(func() { close(hold) }) // before the servers close
	nas := nasFile{WebDAV: []savedServer{
		{Name: "Studio", URL: a.srv.URL + "/music/", Enabled: true},
		{Name: "Slow", URL: slow.srv.URL + "/music/", Enabled: true},
		{Name: "Club", URL: b.srv.URL + "/music/", Enabled: true},
		{Name: "Off", URL: off.srv.URL + "/music/", Enabled: false},
	}}
	raw, _ := json.Marshal(nas)
	r := newWebDAVRig(t, string(raw), "sources=deezer\nscope=deezer\nomni_source=1\nwebdav=1\n")
	start := time.Now()
	got := webdavTitles(r.search(t, "intro"))
	if elapsed := time.Since(start); elapsed > searchTimeout+3*time.Second {
		t.Fatalf("a slow server held the search up for %v", elapsed)
	}
	if !slices.Equal(got, []string{"intro@Club", "intro@Studio"}) {
		t.Fatalf("OmniSource search found %v", got)
	}
	if off.finds.Load() != 0 {
		t.Fatal("a disabled server was searched")
	}
	views := r.status(t).WebDAV
	studio := scopeOf(views, "Studio")
	items := func() []gwItem { uiPost(t, r.gw.URL, "scope="+studio); return r.search(t, "intro") }()
	if got := webdavTitles(items); !slices.Equal(got, []string{"intro@Club", "intro@Studio"}) {
		// Studio's own search plus OmniSource's (which leaves Studio out): each once.
		t.Fatalf("Studio's search with OmniSource found %v", got)
	}
	uiPost(t, r.gw.URL, "omniSource=0")
	if got := webdavTitles(r.search(t, "intro")); !slices.Equal(got, []string{"intro@Studio"}) {
		t.Fatalf("Studio's own search found %v", got)
	}
	uiPost(t, r.gw.URL, "omniSource=1")
	for _, it := range r.search(t, "intro") {
		ref := refOf(r.s.tidal, rowID(it))
		server := map[string]string{"Studio": a.srv.URL, "Club": b.srv.URL}[rowAlbum(it)]
		if !strings.HasPrefix(ref.track.ID, serverRoot(kindWebDAV, server+"/music/")+":") {
			t.Fatalf("result %q carries %q, not its server", rowAlbum(it), ref.track.ID)
		}
		want := map[string]int{"Studio": len(a.audio), "Club": len(b.audio)}[rowAlbum(it)]
		if code, body := r.stream(t, rowID(it)); code != 200 || len(body) != want {
			t.Fatalf("result of %s plays %d bytes (HTTP %d), want %d", rowAlbum(it), len(body), code, want)
		}
	}
	uiPost(t, r.gw.URL, "webdav=0")
	before := a.finds.Load() + b.finds.Load()
	r.search(t, "zzz-not-cached")
	if a.finds.Load()+b.finds.Load() != before {
		t.Fatal("WebDAV switched off was still searched")
	}
}

// WebDAV rows are outside the NAS slot's generation and root (#335): when the
// console moves the SMB NAS to another share, WebDAV rows keep playing from their
// download and keep their Stems entries.
func TestServersWebDAVRowsSurviveAnSMBNASChange(t *testing.T) {
	d := newFakeDAV(t, testMP3(120), nil)
	nas := nasFile{NASConfig: providers.NASConfig{Protocol: "smb", URL: "smb://nas.example/a", Enabled: true},
		WebDAV: []savedServer{{Name: "Studio", URL: d.srv.URL + "/music/", Enabled: true}}}
	raw, _ := json.Marshal(nas)
	r := newWebDAVRig(t, string(raw), "sources=\nscope=\nomni_source=0\n")
	studio := r.status(t).WebDAV[0].Scope
	row := r.tracks(t, r.open(t, studio)["House"])["a"]
	if code, _ := r.stream(t, rowID(row)); code != 200 {
		t.Fatalf("WebDAV row before the NAS change: HTTP %d", code)
	}
	ref := refOf(r.s.tidal, rowID(row))
	if err := r.s.addStemmedAt(studio, stemmedTrack{Track: ref.track, Session: ref.session, Path: "/x/a"}, 0); err != nil {
		t.Fatal(err)
	}
	gen := r.s.jobs.NASGen()
	w := httptest.NewRecorder()
	r.s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/api/nas", strings.NewReader(`{"protocol":"smb","url":"smb://nas.example/b","enabled":true}`)))
	if w.Code != 200 || r.s.jobs.NASGen() == gen {
		t.Fatalf("the SMB NAS did not move: HTTP %d, generation %d -> %d", w.Code, gen, r.s.jobs.NASGen())
	}
	if code, body := r.stream(t, rowID(row)); code != 200 || len(body) != len(d.audio) {
		t.Fatalf("WebDAV row after the SMB NAS moved: HTTP %d", code)
	}
	if n := d.gets.Load(); n != 1 {
		t.Fatalf("the WebDAV track downloaded %d times, want 1 (its download was dropped with the NAS)", n)
	}
	if got := r.s.stemmed(studio); len(got) != 1 {
		t.Fatalf("WebDAV Stems entries after the SMB NAS moved: %d, want 1", len(got))
	}
}

// smbTree fills an SMB fixture with the same tree as every other one here (Root
// track "Opener", folder "Set" with track "Closer"), with audio that says which
// server it is.
func smbTree(t *testing.T, srv *testsmb.Server, audio []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(srv.Root, "Set"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Opener.mp3", "Set/Closer.mp3"} {
		if err := os.WriteFile(filepath.Join(srv.Root, p), audio, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Several NAS (real smbd, needs `hostname localhost` in a private UTS namespace):
// the old NAS config is NAS server 1 ("nas"), an added one is "nas:<id>"; the two
// have the same tree and never mix; editing or removing one clears only its rows,
// Stems entries and search results; OmniSource search covers both in parallel and
// says which server a result is from.
func TestServersSeveralNASNeverMix(t *testing.T) {
	one, two := testsmb.Start(t), testsmb.Start(t)
	audio1, audio2 := testMP3(120), testMP3(150)
	smbTree(t, one, audio1)
	smbTree(t, two, audio2)
	slot := providers.NASConfig{Protocol: "smb", URL: one.URL, Username: one.User, Password: one.Password, Enabled: true}
	raw, _ := json.Marshal(nasFile{NASConfig: slot, Name: "Studio NAS"})
	r := newWebDAVRig(t, string(raw), "sources=\nscope=\nomni_source=1\n")
	views := r.changeKind(t, nasScope, serverChange{Action: "add", Name: "Club NAS", URL: two.URL, Username: two.User, Password: two.Password}, 200)
	club := scopeOf(views, "Club NAS")
	if scopeOf(views, "Studio NAS") != nasScope || club != "nas:"+serverRoot(kindNAS, two.URL) {
		t.Fatalf("NAS servers: %+v", views)
	}
	if v := r.status(t); !slices.Contains(serverSources(v), nasScope) || !slices.Contains(serverSources(v), club) {
		t.Fatalf("Sources = %v, want both NAS servers", serverSources(v))
	}
	r1 := r.tracks(t, r.open(t, nasScope)["Set"])["Closer"]
	r2 := r.tracks(t, r.open(t, club)["Set"])["Closer"]
	if r1 == nil || r2 == nil || rowID(r1) == rowID(r2) {
		t.Fatalf("the same path on two NAS is one row: %v %v", r1, r2)
	}
	if rowAlbum(r2) != "Club NAS" {
		t.Fatalf("the added NAS's row does not name it: %q", rowAlbum(r2))
	}
	if code, got := r.stream(t, rowID(r1)); code != 200 || len(got) != len(audio1) {
		t.Fatalf("NAS 1's row: HTTP %d, %d bytes", code, len(got))
	}
	if code, got := r.stream(t, rowID(r2)); code != 200 || len(got) != len(audio2) {
		t.Fatalf("NAS 2's row: HTTP %d, %d bytes (the other server's file?)", code, len(got))
	}
	ref2 := refOf(r.s.tidal, rowID(r2))
	if err := r.s.addStemmedAt(rowScope(ref2.track), stemmedTrack{Track: ref2.track, Session: ref2.session, Path: "/x/2"}, 0); err != nil {
		t.Fatal(err)
	}
	// From Club NAS's entry: its own search, and OmniSource's for NAS 1.
	if got := webdavTitles(r.search(t, "Closer")); !slices.Equal(got, []string{"Closer@Club NAS", "Closer@Studio NAS"}) {
		t.Fatalf("search over both NAS: %v", got)
	}
	// Moving NAS 1 (the console's server) to another share: only its rows go; NAS 2 plays on.
	w := httptest.NewRecorder()
	r.s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/api/nas", strings.NewReader(`{"protocol":"smb","url":"`+one.URL+`/Set","username":"`+one.User+`","enabled":true}`)))
	if w.Code != 200 {
		t.Fatalf("console move of NAS 1: %d %s", w.Code, w.Body.String())
	}
	if code, _ := r.stream(t, rowID(r1)); code != http.StatusNotFound {
		t.Fatalf("NAS 1's old row after its move: HTTP %d, want 404", code)
	}
	if code, got := r.stream(t, rowID(r2)); code != 200 || len(got) != len(audio2) {
		t.Fatalf("NAS 2's row after NAS 1 moved: HTTP %d", code)
	}
	if len(r.s.stemmed(club)) != 1 {
		t.Fatal("NAS 2's Stems entry went with NAS 1's move")
	}
	// Removing NAS 2: its entry, rows, Stems and results go; NAS 1 stays.
	r.changeKind(t, nasScope, serverChange{Action: "remove", ID: ref2.track.ID[:32]}, 200)
	if code, _ := r.stream(t, rowID(r2)); code != http.StatusNotFound {
		t.Fatalf("a removed NAS's row: HTTP %d, want 404", code)
	}
	if v := r.status(t); slices.Contains(serverSources(v), club) || !slices.Contains(serverSources(v), nasScope) {
		t.Fatalf("Sources after removing NAS 2: %v", serverSources(v))
	}
	if len(r.s.stemmed(club)) != 0 {
		t.Fatal("a removed NAS keeps its Stems entry")
	}
	// NAS 1 moved to another share: the console's new address carries no name yet.
	if got := webdavTitles(r.search(t, "Closer")); !slices.Equal(got, []string{"Closer@" + serverHost(one.URL)}) {
		t.Fatalf("OmniSource search after removing NAS 2: %v", got)
	}
	// The NAS row off: no NAS entry, no NAS result.
	uiPost(t, r.gw.URL, "nas=0")
	if v := r.status(t); len(serverSources(v)) != 0 {
		t.Fatalf("Sources with NAS off: %v", serverSources(v))
	}
	if got := r.search(t, "Opener"); len(got) != 0 {
		t.Fatalf("search with NAS off found %v", webdavTitles(got))
	}
}
