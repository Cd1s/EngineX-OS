package webui

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// tinyFLAC is a FLAC stream with only STREAMINFO: no tags, like a download.
func tinyFLAC() []byte {
	b := append([]byte("fLaC"), 0x80, 0, 0, 34)
	return append(append(b, bytes.Repeat([]byte{1}, 34)...), "AUDIO"...)
}

// The file Engine imports for a streamed row's Stems says what it is (left
// deck, 2026-11-16: the internal library showed NetEase's "冲动的惩罚" as
// "3123276661.flac"). The cache keeps its file under the id.
func TestStemsImportFileIsNamedAndTagged(t *testing.T) {
	dl := t.TempDir()
	cache := filepath.Join(dl, "3123276661.flac")
	if err := os.WriteFile(cache, tinyFLAC(), 0o644); err != nil {
		t.Fatal(err)
	}
	track := providers.Track{ID: "77004", Provider: "netease", Title: "冲动的惩罚 #1?", Artists: []string{"刀郎", "A/B"}, Album: "专辑"}
	got := stemsImportFile(cache, track)
	want := filepath.Join(dl, "Stems", "3123276661", "刀郎, A B - 冲动的惩罚 1.flac")
	if got != want {
		t.Fatalf("import file %q, want %q", got, want)
	}
	file, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"TITLE=冲动的惩罚 #1?", "ARTIST=刀郎, A/B", "ALBUM=专辑"} {
		if !bytes.Contains(file, []byte(field)) {
			t.Errorf("the imported file lacks %q", field)
		}
	}
	if !bytes.HasSuffix(file, []byte("AUDIO")) {
		t.Error("the audio changed")
	}
	if now, _ := os.ReadFile(cache); !bytes.Equal(now, tinyFLAC()) {
		t.Error("the cache file was rewritten")
	}
	// Asked again, Engine gets the file it already imported, untouched.
	before, _ := os.Stat(got)
	again := stemsImportFile(cache, track)
	if after, _ := os.Stat(again); again != got || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("second request: %q, rewritten", again)
	}
	// A format that is not tagged is still named: a link to the download.
	ogg := filepath.Join(dl, "spotify-sp1.ogg")
	os.WriteFile(ogg, []byte("OggS-vorbis"), 0o644)
	named := stemsImportFile(ogg, providers.Track{Title: "Song", Artist: "DJ"})
	a, _ := os.Stat(ogg)
	b, errB := os.Stat(named)
	if named != filepath.Join(dl, "Stems", "spotify-sp1", "DJ - Song.ogg") || errB != nil || !os.SameFile(a, b) {
		t.Fatalf("ogg import file %q (%v)", named, errB)
	}
	// Nothing to name it by: the download, as before.
	if p := stemsImportFile(ogg, providers.Track{ID: "x"}); p != ogg {
		t.Fatalf("untitled track got %q", p)
	}
	// The cache trim counts and evicts files in the download directory only.
	c := &deemixclient.Client{DownloadDir: dl, MaxCache: 1}
	c.Trim()
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("cache eviction removed the imported file: %v", err)
	}
}

// Through the gateway: the path the UI hands to Engine, the path kept in the
// service's Stems list and the path a load is swapped to are that one file.
func TestStemsSourceAnswersTheNamedFile(t *testing.T) {
	uiDir, stateDir, dl := t.TempDir(), t.TempDir(), t.TempDir()
	// An Ogg download is accepted from the cache as it is, without a login.
	if err := os.WriteFile(filepath.Join(dl, "spotify-sp789.ogg"), []byte("OggS-fake-vorbis"), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dl, AllowLossy: true}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	id := strconv.FormatInt(s.tidal.addTrack(trackRef{track: providers.Track{ID: "sp789", Title: "冲动的惩罚", Artist: "刀郎", Provider: "spotify"}}), 10)
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:41000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	var src struct{ Path string }
	json.Unmarshal([]byte(get("/v1/enginex/stems/source?id="+id)), &src)
	want := filepath.Join(dl, "Stems", "spotify-sp789", "刀郎 - 冲动的惩罚.ogg")
	if src.Path != want {
		t.Fatalf("stems source %q, want %q", src.Path, want)
	}
	if list := s.stemmed("spotify"); len(list) != 1 || list[0].Path != want {
		t.Fatalf("Stems list entry %+v", list)
	}
	var local struct{ Path string }
	if json.Unmarshal([]byte(get("/v1/enginex/stems/local?id="+id)), &local); local.Path != want {
		t.Fatalf("stems local %q, want %q", local.Path, want)
	}
}

// A row is in the service's Stems list from the moment its download is handed
// to Engine; the render then takes about as long as the track (left deck:
// 6.3 minutes for 8.6 minutes of music). Until the Stems exist the list must
// not offer it as a stemmed track: it shows a row saying they are not ready.
func TestStemsListSaysWhenStemsAreNotReady(t *testing.T) {
	data := t.TempDir()
	shadowDataRoot = data
	defer func() { shadowDataRoot = "/data" }()
	lib := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	os.MkdirAll(filepath.Join(lib, "Database2"), 0o755)
	os.MkdirAll(filepath.Join(lib, "Stems"), 0o755)
	db, err := sql.Open("sqlite", filepath.Join(lib, "Database2/m.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Engine keeps a row's path relative to the library directory.
	for _, q := range []string{
		`CREATE TABLE Track (id INTEGER PRIMARY KEY, path TEXT, filename TEXT)`,
		`INSERT INTO Track VALUES (86, '../../../../enginex/Stream/Stems/2/Done - Song.flac', 'Done - Song.flac'), (258, '../../../../enginex/Stream/Stems/3123276661/刀郎 - 冲动的惩罚.flac', '刀郎 - 冲动的惩罚.flac')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	os.WriteFile(filepath.Join(lib, "Stems", "86 ad42124b.stems"), []byte("stems"), 0o644)
	done := filepath.Join(data, "enginex/Stream/Stems/2/Done - Song.flac")
	rendering := filepath.Join(data, "enginex/Stream/Stems/3123276661/刀郎 - 冲动的惩罚.flac")

	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	for _, e := range []stemmedTrack{
		{Track: providers.Track{ID: "3", Title: "Elsewhere", Provider: "netease"}, Path: "/sd/Stream/Stems/3/Not In This Library.flac"},
		{Track: providers.Track{ID: "2", Title: "Song", Provider: "netease"}, Path: done},
		{Track: providers.Track{ID: "77004", Title: "冲动的惩罚", Provider: "netease"}, Path: rendering},
	} {
		if err := s.addStemmed("netease", e); err != nil {
			t.Fatal(err)
		}
	}
	titles := func() []string {
		t.Helper()
		w := httptest.NewRecorder()
		s.tidal.stemsListTracks(w, httptest.NewRequest("GET", "/v1/playlists/x/tracks", nil), "netease")
		var list struct {
			Items []struct {
				Item struct {
					Title          string `json:"title"`
					AllowStreaming bool   `json:"allowStreaming"`
				} `json:"item"`
			} `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Items) != 3 {
			t.Fatalf("list %s (%v)", w.Body.String(), err)
		}
		var out []string
		for _, it := range list.Items {
			out = append(out, it.Item.Title+"|"+strconv.FormatBool(it.Item.AllowStreaming))
		}
		return out
	}
	// Newest first: the track being rendered, the finished one, the one this library does not know.
	got := titles()
	if !strings.HasPrefix(got[0], "Stems not ready") || !strings.Contains(got[0], "冲动的惩罚") || !strings.HasSuffix(got[0], "|false") {
		t.Fatalf("a row without Stems is offered as a stemmed track: %q", got[0])
	}
	if got[1] != "Song|true" || got[2] != "Elsewhere|true" {
		t.Fatalf("rows %q", got)
	}
	// The render is done: the row is the track again.
	os.WriteFile(filepath.Join(lib, "Stems", "258 ad42124b.stems"), []byte("stems"), 0o644)
	if got = titles(); got[0] != "冲动的惩罚|true" {
		t.Fatalf("after the render: %q", got[0])
	}
}

// A file name is at most 255 bytes, not 255 characters: a title of 80 emoji
// is 320 bytes. The name was cut by characters, the link failed with "name
// too long" and Engine got the id-named download again.
func TestStemsSourceNameFitsAFileName(t *testing.T) {
	uiDir, stateDir, dl := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dl, "spotify-long.ogg"), []byte("OggS-fake-vorbis"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dl, "spotify-cjk.ogg"), []byte("OggS-fake-vorbis"), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dl, AllowLossy: true}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: uiDir, StateDir: stateDir}, jobs)
	t.Cleanup(s.Close)
	for id, title := range map[string]string{"long": strings.Repeat("\U0001F3B5", 80), "cjk": strings.Repeat("冲动的惩罚", 30)} {
		gid := strconv.FormatInt(s.tidal.addTrack(trackRef{track: providers.Track{ID: id, Title: title, Artist: "刀郎", Provider: "spotify"}}), 10)
		req := httptest.NewRequest("GET", "/v1/enginex/stems/source?id="+gid, nil)
		req.RemoteAddr = "127.0.0.1:41000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		var src struct{ Path string }
		if json.Unmarshal(w.Body.Bytes(), &src); w.Code != 200 || src.Path == "" {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
		name := filepath.Base(src.Path)
		if filepath.Dir(src.Path) != filepath.Join(dl, "Stems", "spotify-"+id) || !strings.HasPrefix(name, "刀郎 - ") || !strings.HasSuffix(name, ".ogg") {
			t.Fatalf("%s: Engine gets %q, not a named file", id, src.Path)
		}
		if len(name) > 255 || len(name) < 240 || !utf8.ValidString(name) {
			t.Fatalf("%s: name of %d bytes, valid UTF-8 %v", id, len(name), utf8.ValidString(name))
		}
		if st, err := os.Stat(src.Path); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s: %v", id, err)
		}
	}
}
