package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mountFixture(t *testing.T) string {
	t.Helper()
	mount := filepath.Join(t.TempDir(), "USB with spaces")
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("../../pkg/dbhealer/testdata/engine-5.0.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema = bytes.ReplaceAll(schema, []byte("CREATE TABLE sqlite_sequence(name,seq);"), nil)
	if _, err = db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('cli',3,0,2); INSERT INTO Track(id,path) VALUES(1,'music.mp3')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return mount
}
func TestCLIHealAndStatusJSON(t *testing.T) {
	mount := mountFixture(t)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--heal-mount", mount}, &out, &errOut); code != 0 {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
	var results []result
	if err := json.Unmarshal(out.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Healthy || results[0].Waveforms != 1 {
		t.Fatal(results)
	}
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	before, _ := os.ReadFile(path)
	out.Reset()
	if code := run(context.Background(), []string{"--status", "--heal-mount", mount}, &out, &errOut); code != 0 {
		t.Fatalf("%d %s", code, &out)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("status changed DB")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatal("status created sidecars")
	}
	out.Reset()
	if code := run(context.Background(), []string{"--status", "--mount-root", filepath.Dir(mount)}, &out, &errOut); code != 0 {
		t.Fatal(code)
	}
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 {
		t.Fatalf("%s %v", &out, err)
	}
}
func TestCLIInvalidAndMissingLibrary(t *testing.T) {
	for _, args := range [][]string{{}, {"--watch", "--status"}, {"--watch", "--interval", "0s"}, {"--status", "--max-sessions", "-1"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, &out, &errOut); code != 2 {
			t.Fatalf("%v: %d", args, code)
		}
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--heal-mount", t.TempDir()}, &out, &errOut); code != 1 {
		t.Fatal(code)
	}
	var results []result
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].Error == "" {
		t.Fatalf("%s %v", &out, err)
	}
}
func TestWatchQuietPeriodAndSignalCancellation(t *testing.T) {
	mount := mountFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"--watch", "--heal-mount", mount, "--interval", "10ms", "--quiet-for", "20ms"}, &out, &errOut); code != 0 {
		t.Fatalf("%d %s", code, &errOut)
	}
	dec := json.NewDecoder(&out)
	var r result
	if err := dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	if r.Waveforms != 1 || !r.Healthy {
		t.Fatal(r)
	}
	if dec.More() {
		t.Fatal("watch repeatedly healed an unchanged library")
	}
}
