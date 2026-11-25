package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Restored #271/#321 (6b) (replaces pkg/bridge/issue321_vfat_linux_test.go:
// TestIssue321AStickWithStaleEntriesIsNotWrittenUntilADiskCheck and
// TestIssue321ARefusedFirstPassLeavesTheStickAsItWas):
// when unsafeToFree detects unowned/stale FAT clusters on a target file, WriteBackCues refuses
// before allocating backups, marks, or temp copies, leaving the stick byte-for-byte untouched.
// Guards pkg/bridge/writeback_files.go:55 (`unsafeToFree`).
func TestRestored_Issue321_StaleClusterRefusalLeavesStickUntouched(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})

	engineWrites(t, mount, `UPDATE Track SET comment='ShouldNotWrite' WHERE id=1`, `UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=1`)
	pdbPath := filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")
	before, err := os.ReadFile(pdbPath)
	if err != nil {
		t.Fatal(err)
	}

	orig := rbstate.UnsafeToFree
	rbstate.UnsafeToFree = func(path string) bool { return filepath.Base(path) == "export.pdb" }
	defer func() { rbstate.UnsafeToFree = orig }()

	res, err := WriteBackCues(context.Background(), mount, WriteBackOptions{DateDir: "2026-11-26", SkipPreparingCheck: true})
	if !errors.Is(err, errUnownedClusters) {
		t.Fatalf("expected errUnownedClusters refusal, got res=%+v err=%v", res, err)
	}
	after, err := os.ReadFile(pdbPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refused pass modified export.pdb: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(mount, ".enginex-backup")); !os.IsNotExist(err) {
		t.Fatalf("refused pass created backup directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dbDir(mount), writeBackMarkName)); !os.IsNotExist(err) {
		t.Fatalf("refused pass left writeback mark: %v", err)
	}
}

// Restored #271/#321 (6d) (replaces pkg/bridge/issue321_keys_test.go:
// TestIssue321FinalPassReusesTheWatchersKeys):
// CueWatcher.Final reuses the watcher's in-memory CueSession key cache even when the on-disk
// shared key cache is cleared, deriving 0 new PBKDF2 keys on the eject pass.
// Guards pkg/bridge/writeback.go:306 (`p.keys = p.opts.Session.keys`) and pkg/bridge/cuewatch.go:195.
func TestRestored_Issue321_FinalPassReusesWatchersKeys(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	diskCache := t.TempDir()
	t.Setenv(keyCacheEnv, diskCache)
	oldPrep := setUSBPreparingFlagPath(filepath.Join(t.TempDir(), "no-preparing"))
	defer func() { setUSBPreparingFlagPath(oldPrep) }()

	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	plainCopy := filepath.Join(t.TempDir(), "plain.db")
	rawPlain, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainCopy, rawPlain, 0o600); err != nil {
		t.Fatal(err)
	}
	pdb, err := sql.Open("sqlite", plainCopy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pdb.Exec(`PRAGMA journal_mode=DELETE; ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		DELETE FROM content; DELETE FROM cue;
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath) VALUES
			(1, 'T1', 12800, 4, ?, 44100, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(2, 'T2', 12400, 4, ?, 44100, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT');`,
		"/"+testfixture.LargeTrackPath(1), "/"+testfixture.LargeTrackPath(2))
	_ = pdb.Close()
	if err != nil {
		t.Fatal(err)
	}
	salt, err := rekordbox.ReadSQLCipherSalt(filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	p2WriteEncryptedExport(t, filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db"), plainCopy, salt)

	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})

	// Wipe disk cache before watcher rescans so watcher Pass derives 1 key into st.session.keys.
	t.Setenv(keyCacheEnv, t.TempDir())
	w := &CueWatcher{
		mounts: func() []string { return []string{mount} },
		pass:   WriteBackCues,
	}
	w.rescan()
	defer w.forget(nil)

	engineWrites(t, mount, `UPDATE Track SET comment='Pass1' WHERE id=1`, `UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=1`)
	d1 := rekordbox.KeyDerivations()
	res1, err := w.Pass(context.Background(), mount)
	if err != nil || res1.TracksChanged != 1 || !res1.OneLibraryUpdated {
		t.Fatalf("watcher Pass 1: res=%+v err=%v", res1, err)
	}
	if got := rekordbox.KeyDerivations() - d1; got != 1 {
		t.Fatalf("watcher Pass 1 derived %d keys, want 1", got)
	}

	// Switch on-disk cache to a fresh empty directory; Final must still derive 0 keys via st.session.keys.
	t.Setenv(keyCacheEnv, t.TempDir())
	engineWrites(t, mount, `UPDATE Track SET comment='Pass2' WHERE id=2`, `UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+200 WHERE id=2`)
	d2 := rekordbox.KeyDerivations()
	res2, err := w.Final(context.Background(), mount)
	if err != nil || res2.TracksChanged != 1 || !res2.OneLibraryUpdated {
		t.Fatalf("watcher Final pass: res=%+v err=%v", res2, err)
	}
	if got := rekordbox.KeyDerivations() - d2; got != 0 {
		t.Fatalf("watcher Final derived %d keys instead of reusing session keys", got)
	}
}
