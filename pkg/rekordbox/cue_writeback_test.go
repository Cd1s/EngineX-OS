package rekordbox

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestEncryptSQLCipher4AndCLIProof(t *testing.T) {
	src := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exportLibrary.db")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	updates := map[uint32][]Cue{
		1: {
			{HotCue: 1, TimeMS: 12500, Name: "Drop A", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 2, TimeMS: 30000, EndMS: 34000, Loop: true, Active: true, Name: "Loop B", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
			{HotCue: 0, TimeMS: 5000, Name: "Intro", Color: [4]byte{255, 0x50, 0xb4, 0xff}, ColorCode: 5},
		},
	}
	outPath := filepath.Join(dir, "exportLibrary.new.db")
	if err := UpdateOneLibraryCues(context.Background(), dbPath, updates, outPath); err != nil {
		t.Fatalf("UpdateOneLibraryCues: %v", err)
	}

	// Proof 1: Our own reader reads back the re-encrypted DB.
	lib, err := ReadSQLite(context.Background(), outPath, nil)
	if err != nil {
		t.Fatalf("ReadSQLite: %v", err)
	}
	if len(lib.Tracks) == 0 || len(lib.Tracks[0].Cues) != 3 {
		t.Fatalf("got tracks=%d cues=%d, want 3 cues on track 1", len(lib.Tracks), len(lib.Tracks[0].Cues))
	}

	// Proof 2: sqlcipher CLI opens it with the same key and passes integrity_check.
	if sqlcipherBin, err := exec.LookPath("sqlcipher"); err == nil {
		cmd := exec.Command(sqlcipherBin, outPath,
			fmtSQLCipherPragma(oneLibraryPassphrase),
			"PRAGMA integrity_check;",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sqlcipher integrity_check failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "ok") {
			t.Fatalf("unexpected sqlcipher integrity_check output: %q", string(out))
		}
	} else {
		t.Skip("sqlcipher CLI not installed on this host")
	}
}

func fmtSQLCipherPragma(key string) string {
	return "PRAGMA key = '" + strings.ReplaceAll(key, "'", "''") + "';"
}

func TestRewriteANLZCuesRoundTrip(t *testing.T) {
	cues := []Cue{
		{HotCue: 1, TimeMS: 1200, Name: "Intro", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1, ColorID: 1},
		{HotCue: 2, TimeMS: 4500, EndMS: 6500, Loop: true, Active: true, Name: "Chorus", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9, ColorID: 9},
		{HotCue: 0, TimeMS: 500, Name: "Mem1", Color: [4]byte{255, 0x50, 0xb4, 0xff}, ColorCode: 5},
	}

	datOut, changed, err := RewriteANLZCues(testfixture.DAT(), cues, false, false)
	if err != nil || !changed {
		t.Fatalf("RewriteANLZCues DAT: changed=%v err=%v", changed, err)
	}
	aDat, err := ParseAnalysis(datOut)
	if err != nil {
		t.Fatalf("ParseAnalysis DAT: %v", err)
	}
	if len(aDat.Cues) != 3 || len(aDat.Beats) == 0 {
		t.Fatalf("DAT cues=%d beats=%d, want 3 cues and >0 beats", len(aDat.Cues), len(aDat.Beats))
	}

	extOut, changed, err := RewriteANLZCues(testfixture.EXT(), cues, true, false)
	if err != nil || !changed {
		t.Fatalf("RewriteANLZCues EXT: changed=%v err=%v", changed, err)
	}
	aExt, err := ParseAnalysis(extOut)
	if err != nil {
		t.Fatalf("ParseAnalysis EXT: %v", err)
	}
	if len(aExt.Cues) != 3 || len(aExt.Waveforms) == 0 {
		t.Fatalf("EXT cues=%d waveforms=%d, want 3 cues and >0 waveforms", len(aExt.Cues), len(aExt.Waveforms))
	}
	var foundChorus bool
	for _, c := range aExt.Cues {
		if c.HotCue == 2 && c.Loop && c.Name == "Chorus" && c.EndMS == 6500 {
			foundChorus = true
		}
	}
	if !foundChorus {
		t.Fatalf("missing Chorus hot loop in EXT: %+v", aExt.Cues)
	}
}

func TestUpdateOneLibraryCuesNoTempLeakOnError(t *testing.T) {
	// Encrypt a valid SQLite DB that lacks the cue table so UpdateOneLibraryCues
	// decrypts to /tmp and then fails inside applyCueUpdatesSQLite.
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "bad-plain.db")
	// Use decrypted fixture and drop the cue table so reserveSize=80 is preserved.
	rawPlain, err := os.ReadFile(filepath.Join("testdata", "sqlcipher4_onelibrary_decrypted.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainPath, rawPlain, 0o600); err != nil {
		t.Fatal(err)
	}
	salt, err := ReadSQLCipherSalt(filepath.Join("testdata", "sqlcipher4_onelibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(dir, "bad-enc.db")
	// Drop the cue table using Go's sql driver while keeping reserveSize=80.
	if err := dropCueTableForTest(plainPath); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := in.Stat()
	if err != nil {
		_ = in.Close()
		t.Fatal(err)
	}
	out, err := os.Create(encPath)
	if err != nil {
		_ = in.Close()
		t.Fatal(err)
	}
	encErr := EncryptSQLCipher4(context.Background(), in, st.Size(), out, salt, []byte(oneLibraryPassphrase))
	_ = in.Close()
	_ = out.Close()
	if encErr != nil {
		t.Fatal(encErr)
	}

	tempsBefore := listTempOneLibraryFiles(t)
	err = UpdateOneLibraryCues(context.Background(), encPath, map[uint32][]Cue{
		1: {{HotCue: 1, TimeMS: 1000}},
	}, filepath.Join(dir, "out.db"))
	if err == nil {
		t.Fatal("expected error when cue table is missing")
	}
	for p := range listTempOneLibraryFiles(t) {
		if !tempsBefore[p] {
			t.Fatalf("UpdateOneLibraryCues leaked temporary decrypted file on error: %s", p)
		}
	}
}

func dropCueTableForTest(plainPath string) error {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: plainPath}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`PRAGMA journal_mode=DELETE; DROP TABLE cue;`)
	return err
}
