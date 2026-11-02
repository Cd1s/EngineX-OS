package deemixclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/sc5000-custom-os/internal/testaudio"
	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
)

func TestMatchingRecordingIdentity(t *testing.T) {
	source := providers.Track{Title: "Song (Explicit) (歌曲译名)", Artist: "Artist", ISRC: "US-AA1-23-45678", DurationMS: 200000}
	exact := providers.Track{ID: "1", Title: "Different catalog title", Artist: "Artist", ISRC: "USAA12345678", DurationMS: 200000, Provider: "deezer"}
	if got, e := Match(source, []providers.Track{exact}); e != nil || got.ID != "1" {
		t.Fatalf("ISRC %v %v", got, e)
	}
	fallback := providers.Track{ID: "2", Title: "Song", Artist: "Artist", DurationMS: 202000}
	if got, e := Match(source, []providers.Track{fallback}); e != nil || got.ID != "2" {
		t.Fatalf("2s boundary %v %v", got, e)
	}
	for _, change := range []func(*providers.Track){func(t *providers.Track) { t.DurationMS++ }, func(t *providers.Track) { t.Title = "Song (Club Mix)" }, func(t *providers.Track) { t.Artist = "Wrong Artist" }, func(t *providers.Track) { t.DurationMS = 0 }} {
		c := fallback
		change(&c)
		if _, e := Match(source, []providers.Track{c}); !errors.Is(e, ErrNoMatch) {
			t.Fatalf("unsafe match %+v", c)
		}
	}
	source.Title = "Song (Club Mix)"
	fallback.Title = "Song (Radio Edit)"
	if _, e := Match(source, []providers.Track{fallback}); e == nil {
		t.Fatal("wrong edition matched")
	}
	source.Title = "Song (Live)"
	fallback.Title = "Song"
	if _, e := Match(source, []providers.Track{fallback}); e == nil {
		t.Fatal("live/studio conflated")
	}
	source.Title = "Song"
	c := fallback
	c.ID = "3"
	if _, e := Match(source, []providers.Track{fallback, c}); e == nil {
		t.Fatal("ambiguous catalog accepted")
	}
}
func writeFLAC(t *testing.T, dir string) string {
	t.Helper()
	var b bytes.Buffer
	if err := testaudio.FLAC(&b, testaudio.Song(4)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "audio.mp3")
	if err := os.WriteFile(p, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestActualQualityAndQueue(t *testing.T) {
	root := t.TempDir()
	path := writeFLAC(t, root)
	q, err := Inspect(path)
	if err != nil || q.Label != "[FLAC]" || q.Bits != 16 || q.SampleRate != 22050 {
		t.Fatalf("FLAC inspection %+v %v", q, err)
	}
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["bitrate"] != float64(9) {
				t.Error("not requesting FLAC")
			}
			json.NewEncoder(w).Encode(QueueStatus{ID: "job", State: "queued"})
		} else {
			polls++
			json.NewEncoder(w).Encode(QueueStatus{ID: "job", State: "completed", Progress: 1, Path: path})
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, DownloadDir: root, PollInterval: time.Millisecond}
	dl, err := c.Download(context.Background(), providers.Track{ID: "123", Provider: "deezer"}, nil)
	if err != nil || dl.Quality.Label != "[FLAC]" || polls != 1 {
		t.Fatalf("download %v %v", dl, err)
	}
	outside := filepath.Join(t.TempDir(), "secret.flac")
	os.WriteFile(outside, []byte("not audio"), 0600)
	os.Symlink(outside, filepath.Join(root, "escape.flac"))
	if _, err = c.SafePath("escape.flac"); err == nil {
		t.Fatal("symlink escape accepted")
	}
}
func TestMP3QualityIsNeverInvented(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.flac")
	var b bytes.Buffer
	for i := 0; i < 3; i++ {
		binary.Write(&b, binary.BigEndian, uint32(0xfffbe000))
		b.Write(make([]byte, 144*320000/44100-4))
	}
	os.WriteFile(path, b.Bytes(), 0600)
	q, err := Inspect(path)
	if err != nil || q.Label != "[320k]" || !strings.Contains(q.Comment(), "FLAC unavailable") {
		t.Fatalf("MP3 %+v %v", q, err)
	}
	data := b.Bytes()
	data[2] = 0x90
	os.WriteFile(path, data, 0600)
	if _, err = Inspect(path); err == nil {
		t.Fatal("128k mislabeled 320k")
	}
	os.WriteFile(path, []byte("FLAC fake"), 0600)
	if _, err = Inspect(path); err == nil {
		t.Fatal("fake lossless accepted")
	}
}
func TestCantStreamAndCancellation(t *testing.T) {
	for _, state := range []string{"CantStream", "queued"} {
		t.Run(state, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(QueueStatus{ID: "job", State: state})
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			_, err := (&Client{BaseURL: srv.URL, PollInterval: time.Millisecond}).Download(ctx, providers.Track{ID: "1", Provider: "deezer"}, nil)
			if err == nil {
				t.Fatal("missing error")
			}
			if state == "CantStream" && !strings.Contains(err.Error(), "CantStream") {
				t.Fatal(err)
			}
		})
	}
}

func TestDirectoryAdapterWatchesOnlyRequestedID(t *testing.T) {
	root := t.TempDir()
	audio := writeFLAC(t, root)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/download" || r.Method != "POST" {
			t.Errorf("unexpected submit %s", r.URL.Path)
		}
		w.Write([]byte(`{"result":true}`))
	}))
	defer srv.Close()
	c := &Client{Mode: "directory", BaseURL: srv.URL, DownloadDir: root, PollInterval: time.Millisecond}
	if err := os.WriteFile(filepath.Join(root, "123.flac"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Download(ctx, providers.Track{ID: "123", Provider: "deezer"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("picked unrelated audio: %v", err)
	}
	if err := os.Rename(audio, filepath.Join(root, "123.flac")); err != nil {
		t.Fatal(err)
	}
	dl, err := c.Download(context.Background(), providers.Track{ID: "123", Provider: "deezer"}, nil)
	if err != nil || dl.Quality.Label != "[FLAC]" {
		t.Fatalf("directory download %v %v", dl, err)
	}
}

func TestChineseAndEmptyTitlesCannotCollide(t *testing.T) {
	source := providers.Track{Title: "【稻香】", Artist: "Artist", DurationMS: 200000}
	same := providers.Track{ID: "1", Title: "稻香", Artist: "Artist", DurationMS: 200000}
	other := same
	other.ID = "2"
	other.Title = "（夜曲）"
	if got, err := Match(source, []providers.Track{same, other}); err != nil || got.ID != "1" {
		t.Fatalf("wrapped title %v %v", got, err)
	}
	source.Title = "!!!"
	same.Title = "???"
	if _, err := Match(source, []providers.Track{same}); err == nil {
		t.Fatal("empty normalized names matched")
	}
	source.Title = "Song"
	same.Title = "Song"
	source.Artist = ""
	same.Artist = ""
	if _, err := Match(source, []providers.Track{same}); err == nil {
		t.Fatal("unknown artist matched")
	}
}
func TestID3TaggedFLACUsesActualHeader(t *testing.T) {
	root := t.TempDir()
	path := writeFLAC(t, root)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header := []byte{'I', 'D', '3', 4, 0, 16, 0, 0, 0, 0}
	footer := append([]byte("3DI"), header[3:]...)
	tagged := append(append(header, footer...), raw...)
	if err = os.WriteFile(path, tagged, 0600); err != nil {
		t.Fatal(err)
	}
	q, err := Inspect(path)
	if err != nil || q.Label != "[FLAC]" {
		t.Fatalf("tagged FLAC: %+v %v", q, err)
	}
}

type collaborationCatalog struct{}

func (collaborationCatalog) ByISRC(context.Context, string) (providers.Track, error) {
	return providers.Track{}, ErrNoMatch
}
func (collaborationCatalog) Search(_ context.Context, q string) ([]providers.Track, error) {
	if strings.Contains(q, "Alice, Bob") {
		return nil, errors.New("search requires a single main artist")
	}
	return []providers.Track{{ID: "1", Title: "Song", Artist: "Alice", DurationMS: 200000, Provider: "deezer"}}, nil
}
func (collaborationCatalog) ByID(context.Context, string) (providers.Track, error) {
	return providers.Track{ID: "1", Title: "Song", Artist: "Alice, Bob", Artists: []string{"Alice", "Bob"}, DurationMS: 200000, Provider: "deezer"}, nil
}
func TestCollaborationFallbackFetchesContributors(t *testing.T) {
	source := providers.Track{Title: "Song", Artist: "Alice, Bob", Artists: []string{"Alice", "Bob"}, DurationMS: 200000, Provider: "netease"}
	got, err := Resolve(context.Background(), collaborationCatalog{}, source)
	if err != nil || got.ID != "1" {
		t.Fatalf("collaboration %v %v", got, err)
	}
}

func mp3Frames(kbpsIndex byte, frames int) []byte {
	var b bytes.Buffer
	for i := 0; i < frames; i++ {
		b.Write([]byte{0xff, 0xfb, kbpsIndex << 4, 0})
		b.Write(make([]byte, 144*mpeg1Layer3Kbps[kbpsIndex]*1000/44100-4))
	}
	return b.Bytes()
}

func TestLowerBitrateMP3IsLabelledHonestly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.mp3")
	os.WriteFile(path, mp3Frames(9, 3), 0o600)
	q, err := Inspect(path)
	if err != nil || q.Label != "[128k]" || q.Bitrate != 128000 || !strings.Contains(q.Comment(), "[128k]") {
		t.Fatalf("%+v %v", q, err)
	}
}

func TestMP3TailAfterManyFramesIsNotDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.mp3")
	os.WriteFile(path, append(mp3Frames(14, trustedFrames), []byte("APETAGEX junk")...), 0o600)
	if q, err := Inspect(path); err != nil || q.Label != "[320k]" {
		t.Fatalf("%+v %v", q, err)
	}
	os.WriteFile(path, append(mp3Frames(14, 3), []byte("APETAGEX junk")...), 0o600)
	if _, err := Inspect(path); err == nil {
		t.Fatal("garbage after a few frames accepted")
	}
}

func TestSaveKeepsOnlyRealAudio(t *testing.T) {
	audio := mp3Frames(14, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/audio":
			w.Write(audio)
		case "/html":
			w.Write([]byte("<!doctype html><title>a web app that answers 200</title>"))
		case "/short":
			w.Header().Set("Content-Length", "99999")
			w.Write(audio[:100])
		default:
			http.Error(w, "no", 404)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	c := &Client{DownloadDir: dir, AllowLossy: true}
	dl, err := c.Save(context.Background(), "netease-1", srv.URL+"/audio")
	if err != nil || filepath.Base(dl.Path) != "netease-1.mp3" {
		t.Fatalf("%+v %v", dl, err)
	}
	if got := c.Cached("netease-1"); got == nil || got.Path != dl.Path {
		t.Fatalf("cache %+v", got)
	}
	for _, path := range []string{"/html", "/short", "/missing"} {
		if _, err := c.Save(context.Background(), "netease-2", srv.URL+path); err == nil {
			t.Fatalf("%s accepted", path)
		} else if strings.Contains(err.Error(), srv.URL) {
			t.Fatalf("error quotes the address: %v", err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftovers %v", entries)
	}
	strict := &Client{DownloadDir: t.TempDir()}
	if _, err := strict.Save(context.Background(), "netease-3", srv.URL+"/audio"); err == nil {
		t.Fatal("MP3 accepted without permission")
	}
}

func TestTrimEvictsOldestSavedFilesOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		os.WriteFile(path, make([]byte, 100), 0o600)
		when := time.Now().Add(-age)
		os.Chtimes(path, when, when)
		return path
	}
	oldest := write("netease-1.flac", 3*time.Hour)
	foreign := write("user-file.flac", 4*time.Hour)
	middle := write("netease-2.mp3", 2*time.Hour)
	keep := write("netease-3.flac", time.Hour)
	(&Client{DownloadDir: dir, MaxCache: 300}).trim(keep)
	if _, err := os.Stat(oldest); err == nil {
		t.Fatal("oldest kept")
	}
	for _, path := range []string{foreign, middle, keep} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(path, err)
		}
	}
}

func TestMatchNearbyAcrossCatalogs(t *testing.T) {
	source := providers.Track{Title: "Gaze", Artist: "Hitam", DurationMS: 300000}
	same := providers.Track{ID: "1", Title: "Gaze (Original Mix)", Artist: "Hitam, Other", DurationMS: 304000}
	closer := providers.Track{ID: "2", Title: "Gaze", Artist: "Hitam", DurationMS: 301000}
	if got, err := MatchNearby(source, []providers.Track{same, closer}, 5000); err != nil || got.ID != "2" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []providers.Track{
		{ID: "3", Title: "Gaze", Artist: "Hitam", DurationMS: 306000},
		{ID: "4", Title: "Gaze", Artist: "Someone", DurationMS: 300000},
		{ID: "5", Title: "Gaze (Club Mix)", Artist: "Hitam", DurationMS: 300000},
		{ID: "6", Title: "Gazes", Artist: "Hitam", DurationMS: 300000},
	} {
		if got, err := MatchNearby(source, []providers.Track{bad}, 5000); err == nil {
			t.Fatalf("matched %+v", got)
		}
	}
}
