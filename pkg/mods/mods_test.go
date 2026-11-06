package mods

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func createTestMod(t *testing.T, baseDir, id, relPath, qmlContent string) string {
	t.Helper()
	modDir := filepath.Join(baseDir, id)
	if err := os.MkdirAll(modDir, 0755); err != nil {
		t.Fatal(err)
	}
	srcFile := "view.qml"
	if err := os.WriteFile(filepath.Join(modDir, srcFile), []byte(qmlContent), 0644); err != nil {
		t.Fatal(err)
	}

	manifest := Manifest{
		ID:          id,
		Name:        "Test Mod " + id,
		Version:     "1.0.0",
		Author:      "Author",
		Description: "A test mod",
		QMLFiles: map[string]string{
			srcFile: relPath,
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "manifest.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return modDir
}

func TestMods_Discover(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()

	// Drive 1 with /mods
	drive1Mods := filepath.Join(mediaRoot, "USB1", "mods")
	createTestMod(t, drive1Mods, "mod-alpha", "Skins/Views/Alpha.qml", "import QtQuick 2.0\nItem {\n    Text { text: \"Alpha\" }\n}\n")

	// Drive 2 with /EngineX/mods
	drive2Mods := filepath.Join(mediaRoot, "USB2", "EngineX", "mods")
	createTestMod(t, drive2Mods, "mod-beta", "Skins/Views/Beta.qml", "import QtQuick 2.0\nItem {\n    Text { text: \"Beta\" }\n}\n")

	mgr := NewManager(mediaRoot, uiDir)
	discovered, err := mgr.Discover()
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if len(discovered) != 2 {
		t.Fatalf("expected 2 discovered mods, got %d", len(discovered))
	}

	foundAlpha := false
	foundBeta := false
	for _, m := range discovered {
		if m.ID == "mod-alpha" {
			foundAlpha = true
			if m.Drive != "USB1" || !m.Valid || m.QMLCount != 1 || m.Enabled {
				t.Errorf("unexpected alpha mod details: %+v", m)
			}
		}
		if m.ID == "mod-beta" {
			foundBeta = true
			if m.Drive != "USB2" || !m.Valid || m.QMLCount != 1 || m.Enabled {
				t.Errorf("unexpected beta mod details: %+v", m)
			}
		}
	}
	if !foundAlpha || !foundBeta {
		t.Fatalf("did not find expected mods: alpha=%v, beta=%v", foundAlpha, foundBeta)
	}
}

func TestMods_EnableDisableAndOverlay(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()
	activePackDir := t.TempDir()

	modsDir := filepath.Join(mediaRoot, "DRIVE", "mods")
	createTestMod(t, modsDir, "mod-one", "Skins/Views/One.qml", "Item { Text { text: \"One\" } }\n")

	mgr := NewManager(mediaRoot, uiDir)

	// Enable mod
	if err := mgr.SetModEnabled("mod-one", true); err != nil {
		t.Fatalf("failed to enable mod: %v", err)
	}

	// Verify state file
	state := mgr.loadStateLocked()
	if !state.Mods["mod-one"].Enabled {
		t.Fatalf("expected mod-one to be enabled in state")
	}

	// Apply overlay
	if err := mgr.ApplyActiveOverlay(activePackDir); err != nil {
		t.Fatalf("ApplyActiveOverlay failed: %v", err)
	}

	// Check overlay file exists in uiDir/mods-overlay and activePackDir
	overlayFile := filepath.Join(uiDir, "mods-overlay", "Skins", "Views", "One.qml")
	if b, err := os.ReadFile(overlayFile); err != nil || !strings.Contains(string(b), "One") {
		t.Fatalf("overlay file missing or incorrect content: %v", err)
	}
	activeFile := filepath.Join(activePackDir, "Skins", "Views", "One.qml")
	if b, err := os.ReadFile(activeFile); err != nil || !strings.Contains(string(b), "One") {
		t.Fatalf("active pack file missing or incorrect content: %v", err)
	}

	// Disable mod
	if err := mgr.SetModEnabled("mod-one", false); err != nil {
		t.Fatalf("failed to disable mod: %v", err)
	}

	// Apply overlay should now clean up
	if err := mgr.ApplyActiveOverlay(activePackDir); err != nil {
		t.Fatalf("ApplyActiveOverlay failed: %v", err)
	}
	if _, err := os.Stat(overlayFile); !os.IsNotExist(err) {
		t.Fatalf("expected overlay file to be removed when mod disabled")
	}
	if _, err := os.Stat(activeFile); !os.IsNotExist(err) {
		t.Fatalf("expected active pack file to be cleaned up when mod disabled")
	}
}

func TestMods_Security_PathTraversal(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()
	modsDir := filepath.Join(mediaRoot, "USB", "mods")

	// Case 1: Source path traversal
	dir1 := filepath.Join(modsDir, "bad-src")
	_ = os.MkdirAll(dir1, 0755)
	manifest1 := Manifest{
		ID:       "bad-src",
		QMLFiles: map[string]string{"../../etc/shadow": "Skins/test.qml"},
	}
	b1, _ := json.Marshal(manifest1)
	_ = os.WriteFile(filepath.Join(dir1, "manifest.json"), b1, 0644)

	// Case 2: Destination path traversal
	dir2 := filepath.Join(modsDir, "bad-dst")
	_ = os.MkdirAll(dir2, 0755)
	_ = os.WriteFile(filepath.Join(dir2, "test.qml"), []byte("Item {}\n"), 0644)
	manifest2 := Manifest{
		ID:       "bad-dst",
		QMLFiles: map[string]string{"test.qml": "../../etc/shadow"},
	}
	b2, _ := json.Marshal(manifest2)
	_ = os.WriteFile(filepath.Join(dir2, "manifest.json"), b2, 0644)

	// Case 3: Destination not ending in .qml
	dir3 := filepath.Join(modsDir, "bad-ext")
	_ = os.MkdirAll(dir3, 0755)
	_ = os.WriteFile(filepath.Join(dir3, "test.qml"), []byte("Item {}\n"), 0644)
	manifest3 := Manifest{
		ID:       "bad-ext",
		QMLFiles: map[string]string{"test.qml": "Skins/test.js"},
	}
	b3, _ := json.Marshal(manifest3)
	_ = os.WriteFile(filepath.Join(dir3, "manifest.json"), b3, 0644)

	mgr := NewManager(mediaRoot, uiDir)
	discovered, err := mgr.Discover()
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	for _, m := range discovered {
		if m.Valid {
			t.Errorf("expected mod %s to be invalid, but got valid", m.ID)
		}
		if m.Error == "" {
			t.Errorf("expected mod %s to have error message", m.ID)
		}
		// Attempting to enable invalid mod must fail
		if err := mgr.SetModEnabled(m.ID, true); err == nil {
			t.Errorf("expected SetModEnabled to fail for invalid mod %s", m.ID)
		}
	}
}

func TestMods_Security_SymlinkEscape(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()
	secretDir := t.TempDir()

	secretFile := filepath.Join(secretDir, "secret.qml")
	_ = os.WriteFile(secretFile, []byte("Item { Text { text: \"secret\" } }\n"), 0644)

	modDir := filepath.Join(mediaRoot, "USB", "mods", "symlink-mod")
	_ = os.MkdirAll(modDir, 0755)

	// Symlink pointing outside mod directory
	linkPath := filepath.Join(modDir, "escape.qml")
	if err := os.Symlink(secretFile, linkPath); err != nil {
		t.Skip("symlinks not supported on this filesystem")
	}

	manifest := Manifest{
		ID:       "symlink-mod",
		QMLFiles: map[string]string{"escape.qml": "Skins/escape.qml"},
	}
	b, _ := json.Marshal(manifest)
	_ = os.WriteFile(filepath.Join(modDir, "manifest.json"), b, 0644)

	mgr := NewManager(mediaRoot, uiDir)
	discovered, err := mgr.Discover()
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if len(discovered) != 1 {
		t.Fatalf("expected 1 mod, got %d", len(discovered))
	}
	if discovered[0].Valid {
		t.Fatalf("expected symlink escape mod to be invalid")
	}
	if !strings.Contains(discovered[0].Error, "escapes mod directory") {
		t.Fatalf("expected escape error, got %q", discovered[0].Error)
	}
}

func TestMods_Security_BracketBalancing(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()
	modsDir := filepath.Join(mediaRoot, "USB", "mods")

	cases := []struct {
		name    string
		content string
		valid   bool
	}{
		{"valid_simple", "Item {\n}\n", true},
		{"valid_nested", "Item { Rectangle { width: (10 + 20) } }\n", true},
		{"valid_strings_with_braces", "Item { Text { text: \"Hello (world) {test}\" } }\n", true},
		{"valid_comments_with_braces", "Item {\n // { ( unmatched in single-line comment\n /* ( unmatched in multi-line */\n}\n", true},
		{"invalid_unclosed_brace", "Item {\n", false},
		{"invalid_extra_brace", "Item { }\n}\n", false},
		{"invalid_unbalanced_paren", "Item { (\n}\n", false},
		{"invalid_mismatched", "Item { ( }\n)\n", false},
		{"invalid_empty", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "mod-" + strings.ReplaceAll(tc.name, "_", "-")
			createTestMod(t, modsDir, id, "Skins/"+id+".qml", tc.content)

			mgr := NewManager(mediaRoot, uiDir)
			discovered, err := mgr.Discover()
			if err != nil {
				t.Fatalf("Discover failed: %v", err)
			}
			var target *Mod
			for i := range discovered {
				if discovered[i].ID == id {
					target = &discovered[i]
					break
				}
			}
			if target == nil {
				t.Fatalf("mod %s not found", id)
			}
			if target.Valid != tc.valid {
				t.Errorf("case %s: got valid=%v, want %v (error: %q)", tc.name, target.Valid, tc.valid, target.Error)
			}
		})
	}
}

func TestMods_SafeModeQuarantine(t *testing.T) {
	mediaRoot := t.TempDir()
	uiDir := t.TempDir()
	activePackDir := t.TempDir()

	modsDir := filepath.Join(mediaRoot, "USB", "mods")
	createTestMod(t, modsDir, "quarantine-test", "Skins/Test.qml", "Item {}\n")

	mgr := NewManager(mediaRoot, uiDir)
	if err := mgr.SetModEnabled("quarantine-test", true); err != nil {
		t.Fatalf("failed to enable mod: %v", err)
	}

	if err := mgr.ApplyActiveOverlay(activePackDir); err != nil {
		t.Fatalf("ApplyActiveOverlay failed: %v", err)
	}

	overlayFile := filepath.Join(uiDir, "mods-overlay", "Skins", "Test.qml")
	if _, err := os.Stat(overlayFile); err != nil {
		t.Fatalf("expected overlay file to exist before quarantine: %v", err)
	}

	// Trigger QuarantineAll
	if err := mgr.QuarantineAll(); err != nil {
		t.Fatalf("QuarantineAll failed: %v", err)
	}

	if !mgr.IsSafeModeQuarantined() {
		t.Fatalf("expected IsSafeModeQuarantined() to be true")
	}

	// Verify overlay directory cleaned up
	if _, err := os.Stat(filepath.Join(uiDir, "mods-overlay")); !os.IsNotExist(err) {
		t.Fatalf("expected mods-overlay to be deleted after quarantine")
	}

	// Verify marker file exists
	if _, err := os.Stat(filepath.Join(uiDir, "mods-quarantined")); err != nil {
		t.Fatalf("expected mods-quarantined file to exist: %v", err)
	}

	// Verify Discover reports Quarantined
	discovered, _ := mgr.Discover()
	if len(discovered) != 1 || !discovered[0].Quarantined {
		t.Fatalf("expected discovered mod to be quarantined, got: %+v", discovered)
	}

	// Reset quarantine
	if err := mgr.ResetQuarantine(); err != nil {
		t.Fatalf("ResetQuarantine failed: %v", err)
	}

	if mgr.IsSafeModeQuarantined() {
		t.Fatalf("expected IsSafeModeQuarantined() to be false after reset")
	}

	if _, err := os.Stat(filepath.Join(uiDir, "mods-quarantined")); !os.IsNotExist(err) {
		t.Fatalf("expected mods-quarantined file to be removed after reset")
	}

	// Overlay should be restored since mod was still enabled
	if _, err := os.Stat(overlayFile); err != nil {
		t.Fatalf("expected overlay file to be restored after quarantine reset: %v", err)
	}
}
