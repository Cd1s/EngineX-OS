package webui

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTracklist268Export(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "USB & set", "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// The history owns its Track rows: m.db can contain unrelated IDs/metadata.
	if err := os.WriteFile(filepath.Join(dir, "m.db"), []byte("not the history database"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hm.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, err := os.ReadFile("../../docs/re-report/ENGINE-5.0.4-hm.db-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;
		INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES
		(1,'Evening <set>',1700000000,'Europe/Berlin',0),(2,'Other set',1700000100,'UTC',0),
		(3,'Deleted',1700000200,'UTC',1),(4,'Unknown clock',1700000300,NULL,0);
		INSERT INTO Track(id,title,artist) VALUES (1,'Song, "one"','Artist'),(2,'=1+1','@artist');
		INSERT INTO HistorylistEntity(id,listId,trackId,startTime) VALUES
		(1,1,1,1700000002),(2,1,2,1700000001),(3,1,1,1700000002),
		(4,2,1,1700000101),(5,3,1,1700000201),(6,4,1,1700000301);`); err != nil {
		t.Fatal(err)
	}
	// Keep Engine's WAL open; a read-only export must see committed WAL entries.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	walBefore, err := os.ReadFile(path + "-wal")
	if err != nil || len(walBefore) == 0 {
		t.Fatalf("fixture has no pending WAL: %v", err)
	}
	s := New(Config{AccessToken: "tracklist-code", MediaRoot: root, StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer s.Close()
	get := func(path string, auth bool, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		if auth {
			r.Header.Set("Authorization", "Bearer tracklist-code")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("GET %s: %d, want %d: %s", path, w.Code, want, w.Body)
		}
		return w
	}
	get("/api/tracklists", false, 401)
	get("/api/tracklists/export?drive=USB+%26+set&id=1&format=csv", false, 401)
	var listed struct {
		Sets []struct {
			ID    int64  `json:"id"`
			Drive string `json:"drive"`
			Title string `json:"title"`
		} `json:"sets"`
		Unavailable []string `json:"unavailable"`
	}
	if err := json.Unmarshal(get("/api/tracklists", true, 200).Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	sets := listed.Sets
	if len(sets) != 3 || sets[0].ID != 4 || sets[2].ID != 1 || sets[2].Drive != "USB & set" || sets[2].Title != "Evening <set>" {
		t.Fatalf("sessions: %+v", sets)
	}
	base := "/api/tracklists/export?drive=USB+%26+set&id=1&format="
	w := get(base+"csv", true, 200)
	if !strings.HasPrefix(w.Body.String(), "\ufeff") {
		t.Fatal("CSV must identify UTF-8 for spreadsheet apps")
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"played_at", "timezone", "artist", "title"},
		{"2023-11-14T22:13:21Z", "Europe/Berlin", "'@artist", "'=1+1"},
		{"2023-11-14T22:13:22Z", "Europe/Berlin", "Artist", `Song, "one"`},
		{"2023-11-14T22:13:22Z", "Europe/Berlin", "Artist", `Song, "one"`},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows: %#v, want %#v", rows, want)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), ".csv") || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("download headers: %v", w.Header())
	}
	text := get(base+"txt", true, 200).Body.String()
	if strings.Count(text, `Song, "one"`) != 2 || !strings.Contains(text, "@artist - =1+1") || strings.Count(text, "\n") != 3 {
		t.Fatalf("text: %q", text)
	}
	unknown := get("/api/tracklists/export?drive=USB+%26+set&id=4&format=csv", true, 200)
	unknownRows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(unknown.Body.String(), "\ufeff"))).ReadAll()
	if err != nil || len(unknownRows) != 2 || unknownRows[1][0] != "" || unknownRows[1][1] != "" {
		t.Fatalf("unknown clock: %#v %v", unknownRows, err)
	}
	for _, query := range []string{
		"drive=..%2Foutside&id=1&format=csv", "drive=USB+%26+set&id=1+OR+1=1&format=csv",
		"drive=USB+%26+set&id=0&format=csv", "drive=USB+%26+set&id=1&format=html",
	} {
		get("/api/tracklists/export?"+query, true, 400)
	}
	get("/api/tracklists/export?drive=absent&id=1&format=csv", true, 404)
	get("/api/tracklists/export?drive=USB+%26+set&id=3&format=csv", true, 404)
	get("/api/tracklists/export?drive=USB+%26+set&id=999&format=csv", true, 404)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("export modified hm.db: %v", err)
	}
	walAfter, err := os.ReadFile(path + "-wal")
	if err != nil || !bytes.Equal(walBefore, walAfter) {
		t.Fatalf("export modified hm.db-wal: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM HistorylistEntity").Scan(&count); err != nil || count != 6 {
		t.Fatalf("history changed: count=%d err=%v", count, err)
	}
	for _, tc := range []struct{ artist, title, csvArtist, csvTitle string }{
		{"  +SUM(1,1)", "-2", "'  +SUM(1,1)", "'-2"},
		{"\t=1", "艺人\n下一行", "'\t=1", "艺人\n下一行"},
	} {
		if _, err := db.Exec("UPDATE Track SET artist=?,title=? WHERE id=2", tc.artist, tc.title); err != nil {
			t.Fatal(err)
		}
		csvBody := get(base+"csv", true, 200).Body.String()
		rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(csvBody, "\ufeff"))).ReadAll()
		if err != nil || len(rows) != 4 || rows[1][2] != tc.csvArtist || rows[1][3] != tc.csvTitle {
			t.Fatalf("CSV metadata: %#v %v", rows, err)
		}
		if text := get(base+"txt", true, 200).Body.String(); strings.Count(text, "\n") != 3 {
			t.Fatalf("metadata split a play across text lines: %q", text)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(get("/api/tracklists", true, 200).Body.Bytes(), &listed); err != nil || len(listed.Sets) != 0 || len(listed.Unavailable) != 0 {
		t.Fatalf("missing history: %+v %v", listed, err)
	}
	get(base+"csv", true, 404)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read created missing hm.db: %v", err)
	}
	if err := os.WriteFile(path, []byte("malformed history"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(get("/api/tracklists", true, 200).Body.Bytes(), &listed); err != nil || len(listed.Sets) != 0 || !reflect.DeepEqual(listed.Unavailable, []string{"USB & set"}) {
		t.Fatalf("corrupt history: %+v %v", listed, err)
	}
	get(base+"csv", true, 500)
	otherDir := filepath.Join(root, "Other USB", "Engine Library", "Database2")
	if err := os.MkdirAll(otherDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "m.db"), []byte("another library"), 0644); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", filepath.Join(otherDir, "hm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec(`CREATE TABLE Historylist(id INTEGER,title TEXT,startTime INTEGER,timezone TEXT,isDeleted INTEGER);
		INSERT INTO Historylist VALUES(1,'Healthy set',1700000000,'UTC',0);`); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(get("/api/tracklists", true, 200).Body.Bytes(), &listed); err != nil || len(listed.Sets) != 1 || listed.Sets[0].Drive != "Other USB" || !reflect.DeepEqual(listed.Unavailable, []string{"USB & set"}) {
		t.Fatalf("one corrupt drive hid healthy sets: %+v %v", listed, err)
	}
}
