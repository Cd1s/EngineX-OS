// Package bridgetest gives other packages' tests a stick as the rekordbox bridge leaves it.
package bridgetest

import (
	"context"
	"testing"

	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge/v2"
)

// Import converts the export on mount the way a deck does over an insert: the mount window,
// the background job, and the window of the job's remount. It fails the test when the library
// is not complete afterwards.
func Import(t testing.TB, mount string) {
	t.Helper()
	ctx := context.Background()
	if _, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{}); err != nil {
		t.Fatalf("mount window: %v", err)
	}
	if _, err := bridge.FillInBackground(ctx, mount, bridge.FillOptions{}); err != nil {
		t.Fatalf("background job: %v", err)
	}
	res, err := bridge.ConvertAtMount(ctx, mount, bridge.MountOptions{Publish: true})
	if err != nil {
		t.Fatalf("publishing window: %v", err)
	}
	if !res.Pending.None() && !res.Bypassed {
		t.Fatalf("the import is not complete: %+v", res.Pending)
	}
}
