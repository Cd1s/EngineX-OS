package bridge

import (
	"context"
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
	start := time.Unix(100, 0)
	old, oldReserve := maxMountHookBudget, quickMountReserve
	quickMountReserve = 8 * time.Second
	maxMountHookBudget = 14 * time.Second
	t.Cleanup(func() { maxMountHookBudget, quickMountReserve = old, oldReserve })
	got := mountPruneDeadline(start, time.Time{})
	if got.After(start.Add(2 * time.Second)) {
		t.Fatalf("prune deadline=%v, exceeds the two-second mount sub-budget", got)
	}
}

func TestConvertAtMountHandsOffBeforeHookKillWhenPruneConsumesBudget(t *testing.T) {
	start := time.Now().Add(-1500 * time.Millisecond)
	old := maxMountHookBudget
	maxMountHookBudget = 2 * time.Second
	t.Cleanup(func() { maxMountHookBudget = old })
	if left := time.Until(start.Add(maxMountHookBudget)); left >= quickMountReserveForBudget() {
		t.Fatalf("test setup left %s, want less than reserve %s", left, quickMountReserveForBudget())
	}
	if deadline := mountPruneDeadline(start, time.Time{}); !deadline.Before(time.Now()) {
		t.Fatalf("prune deadline=%v, want an expired deadline after the budget was consumed", deadline)
	}
}
