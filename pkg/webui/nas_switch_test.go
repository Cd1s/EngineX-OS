package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

// nasSwitchRig is OmniSource with its [NAS] lists and two Subsonic servers that use the
// same playlist and track IDs for different recordings. Server two's downloads wait while
// held.
type nasSwitchRig struct {
	t              *testing.T
	s              *Server
	jobs           *Jobs
	gw             *httptest.Server
	one, two       []byte
	oneURL, twoURL string
	asked          chan struct{} // a held download of server two began
	twoDownloads   atomic.Int32
	mu             sync.Mutex
	gate           chan struct{} // server two's downloads wait for it while set
	listAsked      chan struct{} // a held playlist answer of server one began
	listGate       chan struct{} // server one's playlist answers wait for it while set
}

type nasAnswer struct {
	code int
	body []byte
}

func newNASSwitchRig(t *testing.T, opts ...func(*Pipeline)) *nasSwitchRig {
	return newNASSwitchRigWith(t, testMP3(120), testMP3(150), opts...)
}

// newNASSwitchRigWith is newNASSwitchRig with the two servers' recordings given.
func newNASSwitchRigWith(t *testing.T, one, two []byte, opts ...func(*Pipeline)) *nasSwitchRig {
	rig := &nasSwitchRig{t: t, one: one, two: two, asked: make(chan struct{}, 4), listAsked: make(chan struct{}, 4)}
	nas := func(audio []byte, held bool) string {
		suffix, mime, seconds := "mp3", "audio/mpeg", 210
		if bytes.HasPrefix(audio, []byte("fLaC")) { // a real song: no listed length to compare
			suffix, mime, seconds = "flac", "audio/flac", 0
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/rest/getPlaylists.view":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-42","name":"Club Crate","songCount":1}]}}}`)
			case "/rest/getPlaylist.view":
				if !held {
					rig.mu.Lock()
					gate := rig.listGate
					rig.mu.Unlock()
					if gate != nil {
						rig.listAsked <- struct{}{}
						<-gate
					}
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl-42","name":"Club Crate","entry":[{"id":"tr-99","title":"Warehouse Dub","artist":"DJ Test","album":"NAS Vol 1","duration":%d,"suffix":"%s"}]}}}`, seconds, suffix)
			case "/rest/download.view":
				if held {
					rig.twoDownloads.Add(1)
					rig.mu.Lock()
					gate := rig.gate
					rig.mu.Unlock()
					if gate != nil {
						rig.asked <- struct{}{}
						<-gate
					}
				}
				w.Header().Set("Content-Type", mime)
				w.Write(audio)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	rig.oneURL, rig.twoURL = nas(rig.one, false), nas(rig.two, true)

	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("scope=spotify\nomni_source=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true}}
	for _, opt := range opts {
		opt(&p)
	}
	rig.jobs = NewJobs(p)
	t.Cleanup(rig.jobs.Close)
	rig.s = New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, rig.jobs)
	t.Cleanup(rig.s.Close)
	rig.s.sessions["sp-owner"] = &Session{ID: "sp-owner", Provider: "spotify", Ready: true, p: &browseTestProvider{}}
	rig.gw = httptest.NewServer(rig.s)
	t.Cleanup(rig.gw.Close)
	t.Cleanup(rig.release) // before gw.Close, which waits for a held request
	t.Cleanup(rig.releaseLists)
	return rig
}

// withLibrary gives the rig a real Engine library, so a streamed track is queued for import.
func withLibrary(t *testing.T) func(*Pipeline) {
	return func(p *Pipeline) {
		dbPath, libRoot, _ := testlibrary.Create(t)
		lib, err := analyzer.OpenLibrary(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lib.Close() })
		dl := filepath.Join(libRoot, "stream-cache")
		if err := os.MkdirAll(dl, 0o755); err != nil {
			t.Fatal(err)
		}
		p.Guard = resourceguard.New(resourceguard.Config{Disabled: true})
		p.Deemix = &deemixclient.Client{DownloadDir: dl, AllowLossy: true}
		p.Library = lib
	}
}

// pauseOnce pauses the first request that reaches point until resume; reached
// signals it got there.
func pauseOnce(point string) (hook func(string), reached chan struct{}, resume func()) {
	reached, release := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	var once sync.Once
	hook = func(p string) {
		if p == point && paused.CompareAndSwap(false, true) {
			reached <- struct{}{}
			<-release
		}
	}
	return hook, reached, func() { once.Do(func() { close(release) }) }
}

// get makes no t calls: it also runs in a goroutine.
func (rig *nasSwitchRig) get(path string) nasAnswer {
	resp, err := http.Get(rig.gw.URL + path)
	if err != nil {
		return nasAnswer{code: -1}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return nasAnswer{resp.StatusCode, body}
}

// hold makes server two's downloads wait until release.
func (rig *nasSwitchRig) hold() {
	rig.mu.Lock()
	rig.gate = make(chan struct{})
	rig.mu.Unlock()
}

func (rig *nasSwitchRig) release() {
	rig.mu.Lock()
	if rig.gate != nil {
		close(rig.gate)
		rig.gate = nil
	}
	rig.mu.Unlock()
}

// holdLists makes server one's playlist answers wait until releaseLists.
func (rig *nasSwitchRig) holdLists() {
	rig.mu.Lock()
	rig.listGate = make(chan struct{})
	rig.mu.Unlock()
}

func (rig *nasSwitchRig) releaseLists() {
	rig.mu.Lock()
	if rig.listGate != nil {
		close(rig.listGate)
		rig.listGate = nil
	}
	rig.mu.Unlock()
}

func (rig *nasSwitchRig) saveNAS(url string) {
	rig.t.Helper()
	w := httptest.NewRecorder()
	body := fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":"dj","password":"pw","enabled":true}`, url)
	rig.s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		rig.t.Fatalf("save NAS: %d %s", w.Code, w.Body.String())
	}
}

func (rig *nasSwitchRig) browse() int64 {
	rig.t.Helper()
	var lists, tracks struct {
		Items []map[string]any `json:"items"`
	}
	tidalGet(rig.t, rig.gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &lists)
	for _, it := range lists.Items {
		if it["title"] == "[NAS] Club Crate" {
			tidalGet(rig.t, rig.gw.URL, "/v1/playlists/"+it["uuid"].(string)+"/tracks?limit=50&offset=0", 200, &tracks)
		}
	}
	if len(tracks.Items) != 1 {
		rig.t.Fatalf("no [NAS] Club Crate track: lists=%+v tracks=%+v", lists.Items, tracks.Items)
	}
	return int64(tracks.Items[0]["id"].(float64))
}

// stream makes no t calls: it also runs in a goroutine.
func (rig *nasSwitchRig) stream(id int64) nasAnswer {
	resp, err := http.Get(fmt.Sprintf("%s/v1/stream/%d.mp3", rig.gw.URL, id))
	if err != nil {
		return nasAnswer{code: -1}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return nasAnswer{resp.StatusCode, body}
}

func (rig *nasSwitchRig) streamAsync(id int64) chan nasAnswer {
	ch := make(chan nasAnswer, 1)
	go func() { ch <- rig.stream(id) }()
	return ch
}

func (rig *nasSwitchRig) plays(step string, id int64, want []byte) {
	rig.t.Helper()
	if a := rig.stream(id); a.code != http.StatusOK || !bytes.Equal(a.body, want) {
		rig.t.Fatalf("%s: track %d answered HTTP %d with %d bytes (server one's recording=%v, server two's=%v)",
			step, id, a.code, len(a.body), bytes.Equal(a.body, rig.one), bytes.Equal(a.body, rig.two))
	}
}

func (rig *nasSwitchRig) gone(step string, id int64) {
	rig.t.Helper()
	if a := rig.stream(id); a.code != http.StatusNotFound {
		rig.t.Fatalf("%s: old track %d answered HTTP %d with %d bytes (a server's recording=%v), want 404",
			step, id, a.code, len(a.body), bytes.Equal(a.body, rig.one) || bytes.Equal(a.body, rig.two))
	}
}

func nasWaitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting until %s", what)
	}
}

// #335 round 2: the released [NAS] playlists route has the flaw of the SMB root
// switch. Two servers can use the same playlist and track IDs; after the NAS
// moved to the second one, the first one's download (finished, or still
// running at the change) answered for the second one's track. Nothing of the
// old server may answer once the NAS settings or the mounts page moved the NAS.
func TestNASServerChangeNeverServesTheOldServersTrack(t *testing.T) {
	rig := newNASSwitchRig(t)

	// 1. Server one plays its track. Its address spelled with a trailing or doubled slash
	// is the same NAS (round 3): the track keeps its ID and download.
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	rig.plays("server one", id1, rig.one)
	for _, same := range []string{rig.oneURL + "/", rig.oneURL + "//"} {
		rig.saveNAS(same)
		rig.plays("server one saved as "+same, id1, rig.one)
	}

	// 2. Server two has the same playlist and track IDs: its track comes from it.
	rig.saveNAS(rig.twoURL)
	id2 := rig.browse()
	rig.hold()
	first, second := rig.streamAsync(id2), make(chan nasAnswer, 1)
	select {
	case <-rig.asked:
	case a := <-first:
		t.Fatalf("server two's track %d (the same ID as server one's: %v) answered HTTP %d without asking server two (server one's recording=%v)",
			id2, id2 == id1, a.code, bytes.Equal(a.body, rig.one))
	case <-time.After(10 * time.Second):
		t.Fatal("server two was never asked for its track")
	}
	rig.gone("after the change to server two", id1)
	// A second load of the same track waits for that download (Engine asks for the
	// playback info and the stream of one track at once).
	go func() { second <- rig.stream(id2) }()
	time.Sleep(300 * time.Millisecond)

	// 3. Back to server one while server two's download runs: neither load answers (the
	// waiting one must not fetch the track again, now from server one), nothing is kept
	// for the track, and server one serves it afresh.
	rig.saveNAS(rig.oneURL)
	rig.release()
	for i, ch := range []chan nasAnswer{first, second} {
		if a := <-ch; a.code == http.StatusOK {
			t.Fatalf("load %d of server two's track, begun before the change, answered HTTP 200 after it (server two's recording=%v, server one's=%v)",
				i+1, bytes.Equal(a.body, rig.two), bytes.Equal(a.body, rig.one))
		}
	}
	id3 := rig.browse()
	rig.plays("server one again", id3, rig.one)
	rig.gone("after the change back to server one", id2)

	// 4. Removing the NAS in its settings.
	w := httptest.NewRecorder()
	rig.s.handleNASConfig(w, httptest.NewRequest(http.MethodDelete, "/v1/enginex/nas", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("remove NAS: %d %s", w.Code, w.Body.String())
	}
	rig.gone("after removing the NAS", id3)

	// 5. The mounts page writes nas.json too: a WebDAV share saved there replaces the
	// NAS, and deleting the last WebDAV share removes it.
	mount := func(method, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		rig.s.handleMounts(w, httptest.NewRequest(method, "/v1/enginex/mounts?id=dav-1", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s mount: %d %s", method, w.Code, w.Body.String())
		}
	}
	rig.saveNAS(rig.oneURL)
	id4 := rig.browse()
	rig.plays("server one before the mounts page", id4, rig.one)
	mount(http.MethodPost, fmt.Sprintf(`{"id":"dav-1","name":"dav-1","protocol":"webdav","host":"nas.example","port":8080,"remotePath":"/music","mountPoint":%q,"enabled":false}`,
		filepath.Join(t.TempDir(), "dav")))
	rig.gone("after a WebDAV share on the mounts page replaced the NAS", id4)
	rig.saveNAS(rig.oneURL)
	id5 := rig.browse()
	rig.plays("server one with the WebDAV share saved", id5, rig.one)
	mount(http.MethodDelete, "")
	rig.gone("after deleting the WebDAV share removed the NAS", id5)
}

// The review's first paused load (#335 round 3): a load that looked up server one's
// track before the NAS moved to server two, and fetched only after, played server
// two's recording under server one's ID. It answers 404 and leaves nothing behind.
func TestReviewLookupOverlap(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	looked, resume := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	rig.s.tidal.pause = func(point string) {
		if point == "lookup" && paused.CompareAndSwap(false, true) {
			looked <- struct{}{}
			<-resume
		}
	}
	running := rig.streamAsync(id1)
	nasWaitFor(t, looked, "the load looked up its track")
	rig.saveNAS(rig.twoURL)
	close(resume)
	if a := <-running; a.code != http.StatusNotFound || bytes.Equal(a.body, rig.two) || bytes.Equal(a.body, rig.one) {
		t.Fatalf("a load of server one's track %d, looked up before the change, answered HTTP %d after it (server two's recording=%v, server one's=%v), want 404",
			id1, a.code, bytes.Equal(a.body, rig.two), bytes.Equal(a.body, rig.one))
	}
	if n := rig.twoDownloads.Load(); n != 0 {
		t.Fatalf("the old load asked server two for audio %d times, want 0", n)
	}
	// Nothing was kept for the track: server two's own row is fetched from server two.
	id2 := rig.browse()
	rig.plays("server two's row", id2, rig.two)
	if n := rig.twoDownloads.Load(); n != 1 {
		t.Fatalf("server two's row asked server two %d times, want 1 (a kept download answered instead)", n)
	}
}

// The review's second paused load (#335 round 3): two loads of server two's track
// share one download; the NAS moves back to server one and the download is refused;
// a load of server one's new row files server one's download; only then does the
// waiting old load look again. It found that file and played it under server two's ID.
func TestReviewThreeLoads(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.twoURL)
	id2 := rig.browse()
	waiting, woke, resume := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{})
	rig.jobs.pause = func(point string) {
		switch point {
		case "wait":
			select {
			case waiting <- struct{}{}:
			default:
			}
		case "woke":
			woke <- struct{}{}
			<-resume
		}
	}
	rig.hold()
	first := rig.streamAsync(id2)
	nasWaitFor(t, rig.asked, "server two's download began")
	second := rig.streamAsync(id2)
	nasWaitFor(t, waiting, "the second load waited for that download")
	rig.saveNAS(rig.oneURL)
	rig.release()
	if a := <-first; a.code == http.StatusOK {
		t.Fatalf("the first load of server two's track answered HTTP 200 after the change (server two's recording=%v)", bytes.Equal(a.body, rig.two))
	}
	nasWaitFor(t, woke, "the second load woke up")
	id1 := rig.browse()
	rig.plays("server one's new row", id1, rig.one)
	close(resume)
	if a := <-second; a.code != http.StatusNotFound || bytes.Equal(a.body, rig.one) || bytes.Equal(a.body, rig.two) {
		t.Fatalf("the waiting load of server two's track %d answered HTTP %d after the change (server one's recording=%v, server two's=%v), want 404",
			id2, a.code, bytes.Equal(a.body, rig.one), bytes.Equal(a.body, rig.two))
	}
}

// The review's third probe (#335 round 3b): a listing of server one's playlist that
// began before the NAS moved to server two and finished after it made rows with server
// one's titles whose IDs then played server two's recording. It makes no rows now.
func TestReviewBrowseOutlivesSwitch(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	var lists struct {
		Items []map[string]any `json:"items"`
	}
	tidalGet(t, rig.gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &lists)
	uuid := ""
	for _, it := range lists.Items {
		if it["title"] == "[NAS] Club Crate" {
			uuid, _ = it["uuid"].(string)
		}
	}
	if uuid == "" {
		t.Fatalf("no [NAS] Club Crate list: %+v", lists.Items)
	}
	type answer struct {
		code  int
		items []map[string]any
	}
	done := make(chan answer, 1)
	rig.holdLists()
	go func() { // no t calls
		resp, err := http.Get(rig.gw.URL + "/v1/playlists/" + uuid + "/tracks?limit=50&offset=0")
		if err != nil {
			done <- answer{code: -1}
			return
		}
		defer resp.Body.Close()
		var v struct {
			Items []map[string]any `json:"items"`
		}
		json.NewDecoder(resp.Body).Decode(&v)
		done <- answer{resp.StatusCode, v.Items}
	}()
	nasWaitFor(t, rig.listAsked, "server one was asked for the playlist")
	rig.saveNAS(rig.twoURL)
	rig.releaseLists()
	a := <-done
	for _, it := range a.items {
		id, _ := it["id"].(float64)
		if got := rig.stream(int64(id)); got.code == http.StatusOK {
			t.Fatalf("a listing begun before the change made row %d (%v) after it, and it played (server two's recording=%v)",
				int64(id), it["title"], bytes.Equal(got.body, rig.two))
		}
	}
	if len(a.items) != 0 || rig.twoDownloads.Load() != 0 {
		t.Fatalf("a listing begun before the change made %d rows after it (HTTP %d, server two asked %d times)", len(a.items), a.code, rig.twoDownloads.Load())
	}
}

// Review of fb5a1f6f, HIGH 2: the reset rewrote streaming-stems.json from a partial
// decode, dropping other services' entries. A Stems file that does not decode stays byte
// for byte, and the log line names no entry.
func TestNASChangeLeavesAStemsFileItCannotReadAlone(t *testing.T) {
	uiDir, stateDir := t.TempDir(), t.TempDir()
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, nil)
	t.Cleanup(s.Close)
	stems := []byte(`{"nas":[{"track":{"id":"Sets/Opener.flac","provider":"smb"},"path":"/data/stems/opener.flac"}],` +
		`"spotify":[{"track":{"id":"sp-1","provider":"spotify","title":"Keep Me"},"path":7}]}`)
	path := filepath.Join(stateDir, "streaming-stems.json")
	if err := os.WriteFile(path, stems, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	for _, url := range []string{"http://nas.example:4533", "http://nas.example:4534"} {
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":"dj","password":"pw","enabled":true}`, url)
		s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("save NAS %s: %d %s", url, w.Code, w.Body.String())
		}
	}
	log.SetOutput(os.Stderr)
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, stems) {
		t.Fatalf("a NAS change rewrote a Stems file that does not decode (%v):\n%s", err, got)
	}
	if !strings.Contains(logs.String(), "Stems lists left as they are") {
		t.Fatalf("no log line for the Stems file left as it is: %q", logs.String())
	}
	for _, entry := range []string{"Opener", "opener", "Keep Me", "sp-1"} {
		if strings.Contains(logs.String(), entry) {
			t.Fatalf("the log names a Stems entry (%q): %q", entry, logs.String())
		}
	}
}

// Round 4 (re-check of f35f2c9d, HIGH 1): a Stems list read before the NAS moved from
// root A to root B published root A's row after it, at root B's generation, so it
// played root B's file at the same path. It publishes no NAS row now.
func TestReviewStemsListAcrossSwitch(t *testing.T) {
	uiDir, stateDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	gw := httptest.NewServer(s)
	t.Cleanup(gw.Close)
	save := func(root string) {
		t.Helper()
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"protocol":"smb","url":"smb://nas.example/music/%s","username":"dj","password":"pw","enabled":true}`, root)
		s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("save SMB %s: %d %s", root, w.Code, w.Body.String())
		}
	}
	save("rootA")
	opener := stemmedTrack{Track: providers.Track{ID: "Sets/Opener.flac", Provider: "smb", Title: "Opener"}, Session: nasScope, Path: filepath.Join(stateDir, "opener.flac")}
	if err := s.addStemmed(nasScope, opener); err != nil {
		t.Fatal(err)
	}
	hook, reached, resume := pauseOnce("stems-list")
	t.Cleanup(resume)
	s.tidal.pause = hook
	done := make(chan nasAnswer, 1)
	go func() {
		resp, err := http.Get(gw.URL + "/v1/playlists/" + stemsListID(nasScope) + "/tracks?limit=50&offset=0")
		if err != nil {
			done <- nasAnswer{code: -1}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		done <- nasAnswer{resp.StatusCode, body}
	}()
	nasWaitFor(t, reached, "the Stems list was read")
	save("rootB")
	resume()
	a := <-done
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(a.body, &page); err != nil || a.code != http.StatusOK || len(page.Items) != 0 {
		t.Fatalf("a Stems list read before the move to root B published %d rows after it (HTTP %d): %s", len(page.Items), a.code, a.body)
	}
}

// Round 4 (HIGH 1): a Stems source request that fetched server one's file before the
// change wrote its Stems entry after it, so server two's row with the same track ID
// got server one's local Stems file. Nothing is written or answered now.
func TestReviewLateStemsWrite(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("stems-write")
	t.Cleanup(resume)
	rig.s.tidal.pause = hook
	done := make(chan nasAnswer, 1)
	go func() { done <- rig.get(fmt.Sprintf("/v1/enginex/stems/source?id=%d", id1)) }()
	nasWaitFor(t, reached, "the Stems source fetched server one's file")
	rig.saveNAS(rig.twoURL)
	resume()
	if a := <-done; a.code != http.StatusNotFound {
		t.Fatalf("a Stems source of server one's track, fetched before the change, answered HTTP %d after it: %s, want 404", a.code, a.body)
	}
	id2 := rig.browse()
	if a := rig.get(fmt.Sprintf("/v1/enginex/stems/local?id=%d", id2)); a.code != http.StatusNotFound {
		t.Fatalf("server two's track %d got a local Stems file from a late write for server one's track: HTTP %d %s", id2, a.code, a.body)
	}
}

// Round 4 (HIGH 1): a Stems lookup of server one's row that began before the change and
// resumed after server two's row was stemmed answered with server two's Stems file.
func TestReviewStemsLocalAcrossSwitch(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("lookup")
	t.Cleanup(resume)
	rig.s.tidal.pause = hook
	done := make(chan nasAnswer, 1)
	go func() { done <- rig.get(fmt.Sprintf("/v1/enginex/stems/local?id=%d", id1)) }()
	nasWaitFor(t, reached, "the Stems lookup of server one's track looked it up")
	rig.saveNAS(rig.twoURL)
	id2 := rig.browse()
	if a := rig.get(fmt.Sprintf("/v1/enginex/stems/source?id=%d", id2)); a.code != http.StatusOK {
		t.Fatalf("server two's Stems source: HTTP %d %s", a.code, a.body)
	}
	resume()
	if a := <-done; a.code != http.StatusNotFound {
		t.Fatalf("the Stems lookup of server one's track %d, begun before the change, answered HTTP %d after it: %s, want 404", id1, a.code, a.body)
	}
}

// Round 4 (HIGH 2): a load of server one's track paused after filing and before queueing
// the import took the import slot after the NAS moved to server two: server two's track
// then got no import job. The old load queues nothing now.
func TestReviewImportSlot(t *testing.T) {
	rig := newNASSwitchRig(t, withLibrary(t))
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("filed")
	t.Cleanup(resume)
	rig.jobs.pause = hook
	first := rig.streamAsync(id1)
	nasWaitFor(t, reached, "server one's download was filed")
	rig.saveNAS(rig.twoURL)
	resume()
	old := <-first
	id2 := rig.browse()
	rig.plays("server two's row", id2, rig.two)
	rig.jobs.mu.Lock()
	want := rig.jobs.files[trackKey(providers.Track{Provider: "subsonic", ID: "tr-99"})].dl
	var queued []string
	for _, v := range rig.jobs.items {
		if v.dl != nil {
			queued = append(queued, v.dl.Path)
		}
	}
	rig.jobs.mu.Unlock()
	if want == nil || len(queued) != 1 || queued[0] != want.Path {
		t.Fatalf("import jobs after the change: %q, want only server two's download %+v", queued, want)
	}
	if old.code == http.StatusOK {
		t.Fatalf("the load of server one's track answered HTTP 200 after the change (its recording=%v)", bytes.Equal(old.body, rig.one))
	}
}

// Round 4 (MEDIUM 3): playback info of server one's track that looked it up before the
// change answered 200 once server two's file was cached under the same track ID.
func TestReviewWarmPlaybackInfo(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("lookup")
	t.Cleanup(resume)
	rig.s.tidal.pause = hook
	done := make(chan nasAnswer, 1)
	go func() {
		done <- rig.get(fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", id1))
	}()
	nasWaitFor(t, reached, "the playback info looked up server one's track")
	rig.saveNAS(rig.twoURL)
	id2 := rig.browse()
	rig.plays("server two's row", id2, rig.two)
	resume()
	if a := <-done; a.code != http.StatusNotFound {
		t.Fatalf("playback info of server one's track %d, looked up before the change, answered HTTP %d with server two's file cached, want 404", id1, a.code)
	}
}

// Round 4b: an import of server one's track, accepted before the NAS moved to server
// two and not yet run, absorbed server two's import of the same track ID (same
// session and ID): server two's recording never reached the library.
func TestReviewOldImportKeepsNewImport(t *testing.T) {
	rig := newNASSwitchRig(t, withLibrary(t))
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("run")
	t.Cleanup(resume)
	rig.jobs.pause = hook
	rig.plays("server one", id1, rig.one)
	nasWaitFor(t, reached, "server one's import job started")
	rig.saveNAS(rig.twoURL)
	id2 := rig.browse()
	rig.plays("server two's row", id2, rig.two)
	rig.jobs.mu.Lock()
	want := rig.jobs.files[trackKey(providers.Track{Provider: "subsonic", ID: "tr-99"})].dl
	found, n := false, len(rig.jobs.items)
	for _, v := range rig.jobs.items {
		found = found || (want != nil && v.dl != nil && v.dl.Path == want.Path)
	}
	rig.jobs.mu.Unlock()
	if !found {
		t.Fatalf("server two's track got no import job while server one's import was unfinished (%d jobs)", n)
	}
}

// Round 4b: an import of server one's track accepted before the change ran after it
// and imported server one's recording. It drops itself now: nothing is imported.
func TestReviewOldImportDropsAfterChange(t *testing.T) {
	rig := newNASSwitchRig(t, withLibrary(t))
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("run")
	t.Cleanup(resume)
	rig.jobs.pause = hook
	rig.plays("server one", id1, rig.one)
	nasWaitFor(t, reached, "server one's import job started")
	rig.saveNAS(rig.twoURL)
	resume()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(25 * time.Millisecond) {
		jobs := rig.jobs.List()
		if len(jobs) == 1 && (jobs[0].State == "done" || jobs[0].State == "error") {
			if jobs[0].Error != errNASChanged.Error() || jobs[0].TrackID != 0 {
				t.Fatalf("server one's import, accepted before the change, ran after it: state=%s error=%q library track=%d, want it dropped",
					jobs[0].State, jobs[0].Error, jobs[0].TrackID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server one's import did not finish: %+v", jobs)
		}
	}
}

// stemmedRig is the two-server rig with a stemmed copy of each server's track, as a
// Stems source makes it: server one's row stemmed before the change, server two's after.
func stemmedRig(t *testing.T) (rig *nasSwitchRig, id1, id2 int64, src1, src2 string) {
	rig = newNASSwitchRig(t)
	stem := func(id int64) string {
		t.Helper()
		var v struct {
			Path string `json:"path"`
		}
		tidalGet(t, rig.gw.URL, fmt.Sprintf("/v1/enginex/stems/source?id=%d", id), 200, &v)
		return v.Path
	}
	rig.saveNAS(rig.oneURL)
	id1 = rig.browse()
	src1 = stem(id1)
	rig.saveNAS(rig.twoURL)
	id2 = rig.browse()
	src2 = stem(id2)
	if src1 == "" || src2 == "" || src1 == src2 {
		t.Fatalf("stemmed copies: server one %q, server two %q", src1, src2)
	}
	return rig, id1, id2, src1, src2
}

// Round 5 (re-check of 4480a0c1, HIGH 1): stems/local of server one's row passed the
// generation check, then the NAS moved to server two and its row was stemmed; the old
// request picked server two's stemmed copy. The check and the pick are one step now.
func TestReviewStemsLocalPausedAfterCheck(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("stems-local")
	t.Cleanup(resume)
	rig.s.tidal.pause = hook
	done := make(chan nasAnswer, 1)
	go func() { done <- rig.get(fmt.Sprintf("/v1/enginex/stems/local?id=%d", id1)) }()
	nasWaitFor(t, reached, "the Stems lookup of server one's track passed its lookup")
	rig.saveNAS(rig.twoURL)
	id2 := rig.browse()
	if a := rig.get(fmt.Sprintf("/v1/enginex/stems/source?id=%d", id2)); a.code != http.StatusOK {
		t.Fatalf("server two's Stems source: HTTP %d %s", a.code, a.body)
	}
	resume()
	if a := <-done; a.code != http.StatusNotFound {
		t.Fatalf("stems/local of server one's track %d, begun before the change, answered HTTP %d after it: %s, want 404", id1, a.code, a.body)
	}
}

// Round 5 (HIGH 2): a Stems file with an unrelated service's bad field decoded in part;
// the part kept server one's entry after the change, and server two's new row with the
// same track ID got server one's stemmed copy. Such a file serves nothing now, and is
// never rewritten from the part.
func TestReviewPartialStemsFileServesNothing(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	var src struct {
		Path string `json:"path"`
	}
	tidalGet(t, rig.gw.URL, fmt.Sprintf("/v1/enginex/stems/source?id=%d", id1), 200, &src)
	path := rig.s.stemsFile()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := append(bytes.TrimSuffix(bytes.TrimSpace(raw), []byte("}")), []byte(`,"spotify":[{"track":{"id":"sp-1","provider":"spotify"},"path":7}]}`)...)
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	rig.saveNAS(rig.twoURL)
	id2 := rig.browse()
	if a := rig.get(fmt.Sprintf("/v1/enginex/stems/local?id=%d", id2)); a.code != http.StatusNotFound {
		t.Fatalf("server two's track %d got a stemmed copy from a Stems file that does not decode: HTTP %d %s (server one's copy %q)", id2, a.code, a.body, src.Path)
	}
	if a := rig.get(fmt.Sprintf("/v1/enginex/stems/source?id=%d", id2)); a.code == http.StatusOK {
		t.Fatalf("a Stems source rewrote a Stems file that does not decode: HTTP %d %s", a.code, a.body)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, bad) {
		t.Fatalf("the Stems file that does not decode was rewritten:\n%s", got)
	}
}

// Round 5 (HIGH 2): when the reset could not save the Stems file, server one's entry
// stayed in it and server two's row with the same track ID got server one's copy.
func TestReviewFailedStemsResetServesNoOldEntry(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	var src struct {
		Path string `json:"path"`
	}
	tidalGet(t, rig.gw.URL, fmt.Sprintf("/v1/enginex/stems/source?id=%d", id1), 200, &src)
	saved := saveStemsFile
	saveStemsFile = func(string, any) error { return errors.New("disk full") }
	rig.saveNAS(rig.twoURL)
	saveStemsFile = saved
	id2 := rig.browse()
	if a := rig.get(fmt.Sprintf("/v1/enginex/stems/local?id=%d", id2)); a.code != http.StatusNotFound {
		t.Fatalf("server two's track %d got server one's stemmed copy after the reset could not save: HTTP %d %s", id2, a.code, a.body)
	}
	if list := rig.s.stemmed("subsonic"); len(list) != 0 {
		t.Fatalf("the Stems list still has %d of server one's rows after the reset could not save", len(list))
	}
}

// Round 5 (HIGH 3): an import of server one's track paused after its last generation
// check committed server one's recording after the NAS moved to server two. The check
// and the import are one step now, under a lock the reset takes.
func TestReviewImportPausedAfterLastCheck(t *testing.T) {
	var imported atomic.Int32
	// Real songs, so the analysis passes and the job reaches the library import.
	song := func(seconds int) []byte {
		var buf bytes.Buffer
		if err := testaudio.FLAC(&buf, testaudio.Song(seconds)); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	rig := newNASSwitchRigWith(t, song(8), song(10), withLibrary(t), func(p *Pipeline) {
		p.Library = importerFunc(func(context.Context, analyzer.Import) (int64, error) { imported.Add(1); return 1, nil })
	})
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("import")
	t.Cleanup(resume)
	rig.jobs.pause = hook
	rig.plays("server one", id1, rig.one)
	nasWaitFor(t, reached, "server one's import passed its last check")
	switched := make(chan struct{})
	go func() { rig.saveNAS(rig.twoURL); close(switched) }()
	select {
	case <-switched:
		resume()
		t.Fatal("the NAS change finished while server one's import was between its last check and the library import")
	case <-time.After(300 * time.Millisecond):
	}
	resume()
	nasWaitFor(t, switched, "the NAS change finished after the import")
	if n := imported.Load(); n != 1 {
		t.Fatalf("library imports: %d, want 1 (server one's, before the change)", n)
	}
}

// Round 5 (review note): without a library, a load of server one's track paused after
// filing returned server one's bytes after the change; queueImport returned before its
// generation check.
func TestReviewNoLibraryLoadAfterChange(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("filed")
	t.Cleanup(resume)
	rig.jobs.pause = hook
	first := rig.streamAsync(id1)
	nasWaitFor(t, reached, "server one's download was filed")
	rig.saveNAS(rig.twoURL)
	resume()
	if a := <-first; a.code != http.StatusNotFound {
		t.Fatalf("the load of server one's track answered HTTP %d after the change (its recording=%v), want 404", a.code, bytes.Equal(a.body, rig.one))
	}
}

// Round 5 (review note): playback info of server one's track paused after its generation
// check and before reading the cache advertised a stream after the change. It answers 404.
func TestReviewPlaybackInfoPausedAfterCheck(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	rig.plays("server one", id1, rig.one)
	hook, reached, resume := pauseOnce("codec")
	t.Cleanup(resume)
	rig.s.tidal.pause = hook
	done := make(chan nasAnswer, 1)
	go func() {
		done <- rig.get(fmt.Sprintf("/v1/tracks/%d/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL", id1))
	}()
	nasWaitFor(t, reached, "playback info of server one's track passed its check")
	rig.saveNAS(rig.twoURL)
	id2 := rig.browse()
	rig.plays("server two's row", id2, rig.two) // server two's file cached under the same track ID
	resume()
	if a := <-done; a.code != http.StatusNotFound {
		t.Fatalf("playback info of server one's track %d, checked before the change, answered HTTP %d after it, want 404", id1, a.code)
	}
}

type importerFunc func(context.Context, analyzer.Import) (int64, error)

func (f importerFunc) Import(ctx context.Context, v analyzer.Import) (int64, error) { return f(ctx, v) }

// saveNASAsync is saveNAS for a save that pauses: it reports the HTTP status.
func (rig *nasSwitchRig) saveNASAsync(url string) chan int {
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":"dj","password":"pw","enabled":true}`, url)
		rig.s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(body)))
		done <- w.Code
	}()
	return done
}

// Round 5 (the rule, applied to the reset): the new settings were readable before the
// reset bumped the generation, so a load of server one's row in that gap downloaded
// server two's recording and played it under server one's ID. The writer now resets
// before anyone can read the new settings.
func TestReviewLoadBeforeResetBump(t *testing.T) {
	rig := newNASSwitchRig(t)
	rig.saveNAS(rig.oneURL)
	id1 := rig.browse()
	hook, reached, resume := pauseOnce("reset")
	t.Cleanup(resume)
	rig.jobs.pause = hook
	saved := rig.saveNASAsync(rig.twoURL)
	nasWaitFor(t, reached, "the reset began")
	load := rig.streamAsync(id1)
	var a nasAnswer
	select {
	case a = <-load: // answered inside the gap
	case <-time.After(500 * time.Millisecond):
	}
	resume()
	if a.code == 0 {
		a = <-load
	}
	if code := <-saved; code != http.StatusOK {
		t.Fatalf("save server two: HTTP %d", code)
	}
	if a.code != http.StatusNotFound {
		t.Fatalf("a load of server one's track %d during the change answered HTTP %d (server two's recording=%v), want 404",
			id1, a.code, bytes.Equal(a.body, rig.two))
	}
}

// Round 5 (the rule, applied to the reset): the reset bumped the generation, then dropped
// the NAS Stems entries in a second step; a Stems list read between the two took the new
// generation with root A's entry and published it. Both now change in one step.
func TestReviewStemsListDuringReset(t *testing.T) {
	uiDir, stateDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=nas\nomni_source=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	gw := httptest.NewServer(s)
	t.Cleanup(gw.Close)
	save := func(root string) chan int {
		done := make(chan int, 1)
		go func() {
			w := httptest.NewRecorder()
			body := fmt.Sprintf(`{"protocol":"smb","url":"smb://nas.example/music/%s","username":"dj","password":"pw","enabled":true}`, root)
			s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(body)))
			done <- w.Code
		}()
		return done
	}
	if code := <-save("rootA"); code != http.StatusOK {
		t.Fatalf("save rootA: HTTP %d", code)
	}
	opener := stemmedTrack{Track: providers.Track{ID: "Sets/Opener.flac", Provider: "smb", Title: "Opener"}, Session: nasScope, Path: filepath.Join(stateDir, "opener.flac")}
	if err := s.addStemmed(nasScope, opener); err != nil {
		t.Fatal(err)
	}
	hook, reached, resume := pauseOnce("reset-bumped")
	t.Cleanup(resume)
	jobs.pause = hook
	saved := save("rootB")
	nasWaitFor(t, reached, "the reset bumped the generation")
	list := make(chan nasAnswer, 1)
	go func() {
		resp, err := http.Get(gw.URL + "/v1/playlists/" + stemsListID(nasScope) + "/tracks?limit=50&offset=0")
		if err != nil {
			list <- nasAnswer{code: -1}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		list <- nasAnswer{resp.StatusCode, body}
	}()
	var a nasAnswer
	select {
	case a = <-list:
	case <-time.After(500 * time.Millisecond):
	}
	resume()
	if a.code == 0 {
		a = <-list
	}
	<-saved
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(a.body, &page); err != nil || a.code != http.StatusOK || len(page.Items) != 0 {
		t.Fatalf("a Stems list read during the change published %d rows (HTTP %d): %s", len(page.Items), a.code, a.body)
	}
}
