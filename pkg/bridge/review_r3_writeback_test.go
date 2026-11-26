package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestReviewR3FreshCoincidentDeckCuesSurvive(t *testing.T) {
	env := setupPhantomTestStick(t, nil)
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, map[int]uint32{0: 60, 1: 60})

	db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+5 WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	if _, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err := compat.NewResolver(env.root)
	if err != nil {
		t.Fatal(err)
	}
	got := readANLZCues(r, env.dat1Rel)
	if len(got) != 2 || got[0].TimeMS != 60 || got[1].TimeMS != 60 {
		t.Fatalf("two new deck hot cues at 60 ms must survive and export, got %+v", got)
	}
	lib, err := rekordbox.ReadSQLiteWhole(context.Background(), env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Tracks) == 0 || len(lib.Tracks[0].Cues) != 2 {
		t.Fatalf("two new deck hot cues missing from OneLibrary: %+v", lib.Tracks)
	}
}

func TestReviewR3CueLayoutRepairPreservesLaterGridAndMarks(t *testing.T) {
	env := setupPhantomTestStick(t, nil)
	cues := []rekordbox.Cue{{HotCue: 1, TimeMS: 25}, {HotCue: 2, TimeMS: 25}}
	dat, ext := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, cues)
	wantBeats := []rekordbox.Beat{{Number: 1, TimeMS: 120, BPM: 120}, {Number: 2, TimeMS: 620, BPM: 120}}
	dat, _, err := rekordbox.RewriteANLZGrid(dat, wantBeats, false)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, env.root, env.dat1Rel, dat)
	testfixture.Write(t, env.root, env.ext1Rel, ext)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, cues)
	refreshPhantomSnapStamp(t, env)

	res, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if res.TracksChanged != 1 || res.FilesUpdated != 2 || res.OneLibraryUpdated {
		t.Fatalf("expected only DAT+EXT cue-section repair (no OneLibrary rewrite), got %+v", res)
	}
	gotDAT, err := os.ReadFile(env.dat1Path)
	if err != nil {
		t.Fatal(err)
	}
	if rekordbox.ANLZNeedsCueRepair(gotDAT, false) {
		t.Fatal("repaired DAT still needs cue repair")
	}
	if bytes.Equal(gotDAT, env.origDAT) {
		t.Fatal("cue repair rolled back DAT to whole backup")
	}
	a, err := rekordbox.ParseAnalysis(gotDAT)
	if err != nil {
		t.Fatal(err)
	}
	if !beatsEquivalent(a.Beats, wantBeats, 0.01, 0.01) {
		t.Fatalf("cue repair clobbered later beat grid: got %d beats, want %d", len(a.Beats), len(wantBeats))
	}
	oneDB, err := sql.Open("sqlite", env.onePath)
	if err != nil {
		t.Fatal(err)
	}
	defer oneDB.Close()
	var mod int
	var upd sql.NullInt64
	if err := oneDB.QueryRow(`SELECT COALESCE(hasModified,0), cueUpdateCount FROM content WHERE content_id=1`).Scan(&mod, &upd); err != nil {
		t.Fatal(err)
	}
	if mod != 1 || !upd.Valid || upd.Int64 != 1 {
		t.Fatalf("layout repair unexpectedly altered OneLibrary marks: hasModified=%d cueUpdateCount=%v", mod, upd)
	}
}

func TestReviewR3WriteBackNeverMutatesMDBAndLeavesPhantomSetUntouched(t *testing.T) {
	env := setupPhantomTestStick(t, nil)
	var eight []rekordbox.Cue
	mdbPads := map[int]uint32{}
	for pad := 0; pad < 8; pad++ {
		eight = append(eight, rekordbox.Cue{HotCue: uint32(pad + 1), TimeMS: 23, Name: "Cue", ColorCode: byte(pad + 1)})
		mdbPads[pad] = 23
	}
	brokenDAT, brokenEXT := makeSwappedPairFromCues(t, env.origDAT, env.origEXT, eight)
	testfixture.Write(t, env.root, env.dat1Rel, brokenDAT)
	testfixture.Write(t, env.root, env.ext1Rel, brokenEXT)
	insertOneLibCues(t, env.onePath, 1, 1, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{}, eight)
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, mdbPads)

	// Align snapshot Edit with m.db's current lastEditTime (modeling an already-imported stick
	// with matching 8x23ms phantom state before write-back checks repair eligibility).
	mdbPath := filepath.Join(env.dbDir, "m.db")
	mdbConn, err := sql.Open("sqlite", mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	var edit int64
	if err := mdbConn.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=1`).Scan(&edit); err != nil {
		_ = mdbConn.Close()
		t.Fatal(err)
	}
	_ = mdbConn.Close()
	snapInit, err := rbstate.Load(env.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	snapInit.Tracks["contents/fixturea/track01.mp3"].Edit = edit
	if err := snapInit.Save(env.dbDir); err != nil {
		t.Fatal(err)
	}
	refreshPhantomSnapStamp(t, env)

	mdbBefore, err := os.ReadFile(mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	mdbSHA := sha256.Sum256(mdbBefore)

	res, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{})
	if err != nil {
		t.Fatalf("WriteBackCues: %v", err)
	}
	if res.TracksChanged != 0 || res.FilesUpdated != 0 || res.OneLibraryUpdated {
		t.Fatalf("write-back touched phantom set or OneLibrary: %+v", res)
	}
	mdbAfter, err := os.ReadFile(mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(mdbAfter) != mdbSHA {
		t.Fatal("write-back mutated m.db bytes")
	}
	if cur, _ := os.ReadFile(env.dat1Path); !bytes.Equal(cur, brokenDAT) {
		t.Fatal("phantom set DAT was modified")
	}
	if cur, _ := os.ReadFile(env.ext1Path); !bytes.Equal(cur, brokenEXT) {
		t.Fatal("phantom set EXT was modified")
	}
	snap, err := rbstate.Load(env.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Header.RepairStamp == "" {
		t.Fatal("expected RepairStamp stamped after completing repair check")
	}
}
