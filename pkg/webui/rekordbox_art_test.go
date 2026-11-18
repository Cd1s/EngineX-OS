package webui

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
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
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// artRbxLink is fakeRbxLink whose first track has artwork id 42, with the computer's pictures.
// With entered set, Artwork signals it and then answers only when the session's context ends.
type artRbxLink struct {
	fakeRbxLink
	art     map[uint32][]byte
	calls   *atomic.Int32
	ctx     context.Context
	entered chan struct{}
}

func (f artRbxLink) Tracks() ([]prolink.RekordboxTrack, error) {
	rows, err := f.fakeRbxLink.Tracks()
	rows[0].Art = 42
	return rows, err
}

func (f artRbxLink) Artwork(id uint32) ([]byte, error) {
	f.calls.Add(1)
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.ctx.Done()
		return nil, f.ctx.Err()
	}
	if b, ok := f.art[id]; ok {
		return b, nil
	}
	return nil, errors.New("no such artwork")
}

func useArtLink(paths map[uint32]string, art map[uint32][]byte) *atomic.Int32 {
	calls := new(atomic.Int32)
	dialRekordbox = func(ctx context.Context, _ net.IP, _ byte) (rbxLink, error) {
		return artRbxLink{fakeRbxLink{paths}, art, calls, ctx, nil}, nil
	}
	return calls
}

func testImage(w, h int, enc func(io.Writer, image.Image) error) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	var b bytes.Buffer
	if err := enc(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}

var (
	testJPEG = testImage(16, 16, func(w io.Writer, m image.Image) error { return jpeg.Encode(w, m, nil) })
	testPNG  = testImage(16, 16, png.Encode)
)

func isJPEG(b []byte) bool {
	_, format, err := image.Decode(bytes.NewReader(b))
	return err == nil && format == "jpeg"
}

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
	// rekordbox may hold a PNG: the gateway hands it on as JPEG, which the platter's stm.db row takes.
	calls := useArtLink(map[uint32]string{1: "/Users/dj/Music/a.mp3"}, map[uint32][]byte{42: testPNG})
	ids, _ := e.trackIDs(t, "Tracks")

	g := e.s.tidal
	g.mu.Lock()
	first, second := g.tracks[ids["First"]].track.CoverURL, g.tracks[ids["Second"]].track.CoverURL
	g.mu.Unlock()
	if first != "rekordbox-art://127.0.0.1/42" || second != "" {
		t.Fatalf("cover links: %q %q", first, second)
	}
	var cover []byte
	for i := 0; i < 2; i++ {
		code, body, mime := artGet(t, e.base, "First", "Artist One")
		if code != 200 || !isJPEG(body) || mime != "image/jpeg" || cover != nil && !bytes.Equal(body, cover) {
			t.Fatalf("cover: %d %d bytes %s", code, len(body), mime)
		}
		cover = body
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
	if db.QueryRow(`SELECT albumArt FROM AlbumArt WHERE id = 1`).Scan(&art); !bytes.Equal(art, cover) {
		t.Fatalf("stm.db cover: %d bytes", len(art))
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

// Only a JPEG, or a PNG re-encoded as JPEG, that decodes within maxArtBytes and rbxArtPixels
// is a cover; a bad link or a failing computer is no cover.
func TestRekordboxCoverFormatAndSizeLimits(t *testing.T) {
	e := newRbxEnv(t, nil)
	big := append(bytes.Clone(testJPEG), make([]byte, maxArtBytes)...)
	huge := testImage(1700, 1700, png.Encode) // few bytes, too many pixels
	calls := useArtLink(nil, map[uint32][]byte{1: testJPEG, 2: testPNG, 3: big, 4: []byte("GIF89a"), 5: {}, 6: []byte("<html>"),
		8: testJPEG[:len(testJPEG)/2], 9: []byte("\xff\xd8\xff\xe0 signature only"), 10: huge})
	for art, ok := range map[int]bool{1: true, 2: true, 3: false, 4: false, 5: false, 6: false, 7: false, 8: false, 9: false, 10: false} {
		got := e.s.tidal.artBytes(context.Background(), "rekordbox-art://127.0.0.1/"+strconv.Itoa(art))
		if ok != (got != nil) || ok && !isJPEG(got) || art == 1 && !bytes.Equal(got, testJPEG) {
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

// A cover is read only from the rekordbox computer the deck sees on the link now, and only for
// the gateway's own rekordbox rows: never from an address another source's row names.
func TestRekordboxCoverOnlyFromTheComputerOnTheLink(t *testing.T) {
	e := newRbxEnv(t, nil)
	calls := useArtLink(nil, map[uint32][]byte{1: testJPEG})
	g := e.s.tidal
	for _, address := range []string{"rekordbox-art://10.9.9.9/1", "rekordbox-art://127.0.0.2/1"} {
		if g.artBytes(context.Background(), address) != nil {
			t.Fatalf("%s: a cover from a computer not on the link", address)
		}
	}
	g.addTrack(trackRef{track: providers.Track{ID: "x", Title: "Bait", Artist: "B", Provider: "deezer", CoverURL: "rekordbox-art://127.0.0.1/1"}})
	if code, _, _ := artGet(t, e.base, "Bait", "B"); code != 404 {
		t.Fatalf("another source's row reached the computer: %d", code)
	}
	// Nor through the list's cover prewarm, which sees the rows as their source gave them.
	g.refItems([]trackRef{{track: providers.Track{ID: "y", Title: "Bait 2", Provider: "deezer", CoverURL: "rekordbox-art://127.0.0.1/1"}}})
	for end := time.Now().Add(5 * time.Second); g.warming.Load() && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	if calls.Load() != 0 {
		t.Fatalf("another source's row reached the computer through the prewarm: %d requests", calls.Load())
	}
	e.s.rbxPresenceMu.Lock()
	e.s.rbxPresence["127.0.0.1"].seen = time.Time{} // LINK turned off
	e.s.rbxPresenceMu.Unlock()
	if g.artBytes(context.Background(), "rekordbox-art://127.0.0.1/1") != nil {
		t.Fatal("a cover from a computer that no longer offers its library")
	}
	if calls.Load() != 0 {
		t.Fatalf("%d artwork requests", calls.Load())
	}
}

// A list, a load or an analysis read never waits for a cover: one being read is ended at once,
// and none starts while they hold their claim.
func TestRekordboxCoverGivesWayToListsAndLoads(t *testing.T) {
	e := newRbxEnv(t, nil)
	calls := new(atomic.Int32)
	entered := make(chan struct{}, 1)
	dialRekordbox = func(ctx context.Context, _ net.IP, _ byte) (rbxLink, error) {
		return artRbxLink{fakeRbxLink{nil}, nil, calls, ctx, entered}, nil
	}
	done := make(chan []byte)
	go func() { done <- e.s.tidal.artBytes(context.Background(), "rekordbox-art://127.0.0.1/1") }()
	<-entered
	useArtLink(nil, nil)
	start := time.Now()
	if ids, _ := e.trackIDs(t, "Tracks"); len(ids) != 2 || time.Since(start) > time.Second {
		t.Fatalf("list behind a cover: %d rows after %s", len(ids), time.Since(start))
	}
	if b := <-done; b != nil {
		t.Fatal("the cover that gave way was served")
	}
}

// While a list, a load or an analysis read holds its claim, no cover takes the session.
func TestRekordboxCoverWaitsWhileClaimed(t *testing.T) {
	e := newRbxEnv(t, nil)
	release := rbxClaim()
	more := useArtLink(nil, map[uint32][]byte{1: testJPEG})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	got := e.s.tidal.artBytes(ctx, "rekordbox-art://127.0.0.1/1")
	cancel()
	release()
	if got != nil || more.Load() != 0 {
		t.Fatal("a cover took the session while a list held its claim")
	}
	if !bytes.Equal(e.s.tidal.artBytes(context.Background(), "rekordbox-art://127.0.0.1/1"), testJPEG) {
		t.Fatal("no cover once the claim is released")
	}
}

// In the rekordbox source its own cover wins over a service row with the same title and artist,
// which keeps its own cover.
func TestRekordboxCoverDoesNotReplaceAServicesCover(t *testing.T) {
	e := newRbxEnv(t, nil)
	useArtLink(nil, map[uint32][]byte{42: testJPEG})
	e.trackIDs(t, "Tracks")
	g := e.s.tidal
	g.artMu.Lock()
	g.artCache["https://img.example/first.jpg"] = testPNG
	g.artMu.Unlock()
	g.addTrack(trackRef{track: providers.Track{ID: "d1", Title: "First", Artist: "Artist One", Provider: "deezer", CoverURL: "https://img.example/first.jpg"}})
	if code, body, _ := artGet(t, e.base, "First", "Artist One"); code != 200 || !bytes.Equal(body, testJPEG) {
		t.Fatalf("rekordbox source: %d %d bytes", code, len(body))
	}
	g.mu.Lock()
	kept := false
	for _, address := range g.artURLs {
		kept = kept || address == "https://img.example/first.jpg"
	}
	g.mu.Unlock()
	if !kept {
		t.Fatal("the service row lost its cover")
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
