package webui

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// An edit made while another EngineX program holds the volume's lock (a bridge window that is
// replacing the library, a reserve rebuild, a write-back pass) is refused with a message the
// user can act on, and writes nothing. Before #336 Go review R13 it was written into a file
// that was about to be replaced, and reported as done.

const libraryBusy = "the library is busy, try again in a moment"

func r13Post(t *testing.T, srv *httptest.Server, path, token string, body any) (int, map[string]any, time.Duration) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	t0 := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v, time.Since(t0)
}

func TestR13RelocateWhileTheVolumeIsLockedSaysBusyInTheConsole(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	_, root, db := testlibrary.Create(t)
	if err := os.MkdirAll(filepath.Join(root, "Moved"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Moved", "song.mp3"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(1,'../Old/song.mp3','song.mp3',3)`); err != nil {
		t.Fatal(err)
	}
	stored := func() (p string) {
		_ = db.QueryRow(`SELECT path FROM Track WHERE id=1`).Scan(&p)
		return p
	}
	srv := httptest.NewServer(New(Config{AccessToken: "c", MediaRoot: filepath.Dir(root), StateDir: t.TempDir()}, nil))
	defer srv.Close()

	unlock, err := rbstate.LockVolume(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	code, v, took := r13Post(t, srv, "/api/relocate", "c", map[string]any{"dryRun": false})
	unlock()
	drives, _ := v["drives"].([]any)
	if code != 200 || len(drives) != 1 {
		t.Fatalf("relocate under a held volume lock: %d %v", code, v)
	}
	drive, _ := drives[0].(map[string]any)
	if drive["error"] != libraryBusy {
		t.Errorf("the drive's error is %q, want %q (answer: %v)", drive["error"], libraryBusy, v)
	}
	if drive["applied"] == true || drive["queued"] == true || stored() != "../Old/song.mp3" {
		t.Errorf("the relocation was written or queued under a held volume lock: %v, path now %q", drive, stored())
	}
	if _, err := os.Lstat(filepath.Join(root, "Engine Library", "Database2", "enginex-pending.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused relocation left a queue file: %v", err)
	}
	if took > 5*time.Second {
		t.Errorf("the refusal took %s, want about two seconds at most", took)
	}

	// What the user sees: the page's own relRender, run on that answer.
	if node, err := exec.LookPath("node"); err != nil {
		t.Log("node is not installed: the page's rendering of the answer is not run")
	} else {
		page, err := os.ReadFile("index.html")
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(string(page), "function relRender(v){")
		if start < 0 {
			t.Fatal("the console page has no relRender")
		}
		fn := string(page)[start:]
		fn = fn[:strings.Index(fn, "\n")]
		answer, _ := json.Marshal(v)
		driver := `const els={};const $=id=>els[id]||(els[id]={textContent:'',disabled:false,replaceChildren(){},append(){}});
const t=(k,...a)=>k+' '+a.join(' ');let relPlan=0;const document={createElement:()=>({})};
` + fn + `
relRender(` + string(answer) + `);console.log(JSON.stringify({state:$('relState').textContent,apply:$('relApply').disabled}));`
		out, err := exec.Command(node, "-e", driver).CombinedOutput()
		if err != nil {
			t.Fatalf("the page's relRender: %v\n%s", err, out)
		}
		var shown struct {
			State string `json:"state"`
			Apply bool   `json:"apply"`
		}
		if err = json.Unmarshal(bytes.TrimSpace(out), &shown); err != nil {
			t.Fatalf("the page's relRender printed %q: %v", out, err)
		}
		if want := filepath.Base(root) + ": " + libraryBusy; shown.State != want {
			t.Errorf("the console shows %q, want %q", shown.State, want)
		}
	}

	// With the lock free the same request goes through.
	code, v, _ = r13Post(t, srv, "/api/relocate", "c", map[string]any{"dryRun": false})
	if drives, _ = v["drives"].([]any); code != 200 || len(drives) != 1 || stored() != "../Moved/song.mp3" {
		t.Fatalf("relocate with the lock free: %d %v, path now %q", code, v, stored())
	}
}

func TestR13DeckEditsWhileTheVolumeIsLockedAreRefusedAsBusy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	media := t.TempDir()
	drive := filepath.Join(media, "USB")
	dbDir := filepath.Join(drive, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE Track (id INTEGER PRIMARY KEY, genre TEXT, comment TEXT, label TEXT); INSERT INTO Track VALUES (7, 'Old', 'Old comment', 'Old label')`); err != nil {
		t.Fatal(err)
	}
	genre := func() (g string) {
		_ = db.QueryRow(`SELECT genre FROM Track WHERE id=7`).Scan(&g)
		return g
	}
	srv := httptest.NewServer(New(Config{AccessToken: "t", MediaRoot: media, UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer srv.Close()
	metadata := map[string]any{"drive": "USB", "trackId": 7, "genre": "House", "comment": "Warm-up", "label": "EngineX"}
	cue := map[string]any{"drive": "USB", "trackId": 7, "index": 1, "label": "Chorus", "color": "#FB8C00"}

	unlock, err := rbstate.LockVolume(drive, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []struct {
		path string
		body map[string]any
	}{{"/v1/enginex/track-metadata", metadata}, {"/v1/enginex/hotcues", cue}} {
		code, v, took := r13Post(t, srv, edit.path, "", edit.body)
		if code != http.StatusConflict || v["error"] != libraryBusy {
			t.Errorf("%s under a held volume lock: %d %v, want 409 and %q", edit.path, code, v, libraryBusy)
		}
		if took > 5*time.Second {
			t.Errorf("%s: the refusal took %s, want about two seconds at most", edit.path, took)
		}
	}
	if genre() != "Old" {
		t.Errorf("the metadata was written under a held volume lock: genre=%q", genre())
	}
	for _, name := range []string{"enginex_hotcues.json", "enginex-pending.json"} {
		if _, err := os.Lstat(filepath.Join(dbDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused edit left %s: %v", name, err)
		}
	}
	unlock()

	// With the lock free both go through.
	if code, v, _ := r13Post(t, srv, "/v1/enginex/track-metadata", "", metadata); code != 200 || genre() != "House" {
		t.Errorf("track metadata with the lock free: %d %v genre=%q", code, v, genre())
	}
	if code, v, _ := r13Post(t, srv, "/v1/enginex/hotcues", "", cue); code != 200 {
		t.Errorf("hot cue with the lock free: %d %v", code, v)
	}
	if _, err := os.Lstat(filepath.Join(dbDir, "enginex_hotcues.json")); err != nil {
		t.Errorf("the hot cue was not stored with the lock free: %v", err)
	}
}
