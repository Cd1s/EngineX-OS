package mods

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// Bug hunt 3: uninstall and upgrade ignore a stop script that fails; switching the mod off
// did not, so a mod whose stop fails (its daemon already gone, a missing interpreter) could
// never be switched off.
func TestDisableSurvivesFailingStop(t *testing.T) {
	mgr := NewManager("", filepath.Join(t.TempDir(), "ui"))
	var actions []string
	fail := false
	mgr.runScript = func(mod Mod, action string) error {
		actions = append(actions, mod.ID+":"+action)
		if fail {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	mod, err := mgr.InstallArchive(bytes.NewReader(testArchive(t, map[string]string{"mod.json": `{"id":"d1","script":"run.sh"}`, "run.sh": "exit 0"}, "")))
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetModEnabled(mod.ID, true); err != nil {
		t.Fatal(err)
	}
	fail = true
	actions = nil
	if err := mgr.SetModEnabled(mod.ID, false); err != nil {
		t.Fatalf("switching a mod off must ignore a failing stop: %v", err)
	}
	if strings.Join(actions, ",") != "d1:stop" {
		t.Fatalf("actions: %v, want the stop script tried once", actions)
	}
	if mgr.IsModEnabled(mod.ID) {
		t.Fatal("the mod is still enabled")
	}
	actions = nil
	mgr.StartEnabled() // the next boot: it must not come back
	if len(actions) != 0 {
		t.Fatalf("a switched-off mod was started at boot: %v", actions)
	}
	// Switching on still reports a start that fails, and leaves the mod off.
	if err := mgr.SetModEnabled(mod.ID, true); err == nil {
		t.Fatal("a failing start must be reported")
	}
	if mgr.IsModEnabled(mod.ID) {
		t.Fatal("a mod whose start failed is enabled")
	}
}
