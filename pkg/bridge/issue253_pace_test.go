package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #253 review 4: the background job reads every ANLZ file it checks whole, so its reads are
// paced: after each read it waits as long as the read took and at least detailReadGap. A stop of
// the job confirms nothing more, and the tracks not confirmed keep their copies.
func TestIssue253ChecksArePacedAndStopCleanly(t *testing.T) {
	if InChildForTest(t) {
		return
	}
	defer func(old time.Duration) { detailReadGap = old }(detailReadGap)
	detailReadGap = 20 * time.Millisecond
	ctx := context.Background()
	pace := detailPace(ctx)
	for _, took := range []time.Duration{time.Millisecond, 50 * time.Millisecond} {
		start := time.Now()
		if err := pace(1, took); err != nil {
			t.Fatal(err)
		}
		if waited, want := time.Since(start), max(took, detailReadGap); waited < want {
			t.Fatalf("after a read of %s the job waited %s, want %s", took, waited, want)
		}
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	start := time.Now()
	if err := detailPace(stopped)(1, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("a stopped job's pause: %v after %s", err, time.Since(start))
	}

	root := t.TempDir()
	testfixture.LargeExport(t, root, 3)
	l, err := readExport(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[uint32]bool{}
	for id := range enginedb.ExportRefs(l) {
		keys[id] = true
	}
	if refs, err := confirmedRefs(stopped, r, l, keys, map[string]bool{}); err == nil || len(refs) != 0 {
		t.Fatalf("a stopped check confirmed %d tracks (%v)", len(refs), err)
	}
	refs, err := confirmedRefs(ctx, r, l, keys, map[string]bool{})
	if err != nil || len(refs) != len(keys) || len(keys) != 3 {
		t.Fatalf("check: %d of %d confirmed (%v)", len(refs), len(keys), err)
	}
}
