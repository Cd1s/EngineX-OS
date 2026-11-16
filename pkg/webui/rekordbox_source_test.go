package webui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"golang.org/x/text/unicode/norm"
)

// fakeRbxLink is the library of the computer with rekordbox: two tracks, one folder with a playlist.
type fakeRbxLink struct{ paths map[uint32]string }

// rbxAsked records the asking player each session was opened as.
var rbxAsked struct {
	sync.Mutex
	players []byte
}

func (f fakeRbxLink) Tracks() ([]prolink.RekordboxItem, error) {
	return []prolink.RekordboxItem{{ID: 1, Kind: 4, Label: "First"}, {ID: 2, Kind: 4, Label: "Second"}}, nil
}
func (f fakeRbxLink) Playlists(folder uint32) ([]prolink.RekordboxItem, error) {
	if folder == 7 {
		return []prolink.RekordboxItem{{ID: 9, Kind: prolink.RekordboxPlaylist, Label: "Warm up"}}, nil
	}
	return []prolink.RekordboxItem{{ID: 7, Kind: prolink.RekordboxFolder, Label: "Sets"}}, nil
}
func (f fakeRbxLink) PlaylistTracks(id uint32) ([]prolink.RekordboxItem, error) {
	return []prolink.RekordboxItem{{ID: 2, Kind: 4, Label: "Second"}}, nil
}
func (f fakeRbxLink) Path(id uint32) (string, error) {
	if p, ok := f.paths[id]; ok {
		return p, nil
	}
	return "", errors.New("no path")
}
func (f fakeRbxLink) Close() {}

type rbxEnv struct {
	s     *Server
	base  string
	share string // what a mounted network share of /Users/dj/Music shows
	vol   string // a writable drive under the media root
}

// newRbxEnv is a deck on the link with a rekordbox computer at 127.0.0.1, the share setting
// at /Users/dj/Music and one mounted share. NFS answers nothing unless a test starts a server.
func newRbxEnv(t *testing.T, paths map[uint32]string) *rbxEnv {
	e := &rbxEnv{share: t.TempDir(), vol: t.TempDir()}
	media := t.TempDir()
	if err := os.Symlink(e.vol, filepath.Join(media, "vol")); err != nil {
		t.Fatal(err)
	}
	ui := t.TempDir()
	os.WriteFile(filepath.Join(ui, "settings.conf"), []byte("scope=rekordbox\nprolink_share=1\nrekordbox_share=/Users/dj/Music\n"), 0o644)
	state := t.TempDir()
	snap, _ := json.Marshal(prolink.Snapshot{Devices: []prolink.Device{{Name: "rekordbox", Number: 17, Address: "127.0.0.1", PacketType: 0x29, LastSeen: time.Now()}}})
	os.WriteFile(filepath.Join(state, "prolink.json"), snap, 0o644)
	e.s = New(Config{AccessToken: "code", UIDir: ui, StateDir: state, MediaRoot: media, DeezerURL: fakeDeezer(t).URL}, nil)
	mounts, _ := json.Marshal([]RemoteMount{{ID: "m", Name: "Music", MountPoint: e.share, Enabled: true}})
	os.WriteFile(e.s.mountsConfigPath(), mounts, 0o644)
	srv := httptest.NewServer(e.s)
	t.Cleanup(srv.Close)
	e.base = srv.URL

	oldDial, oldMounted, oldLookup, oldNFS := dialRekordbox, mountedAt, lookupAddr, rbxNFS
	t.Cleanup(func() { dialRekordbox, mountedAt, lookupAddr, rbxNFS = oldDial, oldMounted, oldLookup, oldNFS })
	dialRekordbox = func(_ context.Context, _ net.IP, player byte) (rbxLink, error) {
		rbxAsked.Lock()
		rbxAsked.players = append(rbxAsked.players, player)
		rbxAsked.Unlock()
		return fakeRbxLink{paths}, nil
	}
	mountedAt = func(p string) bool { return p == e.share }
	lookupAddr = func(context.Context, string) ([]string, error) { return []string{"dj-macbook.lan."}, nil }
	silent, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}) // a computer whose NFS server never answers
	t.Cleanup(func() { silent.Close() })
	rbxNFS = func(ip net.IP) *prolink.NFSClient {
		return &prolink.NFSClient{IP: ip, Portmap: silent.LocalAddr().(*net.UDPAddr).Port, Timeout: 50 * time.Millisecond}
	}
	return e
}

func (e *rbxEnv) trackIDs(t *testing.T, list string) (map[string]int64, []string) {
	var lists tidalList
	tidalGet(t, e.base, "/v1/users/1/playlists", 200, &lists)
	var tracks tidalList
	for _, raw := range lists.Items {
		var p tidalPlaylist
		json.Unmarshal(raw, &p)
		if p.Title == list {
			tidalGet(t, e.base, "/v1/playlists/"+p.UUID+"/tracks", 200, &tracks)
		}
	}
	names, ids := titles(t, tracks.Items)
	return ids, names
}

func TestRekordboxSourceNamesTheComputerAndListsItsLibrary(t *testing.T) {
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/a.mp3"})
	var ui struct {
		Sources, LoggedIn []string
		RekordboxName     string
	}
	for i := 0; i < 100 && ui.RekordboxName != "dj-macbook"; i++ { // the name's lookup runs in the background
		tidalGet(t, e.base, "/v1/enginex/ui", 200, &ui)
		time.Sleep(20 * time.Millisecond)
	}
	if ui.RekordboxName != "dj-macbook" || !strings.Contains(strings.Join(ui.Sources, ","), "rekordbox") || !strings.Contains(strings.Join(ui.LoggedIn, ","), "rekordbox") {
		t.Fatalf("status: %+v", ui)
	}
	var lists tidalList
	tidalGet(t, e.base, "/v1/users/1/playlists", 200, &lists)
	var got []string
	for _, raw := range lists.Items {
		var p tidalPlaylist
		json.Unmarshal(raw, &p)
		got = append(got, p.Title)
	}
	if strings.Join(got, "|") != "Tracks|Sets / Warm up" {
		t.Fatalf("lists: %v", got)
	}
	if _, names := e.trackIDs(t, "Sets / Warm up"); strings.Join(names, "|") != "Second" {
		t.Fatalf("playlist tracks: %v", names)
	}

	// Not when Pro DJ Link is off.
	os.WriteFile(filepath.Join(e.s.cfg.UIDir, "settings.conf"), []byte("scope=rekordbox\n"), 0o644)
	ui.Sources, ui.LoggedIn, ui.RekordboxName = nil, nil, ""
	tidalGet(t, e.base, "/v1/enginex/ui", 200, &ui)
	if strings.Contains(strings.Join(ui.Sources, ","), "rekordbox") || ui.RekordboxName != "" {
		t.Fatalf("source without Pro DJ Link: %+v", ui)
	}
}

func TestRekordboxLoadsFromAMountedShare(t *testing.T) {
	// rekordbox names the file in composed form; the share holds it in the decomposed one macOS may keep.
	composed := "Bj\u00f6rk.mp3"
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/Library/" + composed, 2: "/Users/dj/Elsewhere/b.mp3"})
	os.MkdirAll(filepath.Join(e.share, "Library"), 0o755)
	os.WriteFile(filepath.Join(e.share, "Library", norm.NFD.String(composed)), []byte("ID3 one"), 0o644)
	ids, names := e.trackIDs(t, "Tracks")
	if strings.Join(names, "|") != "First|Second" {
		t.Fatalf("no notice row when the share has the first file: %v", names)
	}
	var info struct{ Manifest string }
	tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(ids["First"], 10)+"/playbackinfopostpaywall", 200, &info)
	raw, _ := base64.StdEncoding.DecodeString(info.Manifest)
	var m struct {
		Codecs string
		URLs   []string
	}
	json.Unmarshal(raw, &m)
	if m.Codecs != "mp3" || len(m.URLs) != 1 || !strings.HasSuffix(m.URLs[0], ".mp3") {
		t.Fatalf("manifest %s", raw)
	}
	resp, err := http.Get(e.base + "/v1/stream/" + strconv.FormatInt(ids["First"], 10) + ".mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); string(body) != "ID3 one" {
		t.Fatalf("stream: %d %q", resp.StatusCode, body)
	}
	// A file outside the folder the share shows is out of reach: said plainly, no hang.
	start := time.Now()
	body := tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(ids["Second"], 10)+"/playbackinfopostpaywall", 502, nil)
	if !strings.Contains(string(body), "does not share files with EngineX") || time.Since(start) > 3*time.Second {
		t.Fatalf("out of reach: %s after %s", body, time.Since(start))
	}
}

func TestRekordboxLoadsOverNFSIntoTheBudgetedCache(t *testing.T) {
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/a.flac"})
	os.WriteFile(filepath.Join(e.s.cfg.UIDir, "settings.conf"), []byte("scope=rekordbox\nprolink_share=1\n"), 0o644)
	os.RemoveAll(e.share) // no share: the computer's NFS export is the way
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Users", "dj", "Music"), 0o755)
	flac := append([]byte("fLaC"), make([]byte, 30000)...)
	os.WriteFile(filepath.Join(root, "Users", "dj", "Music", "a.flac"), flac, 0o644)
	nfs := &prolink.NFSServer{Root: root}
	if err := nfs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nfs.Close)
	oldExport, oldNFS, oldRoom := rbxExport, rbxNFS, rbxRoom
	t.Cleanup(func() { rbxExport, rbxNFS, rbxRoom = oldExport, oldNFS, oldRoom })
	// The test disk is not the deck's drive, and the test server shows its root as a Pioneer slot.
	rbxRoom = func(*syscall.Statfs_t, int64) int64 { return 1 << 30 }
	rbxExport = "/C/"
	rbxNFS = func(ip net.IP) *prolink.NFSClient {
		return &prolink.NFSClient{IP: ip, Portmap: nfs.PortmapPort(), Timeout: time.Second, SourcePort: 0}
	}
	ids, names := e.trackIDs(t, "Tracks")
	if strings.Join(names, "|") != "First|Second" {
		t.Fatalf("NFS reaches the file, so no notice row: %v", names)
	}
	url := e.base + "/v1/stream/" + strconv.FormatInt(ids["First"], 10) + ".flac"
	get := func() {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if body, _ := io.ReadAll(resp.Body); string(body) != string(flac) {
			t.Fatalf("stream: %d %d bytes", resp.StatusCode, len(body))
		}
	}
	get()
	cached, _ := filepath.Glob(filepath.Join(e.vol, rbxCacheDir, "*.flac"))
	if len(cached) != 1 {
		t.Fatalf("one copy in the drive's cache folder: %v", cached)
	}
	nfs.Close()
	get() // played again with the computer's NFS gone: the copy is used
	if again, _ := filepath.Glob(filepath.Join(e.vol, rbxCacheDir, "*")); len(again) != 1 {
		t.Fatalf("copies: %v", again)
	}
}

func TestRekordboxNoFileAccessSaysSoAndDoesNotHang(t *testing.T) {
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/a.mp3"})
	os.RemoveAll(e.share)
	start := time.Now()
	ids, names := e.trackIDs(t, "Tracks")
	if len(names) != 3 || !strings.Contains(names[0], "does not share files with EngineX; set up a network share of your music folder") {
		t.Fatalf("a first row says why: %v", names)
	}
	body := tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(ids["First"], 10)+"/playbackinfopostpaywall", 502, nil)
	if !strings.Contains(string(body), "does not share files") || time.Since(start) > 10*time.Second {
		t.Fatalf("%s after %s", body, time.Since(start))
	}
}

func TestRekordboxDropOldestRemovesTheOldestCopy(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"new", "old", "mid"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o644)
		at := time.Now().Add(time.Duration([]int{0, -3, -2}[i]) * time.Hour)
		os.Chtimes(p, at, at)
	}
	if !dropOldest(dir) {
		t.Fatal("nothing dropped")
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); err == nil {
		t.Fatal("old was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "mid")); err != nil {
		t.Fatal("mid was dropped first")
	}
}

func TestRekordboxShareSettingIsValidatedAndKept(t *testing.T) {
	e := newRbxEnv(t, nil)
	resp, err := http.Post(e.base+"/v1/enginex/ui?rekordboxShare=%2FUsers%2Fdj%2FMusic%0Ax", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("a path with a line break: %d", resp.StatusCode)
	}
	resp, err = http.Post(e.base+"/v1/enginex/ui?rekordboxShare=%2FVolumes%2FDJ%20Music", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var ui struct{ RekordboxShare string }
	tidalGet(t, e.base, "/v1/enginex/ui", 200, &ui)
	if resp.StatusCode != 200 || ui.RekordboxShare != "/Volumes/DJ Music" {
		t.Fatalf("%d %+v", resp.StatusCode, ui)
	}
	if b, _ := os.ReadFile(filepath.Join(e.s.cfg.UIDir, "settings.conf")); !strings.Contains(string(b), "rekordbox_share=/Volumes/DJ Music\n") {
		t.Fatalf("settings.conf: %s", b)
	}
}

func TestRekordboxSessionsAskAsTheDecksOwnPlayer(t *testing.T) {
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/a.mp3"})
	oldPlayer := deckPlayer
	t.Cleanup(func() { deckPlayer = oldPlayer })
	asked := func() []byte {
		rbxAsked.Lock()
		defer rbxAsked.Unlock()
		got := rbxAsked.players
		rbxAsked.players = nil
		return got
	}
	asked()
	e.trackIDs(t, "Tracks")
	// Not announcing: the exporter has no number, the client is told 0 and falls back to 5.
	if got := asked(); len(got) == 0 || got[0] != 0 {
		t.Fatalf("a deck with no number: %v", got)
	}
	deckPlayer = func(*Server) byte { return 2 }
	e.trackIDs(t, "Tracks")
	if got := asked(); len(got) == 0 || got[0] != 2 {
		t.Fatalf("the deck is player 2: %v", got)
	}
}
