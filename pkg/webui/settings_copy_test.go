package webui

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSettingsCopyAllowlistAndModel(t *testing.T) {
	source := uiSettings{SpotifySource: "auto", Language: "en", Quality: qualityFLAC, Waveform: "rgb", ShadowMode: "auto", StreamSortRole: "default", StreamSortOrder: "asc", PioneerFx: "off", FxTimeMode: "beats", FxTimeMs: 500, LedBrightness: 100, UIScale: 100, BpmDecimals: 2, FourDeckRole: "13", Scope: "spotify", RelayURL: "https://private.invalid", GitHubRepo: "private/repo", VerboseLog: true, DebugTelemetry: true, UpfaderEcho: true, StemsEqMode: true, BeatJumpPads: true, SdHighSpeed: true}
	payload := copyUIExport(source)
	for _, key := range []string{"fourDeckRole", "scope", "sources", "relayUrl", "githubRepo", "verboseLog", "debugTelemetry", "btOut", "loggedIn"} {
		if _, found := payload[key]; found {
			t.Fatalf("private/identity key exported: %s", key)
		}
	}
	destination := source
	destination.FourDeckRole, destination.Scope = "24", "deezer"
	destination.MasterLimiter = true
	destination.SdHighSpeed = false
	got, skipped, err := copyUIMerge(destination, payload, "SC5000")
	if err != nil {
		t.Fatal(err)
	}
	if got.FourDeckRole != "24" || got.Scope != "deezer" || got.MasterLimiter || got.Waveform != "rgb" || !got.SdHighSpeed {
		t.Fatalf("copy lost identity/local values or false/default settings: %+v", got)
	}
	for _, key := range []string{"upfaderEcho", "stemsEqMode"} {
		if !slices.Contains(skipped, key) {
			t.Errorf("missing model skip %s: %v", key, skipped)
		}
	}
	// The players' Beat Jump pad mode (Shift + LOOP) is copied between players (TestSettingsCopyBeatJumpPadsBetweenPlayers).
	if slices.Contains(skipped, "beatJumpPads") || !got.BeatJumpPads {
		t.Errorf("SC5000 skipped beatJumpPads: %v", skipped)
	}
	if slices.Contains(skipped, "sdHighSpeed") {
		t.Errorf("sdHighSpeed unexpectedly skipped on RK3288 board: %v", skipped)
	}
	az04Got, az04Skipped, err := copyUIMerge(destination, payload, "PRIME4G2")
	if err != nil {
		t.Fatal(err)
	}
	if az04Got.SdHighSpeed || !slices.Contains(az04Skipped, "sdHighSpeed") {
		t.Fatalf("sdHighSpeed must be skipped on AZ04 board: got=%+v skipped=%v", az04Got, az04Skipped)
	}
	for _, tc := range []struct{ key, value string }{{"fourDeckRole", `"13"`}, {"scope", `"tidal"`}, {"uiScale", `88`}, {"waveform", `"unknown"`}, {"masterLimiter", `null`}, {"uiScale", `null`}, {"playhead", `2`}} {
		if _, _, err := copyUIMerge(destination, map[string]json.RawMessage{tc.key: json.RawMessage(tc.value)}, "SC5000"); err == nil {
			t.Errorf("accepted %s=%s", tc.key, tc.value)
		}
	}
}

func TestSettingsCopySnapshotRestoresBytesModeAndAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.conf")
	original := []byte("# preserve comments and ordering\r\nfour_deck_role=24\r\nwaveform=denon\nunknown_local_key=keep\n\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := snapshotCopyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the actual disk JSON round trip used by the private undo record.
	b, _ := json.Marshal(snap)
	var reloaded copyFileSnapshot
	if err := json.Unmarshal(b, &reloaded); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("waveform=rgb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreCopyFile(path, reloaded); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("undo changed bytes: %q, %v", got, err)
	}
	i, _ := os.Stat(path)
	if i.Mode().Perm() != 0o600 {
		t.Fatalf("undo changed permissions: %v", i.Mode())
	}
	missing := filepath.Join(t.TempDir(), "ui-scale.env")
	absent, err := snapshotCopyFile(missing)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(missing, []byte("ENGINEX_UI_SCALE=0.85\n"), 0o644)
	if err := restoreCopyFile(missing, absent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("undo did not restore file absence")
	}
}
