package webui

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// vouchedLibrary is root/Engine Library/Database2 with rb-bridge's own database and its
// vouch for every overview but raw's.
func vouchedLibrary(t *testing.T, root string, raw ...string) string {
	t.Helper()
	dir := filepath.Join(root, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	execClose(t, filepath.Join(dir, ".rb-bridge-state.db"), `CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL, fingerprint TEXT NOT NULL); INSERT INTO BridgeState VALUES('a',1,'x')`)
	if err := enginedb.VouchOverviews(dir, raw); err != nil {
		t.Fatal(err)
	}
	return dir
}

func execClose(t *testing.T, file, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// The deck overview draws a rekordbox track as r5 did unless this vouches for its scale.
// Review of a2cfaa3: the answer came from a read of Engine's m.db while Engine had it
// open, which left WAL and SHM files beside it and held a lock its commits waited for,
// and it followed any path to a library outside /media. It now comes from rb-bridge's
// vouch alone, and only for a real directory chain under the media root.
func TestOverviewScaleAnswersFromTheVouchAlone(t *testing.T) {
	media, data := t.TempDir(), t.TempDir()
	stick := filepath.Join(media, "USB 1")
	dir := vouchedLibrary(t, stick, "../Contents/old.mp3")
	// Engine's database in WAL mode with nothing beside it: any reader leaves m.db-wal and m.db-shm.
	mdb := filepath.Join(dir, "m.db")
	execClose(t, mdb, `PRAGMA journal_mode=WAL; CREATE TABLE Track(id INTEGER PRIMARY KEY, path TEXT); CREATE TABLE PerformanceData(trackId INTEGER PRIMARY KEY, overviewWaveFormData BLOB);
INSERT INTO Track VALUES(1,'../Contents/done.mp3'),(2,'../Contents/old.mp3'); INSERT INTO PerformanceData VALUES(1,NULL),(2,NULL)`)
	beside := func() string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		b, err := os.ReadFile(mdb)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(names, " ") + " " + string(b)
	}
	before := beside()
	if !strings.HasPrefix(before, ".rb-bridge-overview .rb-bridge-state.db m.db ") {
		t.Fatal("fixture has files beside m.db already:", before[:80])
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = data, media
	status := func(path string) int {
		w := httptest.NewRecorder()
		s.overviewScaleGet(w, httptest.NewRequest("GET", "/v1/sc5000/overview-scale?path="+url.QueryEscape(path), nil))
		return w.Code
	}
	expect := func(path string, want int, what string) {
		t.Helper()
		if got := status(path); got != want {
			t.Errorf("%s (%q): status %d, want %d", what, path, got, want)
		}
	}
	done := filepath.Join(stick, "Contents/done.mp3")
	expect(done, http.StatusOK, "a vouched library's track")
	expect(filepath.Join(stick, "Contents/Sub Folder/later.mp3"), http.StatusOK, "a vouched library's other track")
	expect(filepath.Join(stick, "Contents/old.mp3"), http.StatusNotFound, "the track left without band maxima")
	if after := beside(); after != before {
		t.Error("answering touched Engine's library:", strings.SplitN(after, " m.db ", 2)[0]+" m.db ...")
	}

	// Nothing outside a volume under the media root, however the path gets there.
	internal := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine")
	vouchedLibrary(t, internal)
	expect(filepath.Join(internal, "Music/Contents/done.mp3"), http.StatusNotFound, "the internal library")
	expect(filepath.Join(data, "Music/Contents/done.mp3"), http.StatusNotFound, "the internal drive")
	outside := filepath.Join(t.TempDir(), "elsewhere")
	vouchedLibrary(t, outside)
	link := func(target, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	link(outside, filepath.Join(media, "Linked Volume"))
	expect(filepath.Join(media, "Linked Volume/Contents/done.mp3"), http.StatusNotFound, "a symlinked volume")
	link(filepath.Join(outside, "Engine Library"), filepath.Join(media, "USB 2/Engine Library"))
	expect(filepath.Join(media, "USB 2/Contents/done.mp3"), http.StatusNotFound, "a symlinked Engine Library")
	link(filepath.Join(outside, "Engine Library/Database2"), filepath.Join(media, "USB 3/Engine Library/Database2"))
	expect(filepath.Join(media, "USB 3/Contents/done.mp3"), http.StatusNotFound, "a symlinked Database2")
	for i, name := range []string{".rb-bridge-overview", ".rb-bridge-state.db"} {
		// Everything real but one file, which leads to the other library's.
		volume := filepath.Join(media, []string{"USB 4", "USB 5"}[i])
		there := vouchedLibrary(t, volume)
		if got := status(filepath.Join(volume, "Contents/done.mp3")); got != http.StatusOK {
			t.Fatalf("fixture %s: status %d", volume, got)
		}
		for _, file := range []string{".rb-bridge-overview", ".rb-bridge-state.db"} {
			b, err := os.ReadFile(filepath.Join(outside, "Engine Library/Database2", file))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(there, file), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(filepath.Join(there, name)); err != nil {
			t.Fatal(err)
		}
		link(filepath.Join(outside, "Engine Library/Database2", name), filepath.Join(there, name))
		expect(filepath.Join(volume, "Contents/done.mp3"), http.StatusNotFound, "a symlinked "+name)
	}
	for _, path := range []string{"", "Contents/done.mp3", media, stick, stick + "/../../etc/Contents/done.mp3", filepath.Join(media, "9ccebc26d48deedc/Contents/done.mp3")} {
		expect(path, http.StatusNotFound, "no track on a volume")
	}

	// Engine's own name for a volume (the eglfs sandbox: /media/9ccebc26d48deedc for
	// /media/STICK): the volumes that hold the file answer, and all of them have to vouch.
	file := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file(done)
	file(filepath.Join(stick, "Contents/old.mp3"))
	file(filepath.Join(outside, "Contents/elsewhere.mp3"))
	expect(filepath.Join(media, "9ccebc26d48deedc/Contents/done.mp3"), http.StatusOK, "Engine's name for the volume")
	expect(filepath.Join(media, "9ccebc26d48deedc/Contents/old.mp3"), http.StatusNotFound, "Engine's name, the track left without band maxima")
	expect(filepath.Join(media, "9ccebc26d48deedc/Contents/elsewhere.mp3"), http.StatusNotFound, "a file only a symlinked volume holds")
	file(filepath.Join(media, "USB 6/Contents/done.mp3"))
	expect(filepath.Join(media, "9ccebc26d48deedc/Contents/done.mp3"), http.StatusNotFound, "another volume with the same file and no vouch")
	vouchedLibrary(t, filepath.Join(media, "USB 6"))
	expect(filepath.Join(media, "9ccebc26d48deedc/Contents/done.mp3"), http.StatusOK, "both volumes vouch")

	// Without a vouch that still holds, nothing is vouched for.
	execClose(t, filepath.Join(dir, ".rb-bridge-state.db"), `UPDATE BridgeState SET fingerprint='y'`)
	expect(done, http.StatusNotFound, "after an import by any build")
	if err := enginedb.VouchOverviews(dir, nil); err != nil {
		t.Fatal(err)
	}
	expect(done, http.StatusOK, "vouched for again")
	if err := os.Remove(filepath.Join(dir, ".rb-bridge-overview")); err != nil {
		t.Fatal(err)
	}
	expect(done, http.StatusNotFound, "a library without a vouch")
}
