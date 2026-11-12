package bridge_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// Review of 1ec6382: Hot Cue and Relocate edits made in the Web Console while Engine had the
// library open wait in Database2/enginex-pending.json. They were applied only by
// engine-guardian --heal-mount, which the mount hook does not run on a stick with a rekordbox
// export, so on a converted stick they waited for ever. rb-bridge applies them at the mount,
// also when the export is already imported (stamp bypass).
func TestConvertAtMountAppliesQueuedEdits(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !r.Bypassed {
		t.Fatalf("second mount: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	var id int64
	if err := db.QueryRow(`SELECT trackId FROM PerformanceData WHERE quickCues IS NOT NULL ORDER BY trackId LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := enginedb.QueuePending(root, func(p *enginedb.Pending) {
		p.HotCues = append(p.HotCues, enginedb.PendingCue{TrackID: id, Index: 7, Label: "queued on the deck", Color: "#FF0000"})
	}); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(root, "Engine Library", "Database2", enginedb.PendingFile)
	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil || !r.Bypassed {
		t.Fatalf("mount with queued edits: %+v %v", r, err)
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("the queued edits are still waiting after the mount: %v", err)
	}
	var blob []byte
	if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, id).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	raw, err := enginedb.QUncompress(blob, 64<<10)
	if err != nil || !bytes.Contains(raw, []byte("queued on the deck")) {
		t.Fatalf("the queued Hot Cue is not in m.db (%v)", err)
	}
}
