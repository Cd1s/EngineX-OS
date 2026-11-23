package bridge_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// Stamp bypass must not materialize all cover BLOBs again. The files already
// exist; loading the 128 MiB payload serves no purpose on this mount.
func TestIssue233StampBypassDoesNotReloadMigratedArtwork(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	root := t.TempDir()
	testfixture.LargeExport(t, root, 1)
	issue233Mount(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	dir := filepath.Join(root, "Engine Library/Artwork")
	data := make([]byte, 128<<10)
	copy(data, []byte{0xff, 0xd8, 0xff})
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i <= 1024; i++ {
		sum := make([]byte, 20)
		binary.BigEndian.PutUint64(sum, uint64(i))
		if _, err := tx.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, sum, data); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, base64.RawURLEncoding.EncodeToString(sum)+".jpg"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r := issue233Mount(t, root, enginedb.Options{})
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("1024 migrated covers: allocated=%d payload=%d", allocated, len(data)*1024)
	if !r.Bypassed {
		t.Fatal("unchanged export was not bypassed")
	}
	if allocated > 32<<20 {
		t.Fatalf("stamp bypass allocated %d bytes for already migrated covers", allocated)
	}
}

func TestStageImportAndUpdateKeepOverviewProgressAndFastIndex(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	root := t.TempDir()
	testfixture.LargeExport(t, root, 12)
	restore := bridge.SetLargeExportForTest()
	defer restore()
	ctx := context.Background()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.Background {
		t.Fatalf("first import deferred mount: %+v %v", r, err)
	}
	if _, err = bridge.ImportInBackground(ctx, root, nil); err != nil {
		t.Fatalf("firstImportInBackground: %v", err)
	}
	r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.PlaylistsChanged || !r.Background {
		t.Fatalf("firstImport publish mount: %+v %v", r, err)
	}
	if !enginedb.ReadOverviewProgress(root).Done() {
		t.Fatal("stage firstImport did not mark OverviewProgress done")
	}
	if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("firstImport background fill: %+v %v", r, err)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("firstImport publishUpdate: %+v %v", r, err)
	}
	if !enginedb.ReadOverviewProgress(root).Done() {
		t.Fatal("firstImport publishUpdate did not preserve OverviewProgress done")
	}

	db := dbOpen(t, root)
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT EXISTS(SELECT 1 FROM bridge.BridgeState WHERE substr(fingerprint,1,5)='fast:')`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			rows.Close()
			db.Close()
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	rows.Close()
	db.Close()
	if !strings.Contains(plan.String(), "index_BridgeState_fast") {
		t.Fatalf("expected index_BridgeState_fast in query plan, got:\n%s", plan.String())
	}

	testfixture.LargeExport(t, root, 16)
	r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.Background || r.Added != 4 {
		t.Fatalf("quick update mount: %+v %v", r, err)
	}
	if !enginedb.ReadOverviewProgress(root).Done() {
		t.Fatal("quickUpdate did not preserve OverviewProgress done")
	}

	r, err = bridge.ImportInBackground(ctx, root, nil)
	if err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || r.Background {
		t.Fatalf("publishUpdate mount: %+v %v", r, err)
	}
	if !enginedb.ReadOverviewProgress(root).Done() {
		t.Fatal("publishUpdate did not preserve OverviewProgress done")
	}
}
