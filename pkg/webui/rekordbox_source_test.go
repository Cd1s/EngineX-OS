package webui

import (
	"bytes"
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
	get() // served while it was copied: the copy moves into place just after the last byte left
	var cached []string
	for i := 0; i < 200 && len(cached) != 1; i++ {
		cached, _ = filepath.Glob(filepath.Join(e.vol, rbxCacheDir, "*.flac"))
		time.Sleep(10 * time.Millisecond)
	}
	if len(cached) != 1 {
		t.Fatalf("one copy in the drive's cache folder: %v", cached)
	}
	nfs.Close()
	get() // played again with the computer's NFS gone: the copy is used
	if again, _ := filepath.Glob(filepath.Join(e.vol, rbxCacheDir, "*")); len(again) != 1 {
		t.Fatalf("copies: %v", again)
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Range", "bytes=5-14")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 206 || string(body) != string(flac[5:15]) {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
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
	running := filepath.Join(dir, ".fetch-1") // a copy that is still running is never the oldest
	os.WriteFile(running, []byte("x"), 0o644)
	long := time.Now().Add(-9 * time.Hour)
	os.Chtimes(running, long, long)
	if !dropOldest(dir) {
		t.Fatal("nothing dropped")
	}
	if _, err := os.Stat(running); err != nil {
		t.Fatal("a copy still running was dropped")
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

func TestRekordboxCopyGoesOnWhenTheRequestGivesUp(t *testing.T) {
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/big.flac"})
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "Users", "dj", "Music"), 0o755)
	big := append([]byte("fLaC"), bytes.Repeat([]byte{9}, 3<<20)...) // hundreds of 8 KiB reads
	os.WriteFile(filepath.Join(root, "Users", "dj", "Music", "big.flac"), big, 0o644)
	nfs := &prolink.NFSServer{Root: root}
	if err := nfs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nfs.Close)
	oldExport, oldNFS, oldRoom := rbxExport, rbxNFS, rbxRoom
	t.Cleanup(func() { rbxExport, rbxNFS, rbxRoom = oldExport, oldNFS, oldRoom })
	rbxRoom = func(*syscall.Statfs_t, int64) int64 { return 1 << 30 }
	rbxExport = "/C/"
	rbxNFS = func(ip net.IP) *prolink.NFSClient {
		return &prolink.NFSClient{IP: ip, Portmap: nfs.PortmapPort(), Timeout: time.Second}
	}
	ctx, cancel := context.WithCancel(context.Background())
	file, _, err := e.s.tidal.rbxStage(ctx, rbxNFS(net.IPv4(127, 0, 0, 1)), "127.0.0.1", "/Users/dj/Music/big.flac")
	cancel() // the request that started it is gone
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500 && rbxRunning(file) != nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, big) {
		t.Fatalf("the copy did not finish without its request: %d of %d bytes", len(got), len(big))
	}
	// Asked again, the finished copy is used and no second one starts.
	if again, _, err := e.s.tidal.rbxStage(context.Background(), rbxNFS(net.IPv4(127, 0, 0, 1)), "127.0.0.1", "/Users/dj/Music/big.flac"); err != nil || again != file || rbxRunning(file) != nil {
		t.Fatalf("second stage: %s %v", again, err)
	}
}

func TestRekordboxReaderServesWhatHasArrivedAndWaitsForTheRest(t *testing.T) {
	dir := t.TempDir()
	tmp, _ := os.Create(filepath.Join(dir, ".fetch-1"))
	cp := &rbxCopy{file: filepath.Join(dir, "final"), tmp: tmp.Name(), size: 10}
	cp.more = sync.NewCond(&cp.mu)
	w := rbxWriter{cp, tmp}
	rd, _ := os.Open(tmp.Name())
	ctx, cancel := context.WithCancel(context.Background())
	r := &rbxReader{cp: cp, f: rd, ctx: ctx}
	w.Write([]byte("abcd"))
	buf := make([]byte, 8)
	if n, err := r.Read(buf); err != nil || string(buf[:n]) != "abcd" {
		t.Fatalf("what arrived: %q %v", buf[:n], err)
	}
	type result struct {
		b   string
		err error
	}
	next := make(chan result, 1)
	go func() { n, err := r.Read(buf); next <- result{string(buf[:n]), err} }()
	select {
	case got := <-next:
		t.Fatalf("a read past what arrived did not wait: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	w.Write([]byte("efghij"))
	if got := <-next; got.err != nil || got.b != "efghij" {
		t.Fatalf("after the rest arrived: %+v", got)
	}
	if n, err := r.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("at the end: %d %v", n, err)
	}
	if end, _ := r.Seek(0, io.SeekEnd); end != 10 {
		t.Fatalf("the size is known before the copy ends: %d", end)
	}
	// A failed copy ends the reader with its error; a gone request ends it at once.
	r.Seek(4, io.SeekStart)
	cp.mu.Lock()
	cp.got, cp.done, cp.err = 4, true, errors.New("the computer went away")
	cp.mu.Unlock()
	if _, err := r.Read(buf); err == nil || err.Error() != "the computer went away" {
		t.Fatalf("failed copy: %v", err)
	}
	cp.mu.Lock()
	cp.done, cp.err = false, nil
	cp.mu.Unlock()
	go func() { time.Sleep(50 * time.Millisecond); cancel(); cp.mu.Lock(); cp.mu.Unlock(); cp.more.Broadcast() }()
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("gone request: %v", err)
	}
}

func TestRekordboxServeAnswersFromTheCopyAsBytesArrive(t *testing.T) {
	dir := t.TempDir()
	tmp, _ := os.Create(filepath.Join(dir, ".fetch-1"))
	cp := &rbxCopy{file: filepath.Join(dir, "final"), tmp: tmp.Name(), size: 10}
	cp.more = sync.NewCond(&cp.mu)
	w := rbxWriter{cp, tmp}
	for _, tc := range []struct{ rng, body string }{{"", "abcdefghij"}, {"bytes=7-9", "hij"}} {
		cp.mu.Lock()
		cp.got = 0
		cp.mu.Unlock()
		tmp.Truncate(0)
		tmp.Seek(0, io.SeekStart)
		req := httptest.NewRequest("GET", "/v1/stream/1.flac", nil)
		if tc.rng != "" {
			req.Header.Set("Range", tc.rng)
		}
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { cp.serve(rec, req); close(done) }()
		w.Write([]byte("abcde"))
		select {
		case <-done:
			if tc.rng == "" {
				t.Fatal("the answer ended before the copy had all of it")
			}
		case <-time.After(100 * time.Millisecond):
		}
		w.Write([]byte("fghij"))
		<-done
		want := 200
		if tc.rng != "" {
			want = 206
		}
		if rec.Code != want || rec.Body.String() != tc.body || rec.Header().Get("Content-Length") != strconv.Itoa(len(tc.body)) {
			t.Fatalf("%q: %d %q length %s", tc.rng, rec.Code, rec.Body.String(), rec.Header().Get("Content-Length"))
		}
	}
}
