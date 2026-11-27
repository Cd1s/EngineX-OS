package cli

import (
	"bytes"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
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

func TestGuardianDataOnlyDeadlineIsSoft(t *testing.T) {
	mount := mountWithNFDTracks(t, 130)
	var out, errOut bytes.Buffer
	started := time.Now()
	if code := Guardian([]string{"--heal-mount", mount, "--data-only", "--timeout", "10ms"}, &out, &errOut); code != 0 {
		t.Fatalf("soft data-only deadline code=%d out=%s err=%s", code, &out, &errOut)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("soft deadline did not return promptly: %s", elapsed)
	}
	var results []guardianResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || !results[0].Healthy {
		t.Fatalf("soft data-only deadline result=%s err=%v", &out, err)
	}
	if results[0].PathsNormalized >= 130 || results[0].MP3Grids != nil {
		t.Fatalf("first pass did not stop inside the soft budget: %s stderr=%s", &out, &errOut)
	}
	out.Reset()
	if code := Guardian([]string{"--heal-mount", mount, "--data-only", "--timeout", "10s"}, &out, &errOut); code != 0 {
		t.Fatalf("second pass code=%d out=%s err=%s", code, &out, &errOut)
	}
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].PathsNormalized == 0 {
		t.Fatalf("second pass did not finish remaining rows: %s", &out)
	}
}

func TestGuardianRefusesLibraryInUse(t *testing.T) {
	mount := mountFixture(t)
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--heal-mount", mount, "--data-only"}, &out, &errOut); code != 1 {
		t.Fatalf("in-use guardian code=%d out=%s err=%s", code, &out, &errOut)
	}
	var results []guardianResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].Error != enginedb.ErrLibraryInUse.Error() {
		t.Fatalf("in-use guardian result=%s err=%v", &out, err)
	}
}

func TestGuardianDataOnlyRunsMP3WithinSmallBudget(t *testing.T) {
	mount := filepath.Join(t.TempDir(), "USB")
	seedPreR4MP3Mount(t, mount)
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--heal-mount", mount, "--data-only", "--timeout", "2s"}, &out, &errOut); code != 0 {
		t.Fatalf("small-budget data-only code=%d out=%s err=%s", code, &out, &errOut)
	}
	var results []guardianResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].MP3Grids == nil || results[0].MP3Grids.Corrected != 1 {
		t.Fatalf("small-budget MP3 fix was skipped: %s err=%v", &out, err)
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

func mountWithNFDTracks(t *testing.T, count int) string {
	t.Helper()
	mount := mountFixture(t)
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE Track SET isAnalyzed=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	for id := 2; id <= count+1; id++ {
		old := filepath.ToSlash(filepath.Join("..", "Music", "Cafe\u0301", fmt.Sprintf("track-%04d.mp3", id)))
		new := filepath.ToSlash(filepath.Join("..", "Music", "Café", fmt.Sprintf("track-%04d.mp3", id)))
		actual := filepath.Join(mount, "Engine Library", filepath.FromSlash(new))
		if err := os.MkdirAll(filepath.Dir(actual), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(actual, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,isAnalyzed) VALUES(?,?,?,?,0)`, id, old, filepath.Base(old), 1); err != nil {
			t.Fatal(err)
		}
	}
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

	// 3. Standalone --heal-mount --fix-mp3-grids --timeout 5s (Gen-2 convert_mounted path).
	m3 := filepath.Join(t.TempDir(), "Gen2USB")
	seedPreR4MP3Mount(t, m3)
	out.Reset()
	errOut.Reset()
	if code := Guardian([]string{"--heal-mount", m3, "--fix-mp3-grids", "--timeout", "5s"}, &out, &errOut); code != 0 {
		t.Fatalf("standalone fix code=%d out=%s err=%s", code, &out, &errOut)
	}
	var res4 []guardianResult
	if err := json.Unmarshal(out.Bytes(), &res4); err != nil || len(res4) != 1 || res4[0].MP3Grids == nil || res4[0].MP3Grids.Corrected != 1 {
		t.Fatalf("unexpected standalone fix result: %s err=%v", &out, err)
	}
	if got := readFirstBeatOffset(t, m3); math.Abs(got-(5000.0-2257)) > 1e-6 {
		t.Fatalf("beat0=%v, want %v", got, 5000.0-2257)
	}

	// 4. --status --fix-mp3-grids stays read-only (--status wins).
	m4 := filepath.Join(t.TempDir(), "ReadOnlyStatus")
	seedPreR4MP3Mount(t, m4)
	out.Reset()
	errOut.Reset()
	if code := Guardian([]string{"--heal-mount", m4, "--status", "--fix-mp3-grids"}, &out, &errOut); code != 0 {
		t.Fatalf("status+fix code=%d out=%s err=%s", code, &out, &errOut)
	}
	var res5 []guardianResult
	if err := json.Unmarshal(out.Bytes(), &res5); err != nil || len(res5) != 1 || res5[0].MP3Grids != nil {
		t.Fatalf("expected --status to skip MP3Grids: %s err=%v", &out, err)
	}
	if got := readFirstBeatOffset(t, m4); math.Abs(got-5000.0) > 1e-6 {
		t.Fatalf("--status modified beat0=%v, want 5000.0", got)
	}
}

// The mount hook's carry-over check (edisksd_hook.c carry_library): exit 0 accepts the copy, 1
// refuses it, 2 is a usage error.
func TestGuardianCheckCopy(t *testing.T) {
	lib := filepath.Join(mountFixture(t), "Engine Library") // its one track: "music.mp3", not there yet
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--check-copy", lib}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "copied library refused") {
		t.Fatalf("no track found: code=%d err=%s", code, &errOut)
	}
	if err := os.WriteFile(filepath.Join(lib, "music.mp3"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Guardian([]string{"--check-copy", lib}, &out, &errOut); code != 0 {
		t.Fatalf("code=%d err=%s", code, &errOut)
	}
	var r struct{ Tracks, Missing int }
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Tracks != 1 || r.Missing != 0 {
		t.Fatalf("report %s: %+v %v", &out, r, err)
	}
	if code := Guardian([]string{"--check-copy", lib, "--heal-mount", lib}, &out, &errOut); code != 2 {
		t.Fatalf("combined with --heal-mount: code=%d", code)
	}
}

func TestGuardianS11RecoversMissingArtworkFromEmbeddedMP3(t *testing.T) {
	mount := mountFixture(t)
	musicDir := filepath.Join(mount, "Music")
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		t.Fatal(err)
	}

	makeMP3WithArt := func(name string, mime string, art []byte) {
		t.Helper()
		var body bytes.Buffer
		body.WriteByte(0) // ISO-8859-1
		body.WriteString(mime)
		body.WriteByte(0)
		body.WriteByte(3) // front cover
		body.WriteByte(0) // empty description
		body.Write(art)

		var frame bytes.Buffer
		frame.WriteString("APIC")
		var fsz [4]byte
		binary.BigEndian.PutUint32(fsz[:], uint32(body.Len()))
		frame.Write(fsz[:])
		frame.Write([]byte{0, 0})
		frame.Write(body.Bytes())

		sz := frame.Len()
		var hdr [10]byte
		copy(hdr[:3], "ID3")
		hdr[3] = 3
		hdr[6] = byte((sz >> 21) & 0x7f)
		hdr[7] = byte((sz >> 14) & 0x7f)
		hdr[8] = byte((sz >> 7) & 0x7f)
		hdr[9] = byte(sz & 0x7f)

		var mp3 bytes.Buffer
		mp3.Write(hdr[:])
		mp3.Write(frame.Bytes())
		mp3.Write([]byte{0xFF, 0xFB, 0x90, 0x00, 0, 0, 0, 0})
		if err := os.WriteFile(filepath.Join(musicDir, name), mp3.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Track 2: migrated AlbumArt (albumArt=NULL, 20-byte BLOB hash), missing Artwork/<b64>.jpg, valid embedded JPEG.
	var jpgBuf bytes.Buffer
	img1 := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			img1.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	if err := jpeg.Encode(&jpgBuf, img1, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	sum2 := sha1.Sum(jpgBuf.Bytes())
	makeMP3WithArt("covered.mp3", "image/jpeg", jpgBuf.Bytes())

	// Track 3: migrated AlbumArt (albumArt=NULL, 20-byte BLOB hash), missing Artwork/<b64>.jpg, oversized embedded PNG -> downscaled JPEG.
	var pngBuf bytes.Buffer
	img2 := image.NewRGBA(image.Rect(0, 0, 1600, 800))
	if err := png.Encode(&pngBuf, img2); err != nil {
		t.Fatal(err)
	}
	sum3 := sha1.Sum(pngBuf.Bytes())
	makeMP3WithArt("oversized.mp3", "image/png", pngBuf.Bytes())

	// Track 4: untagged WAV with missing Artwork/<b64>.jpg -> Track.albumArtId must NOT be cleared.
	if err := os.WriteFile(filepath.Join(musicDir, "untagged.wav"), []byte("RIFF\x24\x00\x00\x00WAVEfmt "), 0o644); err != nil {
		t.Fatal(err)
	}
	sum4 := sha1.Sum([]byte("missing-wav-cover"))

	dbPath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{q: `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)`},
		{q: `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(2,?,NULL)`, args: []any{sum2[:]}},
		{q: `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(3,?,NULL)`, args: []any{sum3[:]}},
		{q: `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(4,?,NULL)`, args: []any{sum4[:]}},
		{q: `INSERT INTO Track(id,path,albumArtId,isAnalyzed) VALUES(2,'../Music/covered.mp3',2,1)`},
		{q: `INSERT INTO Track(id,path,albumArtId,isAnalyzed) VALUES(3,'../Music/oversized.mp3',3,1)`},
		{q: `INSERT INTO Track(id,path,albumArtId,isAnalyzed) VALUES(4,'../Music/untagged.wav',4,1)`},
	} {
		if _, err := db.Exec(stmt.q, stmt.args...); err != nil {
			db.Close()
			t.Fatalf("%s: %v", stmt.q, err)
		}
	}
	db.Close()

	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--heal-mount", mount, "--data-only", "--timeout", "10s"}, &out, &errOut); code != 0 {
		t.Fatalf("guardian code=%d out=%s err=%s", code, &out, &errOut)
	}

	art2 := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum2[:])+".jpg")
	got2, err := os.ReadFile(art2)
	if err != nil || !bytes.Equal(got2, jpgBuf.Bytes()) {
		t.Fatalf("embedded JPEG cover not reconstructed at %s: len=%d err=%v", art2, len(got2), err)
	}

	art3 := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum3[:])+".jpg")
	got3, err := os.ReadFile(art3)
	if err != nil {
		t.Fatalf("oversized embedded PNG cover not reconstructed at %s: %v", art3, err)
	}
	dec3, fmt3, err := image.Decode(bytes.NewReader(got3))
	if err != nil || fmt3 != "jpeg" || dec3.Bounds().Dx() != 512 || dec3.Bounds().Dy() != 256 {
		var b image.Rectangle
		if dec3 != nil {
			b = dec3.Bounds()
		}
		t.Fatalf("oversized embedded PNG not downscaled JPEG: fmt=%q bounds=%v err=%v", fmt3, b, err)
	}

	// Track 4's albumArtId must remain preserved (never destructively nulled).
	check, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var t4Art sql.NullInt64
	if err := check.QueryRow(`SELECT albumArtId FROM Track WHERE id=4`).Scan(&t4Art); err != nil || !t4Art.Valid || t4Art.Int64 != 4 {
		t.Fatalf("untagged track albumArtId modified: %v err=%v", t4Art, err)
	}
}

func TestGuardianDataOnlyLocalizesOfflineDropboxTracks(t *testing.T) {
	mount := mountFixture(t)
	dbPath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	audio := filepath.Join(mount, "Music", "offline-dropbox.mp3")
	if err := os.MkdirAll(filepath.Dir(audio), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio, []byte("audio-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,streamingSource,uri,streamingFlags,isAnalyzed) VALUES
		(2,'../Music/offline-dropbox.mp3','offline-dropbox.mp3',11,'Dropbox','streaming://Dropbox/Track/offline-dropbox.mp3',5,1),
		(3,'../Music/cloud-only.mp3','cloud-only.mp3',11,'Dropbox','streaming://Dropbox/Track/cloud-only.mp3',5,1),
		(4,'../Music/offline-dropbox.mp3.tidal','offline-dropbox.mp3.tidal',11,'TIDAL','streaming://TIDAL/Track/99',1,1)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// --status stays strictly read-only.
	var out, errOut bytes.Buffer
	if code := Guardian([]string{"--status", "--heal-mount", mount}, &out, &errOut); code != 0 {
		t.Fatalf("status code=%d out=%s err=%s", code, &out, &errOut)
	}
	var results []guardianResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].DropboxLocalized != 0 {
		t.Fatalf("status unexpectedly localized: %s err=%v", &out, err)
	}

	// --data-only localizes only the local-on-SD Dropbox track.
	out.Reset()
	errOut.Reset()
	if code := Guardian([]string{"--heal-mount", mount, "--data-only"}, &out, &errOut); code != 0 {
		t.Fatalf("data-only code=%d out=%s err=%s", code, &out, &errOut)
	}
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || !results[0].Healthy || results[0].DropboxLocalized != 1 {
		t.Fatalf("data-only result=%s err=%v", &out, err)
	}

	check, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var src2, uri2 sql.NullString
	var flags2 int
	if err := check.QueryRow(`SELECT streamingSource, uri, COALESCE(streamingFlags,0) FROM Track WHERE id=2`).Scan(&src2, &uri2, &flags2); err != nil {
		t.Fatal(err)
	}
	if src2.Valid || uri2.Valid || flags2 != 0 {
		t.Fatalf("local dropbox track not cleared: src=%+v uri=%+v flags=%d", src2, uri2, flags2)
	}
	var src3, src4 string
	if err := check.QueryRow(`SELECT COALESCE(streamingSource,'') FROM Track WHERE id=3`).Scan(&src3); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRow(`SELECT COALESCE(streamingSource,'') FROM Track WHERE id=4`).Scan(&src4); err != nil {
		t.Fatal(err)
	}
	if src3 != "Dropbox" || src4 != "TIDAL" {
		t.Fatalf("protected tracks altered: id3=%q id4=%q", src3, src4)
	}
}

func TestGuardianNormalizeReserveRejectsUnsupportedHistoryDB(t *testing.T) {
	mount := mountFixture(t)
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	hm := filepath.Join(dbDir, "hm.db")

	hdb, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hdb.Exec("PRAGMA user_version=4194305"); err != nil {
		hdb.Close()
		t.Fatal(err)
	}
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(hm)
	if err != nil || len(raw) != 4096 {
		t.Fatalf("seed hm.db len=%d err=%v", len(raw), err)
	}
	raw[20] = 12
	raw[105], raw[106] = 0x0f, 0xf4
	if err := os.WriteFile(hm, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	hdb, err = sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	schema, err := os.ReadFile("../../docs/re-report/ENGINE-5.0.4-hm.db-schema.sql")
	if err != nil {
		hdb.Close()
		t.Fatal(err)
	}
	modSchema := strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", "")
	if _, err := hdb.Exec(modSchema); err != nil {
		hdb.Close()
		t.Fatal(err)
	}
	if _, err := hdb.Exec("INSERT INTO Information(id,uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(1,'hist-v4',4,0,2)"); err != nil {
		hdb.Close()
		t.Fatal(err)
	}
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}
	if res, ok, rerr := dbhealer.Reserve(hm); rerr != nil || !ok || res != 12 {
		t.Fatalf("setup hm.db reserve=%d ok=%v err=%v", res, ok, rerr)
	}

	var out, errOut bytes.Buffer
	code := Guardian([]string{"--heal-mount", mount, "--normalize-reserve"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("--normalize-reserve unexpectedly succeeded on schema-4 hm.db: code=%d out=%s err=%s", code, &out, &errOut)
	}
	var results []guardianResult
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].Healthy || results[0].Error == "" {
		t.Fatalf("expected unhealthy guardian result on schema-4 hm.db, got out=%s err=%v", &out, err)
	}
	if res, ok, rerr := dbhealer.Reserve(hm); rerr != nil || !ok || res != 12 {
		t.Fatalf("--normalize-reserve modified schema-4 hm.db reserve: res=%d ok=%v err=%v", res, ok, rerr)
	}
	if _, serr := os.Stat(hm + ".pre-reserve"); !os.IsNotExist(serr) {
		t.Fatalf("--normalize-reserve created hm.db.pre-reserve on schema-4 hm.db: err=%v", serr)
	}
}

func TestGuardianNormalizeReserveRejectsUnsupportedMainBeforeHistory(t *testing.T) {
	for _, mainRes := range []byte{0, 12} {
		t.Run(fmt.Sprintf("main_reserve_%d", mainRes), func(t *testing.T) {
			mount := filepath.Join(t.TempDir(), "USB")
			dbDir := filepath.Join(mount, "Engine Library", "Database2")
			if err := os.MkdirAll(dbDir, 0o755); err != nil {
				t.Fatal(err)
			}
			mPath := filepath.Join(dbDir, "m.db")
			seedEmptyWithReserve := func(p string, reserve byte, schemaRel string, major int) {
				db, err := sql.Open("sqlite", p)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("PRAGMA user_version=4194305"); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if reserve != 0 {
					raw, err := os.ReadFile(p)
					if err != nil || len(raw) != 4096 {
						t.Fatalf("seed %s len=%d err=%v", p, len(raw), err)
					}
					raw[20] = reserve
					raw[105], raw[106] = 0x0f, 0xf4
					if err := os.WriteFile(p, raw, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				db, err = sql.Open("sqlite", p)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				schema, err := os.ReadFile(schemaRel)
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				modSchema := strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", "")
				if _, err := db.Exec(modSchema); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if _, err := db.Exec("INSERT INTO Information(id,uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(1,'u-test',?,0,2)", major); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}

			seedEmptyWithReserve(mPath, mainRes, "../../pkg/dbhealer/testdata/engine-5.0.4.sql", 4)
			hmPath := filepath.Join(dbDir, "hm.db")
			seedEmptyWithReserve(hmPath, 12, "../../docs/re-report/ENGINE-5.0.4-hm.db-schema.sql", 3)

			var out, errOut bytes.Buffer
			code := Guardian([]string{"--heal-mount", mount, "--normalize-reserve"}, &out, &errOut)
			if code == 0 {
				t.Fatalf("--normalize-reserve unexpectedly succeeded with schema-4 m.db: out=%s err=%s", &out, &errOut)
			}
			var results []guardianResult
			if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].Healthy {
				t.Fatalf("expected unhealthy result, got out=%s err=%v", &out, err)
			}
			if res, ok, rerr := dbhealer.Reserve(hmPath); rerr != nil || !ok || res != 12 {
				t.Fatalf("hm.db reserve was rewritten despite schema-4 m.db: res=%d ok=%v err=%v", res, ok, rerr)
			}
			if _, serr := os.Stat(hmPath + ".pre-reserve"); !os.IsNotExist(serr) {
				t.Fatalf("hm.db.pre-reserve created despite schema-4 m.db: err=%v", serr)
			}
		})
	}
}

func TestGuardianNormalizeReserveRejectsMissingTruncatedAndForeignMainDB(t *testing.T) {
	seedReserve12History := func(t *testing.T, hmPath string) {
		t.Helper()
		db, err := sql.Open("sqlite", hmPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA user_version=4194305"); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(hmPath)
		if err != nil || len(raw) != 4096 {
			t.Fatalf("seed hm len=%d err=%v", len(raw), err)
		}
		raw[20] = 12
		raw[105], raw[106] = 0x0f, 0xf4
		if err := os.WriteFile(hmPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		db, err = sql.Open("sqlite", hmPath)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		schema, err := os.ReadFile("../../docs/re-report/ENGINE-5.0.4-hm.db-schema.sql")
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		modSchema := strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", "")
		if _, err := db.Exec(modSchema); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO Information(id,uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(1,'hist-valid',3,0,2)"); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name     string
		mainBody []byte
	}{
		{name: "missing_main", mainBody: nil},
		{name: "truncated_sqlite_header", mainBody: []byte("SQLite format 3\x00")},
		{name: "unrecognized_header", mainBody: []byte("not a database")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mount := filepath.Join(t.TempDir(), "USB")
			dbDir := filepath.Join(mount, "Engine Library", "Database2")
			if err := os.MkdirAll(dbDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.mainBody != nil {
				if err := os.WriteFile(filepath.Join(dbDir, "m.db"), tc.mainBody, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			hmPath := filepath.Join(dbDir, "hm.db")
			seedReserve12History(t, hmPath)

			var out, errOut bytes.Buffer
			code := Guardian([]string{"--heal-mount", mount, "--normalize-reserve"}, &out, &errOut)
			if code == 0 {
				t.Fatalf("--normalize-reserve unexpectedly succeeded (%s): out=%s err=%s", tc.name, &out, &errOut)
			}
			var results []guardianResult
			if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != 1 || results[0].Healthy || results[0].Error == "" {
				t.Fatalf("expected unhealthy error result (%s), got out=%s err=%v", tc.name, &out, err)
			}
			if res, ok, rerr := dbhealer.Reserve(hmPath); rerr != nil || !ok || res != 12 {
				t.Fatalf("hm.db reserve rewritten (%s): res=%d ok=%v err=%v", tc.name, res, ok, rerr)
			}
			if _, serr := os.Stat(hmPath + ".pre-reserve"); !os.IsNotExist(serr) {
				t.Fatalf("hm.db.pre-reserve created (%s): err=%v", tc.name, serr)
			}
		})
	}
}
