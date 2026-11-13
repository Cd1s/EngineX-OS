package mods

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An upgrade keeps the previous version in a directory of its own: another installed mod whose ID
// happens to be "<id>.old" is untouched, and nothing is left behind.
func TestUpgradeKeepsOtherModsAndLeavesNoBackup(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager("", filepath.Join(base, "ui"))
	mgr.runScript = func(Mod, string) error { return nil }
	pkg := func(id, version, marker string) []byte {
		return testArchive(t, map[string]string{"mod.json": `{"id":"` + id + `","version":"` + version + `","script":"start.sh"}`, "start.sh": "true", "marker": marker}, "")
	}
	for _, p := range [][]byte{pkg("alpha", "1.0.0", "a1"), pkg("alpha.old", "1.0.0", "other mod"), pkg("alpha", "1.0.1", "a2")} {
		if _, err := mgr.InstallArchive(bytes.NewReader(p)); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(base, "mods", "alpha.old", "marker")); err != nil || string(b) != "other mod" {
		t.Fatalf("the other mod after the upgrade: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "mods", "alpha", "marker")); string(b) != "a2" {
		t.Fatalf("alpha after the upgrade: %q", b)
	}
	entries, _ := os.ReadDir(filepath.Join(base, "mods"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
	if list, _ := mgr.Discover(); len(list) != 2 {
		t.Fatalf("mods: %+v", list)
	}
}
