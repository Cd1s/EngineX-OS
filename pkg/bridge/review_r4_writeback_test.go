package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestReviewR4LegacyPhantomSetSurvivesMetadataEdit(t *testing.T) {
	env := setupPhantomTestStick(t, nil)
	var cues []rekordbox.Cue
	for pad := uint32(1); pad <= 8; pad++ {
		cues = append(cues, rekordbox.Cue{HotCue: pad, TimeMS: 25})
	}
	dat, ext := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, cues)
	testfixture.Write(t, env.root, env.dat1Rel, dat)
	testfixture.Write(t, env.root, env.ext1Rel, ext)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, cues)
	refreshPhantomSnapStamp(t, env)

	path := filepath.Join(env.dbDir, "m.db")
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, nil)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE Track SET comment='DJ changed only metadata' WHERE id=1"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+999 WHERE id=1"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{})
	if err != nil || res.TracksChanged != 1 {
		t.Fatalf("metadata edit was not written: %+v %v", res, err)
	}
	r, err := compat.NewResolver(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if got := readANLZCues(r, env.dat1Rel); len(got) != len(cues) {
		t.Errorf("a metadata edit caused export to drop the legacy eight-pad set, got %d cues", len(got))
	}
	one, err := rekordbox.ReadSQLiteWhole(context.Background(), env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	var tr1 *rekordbox.Track
	for i := range one.Tracks {
		if one.Tracks[i].ID == 1 {
			tr1 = &one.Tracks[i]
			break
		}
	}
	if tr1 == nil {
		t.Fatal("missing Track 1 in OneLibrary")
	}
	if tr1.Comment != "DJ changed only metadata" {
		t.Errorf("OneLibrary comment = %q, want %q", tr1.Comment, "DJ changed only metadata")
	}
	if len(tr1.Cues) != len(cues) {
		t.Errorf("OneLibrary dropped legacy eight-pad cues: got %d, want %d", len(tr1.Cues), len(cues))
	}
}

func TestReviewR4LegacyPhantomSetSurvivesGridAndMetadataEdit(t *testing.T) {
	env := setupPhantomTestStick(t, nil)
	var cues []rekordbox.Cue
	for pad := uint32(1); pad <= 8; pad++ {
		cues = append(cues, rekordbox.Cue{HotCue: pad, TimeMS: 25})
	}
	dat, ext := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, cues)
	testfixture.Write(t, env.root, env.dat1Rel, dat)
	testfixture.Write(t, env.root, env.ext1Rel, ext)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, cues)
	refreshPhantomSnapStamp(t, env)

	const sr = 44100.0
	step := 60.0 * sr / 126.0
	defGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{{0, 0, 16}, {16 * 20671.875, 16, 0}}
	adjGrid := []struct {
		pos      float64
		idx      int64
		numBeats uint32
	}{{2205.0, 0, 16}, {2205.0 + 16*step, 16, 0}}

	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, nil)
	db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET beatData=? WHERE trackId=1`, buildEngineBeatDataBlob(sr, 20*sr, defGrid, adjGrid, false)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET comment='Grid and comment edit' WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+999 WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	res, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{})
	if err != nil || res.TracksChanged != 1 {
		t.Fatalf("grid+metadata edit failed: %+v %v", res, err)
	}
	r, err := compat.NewResolver(env.root)
	if err != nil {
		t.Fatal(err)
	}
	if got := readANLZCues(r, env.dat1Rel); len(got) != len(cues) {
		t.Fatalf("grid+metadata edit dropped legacy eight-pad ANLZ cues: got %d", len(got))
	}
	beats := readANLZBeats(r, env.dat1Rel)
	if len(beats) == 0 || dominantBeatBPM(beats) != 126.0 {
		t.Fatalf("updated BPM 126.00 was not written to ANLZ: len=%d bpm=%.2f", len(beats), dominantBeatBPM(beats))
	}
	one, err := rekordbox.ReadSQLiteWhole(context.Background(), env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range one.Tracks {
		if tr.ID == 1 {
			if tr.Comment != "Grid and comment edit" || len(tr.Cues) != len(cues) || math.Abs(tr.BPM-126.0) > 0.01 {
				t.Fatalf("OneLibrary mismatch after grid+metadata edit: comment=%q cues=%d bpm=%.2f", tr.Comment, len(tr.Cues), tr.BPM)
			}
			return
		}
	}
	t.Fatal("missing Track 1 in OneLibrary")
}

func TestReviewR4MetadataOnlyEditNeverRewritesANLZCues(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dat1Rel, ext1Rel, _, _ := seedRealRekordboxOneLibraryStick(t, root)
	dat1Path := filepath.Join(root, filepath.FromSlash(dat1Rel))
	ext1Path := filepath.Join(root, filepath.FromSlash(ext1Rel))
	beforeDAT, err := os.ReadFile(dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	beforeEXT, err := os.ReadFile(ext1Path)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an adopted track baseline (Synced != nil, Written == nil) where only cues/loops/tags
	// were synced, and ANLZ carries the cues while OneLibrary cue rows differ or are absent.
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	snap.Tracks["contents/fixturea/track01.mp3"].Written = nil
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}

	// Settle once on untouched Track 1 so Written stays empty and Synced is recorded.
	if _, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true}); err != nil {
		t.Fatal(err)
	}

	mdb, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET comment='Metadata only edit' WHERE id=1`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+55 WHERE id=1`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	_ = mdb.Close()

	res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("metadata-only write-back: %v", err)
	}
	if res.TracksChanged != 1 || res.FilesUpdated != 1 || !res.OneLibraryUpdated {
		t.Fatalf("expected only 1 file updated (OneLibrary) on metadata-only edit, got %+v", res)
	}
	if len(res.Changes) != 1 || len(res.Changes[0].Fields) != 1 {
		t.Fatalf("unexpected diff fields for metadata-only edit: %+v", res.Changes)
	}
	afterDAT, _ := os.ReadFile(dat1Path)
	afterEXT, _ := os.ReadFile(ext1Path)
	if !bytes.Equal(beforeDAT, afterDAT) || !bytes.Equal(beforeEXT, afterEXT) {
		t.Fatal("metadata-only edit rewrote ANLZ files")
	}
}
