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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func serverURLPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func linkPeerForTest(t *testing.T, dst, src *Server, srcHTTPURL string) {
	t.Helper()
	port := serverURLPort(t, srcHTTPURL)
	dst.noteSharePeer("127.0.0.1", time.Now())
	dst.shareMu.Lock()
	if dst.sharePeerPorts == nil {
		dst.sharePeerPorts = map[string]int{}
	}
	dst.sharePeerPorts["127.0.0.1"] = port
	dst.shareMu.Unlock()
	_ = src
}

func TestPeerServersOverLinkBrowsePlayAndZeroCredentials(t *testing.T) {
	davSecret := "super-secret-dav-pass-9876"
	davUser := "davuser"
	davAudio := testMP3(120)
	var authedGets atomic.Int32
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(davUser+":"+davSecret))
		if r.Header.Get("Authorization") != wantAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="dav"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "PROPFIND":
			io.Copy(io.Discard, r.Body)
			var body string
			switch strings.TrimSuffix(r.URL.Path, "/") {
			case "/music":
				body = davXML("/music/", "/music/House/", "/music/intro.mp3")
			case "/music/House":
				body = davXML("/music/House/", "/music/House/a.mp3")
			default:
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			io.WriteString(w, body)
		case http.MethodGet:
			authedGets.Add(1)
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(davAudio)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	defer davSrv.Close()

	smb := testsmb.Start(t)
	smbAudio := testMP3(150)
	if err := os.WriteFile(filepath.Join(smb.Root, "smb-track.mp3"), smbAudio, 0o600); err != nil {
		t.Fatal(err)
	}

	nasCfg := nasFile{
		NASConfig: providers.NASConfig{
			Protocol: "smb",
			URL:      smb.URL,
			Username: smb.User,
			Password: smb.Password,
			Enabled:  true,
		},
		Name: "Deck A SMB",
		WebDAV: []savedServer{
			{
				ID:       "0123456789abcdef0123456789abcdef",
				Name:     "Deck A DAV",
				URL:      davSrv.URL + "/music/",
				Username: davUser,
				Password: davSecret,
				Enabled:  true,
			},
		},
	}
	rawNAS, _ := json.Marshal(nasCfg)

	rigA := newWebDAVRig(t, string(rawNAS), "sources=\nscope=\nomni_source=1\nwebdav=1\nnas=1\n")
	dlB := t.TempDir()
	uiB := t.TempDir()
	stateB := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiB, "settings.conf"), []byte("sources=deezer\nscope=deezer\nomni_source=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rigB := newWebDAVRigAt(t, uiB, stateB, dlB)
	linkPeerForTest(t, rigB.s, rigA.s, rigA.gw.URL)
	rigB.s.refreshPeerServers(t.Context())

	// 1. /peer/servers and /v1/enginex/ui expose both linked servers with zero passwords/URLs.
	peerBody := tidalGet(t, rigA.gw.URL, "/peer/servers", 200, nil)
	statusB := rigB.status(t)
	statusJSON, _ := json.Marshal(statusB)
	for _, secret := range []string{davSecret, smb.Password, smb.URL, davSrv.URL} {
		if strings.Contains(string(peerBody), secret) {
			t.Fatalf("/peer/servers leaked %q: %s", secret, peerBody)
		}
		if strings.Contains(string(statusJSON), secret) {
			t.Fatalf("/v1/enginex/ui leaked %q: %s", secret, statusJSON)
		}
	}

	var davScope, smbScope string
	for _, lv := range statusB.LinkedServers {
		switch lv.Kind {
		case "webdav":
			davScope = lv.Scope
		case "nas":
			smbScope = lv.Scope
		}
	}
	if davScope == "" || smbScope == "" {
		t.Fatalf("missing linked servers on B: %+v", statusB.LinkedServers)
	}
	if !slices.Contains(statusB.Sources, davScope) || !slices.Contains(statusB.LoggedIn, davScope) ||
		!slices.Contains(statusB.Sources, smbScope) || !slices.Contains(statusB.LoggedIn, smbScope) {
		t.Fatalf("linked scopes missing from sources/loggedIn: sources=%v loggedIn=%v", statusB.Sources, statusB.LoggedIn)
	}

	// 2. B browses and plays a track from A's WebDAV over Link (authenticated only by A).
	davLists := rigB.open(t, davScope)
	houseUUID := davLists["House"]
	if houseUUID == "" {
		t.Fatalf("linked WebDAV playlists missing House: %v", davLists)
	}
	davTrack := rigB.tracks(t, houseUUID)["a"]
	if davTrack == nil {
		t.Fatal("linked WebDAV track a missing")
	}
	if rowAlbum(davTrack) != "Deck A DAV" {
		t.Fatalf("unexpected album %q", rowAlbum(davTrack))
	}
	davTrackID := rowID(davTrack)
	if code, got := rigB.stream(t, davTrackID); code != 200 || !bytes.Equal(got, davAudio) {
		t.Fatalf("stream linked WebDAV: HTTP %d len=%d want=%d", code, len(got), len(davAudio))
	}
	if authedGets.Load() == 0 {
		t.Fatal("Deck A did not fetch WebDAV audio with its own Basic Auth credentials")
	}

	// Verify B cached with evictable nas-<sha256>.mp3 filename and no secrets anywhere on B.
	cachedFiles, err := filepath.Glob(filepath.Join(dlB, "nas-*.mp3"))
	if err != nil || len(cachedFiles) == 0 {
		t.Fatalf("expected evictable nas-<sha256>.mp3 cache file in %s, got %v (%v)", dlB, cachedFiles, err)
	}
	evictableRe := regexp.MustCompile(`^nas-[0-9a-f]{64}\.mp3$`)
	if !evictableRe.MatchString(filepath.Base(cachedFiles[0])) {
		t.Fatalf("cache filename %q does not match deemix evictable pattern", filepath.Base(cachedFiles[0]))
	}

	// 3. B browses and plays a track from A's SMB NAS over Link.
	smbLists := rigB.open(t, smbScope)
	var smbListUUID string
	for title, id := range smbLists {
		if id != hintPlaylist {
			smbListUUID = id
			break
		}
		t.Logf("SMB hint playlist %q -> %s", title, id)
	}
	if smbListUUID == "" {
		t.Fatalf("linked NAS playlists failed or empty: %v", smbLists)
	}
	smbTracks := rigB.tracks(t, smbListUUID)
	var smbTrack gwItem
	for _, it := range smbTracks {
		smbTrack = it
		break
	}
	if smbTrack == nil {
		t.Fatalf("linked NAS track missing in %v (lists=%v)", smbTracks, smbLists)
	}
	if code, got := rigB.stream(t, rowID(smbTrack)); code != 200 || !bytes.Equal(got, smbAudio) {
		t.Fatalf("stream linked NAS: HTTP %d len=%d want=%d", code, len(got), len(smbAudio))
	}

	// 4. OmniSource search on B returns tracks from linked peer servers outside current scope.
	found := rigB.search(t, "intro")
	titles := webdavTitles(found)
	if !slices.Contains(titles, "intro@Deck A DAV") {
		t.Fatalf("OmniSource search missing linked WebDAV track: %v", titles)
	}

	// Confirm zero secrets anywhere in B's ui/state/download files.
	for _, dir := range []string{uiB, stateB, dlB} {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil
			}
			data, _ := os.ReadFile(p)
			for _, secret := range []string{davSecret, smb.Password, davSrv.URL, smb.URL} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatalf("secret/URL %q leaked into B file %s", secret, p)
				}
			}
			return nil
		})
	}

	// 5. Adding identical server locally on B deduplicates it out of linkedServers.
	rigB.change(t, serverChange{Action: "add", Name: "Local DAV", URL: davSrv.URL + "/music/", Username: davUser, Password: davSecret}, 200)
	if code, _ := uiPost(t, rigB.gw.URL, "webdav=1"); code != 200 {
		t.Fatalf("enable local webdav: HTTP %d", code)
	}
	afterLocal := rigB.status(t)
	for _, lv := range afterLocal.LinkedServers {
		if lv.Kind == "webdav" {
			t.Fatalf("expected duplicate linked WebDAV to be deduplicated, still got %+v", afterLocal.LinkedServers)
		}
	}

	// 6. When A switches WebDAV off, B drops the linked server and rejects cached playback too.
	if code, _ := uiPost(t, rigA.gw.URL, "webdav=0"); code != 200 {
		t.Fatalf("disable A webdav: HTTP %d", code)
	}
	rigB.s.refreshPeerServers(t.Context())
	if code, _ := rigB.stream(t, davTrackID); code != http.StatusNotFound {
		t.Fatalf("stream after peer disabled WebDAV: HTTP %d, want 404", code)
	}
	ref := refOf(rigB.s.tidal, davTrackID)
	if p := rigB.s.jobs.FileAt(ref.track, ref.gen); p != "" {
		t.Fatalf("FileAt served stale cached peer track path %q after peer disabled WebDAV", p)
	}
}

func TestPeerServersRejectNonLANAndProxiedRequests(t *testing.T) {
	s, _ := copyTestServer(t)

	for _, p := range []string{"/peer/servers", "/peer/servers/webdav/0123456789abcdef0123456789abcdef/playlists"} {
		w := copyTestRequest(s, http.MethodGet, p, "", "8.8.8.8:1234", "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("non-LAN GET %s: HTTP %d, want 403", p, w.Code)
		}

		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.RemoteAddr = "192.168.1.50:1234"
		req.Header.Set("Cf-Ray", "12345")
		rw := httptest.NewRecorder()
		s.ServeHTTP(rw, req)
		if rw.Code != http.StatusNotFound {
			t.Fatalf("proxied GET %s: HTTP %d, want 404", p, rw.Code)
		}
	}

	fwd := httptest.NewServer(s.ForwardHandler())
	defer fwd.Close()
	resp, err := http.Get(fmt.Sprintf("%s/peer/servers", fwd.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("forwarded /peer/servers: HTTP %d, want 404", resp.StatusCode)
	}
}
