package bridge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// BLOCK 1: TakeDeckRowsChecked must return (false, nil) only for a genuine no-op (no swap
// waiting or base matching m.db), and return (false, err != nil) for lock contention, corrupt or
// incompatible swap headers, missing .rb-next.db, or carryOverChecked failure.
func TestR4_Block1_TakeDeckRowsCheckedDistinguishesNoOpFromError(t *testing.T) {
	t.Run("no_swap_and_matching_base_are_safe_noops", func(t *testing.T) {
		mount := nextStick(t, 4)
		looked, err := TakeDeckRowsChecked(context.Background(), mount)
		if looked || err != nil {
			t.Fatalf("without swap: looked=%v err=%v, want (false, nil)", looked, err)
		}
		makeNext(t, mount)
		looked, err = TakeDeckRowsChecked(context.Background(), mount)
		if looked || err != nil {
			t.Fatalf("with matching base: looked=%v err=%v, want (false, nil)", looked, err)
		}
	})

	t.Run("lock_corrupt_swap_rules_missing_next_and_missing_mirror_return_error", func(t *testing.T) {
		mount := nextStick(t, 6)
		makeNext(t, mount)
		engineWrites(t, mount, `UPDATE Track SET rating=80 WHERE id=2`)

		// 1. Volume lock held -> error (never mistaken for safe no-op).
		unlock, err := rbstate.LockVolume(mount, 0)
		if err != nil {
			t.Fatal(err)
		}
		looked, err := TakeDeckRowsChecked(context.Background(), mount)
		unlock()
		if looked || err == nil {
			t.Fatalf("while locked: looked=%v err=%v, want (false, non-nil err)", looked, err)
		}

		// 2. Corrupt swap snapshot -> error.
		swapPath := filepath.Join(dbDir(mount), rbstate.SwapName)
		origSwap, err := os.ReadFile(swapPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(swapPath, []byte("{corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		looked, err = TakeDeckRowsChecked(context.Background(), mount)
		if looked || err == nil {
			t.Fatalf("corrupt swap: looked=%v err=%v, want (false, non-nil err)", looked, err)
		}
		if err := os.WriteFile(swapPath, origSwap, 0o600); err != nil {
			t.Fatal(err)
		}

		// 3. Incompatible Rules version in swap header -> error.
		s, err := rbstate.LoadFile(swapPath)
		if err != nil {
			t.Fatal(err)
		}
		oldRules := []byte(fmt.Sprintf(`"rules":%d`, rbstate.Rules))
		newRules := []byte(fmt.Sprintf(`"rules":%d`, rbstate.Rules+1))
		if !bytes.Contains(origSwap, oldRules) {
			t.Fatalf("swap header missing %s", oldRules)
		}
		if err := os.WriteFile(swapPath, bytes.Replace(origSwap, oldRules, newRules, 1), 0o600); err != nil {
			t.Fatal(err)
		}
		looked, err = TakeDeckRowsChecked(context.Background(), mount)
		if looked || err == nil {
			t.Fatalf("rules mismatch: looked=%v err=%v, want (false, non-nil err)", looked, err)
		}
		if err := os.WriteFile(swapPath, origSwap, 0o600); err != nil {
			t.Fatal(err)
		}

		// 4. Mirror file removed on deck while m.db has content -> carryOverChecked returns ok=false,
		// which TakeDeckRowsChecked must surface as an error so remount does not overwrite m.db.
		mirrorPath := filepath.Join(nextState(s.LibraryUUID), "mirror")
		origMirror, err := os.ReadFile(mirrorPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(mirrorPath); err != nil {
			t.Fatal(err)
		}
		looked, err = TakeDeckRowsChecked(context.Background(), mount)
		if looked || err == nil {
			t.Fatalf("missing mirror with content: looked=%v err=%v, want (false, non-nil err)", looked, err)
		}
		if err := os.WriteFile(mirrorPath, origMirror, 0o600); err != nil {
			t.Fatal(err)
		}

		// 5. Missing .rb-next.db with diverged base -> error.
		nextPath := filepath.Join(dbDir(mount), nextDB)
		origNext, err := os.ReadFile(nextPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(nextPath); err != nil {
			t.Fatal(err)
		}
		looked, err = TakeDeckRowsChecked(context.Background(), mount)
		if looked || err == nil {
			t.Fatalf("missing nextDB: looked=%v err=%v, want (false, non-nil err)", looked, err)
		}
		if err := os.WriteFile(nextPath, origNext, 0o600); err != nil {
			t.Fatal(err)
		}

		// Once clean and unlocked, succeeds and reports looked=true.
		looked, err = TakeDeckRowsChecked(context.Background(), mount)
		if !looked || err != nil {
			t.Fatalf("clean TakeDeckRowsChecked: looked=%v err=%v, want (true, nil)", looked, err)
		}
	})
}

// BLOCK 3 direct checks: each view and SmartMatch helper respects cancellation immediately.
func TestR4_Block3_DirectViewAndSmartMatchHelpersRespectContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lib := &rekordbox.Library{
		Playlists: []rekordbox.Playlist{{ID: 1, Name: "P1", Tracks: []uint32{1}}},
	}
	if _, err := withoutGhostRootLists(ctx, lib, lib); err == nil {
		t.Fatal("withoutGhostRootLists ignored cancelled ctx")
	}
	if _, err := tree(ctx, &SourceLibrary{Lib: lib}, map[uint32]string{1: "k1"}); err == nil {
		t.Fatal("tree ignored cancelled ctx")
	}
	if _, err := unionCtx(ctx, []string{"a"}, []string{"b"}); err == nil {
		t.Fatal("unionCtx ignored cancelled ctx")
	}
	a := []node{{path: []string{"A"}, name: "A", entries: []string{"1"}}}
	b := []node{{path: []string{"B"}, name: "B", entries: []string{"2"}}}
	if _, err := mergeTrees(ctx, a, b); err == nil {
		t.Fatal("mergeTrees ignored cancelled ctx")
	}
	if err := addSmartMatchCtx(ctx, nil, &View{}, 1); err == nil {
		t.Fatal("addSmartMatchCtx ignored cancelled ctx")
	}
}

// FIX 4 (locked re-check): if playback starts between waitIdle() and the locked stamp save,
// FillInBackground must release the lock without writing the stick and wait for idle again.
func TestR4_Fix4_FillStampSaveRechecksBusyInsideLock(t *testing.T) {
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
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}

	setDeck(t, false)
	var once sync.Once
	var lockEntries atomic.Int32
	hookEntered := make(chan struct{})
	oldHook := fillStampLockTestHook
	fillStampLockTestHook = func() {
		lockEntries.Add(1)
		once.Do(func() {
			setDeck(t, true)
			close(hookEntered)
		})
	}
	defer func() { fillStampLockTestHook = oldHook }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	doneCh := make(chan error, 1)
	go func() {
		_, e := FillInBackground(ctx, mount, FillOptions{})
		doneCh <- e
	}()

	<-hookEntered
	time.Sleep(200 * time.Millisecond)
	select {
	case e := <-doneCh:
		t.Fatalf("FillInBackground saved snapshot inside LockVolume after deck became active (err=%v)", e)
	default:
	}
	if cur := loadSnapshot(t, mount); cur.Stamp != before.Stamp {
		t.Fatalf("snapshot stamp overwritten inside LockVolume while deck active: %q -> %q", before.Stamp, cur.Stamp)
	}
	if n := lockEntries.Load(); n != 1 {
		t.Fatalf("FillInBackground re-entered LockVolume while deck was active (%d entries, want 1)", n)
	}

	setDeck(t, false)
	if e := <-doneCh; e != nil {
		t.Fatalf("FillInBackground after deck became idle: %v", e)
	}
	if after := loadSnapshot(t, mount); after.Stamp == before.Stamp {
		t.Fatal("snapshot stamp was not updated once deck became idle")
	}
}
