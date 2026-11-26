package bridge

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func setupReviewR8ColourFixture(t *testing.T, ext2exCue rekordbox.Cue, oneCue rekordbox.Cue, use2EX bool) (root, extRel, twoExRel, onePath, dbDir string, rbOffset float64) {
	t.Helper()
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	root, _, extRel, onePath = setupReviewR6Stick(t, []rekordbox.Cue{ext2exCue}, false)
	dbDir = filepath.Join(root, "Engine Library", "Database2")

	if use2EX {
		twoExRel = "PIONEER/USBANLZ/P001/ANLZ0000.2EX"
		raw2EX, err := os.ReadFile(filepath.Join("..", "rekordbox", "testdata", "rb_hot_mem_full.ext"))
		if err != nil {
			t.Fatal(err)
		}
		rep2EX, _, err := rekordbox.RewriteANLZCues(raw2EX, []rekordbox.Cue{ext2exCue}, true, true)
		if err != nil {
			t.Fatal(err)
		}
		testfixture.Write(t, root, twoExRel, rep2EX)
	}

	insertOneLibCues(t, onePath, 1, 1, sql.NullInt64{Valid: true, Int64: 1}, sql.NullInt64{}, []rekordbox.Cue{oneCue})
	refreshOnDemandSnapExport(t, root, dbDir)

	resv, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	var ok bool
	rbOffset, ok = trackRekordboxOffset(resv, "Contents/FixtureA/Track01.mp3")
	if !ok {
		t.Fatal("missing audio offset for FixtureA Track01")
	}
	return root, extRel, twoExRel, onePath, dbDir, rbOffset
}

func writeDeckPad1Cue(t *testing.T, dbDir string, rbOffset float64, ms uint32, name string, rgb [4]byte) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var slots [8]struct {
		name    string
		samples float64
		color   [4]byte
	}
	for i := range slots {
		slots[i].samples = -1
	}
	samples := float64(ms)*44.1 - rbOffset
	if samples < 0 {
		samples = 0
	}
	slots[0] = struct {
		name    string
		samples float64
		color   [4]byte
	}{name: name, samples: samples, color: rgb}

	var prevEdit int64
	_ = db.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=1`).Scan(&prevEdit)
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=? WHERE trackId=1`,
		buildTestTrackDataBlob(44100, 650*44100), buildEngineQuickCuesBlob(slots)); err != nil {
		t.Fatal(err)
	}
	var curEdit int64
	_ = db.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=1`).Scan(&curEdit)
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=? WHERE id=1`, max(prevEdit, curEdit)+500); err != nil {
		t.Fatal(err)
	}
}

func writeDeckPad1HotLoop(t *testing.T, dbDir string, rbOffset float64, startMS, endMS uint32, name string, rgb [4]byte) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var qSlots [8]struct {
		name    string
		samples float64
		color   [4]byte
	}
	for i := range qSlots {
		qSlots[i].samples = -1
	}
	startSamples := float64(startMS)*44.1 - rbOffset
	if startSamples < 0 {
		startSamples = 0
	}
	endSamples := float64(endMS)*44.1 - rbOffset
	if endSamples <= startSamples {
		endSamples = startSamples + 44100
	}
	qSlots[0] = struct {
		name    string
		samples float64
		color   [4]byte
	}{name: name, samples: startSamples, color: rgb}

	// Build a valid Loops blob using exact sample positions compensated by rbOffset.
	basePerf := enginedb.FastBaseForTrack(rekordbox.Track{
		Cues: []rekordbox.Cue{{
			HotCue:    1,
			Name:      name,
			TimeMS:    uint32(startSamples * 1000 / 44100),
			EndMS:     uint32(endSamples * 1000 / 44100),
			Loop:      true,
			Color:     rgb,
			ColorCode: rekordbox.NearestCueColorCode(rgb),
		}},
		Duration:   650,
		SampleRate: 44100,
	})
	var prevEdit int64
	_ = db.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=1`).Scan(&prevEdit)
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=?, loops=? WHERE trackId=1`,
		buildTestTrackDataBlob(44100, 650*44100), buildEngineQuickCuesBlob(qSlots), basePerf.Loops); err != nil {
		t.Fatal(err)
	}
	var curEdit int64
	_ = db.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=1`).Scan(&curEdit)
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=? WHERE id=1`, max(prevEdit, curEdit)+500); err != nil {
		t.Fatal(err)
	}
}

func settleFixture(t *testing.T, root, dbDir string, syncHelperOnly bool) {
	t.Helper()
	if syncHelperOnly {
		syncTrackCueBaseline(t, dbDir, 1, "contents/fixturea/track01.mp3")
		return
	}
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	if tr := snap.Tracks["contents/fixturea/track01.mp3"]; tr != nil {
		tr.Synced = nil
	}
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
	if res, err := WriteBackCues(context.Background(), root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 0 {
		t.Fatalf("settle WriteBackCues: res=%+v err=%v", res, err)
	}
}

func TestReviewR8ColourEdit(t *testing.T) {
	ctx := context.Background()
	blue := [4]byte{255, 0x30, 0x5a, 0xff}
	pad1Yellow := engineDefaultPadColors[0]
	red := [4]byte{255, 255, 0, 0}

	for _, syncMode := range []struct {
		name           string
		syncHelperOnly bool
	}{
		{name: "settled_by_writeback", syncHelperOnly: false},
		{name: "settled_by_sync_helper", syncHelperOnly: true},
	} {
		t.Run("assign-pad-default/"+syncMode.name, func(t *testing.T) {
			extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: blue, ColorCode: 1}
			oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
			root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", blue)
			settleFixture(t, root, dbDir, syncMode.syncHelperOnly)

			// Edit ONLY Pad 1's colour from blue -> Pad 1 default yellow (#f4d338, palette 33).
			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", pad1Yellow)
			if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
				t.Fatalf("colour edit WriteBackCues: res=%+v err=%v", res, err)
			}

			extCues := readSingleANLZCues(t, root, extRel)
			twoExCues := readSingleANLZCues(t, root, twoExRel)
			oneCues := readOneLibTrack1(t, onePath).Cues
			for label, list := range map[string][]rekordbox.Cue{"EXT": extCues, "2EX": twoExCues, "OneLibrary": oneCues} {
				if len(list) != 1 || list[0].ColorCode != 33 {
					t.Errorf("%s: selected RGB %v got cues %+v (want palette 33)", label, pad1Yellow, list)
				}
			}

			// Re-run with no edits -> 0 changes.
			if res2, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res2.TracksChanged != 0 || res2.FilesUpdated != 0 {
				t.Fatalf("second WriteBackCues: res=%+v err=%v", res2, err)
			}
		})
	}

	for _, colCase := range []struct {
		name string
		rgb  [4]byte
		code byte
	}{
		{name: "blue_1", rgb: blue, code: 1},
		{name: "yellow_33", rgb: pad1Yellow, code: 33},
	} {
		t.Run("unchanged-colour-preserves-uncoloured-onelibrary/"+colCase.name, func(t *testing.T) {
			extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: colCase.rgb, ColorCode: colCase.code}
			oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
			root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", colCase.rgb)
			settleFixture(t, root, dbDir, false)

			// Move Pad 1 without changing colour.
			writeDeckPad1Cue(t, dbDir, rbOffset, 2400, "Shared 1", colCase.rgb)
			if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
				t.Fatalf("move WriteBackCues: res=%+v err=%v", res, err)
			}

			extCues := readSingleANLZCues(t, root, extRel)
			twoExCues := readSingleANLZCues(t, root, twoExRel)
			oneCues := readOneLibTrack1(t, onePath).Cues
			if len(extCues) != 1 || !near(extCues[0].TimeMS, 2400) || extCues[0].ColorCode != colCase.code {
				t.Errorf("EXT lost colour or time: %+v", extCues)
			}
			if len(twoExCues) != 1 || !near(twoExCues[0].TimeMS, 2400) || twoExCues[0].ColorCode != colCase.code {
				t.Errorf("2EX lost colour or time: %+v", twoExCues)
			}
			if len(oneCues) != 1 || !near(oneCues[0].TimeMS, 2400) || !uncoloured(oneCues[0]) {
				t.Errorf("OneLibrary gained colour or lost move: %+v", oneCues)
			}
		})
	}

	t.Run("all-uncoloured-move-and-explicit-edits", func(t *testing.T) {
		uncol := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, uncol, uncol, true)

		// Settle with Engine default green (#00ff00), then move keeping #00ff00 -> all remain uncoloured.
		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", engineDefaultHotCueColor)
		settleFixture(t, root, dbDir, false)

		writeDeckPad1Cue(t, dbDir, rbOffset, 1600, "Shared 1", engineDefaultHotCueColor)
		if _, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil {
			t.Fatal(err)
		}
		for label, list := range map[string][]rekordbox.Cue{
			"EXT":        readSingleANLZCues(t, root, extRel),
			"2EX":        readSingleANLZCues(t, root, twoExRel),
			"OneLibrary": readOneLibTrack1(t, onePath).Cues,
		} {
			if len(list) != 1 || !near(list[0].TimeMS, 1600) || !uncoloured(list[0]) {
				t.Errorf("%s: moved uncoloured cue gained colour: %+v", label, list)
			}
		}

		// Explicit colour edit #00ff00 -> Pad 1 default yellow (#f4d338) propagates palette 33 everywhere.
		writeDeckPad1Cue(t, dbDir, rbOffset, 1600, "Shared 1", pad1Yellow)
		if _, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil {
			t.Fatal(err)
		}
		for label, list := range map[string][]rekordbox.Cue{
			"EXT":        readSingleANLZCues(t, root, extRel),
			"2EX":        readSingleANLZCues(t, root, twoExRel),
			"OneLibrary": readOneLibTrack1(t, onePath).Cues,
		} {
			if len(list) != 1 || list[0].ColorCode != 33 {
				t.Errorf("%s: explicit pad1-default edit lost: %+v", label, list)
			}
		}

		// Explicit red edit propagates everywhere.
		writeDeckPad1Cue(t, dbDir, rbOffset, 1600, "Shared 1", red)
		if _, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil {
			t.Fatal(err)
		}
		wantRed := rekordbox.NearestCueColorCode(red)
		for label, list := range map[string][]rekordbox.Cue{
			"EXT":        readSingleANLZCues(t, root, extRel),
			"2EX":        readSingleANLZCues(t, root, twoExRel),
			"OneLibrary": readOneLibTrack1(t, onePath).Cues,
		} {
			if len(list) != 1 || list[0].ColorCode != wantRed {
				t.Errorf("%s: explicit red edit got %+v, want %d", label, list, wantRed)
			}
		}
	})

	t.Run("hot-loop-assign-pad-default", func(t *testing.T) {
		extLoop := rekordbox.Cue{HotCue: 1, Name: "Loop 1", TimeMS: 800, EndMS: 2800, Loop: true, Color: blue, ColorCode: 1}
		oneLoop := rekordbox.Cue{HotCue: 1, Name: "Loop 1", TimeMS: 800, EndMS: 2800, Loop: true}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extLoop, oneLoop, true)

		writeDeckPad1HotLoop(t, dbDir, rbOffset, 800, 2800, "Loop 1", blue)
		settleFixture(t, root, dbDir, false)

		writeDeckPad1HotLoop(t, dbDir, rbOffset, 800, 2800, "Loop 1", pad1Yellow)
		if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
			t.Fatalf("hot-loop colour edit WriteBackCues: res=%+v err=%v", res, err)
		}
		for label, list := range map[string][]rekordbox.Cue{
			"EXT":        readSingleANLZCues(t, root, extRel),
			"2EX":        readSingleANLZCues(t, root, twoExRel),
			"OneLibrary": readOneLibTrack1(t, onePath).Cues,
		} {
			if len(list) != 1 || !list[0].Loop || list[0].ColorCode != 33 {
				t.Errorf("%s: hot loop colour edit got %+v (want palette 33)", label, list)
			}
		}
	})
}
