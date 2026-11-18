package webui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// #172: Hi-Fi output compensation is default OFF, persisted as hifi_output=1 on
// Gen-1 standalone players (JP07/JP08/JP13/JP14), and suppressed on all-in-ones.
func TestHifiOutputSettingAndBoardGate(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(New(Config{UIDir: dir, StateDir: t.TempDir()}, nil))
	defer srv.Close()
	conf := func() []string {
		b, _ := os.ReadFile(filepath.Join(dir, "settings.conf"))
		return splitLines(b)
	}

	asDeck(t, "inmusic,jp07")
	if _, v := uiPost(t, srv.URL, ""); v.HifiOutput || !v.Player {
		t.Fatalf("JP07 default must be player=true hifiOutput=false: %+v", v)
	}
	if code, v := uiPost(t, srv.URL, "hifiOutput=1"); code != 200 || !v.HifiOutput {
		t.Fatalf("JP07 hifiOutput=1: code=%d hifiOutput=%v", code, v.HifiOutput)
	}
	if l := conf(); !slices.Contains(l, "hifi_output=1") {
		t.Fatalf("JP07 hifiOutput=1 missing from settings.conf: %q", l)
	}
	if code, v := uiPost(t, srv.URL, "hifiOutput=0"); code != 200 || v.HifiOutput {
		t.Fatalf("JP07 hifiOutput=0: code=%d hifiOutput=%v", code, v.HifiOutput)
	}
	if l := conf(); slices.Contains(l, "hifi_output=1") {
		t.Fatalf("JP07 hifiOutput=0 left hifi_output=1 in settings.conf: %q", l)
	}
	for _, bad := range []string{"hifiOutput=2", "hifiOutput=yes"} {
		if code, _ := uiPost(t, srv.URL, bad); code != 400 {
			t.Errorf("%s: got %d, want 400", bad, code)
		}
	}

	// All-in-one consoles never report or persist hifi_output=1.
	asDeck(t, "inmusic,jc11")
	if code, v := uiPost(t, srv.URL, "hifiOutput=1"); code != 200 || v.HifiOutput || v.Player {
		t.Fatalf("JC11 hifiOutput=1 must stay off: code=%d player=%v hifiOutput=%v", code, v.Player, v.HifiOutput)
	}
	if l := conf(); slices.Contains(l, "hifi_output=1") {
		t.Fatalf("JC11 persisted hifi_output=1: %q", l)
	}

	// Settings copy: transferred to SC5000/SC6000, skipped on PRIME4.
	src := uiSettings{SpotifySource: "auto", Language: "en", Quality: qualityFLAC, Waveform: "rgb", ShadowMode: "auto", StreamSortRole: "default", StreamSortOrder: "asc", PioneerFx: "off", FxTimeMode: "beats", FxTimeMs: 500, LedBrightness: 100, UIScale: 100, BpmDecimals: 2, HifiOutput: true}
	payload := copyUIExport(src)
	gotPlayer, skippedPlayer, err := copyUIMerge(uiSettings{}, payload, "SC6000")
	if err != nil || !gotPlayer.HifiOutput || slices.Contains(skippedPlayer, "hifiOutput") {
		t.Fatalf("SC6000 copy merge: err=%v hifi=%v skipped=%v", err, gotPlayer.HifiOutput, skippedPlayer)
	}
	gotConsole, skippedConsole, err := copyUIMerge(uiSettings{}, payload, "PRIME4")
	if err != nil || gotConsole.HifiOutput || !slices.Contains(skippedConsole, "hifiOutput") {
		t.Fatalf("PRIME4 copy merge: err=%v hifi=%v skipped=%v", err, gotConsole.HifiOutput, skippedConsole)
	}
}
