package webui

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
)

// #335 round 2: the released [NAS] playlists route has the flaw of the SMB root
// switch. Two servers can use the same playlist and track IDs; after the NAS
// moved to the second one, the first one's download (finished, or still
// running at the change) answered for the second one's track. Nothing of the
// old server may answer once the NAS settings or the mounts page moved the NAS.
func TestNASServerChangeNeverServesTheOldServersTrack(t *testing.T) {
	newNAS := func(audio []byte, hold, asked chan struct{}) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/rest/getPlaylists.view":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[{"id":"pl-42","name":"Club Crate","songCount":1}]}}}`)
			case "/rest/getPlaylist.view":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl-42","name":"Club Crate","entry":[{"id":"tr-99","title":"Warehouse Dub","artist":"DJ Test","album":"NAS Vol 1","duration":210,"suffix":"mp3"}]}}}`)
			case "/rest/download.view":
				if hold != nil {
					select {
					case asked <- struct{}{}:
					default:
					}
					<-hold
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
	one, two := testMP3(120), testMP3(150)
	hold, asked := make(chan struct{}), make(chan struct{}, 1)
	oneURL, twoURL := newNAS(one, nil, nil), newNAS(two, hold, asked)

	uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("scope=spotify\nomni_source=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dlDir, AllowLossy: true}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	s.sessions["sp-owner"] = &Session{ID: "sp-owner", Provider: "spotify", Ready: true, p: &browseTestProvider{}}
	gw := httptest.NewServer(s)
	t.Cleanup(gw.Close)
	released := false
	release := func() {
		if !released {
			released = true
			close(hold)
		}
	}
	t.Cleanup(release) // before gw.Close, which waits for a held request

	saveNAS := func(url string) {
		t.Helper()
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"protocol":"subsonic","url":%q,"username":"dj","password":"pw","enabled":true}`, url)
		s.handleNASConfig(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/nas", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("save NAS: %d %s", w.Code, w.Body.String())
		}
	}
	browse := func() int64 {
		t.Helper()
		var lists, tracks struct {
			Items []map[string]any `json:"items"`
		}
		tidalGet(t, gw.URL, "/v1/users/1/playlists?limit=50&offset=0", 200, &lists)
		for _, it := range lists.Items {
			if it["title"] == "[NAS] Club Crate" {
				tidalGet(t, gw.URL, "/v1/playlists/"+it["uuid"].(string)+"/tracks?limit=50&offset=0", 200, &tracks)
			}
		}
		if len(tracks.Items) != 1 {
			t.Fatalf("no [NAS] Club Crate track: lists=%+v tracks=%+v", lists.Items, tracks.Items)
		}
		return int64(tracks.Items[0]["id"].(float64))
	}
	type answer struct {
		code int
		body []byte
	}
	stream := func(id int64) answer { // no t calls: also runs in a goroutine
		resp, err := http.Get(fmt.Sprintf("%s/v1/stream/%d.mp3", gw.URL, id))
		if err != nil {
			return answer{code: -1}
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return answer{resp.StatusCode, body}
	}
	plays := func(step string, id int64, want []byte) {
		t.Helper()
		if a := stream(id); a.code != http.StatusOK || !bytes.Equal(a.body, want) {
			t.Fatalf("%s: track %d answered HTTP %d with %d bytes (server one's recording=%v, server two's=%v)",
				step, id, a.code, len(a.body), bytes.Equal(a.body, one), bytes.Equal(a.body, two))
		}
	}
	gone := func(step string, id int64) {
		t.Helper()
		if a := stream(id); a.code != http.StatusNotFound {
			t.Fatalf("%s: old track %d answered HTTP %d with %d bytes (a server's recording=%v), want 404",
				step, id, a.code, len(a.body), bytes.Equal(a.body, one) || bytes.Equal(a.body, two))
		}
	}

	// 1. Server one plays its track.
	saveNAS(oneURL)
	id1 := browse()
	plays("server one", id1, one)

	// 2. Server two has the same playlist and track IDs: its track comes from it.
	saveNAS(twoURL)
	id2 := browse()
	first, second := make(chan answer, 1), make(chan answer, 1)
	go func() { first <- stream(id2) }()
	select {
	case <-asked:
	case a := <-first:
		t.Fatalf("server two's track %d (the same ID as server one's: %v) answered HTTP %d without asking server two (server one's recording=%v)",
			id2, id2 == id1, a.code, bytes.Equal(a.body, one))
	case <-time.After(10 * time.Second):
		t.Fatal("server two was never asked for its track")
	}
	gone("after the change to server two", id1)
	// A second load of the same track waits for that download (Engine asks for the
	// playback info and the stream of one track at once).
	go func() { second <- stream(id2) }()
	time.Sleep(300 * time.Millisecond)

	// 3. Back to server one while server two's download runs: neither load answers (the
	// waiting one must not fetch the track again, now from server one), nothing is kept
	// for the track, and server one serves it afresh.
	saveNAS(oneURL)
	release()
	for i, ch := range []chan answer{first, second} {
		if a := <-ch; a.code == http.StatusOK {
			t.Fatalf("load %d of server two's track, begun before the change, answered HTTP 200 after it (server two's recording=%v, server one's=%v)",
				i+1, bytes.Equal(a.body, two), bytes.Equal(a.body, one))
		}
	}
	id3 := browse()
	plays("server one again", id3, one)
	gone("after the change back to server one", id2)

	// 4. Removing the NAS in its settings.
	w := httptest.NewRecorder()
	s.handleNASConfig(w, httptest.NewRequest(http.MethodDelete, "/v1/enginex/nas", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("remove NAS: %d %s", w.Code, w.Body.String())
	}
	gone("after removing the NAS", id3)

	// 5. The mounts page writes nas.json too: a WebDAV share saved there replaces the
	// NAS, and deleting the last WebDAV share removes it.
	mount := func(method, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleMounts(w, httptest.NewRequest(method, "/v1/enginex/mounts?id=dav-1", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s mount: %d %s", method, w.Code, w.Body.String())
		}
	}
	saveNAS(oneURL)
	id4 := browse()
	plays("server one before the mounts page", id4, one)
	mount(http.MethodPost, fmt.Sprintf(`{"id":"dav-1","name":"dav-1","protocol":"webdav","host":"nas.example","port":8080,"remotePath":"/music","mountPoint":%q,"enabled":false}`,
		filepath.Join(t.TempDir(), "dav")))
	gone("after a WebDAV share on the mounts page replaced the NAS", id4)
	saveNAS(oneURL)
	id5 := browse()
	plays("server one with the WebDAV share saved", id5, one)
	mount(http.MethodDelete, "")
	gone("after deleting the WebDAV share removed the NAS", id5)
}
