package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func relocatePost(t *testing.T, srv *httptest.Server, url, body, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func TestRelocateEndpoints(t *testing.T) {
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
	ui := New(Config{AccessToken: "c", MediaRoot: filepath.Dir(root), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	stored := func() string {
		var p string
		db.QueryRow(`SELECT path FROM Track WHERE id=1`).Scan(&p)
		return p
	}

	if code, _ := relocatePost(t, srv, "/api/relocate", `{}`, ""); code != 401 {
		t.Fatalf("no login: %d", code)
	}
	// An empty request only previews.
	code, v := relocatePost(t, srv, "/api/relocate", `{}`, "c")
	if code != 200 || v["dryRun"] != true || stored() != "../Old/song.mp3" {
		t.Fatalf("%d %v %s", code, v, stored())
	}
	if code, _ = relocatePost(t, srv, "/api/relocate", `{"drive":"nope"}`, "c"); code != 404 {
		t.Fatalf("unknown drive: %d", code)
	}
	// The deck's own route applies, and only when asked to.
	code, v = relocatePost(t, srv, "/v1/sc5000/relocate", `{"dryRun":false}`, "")
	d, _ := v["drives"].([]any)
	if code != 200 || len(d) != 1 || stored() != "../Moved/song.mp3" {
		t.Fatalf("%d %v %s", code, v, stored())
	}
}
