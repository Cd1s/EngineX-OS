package enginedb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Issue #264: SyncResolved previously discarded prepared tracks on Resume if inTime
// was false (!inTime condition), leading to Updated=0 and an unbreakable deadlock.
// On Resume, any track prepared before cutoff must make progress.
func TestIssue264ResumeNeverDeadlocksOnExpiredDeadline(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 10)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	ctx := context.Background()

	// Initial fast import: all 10 tracks deferred with fast: fingerprint.
	r1, err := Sync(ctx, root, l, Options{Fast: true, DatabaseDir: dir})
	if err != nil || r1.Added != 10 {
		t.Fatalf("fast import: %+v %v", r1, err)
	}

	// Resume with a deadline already in the past (simulating expired deadline).
	// Under the bug, !inTime caused track 0 to be deferred, leaving Updated=0, Deferred=10.
	// With the fix, Resume guarantees progress (Updated > 0).
	pastDeadline := time.Now().Add(-10 * time.Second)
	r2, err := Sync(ctx, root, l, Options{Resume: true, DatabaseDir: dir, Deadline: pastDeadline})
	if err != nil {
		t.Fatalf("resume sync: %v", err)
	}
	if r2.Updated == 0 {
		t.Fatalf("resume made 0 progress with expired deadline: %+v", r2)
	}
}
