package bridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestConvertAtMountKeepsOverviewDeadlineWhenImportFails(t *testing.T) {
	if InChildForTest(t) {
		return
	}
	oldHookBudget := maxMountHookBudget
	maxMountHookBudget = 2 * time.Second
	t.Cleanup(func() { maxMountHookBudget = oldHookBudget })
	_, root, db := testlibrary.Create(t)
	if _, err := db.Exec(`INSERT INTO Playlist(id,title) VALUES(1,'Native')`); err != nil {
		t.Fatal(err)
	}
	testfixture.Export(t, root)
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", []byte("invalid export"))
	opts := enginedb.Options{}
	start := time.Now()
	if _, err := convertAtMount(context.Background(), root, &opts, false); err == nil {
		t.Fatal("invalid export was accepted")
	}
	if opts.Deadline.IsZero() {
		t.Fatal("overview budget was not retained; later mount passes would start a fresh budget")
	}
	if opts.Deadline.Before(start) || opts.Deadline.After(time.Now().Add(maxMountHookBudget)) {
		t.Fatalf("deadline outside the shared mount budget: %v", opts.Deadline)
	}
}

func TestPruneRGBOverviewsRespectsMountBudget(t *testing.T) {
	_, root, _ := testlibrary.Create(t)
	dir := filepath.Join(root, "Engine Library", "Database2")
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	start := time.Now()
	pruneRGBOverviews(ctx, dir, filepath.Join(dir, "m.db"), "")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("expired prune context ran for %s", elapsed)
	}
	deadline := mountPruneDeadline(time.Now(), time.Time{})
	if time.Until(deadline) > mountPruneBudget {
		t.Fatalf("prune deadline exceeds sub-budget: %v", deadline)
	}
}

func TestConvertAtMountHandsOffBeforeHookKillWhenPruneConsumesBudget(t *testing.T) {
	_, root, _ := testlibrary.Create(t)
	testfixture.Export(t, root)
	oldBudget, oldReserve := maxMountHookBudget, quickMountReserve
	maxMountHookBudget, quickMountReserve = time.Second, time.Second
	t.Cleanup(func() { maxMountHookBudget, quickMountReserve = oldBudget, oldReserve })
	start := time.Now()
	_, _ = convertAtMount(context.Background(), root, &enginedb.Options{}, false)
	if elapsed := time.Since(start); elapsed > maxMountHookBudget+500*time.Millisecond {
		t.Fatalf("mount path exceeded its deadline: %s", elapsed)
	}
}
