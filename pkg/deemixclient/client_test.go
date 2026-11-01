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
