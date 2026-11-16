package leave

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/migration"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func newDeck(t *testing.T) (Deck, string) {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "data/enginex/version.txt"), "5.3.0.5\n")
	write(t, filepath.Join(root, "data/enginex/bundle/release-tag"), "v5.3.0.5-r1\n")
	write(t, filepath.Join(root, "data/enginex/Stream/song.flac"), "user music")
	return Deck{Root: root, Stdout: io.Discard, Stderr: io.Discard}, root
}
func targetBundle(t *testing.T, version, tag string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "version.txt"), version+"\n")
	write(t, filepath.Join(dir, "release-tag"), tag+"\n")
	write(t, filepath.Join(dir, "install.sh"), "#!/bin/sh\nset -eu\nmkdir -p \"$ENGINEX_CUSTOM\"\ncp version.txt \"$ENGINEX_CUSTOM/version.txt\"\n")
	return dir
}
func TestInstallRefusesDowngradesWithoutLegacyWrites(t *testing.T) {
	for _, version := range []string{"5.3.0.2", "5.3.0.4"} {
		t.Run(version, func(t *testing.T) {
			d, root := newDeck(t)
			target := targetBundle(t, version, "v"+version+"-r5")
			err := d.Install(target)
			if err == nil || !strings.Contains(err.Error(), "upgrade to the latest") {
				t.Fatalf("downgrade result: %v", err)
			}
			b, _ := os.ReadFile(d.data("enginex/version.txt"))
			if string(b) != "5.3.0.5\n" {
				t.Fatalf("installed version changed: %q", b)
			}
			b, _ = os.ReadFile(d.data("enginex/Stream/song.flac"))
			if string(b) != "user music" {
				t.Fatal("music changed")
			}
			if _, err := os.Lstat(filepath.Join(root, migration.DataRelative)); !os.IsNotExist(err) {
				t.Fatalf("legacy path was written: %v", err)
			}
			if _, err := os.Lstat(d.data(journalName)); !os.IsNotExist(err) {
				t.Fatalf("restore journal was written: %v", err)
			}
		})
	}
}
func TestInstallRepairsAndUpgradesCurrentNames(t *testing.T) {
	for _, version := range []string{"5.3.0.5", "5.3.0.6"} {
		t.Run(version, func(t *testing.T) {
			d, root := newDeck(t)
			target := targetBundle(t, version, "v"+version+"-r1")
			if err := d.Install(target); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(d.data("enginex/version.txt"))
			if string(b) != version+"\n" {
				t.Fatalf("version %q", b)
			}
			if _, err := os.Lstat(filepath.Join(root, migration.DataRelative)); !os.IsNotExist(err) {
				t.Fatal("legacy directory created")
			}
		})
	}
}
func TestUpgradeReadsAnOlderRealDirectory(t *testing.T) {
	d, root := newDeck(t)
	if err := os.Rename(d.data("enginex"), filepath.Join(root, migration.DataRelative)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, migration.DataRelative, "version.txt"), "5.3.0.4\n")
	target := targetBundle(t, "5.3.0.5", "v5.3.0.5-r1")
	if older, version, err := d.Older(target); err != nil || older || version != "5.3.0.5" {
		t.Fatalf("upgrade read: %v %s %v", older, version, err)
	}
}
func TestRetiredLayoutAndJournalLeaveDataUntouched(t *testing.T) {
	d, _ := newDeck(t)
	if err := d.Handback(); err == nil {
		t.Fatal("retired layout operation succeeded")
	}
	write(t, d.data(journalName, "stage/user-data"), "parked user data")
	if err := d.Recover(); err == nil {
		t.Fatal("retired restore journal was applied")
	}
	b, err := os.ReadFile(d.data(journalName, "stage/user-data"))
	if err != nil || string(b) != "parked user data" {
		t.Fatalf("parked data changed: %q %v", b, err)
	}
	var out, errout bytes.Buffer
	t.Setenv("ENGINEX_LEAVE_ROOT", d.Root)
	if code := Main([]string{"layout"}, &out, &errout); code == 0 || out.Len() != 0 {
		t.Fatalf("retired CLI: %d %s", code, out.String())
	}
}
func TestActivationWaitsForTheUpgradeLock(t *testing.T) {
	d, _ := newDeck(t)
	script := filepath.Join(t.TempDir(), "activate.sh")
	marker := script + ".ran"
	write(t, script, "test \"$ENGINEX_LEAVE_LOCKED\" = 1\nprintf ready > \"$1\"\n")
	unlock, err := d.lock(true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.activate([]string{script, marker}) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("activation bypassed lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		unlock()
		t.Fatal("activation ran before lock release")
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activation did not resume")
	}
	b, _ := os.ReadFile(marker)
	if string(b) != "ready" {
		t.Fatalf("marker %q", b)
	}
}
