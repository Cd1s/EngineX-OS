package enginedb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// P2-1: a populated library takes the non-Fast path, but an expired deadline
// must still commit all newly exported rows without opening all their MP3s.
func TestIssue233ExpiredIncrementalBudgetDoesNotReadEveryNewMP3(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 1)
	read := func() *rekordbox.Library {
		l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	ctx := context.Background()
	if _, err := Sync(ctx, root, read(), Options{}); err != nil {
		t.Fatal(err)
	}
	testfixture.LargeExport(t, root, 3000)
	old := audioHeader
	t.Cleanup(func() { audioHeader = old })
	headers := 0
	audioHeader = func(p string) (compat.AudioInfo, error) { headers++; return old(p) }
	// The completed existing row is not deferred by a new export (#233 B1).
	r, err := Sync(ctx, root, read(), Options{Deadline: time.Now().Add(-time.Second)})
	if err != nil || r.Added != 2999 || r.Deferred != 2999 {
		t.Fatalf("expired incremental import: %+v %v", r, err)
	}
	// prepareTracks deliberately prepares one track even after the cut-off;
	// every other row must remain export-only until a later mount.
	if headers != 1 {
		t.Fatalf("expired budget opened %d MP3 headers, want 1", headers)
	}
	t.Logf("new rows=%d deferred=%d MP3 headers=%d", r.Added, r.Deferred, headers)
}
