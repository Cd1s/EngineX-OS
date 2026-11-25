package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

type countingCancelCtx struct {
	context.Context
	cancelAt int64
	calls    atomic.Int64
}

func (c *countingCancelCtx) Err() error {
	n := c.calls.Add(1)
	if c.cancelAt > 0 && n >= c.cancelAt {
		return context.Canceled
	}
	return c.Context.Err()
}

func (c *countingCancelCtx) Done() <-chan struct{} {
	return c.Context.Done()
}

func r4BuildLargePlainOneLibrary(t *testing.T, outPath string, nTracks int) {
	t.Helper()
	plainSource := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db")
	raw, err := os.ReadFile(plainSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", outPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		DELETE FROM content;
		DELETE FROM cue;
	`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO content(content_id,title,bpmx100,length,path,samplingRate,djComment,analysisDataFilePath) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatal(err)
	}
	for i := 1; i <= nTracks; i++ {
		if _, err := stmt.Exec(i, fmt.Sprintf("Track %04d", i), 12600, 120, "/"+testfixture.LargeTrackPath(i), 44100, "", "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			_ = db.Close()
			t.Fatal(err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
}

// BLOCK 2: readSourcesPaced must not check !pace.isBusy() once and then read SQLite/SQLCipher
// unpaced into a SongLoading edge; it must halt immediately on loading edges with 0 /data copy.
func TestR4_Block2_SQLCipherDecryptHaltsOnMidReadSongLoading(t *testing.T) {
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, 4)
	if err := os.Remove(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")); err != nil {
		t.Fatal(err)
	}
	plainCopy := filepath.Join(base, "plain-large.db")
	r4BuildLargePlainOneLibrary(t, plainCopy, 1800)

	salt, err := rekordbox.ReadSQLCipherSalt(filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	p2WriteEncryptedExport(t, encPath, plainCopy, salt)

	kc := rekordbox.NewKeyCache()
	if _, err := rekordbox.ReadSQLiteKeys(context.Background(), encPath, nil, kc); err != nil {
		t.Fatalf("warm key cache: %v", err)
	}

	r, err := compat.NewResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	files, err := FindSources(r, mount)
	if err != nil || len(files) != 1 {
		t.Fatalf("FindSources: files=%v err=%v", files, err)
	}

	var copies atomic.Int64
	oldHook := sourceCopyHook
	sourceCopyHook = func(string) { copies.Add(1) }
	defer func() { sourceCopyHook = oldHook }()

	oldChunk, oldPoll := playingReadChunk, loadingPollGap
	playingReadChunk, loadingPollGap = 8192, 10*time.Millisecond
	defer func() { playingReadChunk, loadingPollGap = oldChunk, oldPoll }()

	t.Run("encrypted_halts_mid_decrypt_when_loading_starts_after_initial_check", func(t *testing.T) {
		copies.Store(0)
		var calls atomic.Int32
		var loadStart atomic.Int64 // UnixNano when SongLoading starts
		pace := &pacer{
			ctx:    context.Background(),
			active: func() bool { return false },
			loading: func() bool {
				n := calls.Add(1)
				if n < 5 {
					// Initial checks and Chunk 1 read start idle; SongLoading begins right after Chunk 1.
					return false
				}
				if loadStart.CompareAndSwap(0, time.Now().UnixNano()) {
					return true
				}
				return time.Since(time.Unix(0, loadStart.Load())) < 220*time.Millisecond
			},
		}
		t0 := time.Now()
		libs, failed, err := readSourcesPaced(context.Background(), r, mount, files, kc, pace)
		elapsed := time.Since(t0)
		if err != nil || len(failed) != 0 || len(libs) != 1 || len(libs[0].Lib.Tracks) != 1800 {
			t.Fatalf("readSourcesPaced: libs=%d failed=%v err=%v", len(libs), failed, err)
		}
		if got := calls.Load(); got < 6 {
			t.Fatalf("loading was only polled %d times (SQLCipher read ignored SongLoading)", got)
		}
		if elapsed < 180*time.Millisecond {
			t.Fatalf("decrypt finished in %v without blocking for 220ms SongLoading edge", elapsed)
		}
		if got := copies.Load(); got != 0 {
			t.Fatalf("encrypted SQLCipher read copied %d files to /data, want 0", got)
		}
	})

	t.Run("unencrypted_sqlite_rechecks_loading_edge_and_copies_nothing_when_idle", func(t *testing.T) {
		plainMount := filepath.Join(base, "stick-plain")
		testfixture.LargeExport(t, plainMount, 4)
		_ = os.Remove(filepath.Join(plainMount, "PIONEER", "rekordbox", "export.pdb"))
		rawPlain, err := os.ReadFile(plainCopy)
		if err != nil {
			t.Fatal(err)
		}
		plainDB := filepath.Join(plainMount, "PIONEER", "rekordbox", "exportLibrary.db")
		if err := os.WriteFile(plainDB, rawPlain, 0o600); err != nil {
			t.Fatal(err)
		}
		pr, err := compat.NewResolver(plainMount)
		if err != nil {
			t.Fatal(err)
		}
		pfiles, err := FindSources(pr, plainMount)
		if err != nil || len(pfiles) != 1 {
			t.Fatalf("FindSources(plain): files=%v err=%v", pfiles, err)
		}

		copies.Store(0)
		var calls atomic.Int32
		var loadStart atomic.Int64
		pace := &pacer{
			ctx:    context.Background(),
			active: func() bool { return false },
			loading: func() bool {
				n := calls.Add(1)
				if n < 3 {
					return false
				}
				if loadStart.CompareAndSwap(0, time.Now().UnixNano()) {
					return true
				}
				return time.Since(time.Unix(0, loadStart.Load())) < 180*time.Millisecond
			},
		}
		t0 := time.Now()
		libs, failed, err := readSourcesPaced(context.Background(), pr, plainMount, pfiles, kc, pace)
		elapsed := time.Since(t0)
		if err != nil || len(failed) != 0 || len(libs) != 1 {
			t.Fatalf("readSourcesPaced(plain): libs=%d failed=%v err=%v", len(libs), failed, err)
		}
		if elapsed < 140*time.Millisecond {
			t.Fatalf("plaintext read finished in %v without blocking for SongLoading edge", elapsed)
		}
		if got := copies.Load(); got != 0 {
			t.Fatalf("idle plaintext read copied %d files to /data, want 0", got)
		}
	})
}

// BLOCK 3: view playlist/merge/ghost-list loops and SmartMatch must poll ctx/deadline so large
// libraries hand off safely instead of exhausting the hook margin.
func TestR4_Block3_ViewAndSmartMatchRespectDeadline(t *testing.T) {
	t.Run("build_view_checks_ctx_before_finishing_large_playlist_trees", func(t *testing.T) {
		mkPlaylists := func(offset int, ghost bool) []rekordbox.Playlist {
			pls := make([]rekordbox.Playlist, 0, 120)
			for i := 1; i <= 120; i++ {
				name := fmt.Sprintf("List-%03d", i+offset)
				pls = append(pls, rekordbox.Playlist{ID: uint32(i), Name: name})
			}
			if ghost {
				pls = append(pls, rekordbox.Playlist{ID: 999, Name: "GhostRoot"})
			}
			return pls
		}
		libs := []SourceLibrary{
			{Kind: rbstate.OneLibrary, Source: oneLibraryDB, Lib: &rekordbox.Library{Playlists: mkPlaylists(0, true)}},
			{Kind: rbstate.DeviceLibrary, Source: devicePDB, Lib: &rekordbox.Library{Playlists: mkPlaylists(80, false)}},
		}

		// With 0 tracks and 240 playlists, BuildViewCtx on d8c9e0a2 only calls ctx.Err() inside
		// the final v.Lists loop (1 call for <=256 lists). Cancelling at call #2 must abort inside
		// the ghost-list / tree / merge loops rather than completing with err == nil.
		cctx := &countingCancelCtx{Context: context.Background(), cancelAt: 2}
		if _, err := BuildViewCtx(cctx, libs, rbstate.LayoutFolders, "en"); !errors.Is(err, context.Canceled) {
			t.Fatalf("BuildViewCtx returned err=%v after %d ctx checks, want context.Canceled", err, cctx.calls.Load())
		}

		countCtx := &countingCancelCtx{Context: context.Background()}
		v, err := BuildViewCtx(countCtx, libs, rbstate.LayoutMerged, "en")
		if err != nil || len(v.Lists) == 0 {
			t.Fatalf("BuildViewCtx(merged): len=%d err=%v", len(v.Lists), err)
		}
		if got := countCtx.calls.Load(); got < 15 {
			t.Fatalf("expected frequent ctx checks in tree/merge/ghost loops, got only %d", got)
		}
	})

	t.Run("smart_match_deadline_hands_off_to_background", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 4)

		bgStarted := false
		// Deadline = now + spare + 500ms: BuildViewCtx finishes cleanly before spare, while
		// st.owed (Deadline - 3.75s) is already past, so addSmartMatchCtx must return
		// errWindowDeadline and hand off instead of running SmartMatch and failing lookup.
		res, err := ConvertAtMount(context.Background(), mount, MountOptions{
			SmartMatch: 99,
			Deadline:   time.Now().Add(spare + 500*time.Millisecond),
			StartBackground: func() error {
				bgStarted = true
				return nil
			},
		})
		if err != nil {
			t.Fatalf("ConvertAtMount returned error instead of deadline hand-off: %v", err)
		}
		if !res.Background || !bgStarted {
			t.Fatalf("expected background hand-off when SmartMatch hits deadline, got res=%+v bg=%v", res, bgStarted)
		}
	})
}

// FIX 4: background stamp save must wait for idle AND re-check pace.isBusy() inside LockVolume
// before writing .rb-bridge-snapshot to the stick.
func TestR4_Fix4_FillStampSaveWaitsForPlaybackIdle(t *testing.T) {
	t.Run("defers_stamp_save_while_deck_playing", func(t *testing.T) {
		defer SetPriorityDirForTest(t.TempDir())()
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 4)
		mountNow(t, mount, MountOptions{})
		if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
			t.Fatalf("initial fill: %v", err)
		}
		mountNow(t, mount, MountOptions{Publish: true})

		before := loadSnapshot(t, mount)
		// Touch export.pdb so snap.Stamp != view.stamp with zero structural diff.
		later := time.Now().Add(time.Hour)
		if err := os.Chtimes(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"), later, later); err != nil {
			t.Fatal(err)
		}

		setDeck(t, true)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		doneCh := make(chan error, 1)
		go func() {
			_, e := FillInBackground(ctx, mount, FillOptions{})
			doneCh <- e
		}()

		time.Sleep(220 * time.Millisecond)
		select {
		case e := <-doneCh:
			t.Fatalf("FillInBackground saved snapshot and exited while deck was playing (err=%v)", e)
		default:
		}
		if cur := loadSnapshot(t, mount); cur.Stamp != before.Stamp {
			t.Fatalf("snapshot stamp changed from %q to %q while deck played", before.Stamp, cur.Stamp)
		}

		setDeck(t, false)
		if e := <-doneCh; e != nil {
			t.Fatalf("FillInBackground after deck stopped: %v", e)
		}
		if after := loadSnapshot(t, mount); after.Stamp == before.Stamp {
			t.Fatal("snapshot stamp was not updated once deck became idle")
		}
	})
}

// Item 5: when CueWatcher's pass drops due to errNotQuiet while st.due is zero (e.g.Settled saw
// a change or transient flag right as the pass started without another tick event), CueWatcher
// must schedule a retry instead of permanently dropping the edit until the next m.db write.
func TestR4_Item5_WatcherRetriesErrNotQuietWhenDueZero(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 4)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})

	t.Cleanup(SetCueRunDirForTest(t.TempDir()))
	w := &CueWatcher{
		quiet:  40 * time.Millisecond,
		retry:  60 * time.Millisecond,
		poll:   25 * time.Millisecond,
		mounts: func() []string { return []string{mount} },
	}
	var calls atomic.Int32
	w.pass = func(context.Context, string, WriteBackOptions) (WriteBackResult, error) {
		if calls.Add(1) == 1 {
			return WriteBackResult{}, errNotQuiet
		}
		return WriteBackResult{TracksChanged: 1}, nil
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

	// Wait for initial rescan to attach the stick without touching m.db again afterwards.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		_, ok := w.sticks[mount]
		w.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Single m.db write triggers pass 1; no second m.db write occurs after pass 1 fails errNotQuiet.
	mDB := filepath.Join(dbDir(mount), "m.db")
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(mDB, later, later); err != nil {
		t.Fatal(err)
	}

	retryDeadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(retryDeadline) && calls.Load() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("CueWatcher ran %d pass(es) after errNotQuiet with zero st.due; expected automatic retry (want >= 2)", got)
	}
	// Ensure path helper import isn't flagged if adjusted later.
	if strings.TrimSpace(mount) == "" {
		t.Fatal("empty mount")
	}
}
