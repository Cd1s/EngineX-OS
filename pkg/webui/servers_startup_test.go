package webui

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// nasFileWays are the states a start-up can find nas.json in when it is not the file
// that was saved. The first three cannot be read as a set-up (unreadableNASFile); the
// others read as a set-up without any server.
var nasFileWays = []string{"read error", "malformed", "array", "null", "empty object", "no file"}

func unreadableNASFile(how string) bool {
	return how == "read error" || how == "malformed" || how == "array"
}

// breakNASFile puts nas.json in one of nasFileWays: "read error" puts a directory in its
// place (every read of it fails), "malformed" cuts its JSON short, "array", "null" and
// "empty object" replace it with [], null and {}, "no file" removes it. fix puts the
// unchanged file back.
func breakNASFile(t *testing.T, how, uiDir string) (fix func()) {
	t.Helper()
	path := filepath.Join(uiDir, "nas.json")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	switch how {
	case "read error":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	case "malformed":
		if err := os.WriteFile(path, good[:len(good)/2], 0o600); err != nil {
			t.Fatal(err)
		}
	case "array", "null", "empty object":
		content := map[string]string{"array": "[]", "null": "null", "empty object": "{}"}[how]
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	case "no file":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown way to break nas.json: %s", how)
	}
	return func() {
		t.Helper()
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, good, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// nasFileState is what is at nas.json's place, to tell whether anything wrote it.
func nasFileState(uiDir string) string {
	path := filepath.Join(uiDir, "nas.json")
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return "nothing"
	case info.IsDir():
		return "a directory"
	}
	b, _ := os.ReadFile(path)
	return string(b)
}

// editorsRefuse: while nas.json cannot be read, every editor of it (the deck and phone
// list, the console NAS card, the Mounts card) answers 500 and leaves it as it is, and
// its views answer 500 instead of "nothing set up". On 7a1424de each took the file as
// empty: the views showed no server, and a save wrote a file without the servers it
// named.
func editorsRefuse(t *testing.T, s *Server, uiDir string) {
	t.Helper()
	before, mounts := nasFileState(uiDir), len(s.loadMounts())
	mountPoint := filepath.Join(t.TempDir(), "Share")
	for _, c := range []struct {
		name         string
		h            http.HandlerFunc
		method, body string
		kind         string
	}{
		{"deck list", s.handleServers, http.MethodGet, "", "webdav"},
		{"deck add", s.handleServers, http.MethodPost, `{"action":"add","name":"Other","url":"http://other.example/music/"}`, "webdav"},
		{"console NAS view", s.handleNASConfig, http.MethodGet, "", ""},
		{"console NAS save", s.handleNASConfig, http.MethodPost, `{"protocol":"webdav","url":"http://other.example/dav/","enabled":true}`, ""},
		{"console NAS remove", s.handleNASConfig, http.MethodDelete, "", ""},
		{"Mounts card", s.handleMounts, http.MethodPost, fmt.Sprintf(`{"id":"m9","name":"Other","protocol":"webdav","host":"other.example","remotePath":"/dav","enabled":false,"mountPoint":%q}`, mountPoint), ""},
	} {
		req := httptest.NewRequest(c.method, "/v1/enginex/x", strings.NewReader(c.body))
		if c.kind != "" {
			req.SetPathValue("kind", c.kind)
		}
		w := httptest.NewRecorder()
		c.h(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s without a readable nas.json: HTTP %d %s, want 500", c.name, w.Code, strings.TrimSpace(w.Body.String()))
		}
		if now := nasFileState(uiDir); now != before {
			t.Errorf("%s wrote over nas.json that could not be read: %q became %q", c.name, before, now)
			before = now
		}
	}
	if len(s.loadMounts()) != mounts {
		t.Errorf("the Mounts card saved a WebDAV mount it could not sync to nas.json")
	}
}

// addOtherServer adds an unrelated WebDAV server on the deck: a save of nas.json that
// removes no server.
func addOtherServer(t *testing.T, s *Server) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/enginex/x", strings.NewReader(`{"action":"add","name":"Other","url":"http://other.example/music/"}`))
	req.SetPathValue("kind", "webdav")
	w := httptest.NewRecorder()
	s.handleServers(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("adding another server: HTTP %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// WebDAV round 4 (round-10 re-check, item 4): a start-up that could not read nas.json (a
// read error, or malformed JSON) took it as "no server set up" and cleaned up after
// every server: once the unchanged file was back, the server's gateway row answered 404,
// its download was gone from the index and its Stems entry from the Stems lists. Round 4
// ran no clean-up unless nas.json was read in full.
//
// WebDAV round 5 (round-11 re-check, item 4; PM decision): start-up never deletes. On
// 16563658 a nas.json holding null or {} (or none) read as "no server set up", and the
// start-up clean-up dropped the server's row, download and Stems entry for good. Now
// start-up drops nothing whatever nas.json holds, a save that removes no server drops
// nothing either, and only removing the server clears what was its own.
func TestServersStartUpWithoutTheNASFileDropsNothing(t *testing.T) {
	for _, how := range nasFileWays {
		t.Run(how, func(t *testing.T) {
			d := newFakeDAV(t, testMP3(120), nil)
			uiDir, stateDir, dlDir := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("sources=\nscope=\nomni_source=0\nwebdav=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r := newWebDAVRigAt(t, uiDir, stateDir, dlDir)
			views := r.change(t, serverChange{Action: "add", Name: "Studio", URL: d.srv.URL + "/music/"}, 200)
			scope := scopeOf(views, "Studio")
			id := rowID(r.tracks(t, r.open(t, scope)["House"])["a"])
			if code, got := r.stream(t, id); code != 200 || !bytes.Equal(got, d.audio) {
				t.Fatalf("first play: HTTP %d, %d bytes", code, len(got))
			}
			ref := refOf(r.s.tidal, id)
			if err := r.s.addStemmedAt(rowScope(ref.track), stemmedTrack{Track: ref.track, Session: ref.session, Path: "/x/" + ref.track.ID}, r.s.jobs.NASGen()); err != nil {
				t.Fatal(err)
			}
			restart := func() {
				r.gw.Close()
				r.s.Close()
				r.s.jobs.Close()
				r = newWebDAVRigAt(t, uiDir, stateDir, dlDir)
			}

			// 1. A start without the saved nas.json keeps the server's row. While the file
			// cannot be read no editor writes over it; while it reads as a set-up without
			// servers, a save that removes none (another server added) drops nothing.
			fix := breakNASFile(t, how, uiDir)
			restart()
			if refOf(r.s.tidal, id).track.ID != ref.track.ID {
				t.Fatalf("a start with nas.json as %q dropped the server's row", how)
			}
			if unreadableNASFile(how) {
				editorsRefuse(t, r.s, uiDir)
			} else {
				addOtherServer(t, r.s)
				if refOf(r.s.tidal, id).track.ID != ref.track.ID {
					t.Fatalf("adding another server dropped the row of a server it did not remove")
				}
			}

			// 2. The unchanged file back: the same row plays its download (no new request to
			// the server), and the server keeps its index entry and Stems entry.
			fix()
			restart()
			gets := d.gets.Load()
			if code, got := r.stream(t, id); code != 200 || !bytes.Equal(got, d.audio) {
				t.Fatalf("the row once nas.json was back: HTTP %d, %d bytes (a start without it dropped the row)", code, len(got))
			}
			if n := d.gets.Load() - gets; n != 0 {
				t.Errorf("the row was downloaded again (%d requests): a start without nas.json dropped its download", n)
			}
			if index, _ := os.ReadFile(filepath.Join(dlDir, sourcesFile)); !strings.Contains(string(index), views[0].ID) {
				t.Errorf("the download index no longer names the server: %s", index)
			}
			if n := len(r.s.stemmed(scope)); n != 1 {
				t.Errorf("the server's Stems list holds %d entries, want 1", n)
			}
			if t.Failed() {
				return
			}

			// 3. Removing the server is what clears it: its row, its download and index
			// entry, its Stems entry and its search results.
			if code, _ := uiPost(t, r.gw.URL, "scope="+scope); code != http.StatusOK {
				t.Fatalf("selecting the server: HTTP %d", code)
			}
			r.tracks(t, r.open(t, scope)["House"])
			if got := webdavTitles(r.search(t, "a")); !slices.Equal(got, []string{"a@Studio"}) {
				t.Fatalf("search before the removal: %v", got)
			}
			if r.s.jobs.FileAt(ref.track, r.s.jobs.NASGen()) == "" {
				t.Fatal("the server's download is not used before the removal")
			}
			r.change(t, serverChange{Action: "remove", ID: views[0].ID}, 200)
			if code, _ := r.stream(t, id); code != http.StatusNotFound {
				t.Errorf("the removed server's row: HTTP %d, want 404", code)
			}
			if file := r.s.jobs.FileAt(ref.track, r.s.jobs.NASGen()); file != "" {
				t.Errorf("the removed server's download is still used: %s", file)
			}
			if index, _ := os.ReadFile(filepath.Join(dlDir, sourcesFile)); strings.Contains(string(index), views[0].ID) {
				t.Errorf("the download index still names the removed server: %s", index)
			}
			if n := len(r.s.stemmed(scope)); n != 0 {
				t.Errorf("the removed server's Stems list holds %d entries, want none", n)
			}
			if got := webdavTitles(r.search(t, "a")); len(got) != 0 {
				t.Errorf("search after the removal: %v, want no hit of the removed server", got)
			}
			r.s.tidal.saveTracks()
			if saved, _ := os.ReadFile(filepath.Join(stateDir, "gateway-tracks.json")); strings.Contains(string(saved), views[0].ID) {
				t.Errorf("gateway-tracks.json keeps rows of the removed server")
			}
		})
	}
}

// The NAS slot (here Subsonic, its rows named by bare IDs and guarded by the NAS root):
// a start without a readable nas.json does not know the slot's root. Its row, download
// and Stems entry are kept as saved and none is served in that run. With the unchanged
// file back, the next start plays the row from its download; a save of the same settings
// in that run serves it at once, and a save of another server drops it.
//
// WebDAV round 5: the same when nas.json reads as a set-up that names no server for the
// slot (null, {}, no file). On 16563658 such a start filed the slot's row and download
// under "no NAS" and dropped them for good.
func TestNASStartUpWithoutTheNASFileKeepsTheSlot(t *testing.T) {
	cached := func(rig *nasSwitchRig) string {
		return rig.jobs.FileAt(providers.Track{Provider: "subsonic", ID: "tr-99"}, rig.jobs.NASGen())
	}
	stemmed := func(rig *nasSwitchRig, id int64) int {
		return rig.get(fmt.Sprintf("/v1/enginex/stems/local?id=%d", id)).code
	}
	for _, how := range nasFileWays {
		t.Run(how, func(t *testing.T) {
			rig := newNASSwitchRig(t)
			rig.saveNAS(rig.oneURL)
			id1 := rig.browse()
			rig.plays("before the restart", id1, rig.one) // its download in the index
			if a := rig.get(fmt.Sprintf("/v1/enginex/stems/source?id=%d", id1)); a.code != http.StatusOK {
				t.Fatalf("Stems source: HTTP %d %s", a.code, a.body)
			}
			rig.stop()

			// 1. Without the saved nas.json the slot's row is kept and not served; a save
			// that names no server for the slot (another server added) changes nothing.
			fix := breakNASFile(t, how, rig.uiDir)
			rig.start()
			if refOf(rig.s.tidal, id1).track.ID == "" {
				t.Fatalf("a start with nas.json as %q dropped the slot's row", how)
			}
			if a := rig.stream(id1); a.code == http.StatusOK {
				t.Fatalf("the slot's row played while its root was not known (%d bytes)", len(a.body))
			}
			if cached(rig) != "" {
				t.Fatal("the slot's download was used while its root was not known")
			}
			if unreadableNASFile(how) {
				editorsRefuse(t, rig.s, rig.uiDir)
			} else {
				addOtherServer(t, rig.s)
				if refOf(rig.s.tidal, id1).track.ID == "" {
					t.Fatal("adding another server dropped the slot's row")
				}
			}

			// 2. The unchanged file back and a restart: the row plays its download, with its
			// stemmed copy.
			rig.stop()
			fix()
			rig.start()
			if cached(rig) == "" {
				t.Fatal("a start without nas.json lost the slot's download")
			}
			rig.plays("the slot's row once nas.json was back", id1, rig.one)
			if code := stemmed(rig, id1); code != http.StatusOK {
				t.Fatalf("the slot's row lost its stemmed copy: HTTP %d", code)
			}

			// 3. The file back in the same run and the same settings saved: the slot is
			// served again, its row and download as they were.
			rig.stop()
			fix = breakNASFile(t, how, rig.uiDir)
			rig.start()
			fix()
			rig.saveNAS(rig.oneURL)
			rig.plays("the slot's row after its settings were saved again", id1, rig.one)
			if cached(rig) == "" || stemmed(rig, id1) != http.StatusOK {
				t.Fatal("saving the same settings dropped the slot's download or stemmed copy")
			}

			// 4. Another server saved in such a run: the old row is gone, the new one plays
			// the new server's recording, without the old stemmed copy.
			rig.stop()
			fix = breakNASFile(t, how, rig.uiDir)
			rig.start()
			fix()
			rig.saveNAS(rig.twoURL)
			rig.gone("server one's row after server two was saved", id1)
			id2 := rig.browse()
			rig.plays("server two's row", id2, rig.two)
			if code := stemmed(rig, id2); code != http.StatusNotFound {
				t.Fatalf("server two's row got server one's stemmed copy: HTTP %d", code)
			}
		})
	}
}
