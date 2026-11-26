package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestReviewR4StockStickDoesNotArmFallbackRescan(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Engine Library", "Database2"), 0o700); err != nil {
		t.Fatal(err)
	}
	var scans atomic.Int32
	w := &CueWatcher{
		mountEvents:    make(chan struct{}),
		rescanFallback: 15 * time.Millisecond,
		mounts: func() []string {
			scans.Add(1)
			return []string{root}
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := scans.Load(); n != 1 {
		t.Fatalf("idle stock stick caused %d scans, want only the initial rescan", n)
	}
}

func TestReviewR4UnresolvedMountReadinessWindowExpiresAndLateExportNeedsEvent(t *testing.T) {
	t.Cleanup(SetCueRunDirForTest(t.TempDir()))

	t.Run("ExpiryStopsFallbackAndNeedsNotifyOrMountEvent", func(t *testing.T) {
		root := t.TempDir()
		evCh := make(chan struct{}, 2)
		var scans atomic.Int32
		w := &CueWatcher{
			quiet:           20 * time.Millisecond,
			retry:           50 * time.Millisecond,
			poll:            20 * time.Millisecond,
			rescanFallback:  15 * time.Millisecond,
			readinessWindow: 50 * time.Millisecond,
			ejectDir:        cueEjectingDir,
			useMountWatch:   true,
			mountEvents:     evCh,
			mounts: func() []string {
				scans.Add(1)
				return []string{root}
			},
			pass: WriteBackCues,
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = w.Run(ctx)
		}()
		defer func() {
			cancel()
			<-done
		}()

		// Wait until readiness window expires and fallback polling stops.
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if scans.Load() >= 2 && !w.hasOtherMounts() {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if w.hasOtherMounts() || scans.Load() < 2 {
			t.Fatalf("readiness window did not expire cleanly: hasOtherMounts=%v scans=%d", w.hasOtherMounts(), scans.Load())
		}
		afterExpiry := scans.Load()
		time.Sleep(80 * time.Millisecond)
		if got := scans.Load(); got != afterExpiry {
			t.Fatalf("fallback rescan continued after readiness expiry: %d -> %d", afterExpiry, got)
		}

		// Late export created without mount/notify event stays unnoticed while idle.
		testfixture.LargeExport(t, root, 1)
		time.Sleep(60 * time.Millisecond)
		if w.hasSticks() || scans.Load() != afterExpiry {
			t.Fatalf("unexpected rescan or attach without event: hasSticks=%v scans=%d (want %d)", w.hasSticks(), scans.Load(), afterExpiry)
		}

		// Explicit eligibility notification attaches immediately.
		w.Notify()
		if !WaitWatchingForTest(w, root, 2*time.Second) {
			t.Fatal("Notify did not attach late export after readiness window expired")
		}
	})

	t.Run("NewMountIdentityGetsFreshReadinessWindow", func(t *testing.T) {
		root := t.TempDir()
		evCh := make(chan struct{}, 2)
		var idMu sync.Mutex
		mountID := "mid:1:8:1:/:initial"
		var scans atomic.Int32
		w := &CueWatcher{
			quiet:           20 * time.Millisecond,
			retry:           50 * time.Millisecond,
			poll:            20 * time.Millisecond,
			rescanFallback:  15 * time.Millisecond,
			readinessWindow: 120 * time.Millisecond,
			ejectDir:        cueEjectingDir,
			useMountWatch:   true,
			mountEvents:     evCh,
			mountsInfo: func() []mountEntry {
				scans.Add(1)
				idMu.Lock()
				id := mountID
				idMu.Unlock()
				return []mountEntry{{path: root, id: id}}
			},
			pass: WriteBackCues,
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = w.Run(ctx)
		}()
		defer func() {
			cancel()
			<-done
		}()

		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if scans.Load() >= 2 && !w.hasOtherMounts() {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if w.hasOtherMounts() {
			t.Fatalf("initial mount did not settle after readiness expiry")
		}

		// Coalesced remount at same path with new mount ID gets a fresh readiness window.
		idMu.Lock()
		mountID = "mid:2:8:2:/:remounted"
		idMu.Unlock()
		evCh <- struct{}{}
		deadline = time.Now().Add(time.Second)
		for time.Now().Before(deadline) && !w.hasOtherMounts() {
			time.Sleep(5 * time.Millisecond)
		}
		if !w.hasOtherMounts() {
			t.Fatal("new mount identity did not receive a fresh readiness window")
		}
		testfixture.LargeExport(t, root, 1)
		if !WaitWatchingForTest(w, root, 2*time.Second) {
			t.Fatal("delayed export on remounted identity was not discovered in fresh readiness window")
		}
	})
}
