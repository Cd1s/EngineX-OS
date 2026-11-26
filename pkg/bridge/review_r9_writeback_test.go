package bridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func recolorEXTAnd2EX(t *testing.T, root, extRel, twoExRel string, cues []rekordbox.Cue) {
	t.Helper()
	for i, rel := range []string{extRel, twoExRel} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		rep, _, err := rekordbox.RewriteANLZCues(raw, cues, i == 0 || i == 1, true)
		if err != nil {
			t.Fatal(err)
		}
		testfixture.Write(t, root, rel, rep)
	}
}

func makeSnapshotDigestOnly(t *testing.T, dbDir, key string) {
	t.Helper()
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	tr := snap.Tracks[key]
	if tr == nil || len(tr.Synced) == 0 {
		t.Fatalf("expected non-empty Synced digest for %q, got %+v", key, tr)
	}
	tr.SyncedColorTag = nil
	tr.SyncedHotColors = nil
	tr.SyncedLoopColors = nil
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}
}

func TestReviewR9ColourBaseline(t *testing.T) {
	ctx := context.Background()
	blue := [4]byte{255, 0x30, 0x5a, 0xff}
	red := [4]byte{255, 255, 0, 0}
	wantRed := rekordbox.NearestCueColorCode(red)
	pad1Yellow := engineDefaultPadColors[0]
	key := "contents/fixturea/track01.mp3"

	t.Run("digest_only_move_only_preserves_uncoloured_onelib_and_recoloured_anlz", func(t *testing.T) {
		extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: pad1Yellow, ColorCode: 33}
		oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

		// Settle Pad 1 as yellow, then clear per-pad colour notes to simulate a digest-only snapshot.
		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", pad1Yellow)
		settleFixture(t, root, dbDir, true)
		makeSnapshotDigestOnly(t, dbDir, key)

		// EXT and 2EX are recoloured to blue independently; OneLibrary remains uncoloured.
		blueCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: blue, ColorCode: 1}
		recolorEXTAnd2EX(t, root, extRel, twoExRel, []rekordbox.Cue{blueCue})
		refreshOnDemandSnapExport(t, root, dbDir)

		// Move-only edit on the deck (800 -> 2400 ms, colour stays yellow).
		writeDeckPad1Cue(t, dbDir, rbOffset, 2400, "Shared 1", pad1Yellow)
		if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
			t.Fatalf("move WriteBackCues: res=%+v err=%v", res, err)
		}

		for label, list := range map[string][]rekordbox.Cue{
			"EXT": readSingleANLZCues(t, root, extRel),
			"2EX": readSingleANLZCues(t, root, twoExRel),
		} {
			if len(list) != 1 || !near(list[0].TimeMS, 2400) || list[0].ColorCode != 1 {
				t.Errorf("%s: expected 2400ms palette 1, got %+v", label, list)
			}
		}
		oneCues := readOneLibTrack1(t, onePath).Cues
		if len(oneCues) != 1 || !near(oneCues[0].TimeMS, 2400) || !uncoloured(oneCues[0]) {
			t.Errorf("OneLibrary: expected 2400ms uncoloured (palette 0), got %+v", oneCues)
		}
	})

	for _, mode := range []struct {
		name       string
		digestOnly bool
	}{
		{name: "modern_snapshot", digestOnly: false},
		{name: "digest_only_untouched_destinations", digestOnly: true},
	} {
		t.Run("explicit_edit_propagates/"+mode.name, func(t *testing.T) {
			extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: blue, ColorCode: 1}
			oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
			root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", blue)
			settleFixture(t, root, dbDir, true)
			if mode.digestOnly {
				makeSnapshotDigestOnly(t, dbDir, key)
			}

			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", pad1Yellow)
			if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
				t.Fatalf("explicit colour edit WriteBackCues: res=%+v err=%v", res, err)
			}
			for label, list := range map[string][]rekordbox.Cue{
				"EXT":        readSingleANLZCues(t, root, extRel),
				"2EX":        readSingleANLZCues(t, root, twoExRel),
				"OneLibrary": readOneLibTrack1(t, onePath).Cues,
			} {
				if len(list) != 1 || list[0].ColorCode != 33 {
					t.Errorf("%s: expected palette 33, got %+v", label, list)
				}
			}
		})
	}

	for _, tc := range []struct {
		name   string
		moveMS uint32
	}{
		{name: "colour_only", moveMS: 800},
		{name: "colour_and_move", moveMS: 1600},
	} {
		t.Run("digest_only_independently_recoloured_destinations_keep_own_colour/"+tc.name, func(t *testing.T) {
			extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: blue, ColorCode: 1}
			oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800}
			root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", blue)
			settleFixture(t, root, dbDir, true)
			makeSnapshotDigestOnly(t, dbDir, key)

			// Recolour EXT/2EX independently to red so no destination matches the synced digest.
			redCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: red, ColorCode: wantRed}
			recolorEXTAnd2EX(t, root, extRel, twoExRel, []rekordbox.Cue{redCue})
			refreshOnDemandSnapExport(t, root, dbDir)

			// Deck explicitly recolours blue -> Pad 1 yellow (with or without moving).
			writeDeckPad1Cue(t, dbDir, rbOffset, tc.moveMS, "Shared 1", pad1Yellow)
			res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true})
			if err != nil {
				t.Fatalf("WriteBackCues: err=%v", err)
			}
			wantChanges := 0
			if tc.moveMS != 800 {
				wantChanges = 1
			}
			if res.TracksChanged != wantChanges {
				t.Fatalf("WriteBackCues: got TracksChanged=%d want %d (%+v)", res.TracksChanged, wantChanges, res)
			}
			for label, list := range map[string][]rekordbox.Cue{
				"EXT": readSingleANLZCues(t, root, extRel),
				"2EX": readSingleANLZCues(t, root, twoExRel),
			} {
				if len(list) != 1 || !near(list[0].TimeMS, tc.moveMS) || list[0].ColorCode != wantRed {
					t.Errorf("%s: expected %dms red (%d), got %+v", label, tc.moveMS, wantRed, list)
				}
			}
			oneCues := readOneLibTrack1(t, onePath).Cues
			if len(oneCues) != 1 || !near(oneCues[0].TimeMS, tc.moveMS) || !uncoloured(oneCues[0]) {
				t.Errorf("OneLibrary: expected %dms uncoloured, got %+v", tc.moveMS, oneCues)
			}
		})
	}

	for _, mode := range []struct {
		name       string
		digestOnly bool
	}{
		{name: "modern_snapshot", digestOnly: false},
		{name: "digest_only", digestOnly: true},
	} {
		t.Run("unnamed_uncoloured_explicit_green_to_yellow/"+mode.name, func(t *testing.T) {
			uncol := rekordbox.Cue{HotCue: 1, TimeMS: 800}
			root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, uncol, uncol, true)

			writeDeckPad1Cue(t, dbDir, rbOffset, 800, "", engineDefaultHotCueColor)
			settleFixture(t, root, dbDir, true)
			if mode.digestOnly {
				makeSnapshotDigestOnly(t, dbDir, key)
			}

			// Move keeping green -> all remain uncoloured.
			writeDeckPad1Cue(t, dbDir, rbOffset, 1200, "", engineDefaultHotCueColor)
			if _, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil {
				t.Fatal(err)
			}
			for label, list := range map[string][]rekordbox.Cue{
				"EXT":        readSingleANLZCues(t, root, extRel),
				"2EX":        readSingleANLZCues(t, root, twoExRel),
				"OneLibrary": readOneLibTrack1(t, onePath).Cues,
			} {
				if len(list) != 1 || !near(list[0].TimeMS, 1200) || !uncoloured(list[0]) {
					t.Errorf("%s: unnamed green move gained colour: %+v", label, list)
				}
			}

			if mode.digestOnly {
				makeSnapshotDigestOnly(t, dbDir, key)
			}

			// Explicit edit from green (#00ff00) -> Pad 1 default yellow (#f4d338) must propagate 33.
			writeDeckPad1Cue(t, dbDir, rbOffset, 1200, "", pad1Yellow)
			if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
				t.Fatalf("green->yellow WriteBackCues: res=%+v err=%v", res, err)
			}
			for label, list := range map[string][]rekordbox.Cue{
				"EXT":        readSingleANLZCues(t, root, extRel),
				"2EX":        readSingleANLZCues(t, root, twoExRel),
				"OneLibrary": readOneLibTrack1(t, onePath).Cues,
			} {
				if len(list) != 1 || list[0].ColorCode != 33 {
					t.Errorf("%s: unnamed green->yellow edit lost: %+v", label, list)
				}
			}
		})
	}

	t.Run("move_deck_blue_preserves_independently_yellow_onelib", func(t *testing.T) {
		extCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: blue, ColorCode: 1}
		oneCue := rekordbox.Cue{HotCue: 1, Name: "Shared 1", TimeMS: 800, Color: pad1Yellow, ColorCode: 33}
		root, extRel, twoExRel, onePath, dbDir, rbOffset := setupReviewR8ColourFixture(t, extCue, oneCue, true)

		writeDeckPad1Cue(t, dbDir, rbOffset, 800, "Shared 1", blue)
		settleFixture(t, root, dbDir, true)

		// Move deck-blue cue without changing colour -> OneLibrary must stay yellow (33).
		writeDeckPad1Cue(t, dbDir, rbOffset, 2400, "Shared 1", blue)
		if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 1 {
			t.Fatalf("move WriteBackCues: res=%+v err=%v", res, err)
		}
		for label, list := range map[string][]rekordbox.Cue{
			"EXT": readSingleANLZCues(t, root, extRel),
			"2EX": readSingleANLZCues(t, root, twoExRel),
		} {
			if len(list) != 1 || !near(list[0].TimeMS, 2400) || list[0].ColorCode != 1 {
				t.Errorf("%s: expected 2400ms blue (1), got %+v", label, list)
			}
		}
		oneCues := readOneLibTrack1(t, onePath).Cues
		if len(oneCues) != 1 || !near(oneCues[0].TimeMS, 2400) || oneCues[0].ColorCode != 33 {
			t.Errorf("OneLibrary: expected 2400ms yellow (33), got %+v", oneCues)
		}
	})
}
