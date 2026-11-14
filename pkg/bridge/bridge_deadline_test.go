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
