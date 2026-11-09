package mods

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testArchive(t *testing.T, files map[string]string, link string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if link != "" {
		if err := tw.WriteHeader(&tar.Header{Name: link, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestArchiveLifecycle(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager("", filepath.Join(base, "ui"))
	var actions []string
	mgr.runScript = func(mod Mod, action string) error { actions = append(actions, mod.ID+":"+action); return nil }
	archive := testArchive(t, map[string]string{"package/mod.json": `{"id":"dmx-studio","script":"run.sh","category":"lighting"}`, "package/run.sh": "exit 0"}, "")
	mod, err := mgr.InstallArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if mod.Enabled || !mod.Valid || mod.Drive != "internal" || mod.Category != "lighting" || len(actions) != 0 {
		t.Fatalf("unexpected mod: %+v %v", mod, actions)
	}
	if err := mgr.SetModEnabled(mod.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetModEnabled(mod.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetModEnabled(mod.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := mgr.UninstallMod(mod.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, ",") != "dmx-studio:start,dmx-studio:stop,dmx-studio:start,dmx-studio:stop" {
		t.Fatal(actions)
	}
	list, err := mgr.Discover()
	if err != nil || len(list) != 0 {
		t.Fatalf("remaining mods: %+v %v", list, err)
	}
}

func TestArchiveRejectsUnsafe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		link  string
	}{
		{"traversal", map[string]string{"../escape": "x"}, ""},
		{"symlink", map[string]string{"mod.json": `{"id":"a","script":"run.sh"}`}, "run.sh"},
		{"invalid manifest", map[string]string{"mod.json": `{"id":"../bad","script":"x"}`}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager("", filepath.Join(t.TempDir(), "ui"))
			if _, err := mgr.InstallArchive(bytes.NewReader(testArchive(t, tc.files, tc.link))); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestScriptEnvironment(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager("", filepath.Join(base, "ui"))
	archive := testArchive(t, map[string]string{
		"mod.json": `{"id":"script-test","script":"run.sh"}`,
		"run.sh":   `printf '%s:%s:%s:%s\n' "$ENGINEX_MOD_ID" "$ENGINEX_MOD_DIR" "$ENGINEX_MOD_ACTION" "$1" >> "$ENGINEX_MOD_DIR/log"`,
	}, "")
	mod, err := mgr.InstallArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetModEnabled(mod.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetModEnabled(mod.ID, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(mod.Dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	want := "script-test:" + mod.Dir + ":start:start\nscript-test:" + mod.Dir + ":stop:stop\n"
	if string(data) != want {
		t.Fatalf("got %q, want %q", data, want)
	}
}

// Every shipped official package must install and validate; tailscale-remote is a built-in system Mod.
func TestOfficialModPackagesInstallAndValidate(t *testing.T) {
	paths, err := filepath.Glob("../../docs/mods/packages/*.tar.gz")
	if err != nil || len(paths) != 5 {
		t.Fatalf("official packages: %v %v", paths, err)
	}
	mgr := NewManager("", filepath.Join(t.TempDir(), "ui"))
	mgr.runScript = func(Mod, string) error { return nil }
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(path)
		if strings.HasPrefix(name, "tailscale-remote-") {
			stage := t.TempDir()
			if err := extractForTest(data, stage); err != nil {
				t.Fatal(err)
			}
			var man Manifest
			raw, err := os.ReadFile(filepath.Join(stage, "mod.json"))
			if err != nil || json.Unmarshal(raw, &man) != nil {
				t.Fatalf("%s manifest: %v", name, err)
			}
			if err := ValidateMod(stage, &man); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, err := mgr.InstallArchive(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "built in") {
				t.Fatalf("%s should be rejected as built in, got %v", name, err)
			}
			continue
		}
		mod, err := mgr.InstallArchive(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !mod.Valid || !mod.Official || mod.Enabled || mod.Drive != "internal" || mod.QMLCount != 1 || mod.Category == "" ||
			name != mod.ID+"-"+mod.Version+".tar.gz" {
			t.Fatalf("%s: unexpected mod %+v", name, mod)
		}
		if err := mgr.SetModEnabled(mod.ID, true); err != nil {
			t.Fatalf("%s enable: %v", name, err)
		}
	}
	list, err := mgr.Discover()
	if err != nil || len(list) != 4 {
		t.Fatalf("installed: %+v %v", list, err)
	}
	for _, m := range list {
		if !m.Valid || !m.Enabled {
			t.Fatalf("after enable: %+v", m)
		}
	}
}

func extractForTest(data []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(h.Name)), b, 0755); err != nil {
			return err
		}
	}
}

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

// A mod that replaces a file of the base UI pack must give the original back when disabled.
func TestMods_DisableRestoresBasePackFile(t *testing.T) {
	mediaRoot, uiDir, active := t.TempDir(), t.TempDir(), t.TempDir()
	createTestMod(t, filepath.Join(mediaRoot, "DRIVE", "mods"), "mod-one", "Skins/One.qml", "Item { Text { text: \"mod\" } }\n")
	target := filepath.Join(active, "Skins", "One.qml")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("BASE"), 0644); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(mediaRoot, uiDir)
	if err := mgr.SetModEnabled("mod-one", true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // second apply must not overwrite the backup with the mod's copy
		if err := mgr.ApplyActiveOverlay(active); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), "mod") {
		t.Fatalf("mod file not applied: %q", b)
	}
	if err := mgr.SetModEnabled("mod-one", false); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ApplyActiveOverlay(active); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "BASE" {
		t.Fatalf("base file not restored: %q", b)
	}
}

func TestInstallArchiveAcceptsPaxGlobalHeader(t *testing.T) {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "abc"}})
	body := `{"id":"gh-mod","script":"run.sh"}`
	_ = tw.WriteHeader(&tar.Header{Name: "mod.json", Mode: 0644, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.WriteHeader(&tar.Header{Name: "run.sh", Mode: 0644, Size: 6})
	_, _ = tw.Write([]byte("exit 0"))
	tw.Close()
	gz.Close()
	mgr := NewManager("", filepath.Join(t.TempDir(), "ui"))
	mgr.runScript = func(Mod, string) error { return nil }
	if _, err := mgr.InstallArchive(&b); err != nil {
		t.Fatal(err)
	}
}

func TestInstallArchiveKeepsExecBitsOnly(t *testing.T) {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		mode int64
		body string
	}{{"mod.json", 0644, `{"id":"bin-mod","script":"start.sh"}`}, {"start.sh", 0644, "exit 0"}, {"server", 04755, "ELF"}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body))})
		_, _ = tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	mgr := NewManager("", filepath.Join(t.TempDir(), "ui"))
	mgr.runScript = func(Mod, string) error { return nil }
	mod, err := mgr.InstallArchive(&b)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(mod.Dir, "server"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0100 == 0 || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("server mode = %v, want executable without setuid", st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(mod.Dir, "start.sh")); st.Mode()&0111 != 0 {
		t.Fatalf("start.sh mode = %v, want not executable", st.Mode())
	}
}

func TestUninstallSurvivesFailingStopAndStartEnabledRuns(t *testing.T) {
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
	actions = nil
	mgr.StartEnabled() // boot: the enabled flag survived, the daemon did not
	if strings.Join(actions, ",") != "d1:start" {
		t.Fatalf("StartEnabled actions: %v", actions)
	}
	fail = true
	if err := mgr.UninstallMod(mod.ID); err != nil {
		t.Fatalf("uninstall must ignore a failing stop: %v", err)
	}
	if list, _ := mgr.Discover(); len(list) != 0 {
		t.Fatalf("mod still present: %+v", list)
	}
}

func TestValidateMissingScriptRejected(t *testing.T) {
	dir := t.TempDir()
	if err := ValidateMod(dir, &Manifest{ID: "x", Script: "nope.sh"}); err == nil {
		t.Fatal("missing script must be rejected")
	}
}
