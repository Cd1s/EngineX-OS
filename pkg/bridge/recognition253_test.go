package bridge_test

import (
	"context"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestLargePDBRepeatMountTimings253(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	root := t.TempDir()
	testfixture.LargeExport(t, root, 6000)
	start := time.Now()
	// A library a build before #301 imported from the export alone; mounts read its files.
	r, err := bridge.ImportLikeEarlierBuildsForTest(context.Background(), root, enginedb.Options{})
	if err != nil || r.Added != 6000 {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	t.Logf("6000-track first mount: %s added=%d", time.Since(start), r.Added)
	for mount := 2; mount <= 3; mount++ {
		start = time.Now()
		r, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{Deadline: start.Add(50 * time.Millisecond)}, false)
		if err != nil || r.Added != 0 || r.Updated == 0 || r.Deferred == 0 {
			t.Fatalf("mount %d: %+v %v", mount, r, err)
		}
		t.Logf("6000-track mount %d: %s updated=%d deferred=%d (50ms file-reading allowance, commit included)", mount, time.Since(start), r.Updated, r.Deferred)
	}
}

func TestDefaultMountCompressionThroughputDelta(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	root := t.TempDir()
	testfixture.LargeExport(t, root, 6000)
	ctx := context.Background()
	if r, err := bridge.ImportLikeEarlierBuildsForTest(ctx, root, enginedb.Options{}); err != nil || r.Added != 6000 {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	start := time.Now()
	// The update of a large export goes on after the mount (#301), and reads every file there.
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.Background {
		t.Fatalf("default-budget repeat: %+v %v", r, err)
	}
	t.Logf("6000-track default-budget repeat: elapsed=%s", time.Since(start))
	start = time.Now()
	if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Added != 0 || r.Updated == 0 || r.Deferred != 0 {
		t.Fatalf("update after the mount: %+v %v", r, err)
	}
	t.Logf("6000-track update after the mount: elapsed=%s updated=%d", time.Since(start), r.Updated)
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background || !r.PlaylistsChanged {
		t.Fatalf("mount publishing the update: %+v %v", r, err)
	}
}
