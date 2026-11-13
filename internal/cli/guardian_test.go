package cli

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
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

func seedPreR4MP3Mount(t *testing.T, mount string) {
	t.Helper()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, err := os.ReadFile("../../pkg/dbhealer/testdata/engine-5.0.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema = bytes.ReplaceAll(schema, []byte("CREATE TABLE sqlite_sequence(name,seq);"), nil)
	if _, err = db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('cli-mp3',3,0,2)"); err != nil {
		t.Fatal(err)
	}
	mp3Bytes, err := os.ReadFile(filepath.Join("..", "..", "internal", "audioheader", "testdata", "01-lame-cbr-info-44100.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(mount, "Music", "Stream", "101.mp3")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, mp3Bytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var tb bytes.Buffer
	_ = binary.Write(&tb, binary.BigEndian, 44100.0)
	_ = binary.Write(&tb, binary.BigEndian, uint64(441000))
	_ = binary.Write(&tb, binary.BigEndian, uint32(1))
	for _, pk := range [3]float64{0.68, 0.40, 0.18} {
		_ = binary.Write(&tb, binary.BigEndian, pk)
	}
	td, err := enginedb.QCompress(tb.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var bb bytes.Buffer
	_ = binary.Write(&bb, binary.BigEndian, 44100.0)
	_ = binary.Write(&bb, binary.BigEndian, 441000.0)
	bb.WriteByte(1)
	for range 2 {
		_ = binary.Write(&bb, binary.BigEndian, uint64(2))
		_ = binary.Write(&bb, binary.LittleEndian, 5000.0)
		_ = binary.Write(&bb, binary.LittleEndian, int64(-4))
		_ = binary.Write(&bb, binary.LittleEndian, uint32(24))
		_ = binary.Write(&bb, binary.LittleEndian, uint32(0))
		_ = binary.Write(&bb, binary.LittleEndian, 445000.0)
		_ = binary.Write(&bb, binary.LittleEndian, int64(20))
		_ = binary.Write(&bb, binary.LittleEndian, uint32(0))
		_ = binary.Write(&bb, binary.LittleEndian, uint32(0))
	}
	bb.WriteByte(0)
	bd, err := enginedb.QCompress(bb.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileType,comment,isAnalyzed,isAvailable,isMetadataImported,isBeatGridLocked) VALUES(1,'../Music/Stream/101.mp3','101.mp3','mp3','[320k] MP3 44100 Hz; FLAC unavailable',1,1,1,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,beatData=? WHERE trackId=1`, td, bd); err != nil {
		t.Fatal(err)
	}
}

func readFirstBeatOffset(t *testing.T, mount string) float64 {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(mount, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var blob []byte
	if err := db.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=1`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	raw, err := enginedb.QUncompress(blob, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(raw[25:33]))
}

func TestGuardianFixMP3GridsFlagsAndExecution(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--boot-report", "--fix-mp3-grids"}, &out, &errOut); code != 2 {
		t.Fatalf("--boot-report --fix-mp3-grids: code=%d", code)
	}

	// 1. --normalize-reserve --fix-mp3-grids (internal library pre-start path).
	m1 := filepath.Join(t.TempDir(), "Engine")
	seedPreR4MP3Mount(t, m1)
	out.Reset()
	errOut.Reset()
	if code := Guardian([]string{"--heal-mount", m1, "--normalize-reserve", "--fix-mp3-grids", "--timeout", "10s"}, &out, &errOut); code != 0 {
		t.Fatalf("normalize+fix code=%d out=%s err=%s", code, &out, &errOut)
	}
	var res1 []guardianResult
	if err := json.Unmarshal(out.Bytes(), &res1); err != nil || len(res1) != 1 || res1[0].MP3Grids == nil || res1[0].MP3Grids.Corrected != 1 {
		t.Fatalf("unexpected normalize+fix result: %s err=%v", &out, err)
	}
	if got := readFirstBeatOffset(t, m1); math.Abs(got-(5000.0-2257)) > 1e-6 {
		t.Fatalf("beat0=%v, want %v", got, 5000.0-2257)
	}

	// 2. --heal-mount --data-only (stick mount pre-step path) + second run idempotency.
	m2 := filepath.Join(t.TempDir(), "USB")
	seedPreR4MP3Mount(t, m2)
	out.Reset()
	errOut.Reset()
	if code := Guardian([]string{"--heal-mount", m2, "--data-only"}, &out, &errOut); code != 0 {
		t.Fatalf("data-only code=%d out=%s err=%s", code, &out, &errOut)
	}
	var res2 []guardianResult
	if err := json.Unmarshal(out.Bytes(), &res2); err != nil || len(res2) != 1 || res2[0].MP3Grids == nil || res2[0].MP3Grids.Corrected != 1 {
		t.Fatalf("unexpected data-only result: %s err=%v", &out, err)
	}
	if got := readFirstBeatOffset(t, m2); math.Abs(got-(5000.0-2257)) > 1e-6 {
		t.Fatalf("beat0=%v, want %v", got, 5000.0-2257)
	}
	out.Reset()
	if code := Guardian([]string{"--heal-mount", m2, "--fix-mp3-grids"}, &out, &errOut); code != 0 {
		t.Fatalf("second run code=%d", code)
	}
	var res3 []guardianResult
	if err := json.Unmarshal(out.Bytes(), &res3); err != nil || len(res3) != 1 || res3[0].MP3Grids == nil || !res3[0].MP3Grids.Bypassed || res3[0].MP3Grids.Corrected != 0 {
		t.Fatalf("second run did not bypass: %s err=%v", &out, err)
	}
}
