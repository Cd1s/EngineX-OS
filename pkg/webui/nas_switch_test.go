package webui

import (
	"bytes"
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

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
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
}

type nasAnswer struct {
	code int
	body []byte
}

func newNASSwitchRig(t *testing.T) *nasSwitchRig {
	rig := &nasSwitchRig{t: t, one: testMP3(120), two: testMP3(150), asked: make(chan struct{}, 4)}
	nas := func(audio []byte, held bool) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/rest/getPlaylists.view":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-42","name":"Club Crate","songCount":1}]}}}`)
			case "/rest/getPlaylist.view":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl-42","name":"Club Crate","entry":[{"id":"tr-99","title":"Warehouse Dub","artist":"DJ Test","album":"NAS Vol 1","duration":210,"suffix":"mp3"}]}}}`)
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
				w.Header().Set("Content-Type", "audio/mpeg")
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
	rig.jobs = NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true}})
	t.Cleanup(rig.jobs.Close)
	rig.s = New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, rig.jobs)
	t.Cleanup(rig.s.Close)
	rig.s.sessions["sp-owner"] = &Session{ID: "sp-owner", Provider: "spotify", Ready: true, p: &browseTestProvider{}}
	rig.gw = httptest.NewServer(rig.s)
	t.Cleanup(rig.gw.Close)
	t.Cleanup(rig.release) // before gw.Close, which waits for a held request
	return rig
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
