package mods

import (
	"os"
	"path/filepath"
	"testing"
)

// A mod on a removable drive stays enabled only with the content and drive it was enabled with:
// different content under the same ID, or the same ID from another drive, needs enabling again and
// none of its scripts run before that.
func TestRemovableModStaysEnabledOnlyWithTheSameContent(t *testing.T) {
	base := t.TempDir()
	media := filepath.Join(base, "media")
	write := func(drive, script string) {
		dir := filepath.Join(media, drive, "mods", "visual-a")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"id":"visual-a","version":"1.0.0","script":"start.sh"}`), 0o644)
		os.WriteFile(filepath.Join(dir, "start.sh"), []byte(script), 0o755)
	}
	var ran []string
	mgr := NewManager(media, filepath.Join(base, "ui"))
	mgr.runScript = func(mod Mod, action string) error {
		b, _ := os.ReadFile(filepath.Join(mod.Dir, "start.sh"))
		ran = append(ran, mod.Drive+":"+string(b)+":"+action)
		return nil
	}
	enabled := func() bool {
		list, err := mgr.Discover()
		if err != nil || len(list) != 1 {
			t.Fatalf("discover: %+v %v", list, err)
		}
		return list[0].Enabled
	}

	write("STICK", "reviewed")
	if err := mgr.SetModEnabled("visual-a", true); err != nil {
		t.Fatal(err)
	}
	if !enabled() {
		t.Fatal("the enabled mod is not enabled")
	}
	ran = nil
	mgr.StartEnabled()
	if len(ran) != 1 || ran[0] != "STICK:reviewed:start" {
		t.Fatalf("start of the unchanged mod: %q", ran)
	}

	// Same ID, other content on the same drive.
	write("STICK", "something else")
	ran = nil
	mgr.StartEnabled()
	if enabled() || len(ran) != 0 {
		t.Fatalf("changed content: enabled=%v, ran %q", enabled(), ran)
	}
	if err := mgr.SetModEnabled("visual-a", false); err != nil || len(ran) != 0 {
		t.Fatalf("disabling the changed mod ran %q (%v)", ran, err)
	}

	// Same ID and content, another drive.
	write("STICK", "reviewed")
	if err := mgr.SetModEnabled("visual-a", true); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(media, "STICK"))
	write("OTHER", "reviewed")
	ran = nil
	mgr.StartEnabled()
	if enabled() || len(ran) != 0 {
		t.Fatalf("other drive: enabled=%v, ran %q", enabled(), ran)
	}
	// An explicit enable approves it again.
	if err := mgr.SetModEnabled("visual-a", true); err != nil || !enabled() {
		t.Fatalf("re-enable: %v", err)
	}
}
