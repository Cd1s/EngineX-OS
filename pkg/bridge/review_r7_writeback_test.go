package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

type oneLibCueSnapshot struct {
	cueUpdateCount sql.NullInt64
	rowIDs         []int64
}

func snapshotOneLibCueRows(t *testing.T, onePath string, contentID uint32) oneLibCueSnapshot {
	t.Helper()
	db, err := sql.Open("sqlite", onePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var snap oneLibCueSnapshot
	if err := db.QueryRow(`SELECT cueUpdateCount FROM content WHERE content_id=?`, contentID).Scan(&snap.cueUpdateCount); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT cue_id FROM cue WHERE content_id=? ORDER BY cue_id`, contentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		snap.rowIDs = append(snap.rowIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snap
}

func rewriteSingleANLZVariantCues(t *testing.T, root, rel string, cues []rekordbox.Cue, extended bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	upd, _, err := rekordbox.RewriteANLZCues(raw, cues, extended, false)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, rel, upd)
}

func readSingleANLZCues(t *testing.T, root, rel string) []rekordbox.Cue {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	a, err := rekordbox.ParseAnalysis(raw)
	if err != nil {
		t.Fatal(err)
	}
	return a.Cues
}

func cueSignatureList(cues []rekordbox.Cue) []string {
	sorted := sortedCues(cues)
	out := make([]string, 0, len(sorted))
	for _, c := range sorted {
		out = append(out, fmt.Sprintf("hc=%d loop=%v t=%d..%d", c.HotCue, c.Loop, c.TimeMS, c.EndMS))
	}
	return out
}

func testDATCues(in []rekordbox.Cue) []rekordbox.Cue {
	var out []rekordbox.Cue
	for _, c := range in {
		if c.HotCue > 3 {
			continue
		}
		out = append(out, rekordbox.Cue{
			HotCue: c.HotCue,
			TimeMS: c.TimeMS,
			EndMS:  c.EndMS,
			Loop:   c.Loop,
			Active: c.Active,
		})
	}
	return out
}

func TestReviewR7DistinctUnknownPart(t *testing.T) {
	ctx := context.Background()

	t.Run("oneLibraryExtraPad_quickCuesNull_commentOnly", func(t *testing.T) {
		shared := []rekordbox.Cue{{HotCue: 1, Name: "Shared 1", TimeMS: 800}}
		root, dat, ext, onePath := setupReviewR6Stick(t, shared, false)
		dbDir := filepath.Join(root, "Engine Library", "Database2")

		// Initial settle pass establishes a clean Synced + Written baseline.
		if res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil || res.TracksChanged != 0 {
			t.Fatalf("initial settle: res=%+v err=%v", res, err)
		}

		// Add a second hot cue only in OneLibrary and refresh export stamp + RecordHash.
		oneOnlyCues := []rekordbox.Cue{
			{HotCue: 1, Name: "Shared 1", TimeMS: 800},
			{HotCue: 2, Name: "OneLib Only 2", TimeMS: 1600},
		}
		insertOneLibCues(t, onePath, 1, 1, sql.NullInt64{Valid: true, Int64: 1}, sql.NullInt64{}, oneOnlyCues)
		refreshOnDemandSnapExport(t, root, dbDir)

		readFile := func(rel string) []byte {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
		beforeD, beforeE := readFile(dat), readFile(ext)
		before := readOneLibTrack1(t, onePath)
		beforeRows := snapshotOneLibCueRows(t, onePath, 1)
		if len(before.Cues) != 2 {
			t.Fatalf("expected 2 OneLibrary pads before write-back, got %d", len(before.Cues))
		}

		db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=NULL WHERE trackId=1`, buildTestTrackDataBlob(44100, 650*44100)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET comment='DJ comment only' WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+500 WHERE id=1`); err != nil {
			t.Fatal(err)
		}

		res, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		after := readOneLibTrack1(t, onePath)
		afterRows := snapshotOneLibCueRows(t, onePath, 1)
		if after.Comment != "DJ comment only" {
			t.Errorf("comment = %q, want %q", after.Comment, "DJ comment only")
		}
		if !bytes.Equal(beforeD, readFile(dat)) || !bytes.Equal(beforeE, readFile(ext)) {
			t.Errorf("comment-only edit modified ANLZ files: DAT unchanged=%v EXT unchanged=%v", bytes.Equal(beforeD, readFile(dat)), bytes.Equal(beforeE, readFile(ext)))
		}
		if !reflect.DeepEqual(before.Cues, after.Cues) {
			t.Errorf("NULL quickCues: OneLibrary pads %d -> %d, changed=%d files=%d", len(before.Cues), len(after.Cues), res.TracksChanged, res.FilesUpdated)
		}
		if !reflect.DeepEqual(beforeRows, afterRows) {
			t.Errorf("OneLibrary cue rows/update count touched on metadata-only write-back: before=%+v after=%+v", beforeRows, afterRows)
		}
	})

	t.Run("distinctDAT_EXT_OneLib_quickCuesNull_loopEdit", func(t *testing.T) {
		shared := []rekordbox.Cue{{HotCue: 1, Name: "Shared 1", TimeMS: 800}}
		root, dat, ext, onePath := setupReviewR6Stick(t, shared, false)
		dbDir := filepath.Join(root, "Engine Library", "Database2")

		// Give EXT an extra pad 2, and OneLibrary extra pads 2 and 3, while DAT keeps only pad 1.
		extOrigCues := []rekordbox.Cue{
			{HotCue: 1, Name: "Shared 1", TimeMS: 800},
			{HotCue: 2, Name: "EXT Pad 2", TimeMS: 1600},
		}
		oneOrigCues := []rekordbox.Cue{
			{HotCue: 1, Name: "Shared 1", TimeMS: 800},
			{HotCue: 2, Name: "EXT Pad 2", TimeMS: 1600},
			{HotCue: 3, Name: "OneLib Pad 3", TimeMS: 2400},
		}
		rewriteSingleANLZVariantCues(t, root, ext, extOrigCues, true)
		insertOneLibCues(t, onePath, 1, 1, sql.NullInt64{Valid: true, Int64: 1}, sql.NullInt64{}, oneOrigCues)
		refreshOnDemandSnapExport(t, root, dbDir)

		// Settle the deck baseline once on quickCues/loops before introducing quickCues=NULL + loop edit.
		syncTrackCueBaseline(t, dbDir, 1, "contents/fixturea/track01.mp3")

		db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Deck encodes a saved loop; WriteBack adds trackRekordboxOffset for the LAME MP3 fixture.
		resv, err := compat.NewResolver(root)
		if err != nil {
			t.Fatal(err)
		}
		rbOffset, ok := trackRekordboxOffset(resv, "Contents/FixtureA/Track01.mp3")
		if !ok {
			t.Fatal("missing audio offset for FixtureA Track01")
		}
		loopBase := enginedb.FastBaseForTrack(rekordbox.Track{
			Cues:       []rekordbox.Cue{{HotCue: 0, TimeMS: 5000, EndMS: 7000, Loop: true}},
			Duration:   650,
			SampleRate: 44100,
		})
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, quickCues=NULL, loops=? WHERE trackId=1`, buildTestTrackDataBlob(44100, 650*44100), loopBase.Loops); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+500 WHERE id=1`); err != nil {
			t.Fatal(err)
		}

		if _, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil {
			t.Fatal(err)
		}

		wantLoop := rekordbox.Cue{
			HotCue:  0,
			TimeMS:  deckSamplesToMS(5000*44100/1000.0, rbOffset, 44100),
			EndMS:   deckSamplesToMS(7000*44100/1000.0, rbOffset, 44100),
			Loop:    true,
			ColorID: rekordbox.NearestMemoryColorID([4]byte{255, 255, 127, 0}),
		}
		datAfter := readSingleANLZCues(t, root, dat)
		extAfter := readSingleANLZCues(t, root, ext)
		oneAfter := readOneLibTrack1(t, onePath).Cues

		if !cuesEquivalent(testDATCues(datAfter), testDATCues(append(append([]rekordbox.Cue(nil), shared...), wantLoop))) {
			t.Errorf("DAT cues mismatch: got %v", cueSignatureList(datAfter))
		}
		if !cuesEquivalent(extAfter, append(append([]rekordbox.Cue(nil), extOrigCues...), wantLoop)) {
			t.Errorf("EXT cues mismatch: got %v", cueSignatureList(extAfter))
		}
		if !cuesEquivalent(oneAfter, append(append([]rekordbox.Cue(nil), oneOrigCues...), wantLoop)) {
			t.Errorf("OneLibrary cues mismatch: got %v", cueSignatureList(oneAfter))
		}
	})

	t.Run("distinctDAT_EXT_OneLib_loopsNull_commentOnly_and_padEdit", func(t *testing.T) {
		shared := []rekordbox.Cue{{HotCue: 1, Name: "Shared 1", TimeMS: 800}}
		root, dat, ext, onePath := setupReviewR6Stick(t, shared, false)
		dbDir := filepath.Join(root, "Engine Library", "Database2")

		loop1 := rekordbox.Cue{HotCue: 0, TimeMS: 5000, EndMS: 7000, Loop: true}
		loop2 := rekordbox.Cue{HotCue: 0, TimeMS: 9000, EndMS: 11000, Loop: true}
		extOrigCues := []rekordbox.Cue{shared[0], loop1}
		oneOrigCues := []rekordbox.Cue{shared[0], loop1, loop2}
		rewriteSingleANLZVariantCues(t, root, ext, extOrigCues, true)
		insertOneLibCues(t, onePath, 1, 1, sql.NullInt64{Valid: true, Int64: 1}, sql.NullInt64{}, oneOrigCues)
		refreshOnDemandSnapExport(t, root, dbDir)
		syncTrackCueBaseline(t, dbDir, 1, "contents/fixturea/track01.mp3")

		readFile := func(rel string) []byte {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
		beforeD, beforeE := readFile(dat), readFile(ext)
		beforeOne := readOneLibTrack1(t, onePath)
		beforeRows := snapshotOneLibCueRows(t, onePath, 1)

		db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		// Pass 1: loops=NULL + comment-only edit must touch only metadata columns, keeping every destination's cues intact.
		if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?, loops=NULL WHERE trackId=1`, buildTestTrackDataBlob(44100, 650*44100)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET comment='Loops null comment' WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+500 WHERE id=1`); err != nil {
			t.Fatal(err)
		}

		res1, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		afterOne1 := readOneLibTrack1(t, onePath)
		afterRows1 := snapshotOneLibCueRows(t, onePath, 1)
		if afterOne1.Comment != "Loops null comment" {
			t.Errorf("pass 1 comment = %q, want %q", afterOne1.Comment, "Loops null comment")
		}
		if !bytes.Equal(beforeD, readFile(dat)) || !bytes.Equal(beforeE, readFile(ext)) {
			t.Errorf("pass 1 modified ANLZ files: DAT unchanged=%v EXT unchanged=%v", bytes.Equal(beforeD, readFile(dat)), bytes.Equal(beforeE, readFile(ext)))
		}
		if !reflect.DeepEqual(beforeOne.Cues, afterOne1.Cues) || !reflect.DeepEqual(beforeRows, afterRows1) {
			t.Errorf("pass 1 modified OneLibrary cue rows: cues %d->%d changed=%d rows %+v->%+v",
				len(beforeOne.Cues), len(afterOne1.Cues), res1.TracksChanged, beforeRows, afterRows1)
		}

		// Pass 2: while loops stays NULL, add hot cue pad 2 on deck -> each destination keeps its own unknown loops.
		resv, err := compat.NewResolver(root)
		if err != nil {
			t.Fatal(err)
		}
		rbOffset, ok := trackRekordboxOffset(resv, "Contents/FixtureA/Track01.mp3")
		if !ok {
			t.Fatal("missing audio offset for FixtureA Track01")
		}
		pad2Pos := 2000.0
		if err := enginedb.SetQuickCue(ctx, db, 1, 1, "Deck Pad 2", "#ff0000", &pad2Pos); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+900 WHERE id=1`); err != nil {
			t.Fatal(err)
		}

		if _, err := WriteBackCues(ctx, root, WriteBackOptions{OnEject: true, SkipPreparingCheck: true}); err != nil {
			t.Fatal(err)
		}
		pad2 := rekordbox.Cue{HotCue: 2, Name: "Deck Pad 2", TimeMS: deckSamplesToMS(2000*44100/1000.0, rbOffset, 44100), Color: [4]byte{255, 255, 0, 0}, ColorCode: rekordbox.NearestCueColorCode([4]byte{255, 255, 0, 0})}
		datAfter := readSingleANLZCues(t, root, dat)
		extAfter := readSingleANLZCues(t, root, ext)
		oneAfter := readOneLibTrack1(t, onePath).Cues

		if !cuesEquivalent(testDATCues(datAfter), testDATCues([]rekordbox.Cue{shared[0], pad2})) {
			t.Errorf("pass 2 DAT cues mismatch: got %v", cueSignatureList(datAfter))
		}
		if !cuesEquivalent(extAfter, []rekordbox.Cue{shared[0], pad2, loop1}) {
			t.Errorf("pass 2 EXT cues mismatch: got %v", cueSignatureList(extAfter))
		}
		if !cuesEquivalent(oneAfter, []rekordbox.Cue{shared[0], pad2, loop1, loop2}) {
			t.Errorf("pass 2 OneLibrary cues mismatch: got %v", cueSignatureList(oneAfter))
		}
	})
}
