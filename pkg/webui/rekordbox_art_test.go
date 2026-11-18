package webui

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

// artRbxLink is fakeRbxLink whose first track has artwork id 42, with the computer's pictures.
type artRbxLink struct {
	fakeRbxLink
	art   map[uint32][]byte
	calls *atomic.Int32
}

func (f artRbxLink) Tracks() ([]prolink.RekordboxTrack, error) {
	rows, err := f.fakeRbxLink.Tracks()
	rows[0].Art = 42
	return rows, err
}

func (f artRbxLink) Artwork(id uint32) ([]byte, error) {
	f.calls.Add(1)
	if b, ok := f.art[id]; ok {
		return b, nil
	}
	return nil, errors.New("no such artwork")
}

func useArtLink(paths map[uint32]string, art map[uint32][]byte) *atomic.Int32 {
	calls := new(atomic.Int32)
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) {
		return artRbxLink{fakeRbxLink{paths}, art, calls}, nil
	}
	return calls
}

var testJPEG = []byte("\xff\xd8\xff\xe0 a rekordbox cover")

func artGet(t *testing.T, base, song, artist string) (int, []byte, string) {
	t.Helper()
	resp, err := http.Get(base + "/v1/enginex/art?song=" + url.QueryEscape(song) + "&artist=" + url.QueryEscape(artist))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header.Get("Content-Type")
}

// A computer's track row carries its cover; the row and the deck ask /v1/enginex/art by title
// and artist, which reads the picture over dbserver once, and the loaded track's stm.db row
// gets it for the jog wheel, as a service's track does.
func TestRekordboxCoverComesFromTheComputer(t *testing.T) {
	e := newRbxEnv(t, nil)
	os.WriteFile(filepath.Join(e.share, "a.mp3"), []byte("ID3 one"), 0o644)
	calls := useArtLink(map[uint32]string{1: "/Users/dj/Music/a.mp3"}, map[uint32][]byte{42: testJPEG})
	ids, _ := e.trackIDs(t, "Tracks")

	g := e.s.tidal
	g.mu.Lock()
	first, second := g.tracks[ids["First"]].track.CoverURL, g.tracks[ids["Second"]].track.CoverURL
	g.mu.Unlock()
	if first != "rekordbox-art://127.0.0.1/42" || second != "" {
		t.Fatalf("cover links: %q %q", first, second)
	}
	for i := 0; i < 2; i++ {
		if code, body, mime := artGet(t, e.base, "First", "Artist One"); code != 200 || !bytes.Equal(body, testJPEG) || mime != "image/jpeg" {
			t.Fatalf("cover: %d %q %s", code, body, mime)
		}
	}
	if code, _, _ := artGet(t, e.base, "Second", ""); code != 404 {
		t.Fatalf("a track without artwork: %d", code)
	}
	if calls.Load() != 1 {
		t.Fatalf("%d artwork requests, want 1 (cached, none for a track without artwork)", calls.Load())
	}
	if !rbxSession.TryLock() {
		t.Fatal("the cover kept the dbserver session")
	}
	rbxSession.Unlock()

	// The jog wheel's stm.db row gets the computer's cover.
	data := t.TempDir()
	lib := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	os.MkdirAll(filepath.Join(lib, "Database2"), 0o755)
	db, err := sql.Open("sqlite", filepath.Join(lib, "Database2", "stm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE AlbumArt (id INTEGER PRIMARY KEY, hash TEXT, albumArt BLOB)`,
		`CREATE TABLE Track (id INTEGER PRIMARY KEY, uri TEXT, albumArtId INTEGER)`,
		`INSERT INTO AlbumArt VALUES (1, 'image://streaming/', NULL)`,
		fmt.Sprintf(`INSERT INTO Track VALUES (1, 'streaming://TIDAL/Track/%d', 1)`, ids["First"]),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old := shadowDataRoot
	shadowDataRoot = data
	g.mu.Lock()
	ref := g.tracks[ids["First"]]
	g.mu.Unlock()
	g.syncStmAlbumArt(context.Background(), ids["First"], ref.track)
	shadowDataRoot = old
	var art []byte
	if db.QueryRow(`SELECT albumArt FROM AlbumArt WHERE id = 1`).Scan(&art); !bytes.Equal(art, testJPEG) {
		t.Fatalf("stm.db cover: %q", art)
	}

	// Loading the track starts that stm.db update, as a service's stream does: its four tries
	// each ask the computer here, since the picture is no longer cached and is not an image.
	g.artMu.Lock()
	delete(g.artCache, first)
	g.artMu.Unlock()
	calls = useArtLink(map[uint32]string{1: "/Users/dj/Music/a.mp3"}, map[uint32][]byte{42: []byte("GIF89a")})
	tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(ids["First"], 10)+"/playbackinfopostpaywall", 200, nil)
	resp, err := http.Get(e.base + "/v1/stream/" + strconv.FormatInt(ids["First"], 10) + ".mp3")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	for end := time.Now().Add(15 * time.Second); calls.Load() < 4 && time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
	}
	if calls.Load() != 4 {
		t.Fatalf("the load asked the computer for its cover %d times, want 4", calls.Load())
	}
}

// Only a JPEG or PNG within maxArtBytes is a cover; a bad link or a failing computer is no cover.
func TestRekordboxCoverFormatAndSizeLimits(t *testing.T) {
	e := newRbxEnv(t, nil)
	png := []byte("\x89PNG\r\n\x1a\n rest")
	big := append(bytes.Clone(testJPEG), make([]byte, maxArtBytes)...)
	calls := useArtLink(nil, map[uint32][]byte{1: testJPEG, 2: png, 3: big, 4: []byte("GIF89a"), 5: {}, 6: []byte("<html>")})
	for art, want := range map[uint32][]byte{1: testJPEG, 2: png, 3: nil, 4: nil, 5: nil, 6: nil, 7: nil} {
		if got := e.s.tidal.artBytes(context.Background(), "rekordbox-art://127.0.0.1/"+strconv.Itoa(int(art))); !bytes.Equal(got, want) || (want == nil) != (got == nil) {
			t.Fatalf("artwork %d: %d bytes", art, len(got))
		}
	}
	n := calls.Load()
	for _, address := range []string{"rekordbox-art://127.0.0.1/0", "rekordbox-art://127.0.0.1/x", "rekordbox-art://not-an-ip/1", "rekordbox-art://127.0.0.1/99999999999", "http://127.0.0.1/1"} {
		if e.s.tidal.artBytes(context.Background(), address) != nil {
			t.Fatalf("%s gave a cover", address)
		}
	}
	if calls.Load() != n {
		t.Fatal("a malformed cover link reached the computer")
	}
}

// Covers share the gateway's bounded cache with the services' covers.
func TestRekordboxCoverCacheIsBounded(t *testing.T) {
	e := newRbxEnv(t, nil)
	art := map[uint32][]byte{}
	for i := uint32(1); i <= maxArtEntries+20; i++ {
		art[i] = testJPEG
	}
	useArtLink(nil, art)
	g := e.s.tidal
	for i := 1; i <= maxArtEntries+20; i++ {
		if g.artBytes(context.Background(), "rekordbox-art://127.0.0.1/"+strconv.Itoa(i)) == nil {
			t.Fatalf("artwork %d", i)
		}
	}
	g.artMu.Lock()
	defer g.artMu.Unlock()
	if len(g.artCache) != maxArtEntries || len(g.artAge) != maxArtEntries || g.artCache["rekordbox-art://127.0.0.1/1"] != nil {
		t.Fatalf("cache holds %d covers, %d ages", len(g.artCache), len(g.artAge))
	}
}

// A computer that is gone, or a session held by a list or a load, gives no cover at once or
// within the cover's wait; it never waits on the session for longer.
func TestRekordboxCoverFailureIsNoImageWithoutHanging(t *testing.T) {
	e := newRbxEnv(t, nil)
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) { return nil, errors.New("computer gone") }
	if code, _, _ := artGet(t, e.base, "x", ""); code != 404 {
		t.Fatalf("unknown row: %d", code)
	}
	if e.s.tidal.artBytes(context.Background(), "rekordbox-art://127.0.0.1/1") != nil {
		t.Fatal("cover from a computer that is gone")
	}
	useArtLink(nil, map[uint32][]byte{1: testJPEG})
	rbxSession.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	start := time.Now()
	got := e.s.tidal.artBytes(ctx, "rekordbox-art://127.0.0.1/1")
	cancel()
	rbxSession.Unlock()
	if got != nil || time.Since(start) > time.Second {
		t.Fatalf("busy session: %d bytes after %s", len(got), time.Since(start))
	}
	// No cache entry for a failure: the next request, with the session free, gets it.
	if !bytes.Equal(e.s.tidal.artBytes(context.Background(), "rekordbox-art://127.0.0.1/1"), testJPEG) {
		t.Fatal("no cover once the session is free")
	}
}
