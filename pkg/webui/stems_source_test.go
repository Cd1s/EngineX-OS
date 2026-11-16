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
	got := stemsImportFile(cache, track, 0)
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
	again := stemsImportFile(cache, track, 0)
	if after, _ := os.Stat(again); again != got || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("second request: %q, rewritten", again)
	}
	// A format that is not tagged is still named: a link to the download.
	ogg := filepath.Join(dl, "spotify-sp1.ogg")
	os.WriteFile(ogg, []byte("OggS-vorbis"), 0o644)
	named := stemsImportFile(ogg, providers.Track{Title: "Song", Artist: "DJ"}, 0)
	a, _ := os.Stat(ogg)
	b, errB := os.Stat(named)
	if named != filepath.Join(dl, "Stems", "spotify-sp1", "DJ - Song.ogg") || errB != nil || !os.SameFile(a, b) {
		t.Fatalf("ogg import file %q (%v)", named, errB)
	}
	// Nothing to name it by: the download, as before.
	if p := stemsImportFile(ogg, providers.Track{ID: "x"}, 0); p != ogg {
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

func stemsSourceServer(t *testing.T, dl string, minFree int64) (*Server, func(id, title string) string) {
	t.Helper()
	jobs := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dl, AllowLossy: true, MinFree: minFree}})
	t.Cleanup(jobs.Close)
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir()}, jobs)
	t.Cleanup(s.Close)
	return s, func(id, title string) string {
		t.Helper()
		gid := strconv.FormatInt(s.tidal.addTrack(trackRef{track: providers.Track{ID: id, Title: title, Artist: "DJ", Provider: "spotify"}}), 10)
		req := httptest.NewRequest("GET", "/v1/enginex/stems/source?id="+gid, nil)
		req.RemoteAddr = "127.0.0.1:41000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		var src struct{ Path string }
		if json.Unmarshal(w.Body.Bytes(), &src); w.Code != 200 || src.Path == "" {
			t.Fatalf("stems source %s: %d %s", id, w.Code, w.Body.String())
		}
		return src.Path
	}
}

// Nothing evicts a file Engine has imported, so the imported sources must not
// take the room the stream cache keeps free on its volume: below that reserve
// Engine gets the download itself, as before the files were named.
func TestStemsSourceKeepsTheCacheReserve(t *testing.T) {
	dl := t.TempDir()
	cache := filepath.Join(dl, "spotify-a.ogg")
	if err := os.WriteFile(cache, []byte("OggS-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, source := stemsSourceServer(t, dl, 1<<62) // more than any volume has free
	if got := source("a", "One"); got != cache {
		t.Fatalf("below the reserve Engine got %q, want the download %q", got, cache)
	}
	if _, err := os.Stat(filepath.Join(dl, "Stems")); err == nil {
		t.Fatal("a file was added below the reserve")
	}
}

// The same rule at its two limits, with the free space given: a copy that
// would go below the reserve is not written (the name is a link, which takes
// no room), and a copy that fits is tagged.
func TestStemsImportFileCopiesOnlyAboveTheReserve(t *testing.T) {
	dl := t.TempDir()
	cache := filepath.Join(dl, "3123276661.flac")
	if err := os.WriteFile(cache, tinyFLAC(), 0o644); err != nil {
		t.Fatal(err)
	}
	track := providers.Track{ID: "77004", Provider: "netease", Title: "冲动的惩罚", Artist: "刀郎"}
	named := filepath.Join(dl, "Stems", "3123276661", "刀郎 - 冲动的惩罚.flac")
	var free int64
	stemsFreeSpace = func(string) (int64, int64, bool, error) { return free, 1 << 30, false, nil }
	defer func() { stemsFreeSpace = freeSpace }()
	const reserve = 1000
	size := int64(len(tinyFLAC()))

	free = reserve - 1
	if got := stemsImportFile(cache, track, reserve); got != cache {
		t.Fatalf("below the reserve: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dl, "Stems")); err == nil {
		t.Fatal("a directory was left below the reserve")
	}
	free = reserve + size - 1
	got := stemsImportFile(cache, track, reserve)
	a, _ := os.Stat(cache)
	b, err := os.Stat(got)
	if got != named || err != nil || !os.SameFile(a, b) {
		t.Fatalf("no room for a copy: %q (%v), want a link to the download", got, err)
	}
	os.Remove(named)
	free = reserve + size
	got = stemsImportFile(cache, track, reserve)
	file, _ := os.ReadFile(got)
	if got != named || !bytes.Contains(file, []byte("TITLE=冲动的惩罚")) {
		t.Fatalf("room for a copy: %q without tags", got)
	}
}

// An imported source that nothing refers to any more is removed: its record
// fell off the end of the 500-row list, the track came again under another
// name, or a copy was left half written. A file Engine has as a track stays
// whatever the lists say, and so does everything outside the tree the import
// writes.
func TestStemsSourcesNothingRefersToAreRemoved(t *testing.T) {
	data := t.TempDir()
	shadowDataRoot = data
	defer func() { shadowDataRoot = "/data" }()
	lib := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	dl := filepath.Join(data, "enginex/Stream")
	write := func(p string) string {
		t.Helper()
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("OggS-"+filepath.Base(p)), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exists := func(p string) bool { _, err := os.Lstat(p); return err == nil }
	for _, id := range []string{"a", "b", "c"} {
		write(filepath.Join(dl, "spotify-"+id+".ogg"))
	}
	os.MkdirAll(filepath.Join(lib, "Database2"), 0o755)
	mdb := filepath.Join(lib, "Database2/m.db")
	db, err := sql.Open("sqlite", mdb)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE Track (id INTEGER PRIMARY KEY, path TEXT, filename TEXT)`,
		`INSERT INTO Track VALUES (7, '../../../../enginex/Stream/Stems/spotify-k/DJ - Kept.ogg', 'DJ - Kept.ogg')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, source := stemsSourceServer(t, dl, 0)

	first := source("a", "One")
	if first != filepath.Join(dl, "Stems/spotify-a/DJ - One.ogg") || !exists(first) {
		t.Fatalf("first import %q", first)
	}
	inLibrary := write(filepath.Join(dl, "Stems/spotify-k/DJ - Kept.ogg")) // a track in Engine's library, in no list
	halfWritten := write(filepath.Join(dl, "Stems/spotify-a/.part-1"))
	notOurs := []string{
		write(filepath.Join(dl, "keep.flac")),
		write(filepath.Join(dl, "Stems/loose.txt")),
		write(filepath.Join(dl, "Stems/spotify-k/sub/deep.flac")),
	}
	// 499 newer rows: the next import pushes the first one off the list.
	var rows []stemmedTrack
	for i := 0; i < maxStemmed-1; i++ {
		n := strconv.Itoa(i)
		rows = append(rows, stemmedTrack{Track: providers.Track{ID: "f" + n, Provider: "spotify", Title: n}, Path: "/elsewhere/" + n + ".ogg"})
	}
	if err := providers.SaveJSON(s.stemsFile(), map[string][]stemmedTrack{"spotify": append(rows, s.stemmed("spotify")...)}); err != nil {
		t.Fatal(err)
	}
	second := source("b", "Two")
	if exists(first) || exists(filepath.Dir(first)) {
		t.Fatalf("%q stayed after its record left the list", first)
	}
	if exists(halfWritten) {
		t.Fatal("a half-written copy stayed")
	}
	for _, p := range append(notOurs, inLibrary, second, filepath.Join(dl, "spotify-a.ogg")) {
		if !exists(p) {
			t.Fatalf("%q was removed", p)
		}
	}
	// The same track under another name: the earlier file is in no list and is not a track.
	renamed := source("b", "Two (Remaster)")
	if renamed == second || !exists(renamed) || exists(second) {
		t.Fatalf("after a new name: %q (there: %v), earlier %q (there: %v)", renamed, exists(renamed), second, exists(second))
	}
	// A library that cannot be read proves nothing: nothing is removed.
	if err := os.Rename(mdb, mdb+".away"); err != nil {
		t.Fatal(err)
	}
	unknown := write(filepath.Join(dl, "Stems/spotify-z/DJ - Unknown.ogg"))
	source("c", "Three")
	if !exists(unknown) || !exists(inLibrary) {
		t.Fatal("a file was removed while the library could not be read")
	}
}

// A download directory whose path is under /data can be a link to a drive.
// The files there are tracks in that drive's library, which the cleanup does
// not read: asking the internal library about them proves nothing, and they
// were removed. (A drive mounted on the directory: stems_sweep_linux_test.go.)
func TestStemsSweepLeavesADriveBehindALinkAlone(t *testing.T) {
	data, drive := t.TempDir(), t.TempDir()
	shadowDataRoot = data
	defer func() { shadowDataRoot = "/data" }()
	stemsSweepLibrary(t, data)
	os.MkdirAll(filepath.Join(drive, "Stream"), 0o755)
	os.MkdirAll(filepath.Join(data, "enginex"), 0o755)
	dl := filepath.Join(data, "enginex/Stream")
	if err := os.Symlink(filepath.Join(drive, "Stream"), dl); err != nil {
		t.Fatal(err)
	}
	stemsSweepKeeps(t, dl)
}

// stemsSweepLibrary is an internal library that has no track at all.
func stemsSweepLibrary(t *testing.T, data string) {
	t.Helper()
	dir := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library/Database2")
	os.MkdirAll(dir, 0o755)
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE Track (id INTEGER PRIMARY KEY, path TEXT, filename TEXT)`); err != nil {
		t.Fatal(err)
	}
}

// stemsSweepKeeps imports a row into the download directory dl, which is not
// on the deck's own file system, and wants a file there that no list has
// (that drive's library may have it as a track) to be left alone.
func stemsSweepKeeps(t *testing.T, dl string) {
	t.Helper()
	for _, p := range []string{filepath.Join(dl, "spotify-a.ogg"), filepath.Join(dl, "Stems/spotify-k/DJ - Kept.ogg")} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("OggS"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, source := stemsSourceServer(t, dl, 0)
	if got := source("a", "One"); got != filepath.Join(dl, "Stems/spotify-a/DJ - One.ogg") {
		t.Fatalf("import %q", got)
	}
	if _, err := os.Stat(filepath.Join(dl, "Stems/spotify-k/DJ - Kept.ogg")); err != nil {
		t.Fatalf("a file that is not on the deck's own file system was removed: %v", err)
	}
}
