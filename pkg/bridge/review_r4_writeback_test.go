package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
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

func TestReviewR4WatcherReadsCommittedWAL(t *testing.T) {
	t.Run("SettledPass", func(t *testing.T) {
		root := t.TempDir()
		seedRealRekordboxOneLibraryStick(t, root)
		dir := filepath.Join(root, "Engine Library", "Database2")
		path := filepath.Join(dir, "m.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		var mode string
		if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("WAL mode = %q, err %v", mode, err)
		}
		for _, stmt := range []string{"PRAGMA wal_autocheckpoint=0", "PRAGMA wal_checkpoint(TRUNCATE)"} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
		header := func() [2]uint32 {
			t.Helper()
			raw, err := os.ReadFile(path)
			if err != nil || len(raw) < 100 {
				t.Fatalf("read SQLite header: %v", err)
			}
			return [2]uint32{binary.BigEndian.Uint32(raw[24:28]), binary.BigEndian.Uint32(raw[92:96])}
		}
		s := &CueSession{}
		ctx := context.Background()
		if _, err := WriteBackCues(ctx, root, WriteBackOptions{Session: s}); err != nil {
			t.Fatal(err)
		}
		before := header()
		if _, err := db.Exec("UPDATE Track SET comment='Committed WAL edit' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+999 WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if after := header(); after != before {
			t.Fatalf("uncheckpointed WAL commit changed main header: %v -> %v", before, after)
		}
		wal, err := os.Stat(path + "-wal")
		if err != nil || wal.Size() == 0 {
			t.Fatalf("missing committed WAL: %v", err)
		}
		if _, ok := readMDBCommitIdent(dir); ok {
			t.Error("nonempty WAL must invalidate cached dirty scan")
		}
		res, err := WriteBackCues(ctx, root, WriteBackOptions{Session: s, Settled: func() error { return nil }})
		watcherChanged := res.TracksChanged
		if err != nil || watcherChanged != 1 {
			t.Fatalf("watcher missed committed WAL edit: tracks read=%d changed=%d err=%v", res.TracksRead, res.TracksChanged, err)
		}
		one, err := rekordbox.ReadSQLiteWhole(ctx, filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db"))
		if err != nil {
			t.Fatal(err)
		}
		foundComment := false
		for _, tr := range one.Tracks {
			if tr.ID == 1 && tr.Comment == "Committed WAL edit" {
				foundComment = true
				break
			}
		}
		if !foundComment {
			t.Fatalf("OneLibrary did not receive committed WAL edit: %+v", one.Tracks)
		}
		res, err = WriteBackCues(ctx, root, WriteBackOptions{OnEject: true})
		if err != nil || res.TracksChanged != 0 {
			t.Fatalf("eject control saw leftover WAL edit after watcher: tracks read=%d changed=%d err=%v", res.TracksRead, res.TracksChanged, err)
		}
	})

	t.Run("LiveCueWatcherLoop", func(t *testing.T) {
		t.Cleanup(SetCueRunDirForTest(t.TempDir()))
		root := t.TempDir()
		seedRealRekordboxOneLibraryStick(t, root)
		dir := filepath.Join(root, "Engine Library", "Database2")
		path := filepath.Join(dir, "m.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE);`); err != nil {
			t.Fatal(err)
		}

		evCh := make(chan struct{}, 2)
		w := &CueWatcher{
			quiet:          25 * time.Millisecond,
			retry:          50 * time.Millisecond,
			poll:           20 * time.Millisecond,
			rescanFallback: 10 * time.Second,
			ejectDir:       cueEjectingDir,
			useMountWatch:  true,
			mountEvents:    evCh,
			mounts:         func() []string { return []string{root} },
			pass:           WriteBackCues,
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
		if !WaitWatchingForTest(w, root, 2*time.Second) {
			t.Fatal("watcher did not attach WAL stick")
		}

		if _, err := db.Exec("UPDATE Track SET comment='Live Watcher WAL' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+888 WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		onePath := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
		deadline := time.Now().Add(3 * time.Second)
		synced := false
		for time.Now().Before(deadline) {
			one, rerr := rekordbox.ReadSQLiteWhole(context.Background(), onePath)
			if rerr == nil {
				for _, tr := range one.Tracks {
					if tr.ID == 1 && tr.Comment == "Live Watcher WAL" {
						synced = true
						break
					}
				}
			}
			if synced {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !synced {
			t.Fatal("live CueWatcher missed committed uncheckpointed WAL edit")
		}
	})
}

func TestReviewR4WALStatErrorMarksNotClean(t *testing.T) {
	// Passing a regular file as dbDir causes os.Stat(filepath.Join(dbDir, "m.db-wal"))
	// to fail with ENOTDIR (non-ENOENT regardless of effective UID).
	notDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasLiveWAL(notDir) {
		t.Fatal("hasLiveWAL returned false on unexpected stat error (ENOTDIR)")
	}
	if _, ok := readMDBCommitIdent(notDir); ok {
		t.Fatal("readMDBCommitIdent treated non-ENOENT sidecar stat error as clean")
	}
}

func TestReviewR4CleanScanDoesNotSurviveNewSnapshotCommit(t *testing.T) {
	root := t.TempDir()
	seedRealRekordboxOneLibraryStick(t, root)
	dir := filepath.Join(root, "Engine Library", "Database2")
	snapPath := filepath.Join(dir, rbstate.SnapshotName)
	s := &CueSession{}
	ctx := context.Background()
	if _, err := WriteBackCues(ctx, root, WriteBackOptions{Session: s}); err != nil {
		t.Fatal(err)
	}
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapIdent1, ok1 := statIdent(snapPath)
	identity, ok := readMDBCommitIdent(dir)
	if !s.hasCleanMDBScan(identity, ok, headerIdent(&snap.Header), snapIdent1, ok1) {
		t.Fatal("fixture requires a cached clean scan")
	}

	// External replacement bumping Commit while keeping LibraryUUID and Stamp equal.
	snap.Commit++
	if err := snap.Save(dir); err != nil {
		t.Fatal(err)
	}
	snapIdent2, ok2 := statIdent(snapPath)
	if _, _, err := s.loadSnapshot(dir); err != nil {
		t.Fatal(err)
	}
	if s.hasCleanMDBScan(identity, ok, headerIdent(&snap.Header), snapIdent2, ok2) {
		t.Fatal("clean scan survived replacement by a different snapshot commit")
	}

	// End-to-end: an untouched m.db against an external snapshot replacement (same UUID+Stamp,
	// Commit++ and reset Edit=1 while m.db stays at Edit=99) must run the dirty scan.
	mdb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET comment='Seen after commit bump', lastEditTime=99 WHERE id=1`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	_ = mdb.Close()
	if _, err := WriteBackCues(ctx, root, WriteBackOptions{Session: s}); err != nil {
		t.Fatal(err)
	}
	snap3, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap3.Commit++
	snap3.Tracks["contents/fixturea/track01.mp3"].Edit = 1
	if err := snap3.Save(dir); err != nil {
		t.Fatal(err)
	}
	var dirtyScans atomic.Int32
	prevHook := writeBackDirtyScanHook
	writeBackDirtyScanHook = func() { dirtyScans.Add(1) }
	t.Cleanup(func() { writeBackDirtyScanHook = prevHook })

	res, err := WriteBackCues(ctx, root, WriteBackOptions{Session: s})
	if err != nil {
		t.Fatal(err)
	}
	if dirtyScans.Load() != 1 || res.TracksRead == 0 {
		t.Fatalf("clean scan skipped dirty scan after external snapshot replacement: scans=%d res=%+v", dirtyScans.Load(), res)
	}
}

func TestReviewR4SettleReusesCachedUnsettledQueue6000Tracks(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	const nTracks = 6000
	root := t.TempDir()
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", []byte("pdb-header"))
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mdb, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`CREATE TABLE Track(id INTEGER PRIMARY KEY, path TEXT, lastEditTime INTEGER, genre TEXT, comment TEXT, label TEXT); CREATE TABLE PerformanceData(trackId INTEGER PRIMARY KEY, beatData BLOB, trackData BLOB, quickCues BLOB, loops BLOB, activeOnLoadLoops INTEGER);`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	tx, err := mdb.Begin()
	if err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	insT, err := tx.Prepare(`INSERT INTO Track(id, path, lastEditTime, genre, comment, label) VALUES(?, ?, 1, '', '', '')`)
	if err != nil {
		_ = tx.Rollback()
		_ = mdb.Close()
		t.Fatal(err)
	}
	insP, err := tx.Prepare(`INSERT INTO PerformanceData(trackId, beatData, trackData, quickCues, loops, activeOnLoadLoops) VALUES(?, NULL, NULL, NULL, NULL, 0)`)
	if err != nil {
		_ = tx.Rollback()
		_ = mdb.Close()
		t.Fatal(err)
	}
	snap := rbstate.New("uuid-6000")
	snap.Header.Layout = rbstate.LayoutDevice
	sources, stamp, err := exportNow(root, snap.Header.Layout)
	if err != nil {
		_ = tx.Rollback()
		_ = mdb.Close()
		t.Fatal(err)
	}
	snap.Header.Sources, snap.Header.Stamp = sources, stamp
	metaDigest := enginedb.MetadataDigest("", "", "")
	for i := 1; i <= nTracks; i++ {
		rel := fmt.Sprintf("Contents/Album/%04d.mp3", i)
		key := rbstate.FoldPath(rel)
		if _, err := insT.Exec(i, "../"+rel); err != nil {
			_ = tx.Rollback()
			_ = mdb.Close()
			t.Fatal(err)
		}
		if _, err := insP.Exec(i); err != nil {
			_ = tx.Rollback()
			_ = mdb.Close()
			t.Fatal(err)
		}
		snap.Tracks[key] = &rbstate.Track{
			Key:        key,
			Rel:        rel,
			EngineID:   int64(i),
			Edit:       1,
			Written:    []byte{1},
			MetaDigest: metaDigest,
			Refs:       []rbstate.SourceRef{{Kind: rbstate.DeviceLibrary, ID: uint32(i)}},
		}
	}
	_ = insT.Close()
	_ = insP.Close()
	if err := tx.Commit(); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	_ = mdb.Close()
	if err := snap.Save(dbDir); err != nil {
		t.Fatal(err)
	}

	idxStart := time.Now()
	_, indexed := indexSnapshotTracks(snap)
	idxDur := time.Since(idxStart)
	if len(indexed) != nTracks || indexed[0] != 1 || indexed[nTracks-1] != nTracks {
		t.Fatalf("unexpected indexed unsettled queue: len=%d", len(indexed))
	}

	var snapLoads atomic.Int32
	prevLoad := writeBackSnapshotLoadHook
	writeBackSnapshotLoadHook = func() { snapLoads.Add(1) }
	t.Cleanup(func() { writeBackSnapshotLoadHook = prevLoad })

	sess := &CueSession{}
	ctx := context.Background()
	p1Start := time.Now()
	res1, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, SettleBatch: cueWatcherSettleBatch, Session: sess})
	p1Dur := time.Since(p1Start)
	if err != nil || res1.TracksRead != cueWatcherSettleBatch || res1.PendingSettle != nTracks-cueWatcherSettleBatch {
		t.Fatalf("6000-track batch 1 failed: res=%+v err=%v", res1, err)
	}
	if snapLoads.Load() != 1 {
		t.Fatalf("expected 1 snapshot load on batch 1, got %d", snapLoads.Load())
	}
	if got := len(sess.unsettledIDs); got != nTracks-cueWatcherSettleBatch {
		t.Fatalf("cached unsettled queue after own save = %d, want %d", got, nTracks-cueWatcherSettleBatch)
	}

	// Pass 2 reuses cached snapshot + cached unsettled queue without re-parsing gob, and advances cursor to 512.
	p2Start := time.Now()
	res2, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, SettleBatch: cueWatcherSettleBatch, Session: sess})
	p2Dur := time.Since(p2Start)
	if err != nil || res2.TracksRead != cueWatcherSettleBatch || res2.PendingSettle != nTracks-2*cueWatcherSettleBatch {
		t.Fatalf("6000-track batch 2 failed: res=%+v err=%v", res2, err)
	}
	if snapLoads.Load() != 1 {
		t.Fatalf("batch 2 re-loaded snapshot from disk (snapLoads=%d)", snapLoads.Load())
	}
	h2, err := rbstate.ReadHeader(dbDir)
	if err != nil || h2.SettleCursor != 2*cueWatcherSettleBatch {
		t.Fatalf("SettleCursor = %d, want %d (err=%v)", h2.SettleCursor, 2*cueWatcherSettleBatch, err)
	}
	t.Logf("6000-track timings: indexSnapshotTracks=%v batch1=%v batch2=%v", idxDur, p1Dur, p2Dur)
}
