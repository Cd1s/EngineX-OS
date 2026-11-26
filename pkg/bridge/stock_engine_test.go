package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

type treeDigest map[string]string

func snapshotTree(t *testing.T, root string) treeDigest {
	t.Helper()
	out := make(treeDigest)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			out[filepath.ToSlash(rel)] = "dir"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = fmt.Sprintf("file:%d:%s", len(b), hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotTree(%s): %v", root, err)
	}
	return out
}

func makeStockEngineStick(t *testing.T, mount string) {
	t.Helper()
	testlibrary.ExportDrive(t, mount)
	// Add stock OverviewData and Artwork files as Engine DJ writes them.
	ov := filepath.Join(mount, "Engine Library", "Database2", "OverviewData", "gate-internal-library-uuid", "1.rgb")
	if err := os.MkdirAll(filepath.Dir(ov), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ov, []byte("stock-rgb-waveform"), 0o644); err != nil {
		t.Fatal(err)
	}
	art := filepath.Join(mount, "Engine Library", "Artwork", "stockart.jpg")
	if err := os.MkdirAll(filepath.Dir(art), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(art, []byte("\xff\xd8\xff\xd9"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Item 6 proof: a stock Engine DJ export stick (Engine Library/Database2/m.db with NO
// PIONEER/ folder) must be completely unaffected across FindSources, ConvertAtMount
// (both OnDemand and Full), FillInBackground, CueWatcher, WriteBackCues, and
// OnDemandBuilder: zero on-demand results, zero write-back passes, zero snapshot/stage/
// .rgb/.rb-bridge-browse writes, and a byte-identical filesystem tree.
func TestStockEngineStickCompletelyUnaffected(t *testing.T) {
	stageDir := filepath.Join(t.TempDir(), "stage")
	t.Setenv(rbstate.StageEnv, stageDir)

	mediaRoot := t.TempDir()
	mount := filepath.Join(mediaRoot, "ENGINE_STOCK")
	makeStockEngineStick(t, mount)
	before := snapshotTree(t, mount)

	// 1. FindSources (pkg/bridge/source.go:40-62) returns 0 sources.
	r, err := compat.NewResolver(mount)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	files, err := FindSources(r, mount)
	if err != nil || len(files) != 0 {
		t.Fatalf("FindSources on stock Engine stick = (%v, %v), want empty", files, err)
	}

	// 2. ConvertAtMount (pkg/bridge/mount.go:297-303) bypasses immediately in both OnDemand and Full modes.
	var bgStarts atomic.Int32
	for _, opts := range []MountOptions{
		{OnDemand: true, StartBackground: func() error { bgStarts.Add(1); return nil }},
		{Full: true, StartBackground: func() error { bgStarts.Add(1); return nil }},
		{Publish: true, OnDemand: true, StartBackground: func() error { bgStarts.Add(1); return nil }},
	} {
		res, err := ConvertAtMount(context.Background(), mount, opts)
		if err != nil {
			t.Fatalf("ConvertAtMount(%+v) error: %v", opts, err)
		}
		if !res.Bypassed || res.Added != 0 || res.Updated != 0 || res.Removed != 0 || res.Background || res.Applied != 0 || res.PlaylistsChanged {
			t.Fatalf("ConvertAtMount(%+v) unexpected result: %+v", opts, res)
		}
	}
	if bgStarts.Load() != 0 {
		t.Fatalf("StartBackground called %d times on stock Engine stick, want 0", bgStarts.Load())
	}

	// 3. FillInBackground (pkg/bridge/fill.go:128-144) fails closed with ErrNoSnapshot and creates no stage.
	for _, onDemand := range []bool{false, true} {
		fres, err := FillInBackground(context.Background(), mount, FillOptions{OnDemand: onDemand})
		if !errors.Is(err, rbstate.ErrNoSnapshot) || fres.Staged != 0 || fres.Publish {
			t.Fatalf("FillInBackground(onDemand=%v) = (%+v, %v), want ErrNoSnapshot", onDemand, fres, err)
		}
	}
	if entries, err := os.ReadDir(stageDir); err == nil && len(entries) > 0 {
		t.Fatalf("stage directory should stay empty for stock Engine stick, found %d entries", len(entries))
	}

	// 4. OnDemandBuilder (pkg/bridge/ondemand.go:516-524,755-765) refuses with ErrOnDemandNotFound.
	od := NewOnDemandBuilderWithConfig(OnDemandBuilderConfig{MediaRoot: mediaRoot})
	if res, err := od.Build(context.Background(), OnDemandRequest{
		Mount:      mount,
		UUID:       "gate-internal-library-uuid",
		EnginePath: "../../Music/Track001.wav",
		EngineID:   1,
	}); !errors.Is(err, ErrOnDemandNotFound) || res != nil {
		t.Fatalf("OnDemandBuilder.Build on stock Engine stick = (%+v, %v), want ErrOnDemandNotFound", res, err)
	}

	// 5. CueWatcher (pkg/bridge/cuewatch.go:65-72,92-98) ignores stock Engine stick -> 0 passes.
	if rekordboxStick(mount) {
		t.Fatal("rekordboxStick returned true for stock Engine stick")
	}
	t.Cleanup(SetCueRunDirForTest(t.TempDir()))
	w := CueWatcherForTest([]string{mount}, 50*time.Millisecond, 50*time.Millisecond)
	var wbPasses atomic.Int32
	WrapCuePassForTest(w, func(next CuePass) CuePass {
		return func(ctx context.Context, m string, opts WriteBackOptions) (WriteBackResult, error) {
			wbPasses.Add(1)
			return next(ctx, m, opts)
		}
	})
	wctx, wcancel := context.WithCancel(context.Background())
	wdone := make(chan struct{})
	go func() {
		defer close(wdone)
		_ = w.Run(wctx)
	}()
	// Even after touching m.db mtime, CueWatcher must never attach or run write-back.
	mdbPath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	stBefore, err := os.Stat(mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	future := stBefore.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(mdbPath, future, future); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	wcancel()
	<-wdone
	if wbPasses.Load() != 0 {
		t.Fatalf("CueWatcher ran %d write-back passes on stock Engine stick, want 0", wbPasses.Load())
	}
	if err := os.Chtimes(mdbPath, stBefore.ModTime(), stBefore.ModTime()); err != nil {
		t.Fatal(err)
	}

	// 6. Direct WriteBackCues (pkg/bridge/writeback.go:326-328) skips cleanly with zero writes.
	wres, err := WriteBackCues(context.Background(), mount, WriteBackOptions{SkipPreparingCheck: true})
	if err != nil || wres.TracksChecked != 0 || wres.TracksRead != 0 || wres.TracksChanged != 0 || wres.FilesUpdated != 0 || wres.OneLibraryUpdated {
		t.Fatalf("WriteBackCues on stock Engine stick = (%+v, %v), want zero activity", wres, err)
	}

	// 7. Verify NO bridge files were created and every existing file is byte-identical.
	for _, forbidden := range []string{
		"Engine Library/Database2/.rb-bridge-snapshot",
		"Engine Library/Database2/.rb-bridge-snapshot.swap",
		"Engine Library/Database2/.rb-bridge-browse",
		"Engine Library/Database2/.rb-next.db",
		"Engine Library/Database2/.rb-bridge-writeback",
		".enginex-backup",
	} {
		if _, err := os.Lstat(filepath.Join(mount, filepath.FromSlash(forbidden))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected bridge artifact %s on stock Engine stick: %v", forbidden, err)
		}
	}
	after := snapshotTree(t, mount)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("stock Engine stick mutated:\nbefore=%v\nafter=%v", before, after)
	}
}

// Item 6 neighbour: a stock Engine stick carrying only CDJ player settings (PIONEER/MYSETTING.DAT)
// or EngineX's own CDJ-link generated export.pdb (.enginex-generated) is also bypassed untouched.
func TestStockEngineStickWithNonExportPioneerFolderUnaffected(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	mount := filepath.Join(t.TempDir(), "ENGINE_WITH_CDJ_SETTINGS")
	makeStockEngineStick(t, mount)

	// Bare PIONEER/MYSETTING.DAT (no rekordbox/ directory).
	myset := filepath.Join(mount, "PIONEER", "MYSETTING.DAT")
	if err := os.MkdirAll(filepath.Dir(myset), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(myset, []byte("cdj-settings"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Also place EngineX's own Pro DJ Link export.pdb with matching .enginex-generated stamp.
	pdb := filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")
	if err := os.MkdirAll(filepath.Dir(pdb), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pdb, []byte("generated-by-prolink"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	genMark := filepath.Join(mount, "PIONEER", "rekordbox", ".enginex-generated")
	if err := os.WriteFile(genMark, []byte(fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())), 0o644); err != nil {
		t.Fatal(err)
	}

	before := snapshotTree(t, mount)
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{OnDemand: true})
	if err != nil || !res.Bypassed || res.Added != 0 || res.Background {
		t.Fatalf("ConvertAtMount on Engine stick with generated export = (%+v, %v), want bypassed", res, err)
	}
	wres, err := WriteBackCues(context.Background(), mount, WriteBackOptions{SkipPreparingCheck: true})
	if err != nil || wres.TracksChecked != 0 || wres.FilesUpdated != 0 {
		t.Fatalf("WriteBackCues = (%+v, %v), want zero activity", wres, err)
	}
	after := snapshotTree(t, mount)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("stock Engine stick with non-export PIONEER folder mutated")
	}
}
