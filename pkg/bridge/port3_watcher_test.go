package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/bridgetest"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func p3WaitCue(t *testing.T, mount, name string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if a, err := rekordbox.ReadAnalysis(filepath.Join(mount, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT")); err == nil {
			for _, cue := range a.Cues {
				if cue.Name == name {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cue %q was not written within %s", name, within)
}

func p3StartWatcher(t *testing.T, mount string, quiet, retry time.Duration, runDir string) (*bridge.CueWatcher, *atomic.Int32, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	t.Cleanup(bridge.SetCueRunDirForTest(runDir))
	w := bridge.CueWatcherForTest([]string{mount}, quiet, retry)
	var passes atomic.Int32
	bridge.WrapCuePassForTest(w, func(next bridge.CuePass) bridge.CuePass {
		return func(ctx context.Context, m string, opts bridge.WriteBackOptions) (bridge.WriteBackResult, error) {
			passes.Add(1)
			return next(ctx, m, opts)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	time.Sleep(150 * time.Millisecond)
	return w, &passes, cancel, done
}

func TestP3WatcherWritesOnceAfterQuietPeriod(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 8)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	_, passes, cancel, done := p3StartWatcher(t, mount, 150*time.Millisecond, 100*time.Millisecond, t.TempDir())
	p3DeckCue(t, db, p3FirstTrackID(t, db), "watcher cue", 1, 1000)
	_ = db.Close()
	p3WaitCue(t, mount, "watcher cue", 4*time.Second)
	time.Sleep(350 * time.Millisecond)
	if got := passes.Load(); got != 1 {
		t.Fatalf("watcher ran %d passes for one edit burst", got)
	}
	cancel()
	<-done
}

func TestP3WatcherDefersConversionAndFinalPassWritesEjectEdit(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 8)
	bridgetest.Import(t, mount)
	runDir := t.TempDir()
	flag := filepath.Join(runDir, "enginex-usb-preparing")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db := p3OpenDB(t, mount)
	w, _, cancel, done := p3StartWatcher(t, mount, 120*time.Millisecond, 100*time.Millisecond, runDir)
	defer func() { cancel(); <-done }()
	p3DeckCue(t, db, p3FirstTrackID(t, db), "after conversion", 1, 1200)
	_ = db.Close()
	time.Sleep(450 * time.Millisecond)
	if p3AnalysisHas(t, mount, "after conversion") {
		t.Fatal("watcher wrote while conversion was marked in progress")
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	p3WaitCue(t, mount, "after conversion", 3*time.Second)
	// edisksd_hook creates enginex-rb-ejected-* on eager eject (and clears it if umount fails);
	// cuewatch checks that marker so a failed eject does not permanently disable write-back.
	ejectMark := filepath.Join(runDir, bridge.EjectedNameForTest(mount))
	if err := os.WriteFile(ejectMark, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db = p3OpenDB(t, mount)
	p3DeckCue(t, db, p3FirstTrackID(t, db), "at eject", 1, 1400)
	// Engine's trigger dates a row to the second, and p3DeckCue adds a fixed 7: two edits within
	// one second would carry the same time, which a deck's 5 s quiet period rules out.
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+100 WHERE id=?`, p3FirstTrackID(t, db)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	time.Sleep(450 * time.Millisecond)
	if p3AnalysisHas(t, mount, "at eject") {
		t.Fatal("watcher wrote while eject was marked")
	}
	res, err := w.Final(context.Background(), mount)
	if err != nil || res.TracksChanged != 1 || !p3AnalysisHas(t, mount, "at eject") {
		t.Fatalf("final eject pass: %+v %v", res, err)
	}
}
