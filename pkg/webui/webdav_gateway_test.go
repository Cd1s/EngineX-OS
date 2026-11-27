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

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
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

// Two servers with the same folders never mix (#335's root identity per server):
// each is its own entry under WebDAV, their rows differ, each plays its own audio,
// and their downloads, tempo and Stems entries are kept apart.
func TestWebDAVTwoServersWithTheSameFoldersNeverMix(t *testing.T) {
	a, b := newFakeDAV(t, testMP3(120), nil), newFakeDAV(t, testMP3(150), nil)
	r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=0\nwebdav=1\n")
	r.change(t, webdavChange{Action: "add", Name: "Studio", URL: a.srv.URL + "/music/"}, 200)
	r.change(t, webdavChange{Action: "add", Name: "Club", URL: b.srv.URL + "/music/"}, 200)
	if code, _ := uiPost(t, r.gw.URL, "scope=webdav"); code != 200 {
		t.Fatalf("scope=webdav: %d", code)
	}
	lists := r.lists(t)
	for _, want := range []string{"[Studio] Root", "[Studio] House", "[Club] Root", "[Club] House"} {
		if lists[want] == "" {
			t.Fatalf("WebDAV source lists %v, missing %q", lists, want)
		}
	}
	ta, tb := r.tracks(t, lists["[Studio] House"]), r.tracks(t, lists["[Club] House"])
	ia, ib := ta["a"], tb["a"]
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
	// The rows' tracks name their server; Stems entries of both stay apart.
	refA := refOf(r.s.tidal, rowID(ia))
	refB := refOf(r.s.tidal, rowID(ib))
	if refA.track.ID == refB.track.ID || !strings.HasPrefix(refA.track.ID, webdavRoot(a.srv.URL+"/music/")+":") {
		t.Fatalf("track IDs: %q %q", refA.track.ID, refB.track.ID)
	}
	for _, ref := range []trackRef{refA, refB} {
		if err := r.s.addStemmedAt(webdavScope, stemmedTrack{Track: ref.track, Session: webdavScope, Path: "/x/" + ref.track.ID}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.s.stemmed(webdavScope); len(got) != 2 {
		t.Fatalf("Stems entries of two servers: %d, want 2", len(got))
	}
	if lists := r.lists(t); lists["Stems"] == "" {
		t.Fatal("the WebDAV source has no Stems list")
	}
}

// Removing a server removes its rows, lists, Stems entries and search results;
// moving a server to another address does the same for its old rows. The other
// server keeps playing.
func TestWebDAVRemovingAServerRemovesItsRows(t *testing.T) {
	a, b := newFakeDAV(t, testMP3(120), nil), newFakeDAV(t, testMP3(150), nil)
	r := newWebDAVRig(t, "", "sources=\nscope=\nomni_source=0\nwebdav=1\n")
	list := r.change(t, webdavChange{Action: "add", Name: "Studio", URL: a.srv.URL + "/music/"}, 200)
	r.change(t, webdavChange{Action: "add", Name: "Club", URL: b.srv.URL + "/music/"}, 200)
	uiPost(t, r.gw.URL, "scope=webdav")
	lists := r.lists(t)
	ia, ib := r.tracks(t, lists["[Studio] House"])["a"], r.tracks(t, lists["[Club] House"])["a"]
	r.tracks(t, lists["[Studio] Root"])
	refA := refOf(r.s.tidal, rowID(ia))
	refB := refOf(r.s.tidal, rowID(ib))
	for _, ref := range []trackRef{refA, refB} {
		if err := r.s.addStemmedAt(webdavScope, stemmedTrack{Track: ref.track, Session: webdavScope, Path: "/x/" + ref.track.ID}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := webdavTitles(r.search(t, "a")); !slices.Equal(got, []string{"a@Club", "a@Studio"}) {
		t.Fatalf("search before the removal: %v", got)
	}
	r.change(t, webdavChange{Action: "remove", ID: list[0].ID}, 200)
	if code, _ := r.stream(t, rowID(ia)); code != http.StatusNotFound {
		t.Fatalf("a removed server's row: HTTP %d, want 404", code)
	}
	if code, got := r.stream(t, rowID(ib)); code != 200 || len(got) != len(b.audio) {
		t.Fatalf("the other server's row after the removal: HTTP %d", code)
	}
	for title := range r.lists(t) {
		if strings.HasPrefix(title, "[Studio]") {
			t.Fatalf("a removed server's list is still offered: %q", title)
		}
	}
	if got := webdavTitles(r.search(t, "a")); !slices.Equal(got, []string{"a@Club"}) {
		t.Fatalf("search after the removal: %v", got)
	}
	if got := r.s.stemmed(webdavScope); len(got) != 1 || got[0].Track.ID != refB.track.ID {
		t.Fatalf("Stems entries after the removal: %+v", got)
	}
	r.s.tidal.saveTracks()
	saved, _ := os.ReadFile(filepath.Join(r.s.cfg.StateDir, "gateway-tracks.json"))
	if strings.Contains(string(saved), webdavRoot(a.srv.URL+"/music/")) {
		t.Fatal("gateway-tracks.json keeps rows of the removed server")
	}
	// Moving Club to another address: its old rows go too, browsed again they play.
	club := webdavRoot(b.srv.URL + "/music/")
	r.change(t, webdavChange{Action: "edit", ID: club, Name: "Club", URL: b.srv.URL + "/music/House/"}, 200)
	if code, _ := r.stream(t, rowID(ib)); code != http.StatusNotFound {
		t.Fatalf("a moved server's old row: HTTP %d, want 404", code)
	}
	if got := r.lists(t); got["[Club] Root"] == "" {
		t.Fatalf("the moved server is not listed: %v", got)
	}
}

// A released deck: WebDAV in nas.json's slot, OmniSource's [NAS] rows of it in
// gateway-tracks.json and its download in the index (no NAS root recorded). After
// the upgrade the old row answers 404 once (maintainer 2026-11-27); the WebDAV
// source lists the server with no re-entry and its rows play.
func TestWebDAVUpgradeFromAReleasedNASSetup(t *testing.T) {
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
	if !slices.Contains(v.Sources, webdavScope) || len(v.WebDAV) != 1 {
		t.Fatalf("no WebDAV source after the upgrade: %v %+v", v.Sources, v.WebDAV)
	}
	if code, _ := uiPost(t, r.gw.URL, "scope=webdav"); code != 200 {
		t.Fatalf("scope=webdav: %d", code)
	}
	name := d.srv.Listener.Addr().String()
	row := r.tracks(t, r.lists(t)["["+name+"] House"])["a"]
	if row == nil {
		t.Fatalf("the server's House list has no row a")
	}
	if code, got := r.stream(t, rowID(row)); code != 200 || len(got) != len(d.audio) {
		t.Fatalf("the WebDAV source's row: HTTP %d", code)
	}
	if d.gets.Load() != 1 {
		t.Fatalf("downloads after the upgrade: %d, want 1 (the released entry is not used)", d.gets.Load())
	}
}

func newWebDAVRigAt(t *testing.T, uiDir, stateDir, dlDir string) *webdavRig {
	t.Helper()
	r := newWebDAVRig(t, "", "")
	r.gw.Close()
	r.s.Close()
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	gw := httptest.NewServer(s)
	t.Cleanup(gw.Close)
	return &webdavRig{s: s, gw: gw, uiDir: uiDir}
}

// A WebDAV server shows once: under WebDAV, not again in OmniSource's [NAS] lists
// (a released deck's slot) or as an [NAS: name] share (a WebDAV mount of it).
func TestWebDAVIsNotListedTwice(t *testing.T) {
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
	if code, _ := uiPost(t, r.gw.URL, "scope=webdav"); code != 200 {
		t.Fatalf("scope=webdav: %d", code)
	}
	name := d.srv.Listener.Addr().String()
	if got := r.lists(t); got["["+name+"] House"] == "" {
		t.Fatalf("WebDAV source lists %v", got)
	}
}

// Search: the WebDAV source's search and OmniSource's search ask every enabled
// server at once, each in the NAS search's time budget, so a slow server holds up
// nobody; a disabled server and WebDAV switched off are not searched.
func TestWebDAVSearchEveryEnabledServerWithinTheBudget(t *testing.T) {
	hold := make(chan struct{})
	a, b := newFakeDAV(t, testMP3(120), nil), newFakeDAV(t, testMP3(150), nil)
	slow, off := newFakeDAV(t, testMP3(90), hold), newFakeDAV(t, testMP3(80), nil)
	t.Cleanup(func() { close(hold) }) // before the servers close
	nas := nasFile{WebDAV: []webdavServer{
		{Name: "Studio", URL: a.srv.URL + "/music/", Enabled: true},
		{Name: "Slow", URL: slow.srv.URL + "/music/", Enabled: true},
		{Name: "Club", URL: b.srv.URL + "/music/", Enabled: true},
		{Name: "Off", URL: off.srv.URL + "/music/", Enabled: false},
	}}
	raw, _ := json.Marshal(nas)
	r := newWebDAVRig(t, string(raw), "sources=deezer\nscope=deezer\nomni_source=1\nwebdav=1\n")
	start := time.Now()
	got := webdavTitles(r.search(t, "intro")) // OmniSource's search, from another source
	if elapsed := time.Since(start); elapsed > searchTimeout+3*time.Second {
		t.Fatalf("a slow server held the search up for %v", elapsed)
	}
	if !slices.Equal(got, []string{"intro@Club", "intro@Studio"}) {
		t.Fatalf("OmniSource search found %v", got)
	}
	if off.finds.Load() != 0 {
		t.Fatal("a disabled server was searched")
	}
	// The WebDAV source's own search: the same servers, each row naming its own.
	uiPost(t, r.gw.URL, "scope=webdav")
	items := r.search(t, "intro")
	if got := webdavTitles(items); !slices.Equal(got, []string{"intro@Club", "intro@Studio"}) {
		t.Fatalf("WebDAV search found %v", got)
	}
	for _, it := range items {
		ref := refOf(r.s.tidal, rowID(it))
		server := map[string]string{"Studio": a.srv.URL, "Club": b.srv.URL}[rowAlbum(it)]
		if !strings.HasPrefix(ref.track.ID, webdavRoot(server+"/music/")+":") {
			t.Fatalf("result %q carries %q, not its server", rowAlbum(it), ref.track.ID)
		}
		want := map[string]int{"Studio": len(a.audio), "Club": len(b.audio)}[rowAlbum(it)]
		if code, body := r.stream(t, rowID(it)); code != 200 || len(body) != want {
			t.Fatalf("result of %s plays %d bytes (HTTP %d), want %d", rowAlbum(it), len(body), code, want)
		}
	}
	// WebDAV off: no server is asked.
	uiPost(t, r.gw.URL, "webdav=0")
	before := a.finds.Load() + b.finds.Load()
	r.search(t, "zzz-not-cached")
	if a.finds.Load()+b.finds.Load() != before {
		t.Fatal("WebDAV switched off was still searched")
	}
}

// WebDAV rows are outside the NAS slot's generation and root (#335): when the
// console moves the SMB NAS to another share, WebDAV rows keep playing from
// their download and keep their Stems entries.
func TestWebDAVRowsSurviveAnSMBNASChange(t *testing.T) {
	d := newFakeDAV(t, testMP3(120), nil)
	nas := nasFile{NASConfig: providers.NASConfig{Protocol: "smb", URL: "smb://nas.example/a", Enabled: true},
		WebDAV: []webdavServer{{Name: "Studio", URL: d.srv.URL + "/music/", Enabled: true}}}
	raw, _ := json.Marshal(nas)
	r := newWebDAVRig(t, string(raw), "sources=\nscope=\nomni_source=0\n")
	uiPost(t, r.gw.URL, "scope=webdav")
	row := r.tracks(t, r.lists(t)["[Studio] House"])["a"]
	if code, _ := r.stream(t, rowID(row)); code != 200 {
		t.Fatalf("WebDAV row before the NAS change: HTTP %d", code)
	}
	ref := refOf(r.s.tidal, rowID(row))
	if err := r.s.addStemmedAt(webdavScope, stemmedTrack{Track: ref.track, Session: webdavScope, Path: "/x/a"}, 0); err != nil {
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
	if got := r.s.stemmed(webdavScope); len(got) != 1 {
		t.Fatalf("WebDAV Stems entries after the SMB NAS moved: %d, want 1", len(got))
	}
}
