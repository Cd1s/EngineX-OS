package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestReviewR3SessionDropsRowsOnSnapshotReplacement(t *testing.T) {
	t.Run("UnitSnapshotReplacement", func(t *testing.T) {
		dir := t.TempDir()
		s := &CueSession{}
		a := rbstate.New("first")
		if err := a.Save(dir); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.loadSnapshot(dir); err != nil {
			t.Fatal(err)
		}
		s.add(map[string]noted{"contents/track.mp3": {edit: 9}})
		b := rbstate.New("second")
		if err := b.Save(dir); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.loadSnapshot(dir)
		if err != nil || got.LibraryUUID != "second" {
			t.Fatalf("failed to load replacement snapshot: %v", err)
		}
		if s.has("contents/track.mp3", 9) {
			t.Fatal("old edit timestamp still suppresses work after the snapshot was replaced")
		}
	})

	t.Run("EndToEndWriteBackProcessesNewStickAtSameEditTime", func(t *testing.T) {
		env := setupPhantomTestStick(t, nil)
		sess := &CueSession{}
		if _, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{}); err != nil {
			t.Fatalf("baseline pass: %v", err)
		}

		// Warm sess.rows with Track 1's rewritten no-op edit timestamp=77.
		db1, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db1.Exec(`UPDATE Track SET lastEditTime=77 WHERE id=1`); err != nil {
			_ = db1.Close()
			t.Fatal(err)
		}
		_ = db1.Close()
		res1, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{Session: sess})
		if err != nil || res1.TracksChanged != 0 {
			t.Fatalf("warm pass: res=%+v err=%v", res1, err)
		}
		if !sess.has("contents/fixturea/track01.mp3", 77) {
			t.Fatal("expected sess.rows to cache rewritten row at edit=77")
		}

		// Replace snapshot with a new LibraryUUID (at initial Edit=1) and give Track 1 a real cue edit at edit=77.
		snap, err := rbstate.Load(env.dbDir)
		if err != nil {
			t.Fatal(err)
		}
		snap.Header.LibraryUUID = "replacement-stick-uuid"
		snap.Tracks["contents/fixturea/track01.mp3"].Edit = 1
		if err := snap.Save(env.dbDir); err != nil {
			t.Fatal(err)
		}
		setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, map[int]uint32{0: 5000})
		db2, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db2.Exec(`UPDATE Track SET lastEditTime=77 WHERE id=1`); err != nil {
			_ = db2.Close()
			t.Fatal(err)
		}
		_ = db2.Close()

		res2, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{Session: sess})
		if err != nil {
			t.Fatalf("replacement pass: %v", err)
		}
		if res2.TracksChanged != 1 {
			t.Fatalf("stale session row suppressed replacement stick cue edit: %+v", res2)
		}
	})
}

func TestReviewR3SnapshotBoundAfterVolumeLock(t *testing.T) {
	t.Run("LoadSnapshotHoldsVolumeLock", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("rbstate.LockVolume is a no-op off Linux")
		}
		env := setupPhantomTestStick(t, nil)
		sess := &CueSession{}
		var lockHeld atomic.Bool
		oldHook := writeBackSnapshotLoadHook
		writeBackSnapshotLoadHook = func() {
			unlock, err := rbstate.LockVolume(env.root, 0)
			if errors.Is(err, rbstate.ErrLocked) {
				lockHeld.Store(true)
			} else if err == nil {
				unlock()
			}
		}
		defer func() { writeBackSnapshotLoadHook = oldHook }()

		if _, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{Session: sess}); err != nil {
			t.Fatalf("WriteBackCues: %v", err)
		}
		if !lockHeld.Load() {
			t.Fatal("snapshot loaded before acquiring rbstate.LockVolume")
		}
	})

	t.Run("RevalidatesSnapshotReplacedBeforeLock", func(t *testing.T) {
		env := setupPhantomTestStick(t, nil)
		sess := &CueSession{}
		// Warm session on stick's initial snapshot with a cached row at edit=91.
		sess.add(map[string]noted{"contents/fixturea/track01.mp3": {edit: 91}})
		if _, _, err := sess.loadSnapshot(env.dbDir); err != nil {
			t.Fatal(err)
		}
		sess.add(map[string]noted{"contents/fixturea/track01.mp3": {edit: 91}})

		// Pre-stage cue edit at edit=91 in m.db.
		setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, map[int]uint32{0: 6000})
		db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET lastEditTime=91 WHERE id=1`); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.Close()

		// Simulate another mount replacing the snapshot right after the pre-check, before LockVolume.
		oldBeforeLock := writeBackBeforeLockHook
		replacedOnce := false
		writeBackBeforeLockHook = func() {
			if replacedOnce {
				return
			}
			replacedOnce = true
			snap, err := rbstate.Load(env.dbDir)
			if err == nil {
				snap.Header.LibraryUUID = "concurrent-mount-uuid"
				_ = snap.Save(env.dbDir)
			}
		}
		defer func() { writeBackBeforeLockHook = oldBeforeLock }()

		res, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{Session: sess})
		if err != nil {
			t.Fatalf("WriteBackCues: %v", err)
		}
		if res.TracksChanged != 1 {
			t.Fatalf("expected replaced snapshot under LockVolume to invalidate stale session row: %+v", res)
		}
	})
}

func TestReviewR3CueWatcherResetsSessionOnCoalescedSamePathRemount(t *testing.T) {
	t.Cleanup(SetCueRunDirForTest(t.TempDir()))
	env := setupPhantomTestStick(t, nil)
	if _, err := WriteBackCues(context.Background(), env.root, WriteBackOptions{}); err != nil {
		t.Fatalf("baseline pass: %v", err)
	}

	// Make Track 1 appear touched without changing cues so Pass populates CueSession.rows.
	db, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=123 WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	evCh := make(chan struct{}, 4)
	var curIDMu sync.Mutex
	curMountID := "mid:10:8:1:/:initial"
	w := &CueWatcher{
		quiet:          20 * time.Millisecond,
		retry:          50 * time.Millisecond,
		poll:           20 * time.Millisecond,
		rescanFallback: 10 * time.Second,
		ejectDir:       cueEjectingDir,
		useMountWatch:  true,
		mountEvents:    evCh,
		mountsInfo: func() []mountEntry {
			curIDMu.Lock()
			defer curIDMu.Unlock()
			return []mountEntry{{path: env.root, id: curMountID}}
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

	if !WaitWatchingForTest(w, env.root, 2*time.Second) {
		t.Fatal("watcher did not attach initial mount")
	}
	res1, err := w.Pass(ctx, env.root)
	if err != nil || res1.TracksRead != 1 {
		t.Fatalf("initial w.Pass: res=%+v err=%v", res1, err)
	}
	w.mu.Lock()
	firstStick := w.sticks[env.root]
	hasRow := firstStick != nil && firstStick.session.has("contents/fixturea/track01.mp3", 123)
	w.mu.Unlock()
	if !hasRow {
		t.Fatal("expected initial cueStick session to cache edit=123")
	}

	// Coalesced unmount+remount at exact same path with new mount ID (no empty unmount intermediate scan)
	// and a real cue edit at the same lastEditTime=123.
	snap2, err := rbstate.Load(env.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	snap2.Tracks["contents/fixturea/track01.mp3"].Edit = 1
	if err := snap2.Save(env.dbDir); err != nil {
		t.Fatal(err)
	}
	setMDBQuickCues(t, env.dbDir, 1, env.rbOffset, map[int]uint32{0: 8000})
	db2, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec(`UPDATE Track SET lastEditTime=123 WHERE id=1`); err != nil {
		_ = db2.Close()
		t.Fatal(err)
	}
	_ = db2.Close()
	curIDMu.Lock()
	curMountID = "mid:11:8:2:/:remounted"
	curIDMu.Unlock()
	evCh <- struct{}{}

	deadline := time.Now().Add(2 * time.Second)
	replaced := false
	for time.Now().Before(deadline) {
		w.mu.Lock()
		cur := w.sticks[env.root]
		w.mu.Unlock()
		if cur != nil && cur != firstStick {
			replaced = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !replaced {
		t.Fatal("watcher retained old cueStick across coalesced same-path mount ID change")
	}
	res2, err := w.Pass(ctx, env.root)
	if err != nil || res2.TracksChanged != 1 {
		t.Fatalf("expected fresh stick pass after remount to export cue edit, got res=%+v err=%v", res2, err)
	}
}

func TestReviewR3CueWatcherDiscoversDelayedExportOnMountedVolume(t *testing.T) {
	t.Cleanup(SetCueRunDirForTest(t.TempDir()))

	for _, mode := range []string{"Notify", "BoundedFallback"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir() // mounted empty volume (initially ineligible)
			evCh := make(chan struct{}, 2)
			fallback := 10 * time.Second
			if mode == "BoundedFallback" {
				fallback = 35 * time.Millisecond
			}
			w := &CueWatcher{
				quiet:          20 * time.Millisecond,
				retry:          50 * time.Millisecond,
				poll:           20 * time.Millisecond,
				rescanFallback: fallback,
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

			// Wait for initial rescan to classify root as an ineligible mount.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) && !w.hasOtherMounts() {
				time.Sleep(5 * time.Millisecond)
			}
			if !w.hasOtherMounts() || w.hasSticks() {
				t.Fatalf("expected initial state otherMounts>0 & sticks==0, got other=%v sticks=%v", w.hasOtherMounts(), w.hasSticks())
			}

			// Rekordbox export files appear on the already-mounted directory with NO mount event.
			testfixture.LargeExport(t, root, 1)
			if mode == "Notify" {
				w.Notify()
			}
			if !WaitWatchingForTest(w, root, 2*time.Second) {
				t.Fatalf("watcher did not discover delayed export via %s", mode)
			}
		})
	}
}

func seedUnsettledNTrackStick(t *testing.T, mount string, n int) {
	t.Helper()
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	ctx := context.Background()
	testfixture.LargeExport(t, mount, n)
	if _, err := ConvertAtMount(ctx, mount, MountOptions{Full: true}); err != nil {
		t.Fatalf("ConvertAtMount(Full #1, %d): %v", n, err)
	}
	if _, err := FillInBackground(ctx, mount, FillOptions{}); err != nil {
		t.Fatalf("FillInBackground(%d): %v", n, err)
	}
	res, err := ConvertAtMount(ctx, mount, MountOptions{Full: true})
	if err != nil || !res.Pending.None() {
		t.Fatalf("ConvertAtMount(Full #2, %d) = (%+v, %v), want Pending.None()", n, res, err)
	}
	dir := filepath.Join(mount, "Engine Library", "Database2")
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	mdb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()
	for _, tr := range snap.Tracks {
		if tr.Native {
			continue
		}
		var edit int64
		if err := mdb.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=?`, tr.EngineID).Scan(&edit); err != nil {
			t.Fatal(err)
		}
		tr.Edit = edit
		tr.Synced = nil
	}
	if err := snap.Save(dir); err != nil {
		t.Fatal(err)
	}
}

func TestReviewR3LargeBatchUsesIDFilteredPerformanceQuery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const nTracks = 120
	seedUnsettledNTrackStick(t, root, nTracks)

	dir := filepath.Join(root, "Engine Library", "Database2")
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Mark Tracks 1..50 already Synced, leaving 51..120 (70 tracks > 64) unsettled, and poison
	// PerformanceData.activeOnLoadLoops on an unselected settled Track 10 with a non-integer TEXT
	// sentinel so any full-table PerformanceData join scan fails immediately.
	var sentinelID int64
	for _, tr := range snap.Tracks {
		if tr.Native {
			continue
		}
		if tr.EngineID <= 50 {
			tr.Synced = []byte{1, 2, 3, 4}
			if tr.EngineID == 10 {
				sentinelID = tr.EngineID
			}
		}
	}
	if sentinelID == 0 {
		t.Fatal("missing settled track 10")
	}
	if err := snap.Save(dir); err != nil {
		t.Fatal(err)
	}

	mdb, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE PerformanceData SET activeOnLoadLoops='sentinel-unselected' WHERE trackId=?`, sentinelID); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	// PerformanceData UPDATE triggers Track.lastEditTime; sync snapshot Edit for the settled
	// track so only the 70 unsettled tracks (51..120) are in the batch while Track 10 stays settled.
	var sentinelEdit int64
	if err := mdb.QueryRow(`SELECT COALESCE(lastEditTime,0) FROM Track WHERE id=?`, sentinelID).Scan(&sentinelEdit); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	_ = mdb.Close()
	snap2, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range snap2.Tracks {
		if tr.EngineID == sentinelID {
			tr.Edit = sentinelEdit
			break
		}
	}
	if err := snap2.Save(dir); err != nil {
		t.Fatal(err)
	}

	res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, SettleBatch: 70})
	if err != nil {
		t.Fatalf("large batch scanned unselected settled row: %v", err)
	}
	if res.TracksRead != 70 || res.PendingSettle != 0 {
		t.Fatalf("unexpected large-batch result: %+v, want TracksRead=70 PendingSettle=0", res)
	}
}

func TestReviewR3PersistedSettleCursorAdvancesAndSkipsRescanWhenMDBUnchanged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const nTracks = 12
	seedUnsettledNTrackStick(t, root, nTracks)
	dir := filepath.Join(root, "Engine Library", "Database2")

	var dirtyScans atomic.Int32
	oldHook := writeBackDirtyScanHook
	writeBackDirtyScanHook = func() { dirtyScans.Add(1) }
	t.Cleanup(func() { writeBackDirtyScanHook = oldHook })

	sess := &CueSession{}
	// Pass 1: settles IDs 1..4 (cursor -> 4, 1 dirty scan).
	res1, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, SettleBatch: 4, Session: sess})
	if err != nil || res1.TracksRead != 4 || res1.PendingSettle != 8 {
		t.Fatalf("pass 1: res=%+v err=%v", res1, err)
	}
	if dirtyScans.Load() != 1 {
		t.Fatalf("expected 1 dirty scan on pass 1, got %d", dirtyScans.Load())
	}
	h1, err := rbstate.ReadHeader(dir)
	if err != nil || h1.SettleCursor != 4 {
		t.Fatalf("expected persisted SettleCursor=4 after pass 1, got %+v err=%v", h1, err)
	}

	// Pass 2 (m.db unchanged): skips dirty Track scan (still 1!) and settles IDs 5..8 (cursor -> 8).
	res2, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, SettleBatch: 4, Session: sess})
	if err != nil || res2.TracksRead != 4 || res2.PendingSettle != 4 {
		t.Fatalf("pass 2: res=%+v err=%v", res2, err)
	}
	if dirtyScans.Load() != 1 {
		t.Fatalf("expected 0 extra dirty scans on unchanged m.db pass 2, got %d", dirtyScans.Load())
	}
	h2, err := rbstate.ReadHeader(dir)
	if err != nil || h2.SettleCursor != 8 {
		t.Fatalf("expected persisted SettleCursor=8 after pass 2, got %+v err=%v", h2, err)
	}

	// Pass 3 (fresh session, reading persisted cursor=8 from snapshot): settles IDs 9..12 (cursor -> 0).
	res3, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, SettleBatch: 4})
	if err != nil || res3.TracksRead != 4 || res3.PendingSettle != 0 {
		t.Fatalf("pass 3: res=%+v err=%v", res3, err)
	}
	h3, err := rbstate.ReadHeader(dir)
	if err != nil || h3.SettleCursor != 0 {
		t.Fatalf("expected SettleCursor reset to 0 when all tracks settled, got %+v err=%v", h3, err)
	}

	// FAT32 safety: warm clean scan on sess, then edit Track 1 in m.db AND restore exact mtime;
	// SQLite file change counter in header bytes [24:28] must still detect the commit.
	_, err = WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: sess})
	if err != nil {
		t.Fatal(err)
	}
	mdbPath := filepath.Join(dir, "m.db")
	stBefore, err := os.Stat(mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	mdb, err := sql.Open("sqlite", mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET comment='FAT32 Same MTime Edit' WHERE id=1`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+99 WHERE id=1`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	_ = mdb.Close()
	if err := os.Chtimes(mdbPath, stBefore.ModTime(), stBefore.ModTime()); err != nil {
		t.Fatal(err)
	}
	scansBeforeEdit := dirtyScans.Load()
	res4, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: sess})
	if err != nil || res4.TracksChanged != 1 || dirtyScans.Load() != scansBeforeEdit+1 {
		t.Fatalf("same-mtime SQLite commit was missed by mdbCommitIdent cache: res=%+v dirtyDelta=%d err=%v",
			res4, dirtyScans.Load()-scansBeforeEdit, err)
	}
}

func TestReviewR3OnEjectSkipsLegacyRepairAndDoesNotStampRepairStamp(t *testing.T) {
	ctx := context.Background()
	env := setupPhantomTestStick(t, nil)

	// Make Track 1's DAT have legacy swapped cue layout (due for repair against backup).
	brokenDAT := swapCompactDATCueOrder(t, env.origDAT)
	testfixture.Write(t, env.root, env.dat1Rel, brokenDAT)
	refreshPhantomSnapStamp(t, env)

	// Give Track 2 a live deck cue edit on the stick.
	setMDBQuickCues(t, env.dbDir, 2, env.rbOffset, map[int]uint32{0: 9000})
	mdb, err := sql.Open("sqlite", filepath.Join(env.dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+15 WHERE id=2`); err != nil {
		_ = mdb.Close()
		t.Fatal(err)
	}
	_ = mdb.Close()

	// Final OnEject pass must export ONLY live Track 2 edit, skip legacy Track 1 repair, and NOT stamp RepairStamp.
	res, err := WriteBackCues(ctx, env.root, WriteBackOptions{SkipPreparingCheck: true, OnEject: true})
	if err != nil {
		t.Fatalf("OnEject: %v", err)
	}
	if res.TracksChanged != 1 || len(res.Changes) != 1 || res.Changes[0].EngineID != 2 {
		t.Fatalf("OnEject expected only Track 2 live edit (skipping Track 1 bulk repair), got %+v", res)
	}
	if cur, _ := os.ReadFile(env.dat1Path); !bytes.Equal(cur, brokenDAT) {
		t.Fatal("OnEject unexpectedly repaired legacy Track 1 DAT")
	}
	hdrAfterEject, err := rbstate.ReadHeader(env.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	if hdrAfterEject.RepairStamp != "" {
		t.Fatalf("OnEject falsely stamped RepairStamp=%q although legacy repair was skipped", hdrAfterEject.RepairStamp)
	}

	// Next idle pass (OnEject=false) runs the deferred repair cleanly and stamps RepairStamp.
	resIdle, err := WriteBackCues(ctx, env.root, WriteBackOptions{SkipPreparingCheck: true})
	if err != nil {
		t.Fatalf("post-eject idle repair: %v", err)
	}
	if resIdle.TracksChanged != 1 || len(resIdle.Changes) != 1 || resIdle.Changes[0].EngineID != 1 {
		t.Fatalf("idle pass did not repair Track 1: %+v", resIdle)
	}
	hdrAfterIdle, err := rbstate.ReadHeader(env.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	if hdrAfterIdle.RepairStamp == "" {
		t.Fatal("expected RepairStamp stamped after idle repair completed")
	}
}

func TestReviewR3CheckpointSaveHonorsCanceledContext(t *testing.T) {
	env := setupPhantomTestStick(t, nil)
	snapPath := filepath.Join(env.dbDir, rbstate.SnapshotName)
	before, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := noteWriteBackCtx(cctx, env.root, nil, nil, nil, true, 1, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("noteWriteBackCtx with canceled context returned err=%v, want context.Canceled", err)
	}
	after, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("canceled noteWriteBackCtx modified snapshot on disk")
	}
}
