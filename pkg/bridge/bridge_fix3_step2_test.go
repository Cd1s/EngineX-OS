package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// Finding 2.1: errWindowDeadline must hand off to the background job (err == nil, Background == true)
// rather than failing ConvertAtMount; *Ctx helpers must respect cancellation; and legacy adopt cost
// must be included in the mount window's ahead estimate.
func TestFix3_Finding2_1_DeadlineHandoffAndCtxWiring(t *testing.T) {
	t.Run("deadline_inside_commit_band_hands_off_without_error", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 8)

		bgStarted := false
		// spare=3s, commitReserve=3.75s: setting Deadline = now + spare + 500ms lets ahead fit
		// and BuildLibrary finish, while st.owed (Deadline - 3.75s) is already past so w.build()
		// returns errWindowDeadline. ConvertAtMount must hand off cleanly instead of returning error.
		res, err := ConvertAtMount(context.Background(), mount, MountOptions{
			Deadline: time.Now().Add(spare + 500*time.Millisecond),
			StartBackground: func() error {
				bgStarted = true
				return nil
			},
		})
		if err != nil {
			t.Fatalf("ConvertAtMount returned error on deadline handoff: %v", err)
		}
		if !res.Background || !bgStarted {
			t.Fatalf("expected background handoff: res=%+v bgStarted=%v", res, bgStarted)
		}
		if _, statErr := os.Stat(filepath.Join(dbDir(mount), "m.db")); !os.IsNotExist(statErr) {
			t.Fatalf("m.db should not be installed past commitReserve: statErr=%v", statErr)
		}
	})

	t.Run("ctx_helpers_stop_cleanly_at_deadline", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 4)
		fix3WritePlainOneLibrary(t, mount)
		if err := os.MkdirAll(dbDir(mount), 0o755); err != nil {
			t.Fatal(err)
		}
		markPath := filepath.Join(dbDir(mount), writeBackMarkName)
		if err := os.WriteFile(markPath, []byte("PIONEER/rekordbox\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		// CleanWriteBackLeftoversCtx must stop immediately on a cancelled context without deleting mark.
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		_ = CleanWriteBackLeftoversCtx(cancelled, mount)
		if _, err := os.Stat(markPath); err != nil {
			t.Fatalf("CleanWriteBackLeftoversCtx ignored cancelled ctx and removed mark: %v", err)
		}

		// ConvertAtMount whose deadline expires right after startBG() must wire deadline ctx into
		// CleanWriteBackLeftoversCtx (leaving mark untouched) and hand off cleanly with err == nil.
		bgStarted := false
		res, err := ConvertAtMount(context.Background(), mount, MountOptions{
			Deadline: time.Now().Add(spare + 25*time.Millisecond),
			StartBackground: func() error {
				bgStarted = true
				time.Sleep(45 * time.Millisecond)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("ConvertAtMount returned error when ctx deadline expired: %v", err)
		}
		if !res.Background || !bgStarted {
			t.Fatalf("expected background handoff on expired ctx: res=%+v bgStarted=%v", res, bgStarted)
		}
		if _, err := os.Stat(markPath); err != nil {
			t.Fatalf("ConvertAtMount did not wire deadline ctx into CleanWriteBackLeftoversCtx: %v", err)
		}
	})

	t.Run("adopt_cost_defers_large_legacy_takeover", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 2)
		mountNow(t, mount, MountOptions{})
		// Remove v2 snapshot and plant a 6 MiB legacy .rb-bridge-state.db (estimates ~25k rows -> >11 s adopt cost).
		if err := os.Remove(filepath.Join(dbDir(mount), rbstate.SnapshotName)); err != nil {
			t.Fatal(err)
		}
		oldState := filepath.Join(dbDir(mount), oldStateName)
		f, err := os.Create(oldState)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(6 << 20); err != nil {
			f.Close()
			t.Fatal(err)
		}
		_ = f.Close()

		// 4 s before spare fits a 2-track export read easily, but NOT a 6 MiB legacy adopt takeover.
		bgStarted := false
		res, err := ConvertAtMount(context.Background(), mount, MountOptions{
			Deadline: time.Now().Add(spare + 4*time.Second),
			StartBackground: func() error {
				bgStarted = true
				return nil
			},
		})
		if err != nil {
			t.Fatalf("ConvertAtMount failed instead of deferring large legacy adopt: %v", err)
		}
		if !res.Background || !bgStarted {
			t.Fatalf("expected background handoff for large legacy adopt: res=%+v bgStarted=%v", res, bgStarted)
		}
		if _, err := os.Stat(oldState); err != nil {
			t.Fatalf("legacy state should remain for background takeover: %v", err)
		}
	})
}

// Finding 4.4: readSourcesPaced must refuse dirty -journal exports even while a deck is playing,
// must not copy exports to /data on idle runs or for PDB files, and FillInBackground must clean
// orphaned .rb-src-* files left behind by a killed job.
func TestFix3_Finding4_4_ReadSourcesPacedDirtyJournalAndNoIdleCopy(t *testing.T) {
	t.Run("dirty_journal_refused_while_playing", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 2)
		fix3WritePlainOneLibrary(t, mount)
		if err := os.Remove(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")); err != nil {
			t.Fatal(err)
		}
		journal := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db-journal")
		if err := os.WriteFile(journal, []byte("uncheckpointed rollback journal"), 0o644); err != nil {
			t.Fatal(err)
		}

		r, err := compat.NewResolver(mount)
		if err != nil {
			t.Fatal(err)
		}
		files, err := FindSources(r, mount)
		if err != nil || len(files) != 1 {
			t.Fatalf("FindSources: files=%v err=%v", files, err)
		}
		defer SetPriorityDirForTest(t.TempDir())()
		setDeck(t, true)
		pace := newPacer(context.Background())
		libs, failed, err := readSourcesPaced(context.Background(), r, mount, files, nil, pace)
		if err == nil || len(libs) != 0 || len(failed) != 1 {
			t.Fatalf("expected dirty -journal refusal while playing: libs=%d failed=%v err=%v", len(libs), failed, err)
		}
		if !strings.Contains(err.Error(), "-journal") {
			t.Fatalf("expected -journal in refusal error, got: %v", err)
		}
	})

	t.Run("no_data_copy_on_idle_or_pdb_and_orphan_cleanup", func(t *testing.T) {
		defer SetPriorityDirForTest(t.TempDir())()
		base := t.TempDir()
		stageDir := filepath.Join(base, "stage")
		t.Setenv(rbstate.StageEnv, stageDir)
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			t.Fatal(err)
		}
		orphan := filepath.Join(stageDir, ".rb-src-killed-prev")
		if err := os.WriteFile(orphan, []byte("leftover"), 0o600); err != nil {
			t.Fatal(err)
		}

		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 4)
		fix3WritePlainOneLibrary(t, mount)
		mountNow(t, mount, MountOptions{})

		var copies atomic.Int64
		oldHook := sourceCopyHook
		sourceCopyHook = func(string) { copies.Add(1) }
		defer func() { sourceCopyHook = oldHook }()

		// Idle FillInBackground must clean orphaned .rb-src-* and copy ZERO files to /data.
		setDeck(t, false)
		if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
			t.Fatalf("idle FillInBackground: %v", err)
		}
		if _, err := os.Stat(orphan); !os.IsNotExist(err) {
			t.Fatalf("orphaned .rb-src-* file was not cleaned on FillInBackground startup: %v", err)
		}
		if got := copies.Load(); got != 0 {
			t.Fatalf("idle FillInBackground copied %d export files to /data, want 0", got)
		}

		// Playing deck reading export.pdb must pace in place with ZERO /data copy.
		oldGap, oldCache := playingReadGap, fromCache
		playingReadGap, fromCache = time.Millisecond, 0
		defer func() { playingReadGap, fromCache = oldGap, oldCache }()
		r, err := compat.NewResolver(mount)
		if err != nil {
			t.Fatal(err)
		}
		files, err := FindSources(r, mount)
		if err != nil {
			t.Fatal(err)
		}
		var pdbOnly []rbstate.FileSig
		for _, f := range files {
			if strings.HasSuffix(strings.ToLower(f.Rel), ".pdb") {
				pdbOnly = append(pdbOnly, f)
			}
		}
		setDeck(t, true)
		pace := newPacer(context.Background())
		libs, _, err := readSourcesPaced(context.Background(), r, mount, pdbOnly, nil, pace)
		if err != nil || len(libs) != 1 {
			t.Fatalf("readSourcesPaced PDB while playing: libs=%d err=%v", len(libs), err)
		}
		if got := copies.Load(); got != 0 {
			t.Fatalf("playing PDB read copied %d files to /data, want 0 (must read in place)", got)
		}
		if pace.pauses == 0 {
			t.Fatal("in-place PDB read did not pace while deck was playing")
		}
	})
}
