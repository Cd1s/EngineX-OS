package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuardianCommandDeadlineWithoutTimeoutUtility(t *testing.T) {
	if os.Getenv("ENGINEX_GUARDIAN_DEADLINE_TEST") == "1" {
		guardianBootReport = func(string, string) error { select {} }
		os.Exit(Guardian([]string{"--boot-report", "--timeout", "50ms"}, os.Stdout, os.Stderr))
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGuardianCommandDeadlineWithoutTimeoutUtility$")
	cmd.Env = append(os.Environ(), "ENGINEX_GUARDIAN_DEADLINE_TEST=1")
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 124 {
		t.Fatalf("blocked CLI did not enforce its own deadline: %v", err)
	}
}

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
	if code := Guardian([]string{"--heal-mount", mount}, &out, &errOut); code != 0 {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
	var results []guardianResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Healthy || results[0].Waveforms != 1 {
		t.Fatal(results)
	}
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	before, _ := os.ReadFile(path)
	out.Reset()
	if code := Guardian([]string{"--status", "--heal-mount", mount}, &out, &errOut); code != 0 {
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
	if code := Guardian([]string{"--status", "--mount-root", filepath.Dir(mount)}, &out, &errOut); code != 0 {
		t.Fatal(code)
	}
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 {
		t.Fatalf("%s %v", &out, err)
	}
}
func TestCLIInvalidAndMissingLibrary(t *testing.T) {
	for _, args := range [][]string{{}, {"--status", "--max-sessions", "-1"}, {"--heal-mount", "x", "extra"}} {
		var out, errOut bytes.Buffer
		if code := Guardian(args, &out, &errOut); code != 2 {
			t.Fatalf("%v: %d", args, code)
		}
	}
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--heal-mount", t.TempDir()}, &out, &errOut); code != 1 {
		t.Fatal(code)
	}
	var results []guardianResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].Error == "" {
		t.Fatalf("%s %v", &out, err)
	}
}
func TestRetiredWatchExitsAtOnce(t *testing.T) {
	mount := mountFixture(t)
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	before, _ := os.ReadFile(path)
	// The unit in the player's base /etc runs exactly this.
	for _, args := range [][]string{{"--watch"}, {"--watch", "--heal-mount", mount, "--interval", "10ms", "--quiet-for", "20ms"}} {
		var out, errOut bytes.Buffer
		start := time.Now()
		if code := Guardian(args, &out, &errOut); code != 0 || out.Len() != 0 || time.Since(start) > time.Second {
			t.Fatalf("%v: %d %q %s", args, code, &out, time.Since(start))
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("retired watch touched the library")
	}
}

// --status is read-only even when --normalize-reserve is also given.
func TestStatusWinsOverReserve(t *testing.T) {
	mount := mountFixture(t)
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	before, _ := os.ReadFile(path)
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--status", "--normalize-reserve", "--heal-mount", mount}, &out, &errOut); code != 0 {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("status changed DB")
	}
}

func TestGuardianBootReportDoesNotHealOrWatch(t *testing.T) {
	mount := mountFixture(t)
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := guardianBootReport
	t.Cleanup(func() { guardianBootReport = saved })
	calls := 0
	ui := t.TempDir()
	guardianBootReport = func(gotUI, gotMedia string) error {
		calls++
		if gotUI != ui || gotMedia != mount {
			t.Fatalf("wrong report paths")
		}
		return nil
	}
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--boot-report", "--ui-dir", ui, "--mount-root", mount}, &out, &errOut); code != 0 || calls != 1 || out.Len() != 0 {
		t.Fatalf("report code=%d calls=%d", code, calls)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("report changed database")
	}
	for _, args := range [][]string{
		{"--boot-report", "--heal-mount", mount},
		{"--boot-report", "--status"},
		{"--boot-report", "--normalize-reserve"},
		{"--boot-report", "--data-only"},
	} {
		if code := Guardian(args, &out, &errOut); code != 2 || calls != 1 {
			t.Fatalf("database option accepted: %v code=%d", args, code)
		}
	}
	if code := Guardian([]string{"--watch", "--boot-report"}, &out, &errOut); code != 0 || calls != 1 {
		t.Fatal("retired watch collected report")
	}
	guardianBootReport = func(string, string) error { return fmt.Errorf("secret details should never appear") }
	errOut.Reset()
	if code := Guardian([]string{"--boot-report", "--ui-dir", ui, "--mount-root", mount}, &out, &errOut); code != 1 || strings.Contains(errOut.String(), "secret details") {
		t.Fatalf("report failure %d %q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(ui, "boot-progress")); !os.IsNotExist(err) {
		t.Fatal("report emitted work progress")
	}
}

func TestGuardianBootProgressOnlyAfterCompletedWork(t *testing.T) {
	mount := mountFixture(t)
	ui := t.TempDir()
	if err := os.WriteFile(filepath.Join(ui, "boot-token"), []byte("attempt-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	run := func(args ...string) int { return Guardian(append(args, "--ui-dir", ui), &out, &errOut) }
	if code := run("--status", "--heal-mount", mount); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(filepath.Join(ui, "boot-progress")); !os.IsNotExist(err) {
		t.Fatal("status emitted progress")
	}
	if code := run("--heal-mount", mount); code != 0 {
		t.Fatal(code)
	}
	first, err := os.ReadFile(filepath.Join(ui, "boot-progress"))
	if err != nil || !strings.HasPrefix(string(first), "attempt-one database ") {
		t.Fatalf("missing completed-work record: %q %v", first, err)
	}
	if code := run("--normalize-reserve", "--heal-mount", mount); code != 0 {
		t.Fatal(code)
	}
	next, err := os.ReadFile(filepath.Join(ui, "boot-progress"))
	if err != nil || bytes.Equal(first, next) {
		t.Fatal("normalize did not advance progress")
	}
	if code := run("--heal-mount", t.TempDir()); code == 0 {
		t.Fatal("bad mount unexpectedly healed")
	}
	unchanged, err := os.ReadFile(filepath.Join(ui, "boot-progress"))
	if err != nil || !bytes.Equal(next, unchanged) {
		t.Fatal("failed work emitted progress")
	}
	if err := os.WriteFile(filepath.Join(ui, "safe-mode"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run("--normalize-reserve", "--heal-mount", mount); code != 0 {
		t.Fatal(code)
	}
	unchanged, err = os.ReadFile(filepath.Join(ui, "boot-progress"))
	if err != nil || !bytes.Equal(next, unchanged) {
		t.Fatal("fallback work emitted progress")
	}
}
