package bridge_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestPublishGeneratesAndPreservesRGBOverviews(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	root := t.TempDir()
	testfixture.LargeExport(t, root, 5)
	defer bridge.SetLargeExportForTest()()

	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	if _, err := bridge.ImportInBackground(ctx, root, nil); err != nil {
		t.Fatalf("fast build: %v", err)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("publish fast: %+v %v", r, err)
	}
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background fill: %+v %v", r, err)
	}
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatalf("publish full: %v", err)
	}

	dir := filepath.Join(root, "Engine Library", "Database2")
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var uuid string
	if err := db.QueryRow(`SELECT uuid FROM Information LIMIT 1`).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT id, path FROM Track ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 5 {
		t.Fatalf("got %d tracks, want 5", len(ids))
	}

	for _, id := range ids {
		rgbPath := filepath.Join(dir, "OverviewData", uuid, strconv.FormatInt(id, 10)+".rgb")
		b, err := os.ReadFile(rgbPath)
		if err != nil {
			t.Fatalf("missing RGB overview for track %d: %v", id, err)
		}
		raw, err := enginedb.QUncompress(b, 1<<20)
		if err != nil || !enginedb.ValidRGBOverview(raw) {
			t.Fatalf("invalid RGB overview for track %d: len=%d err=%v", id, len(raw), err)
		}
	}

	// Subsequent mount prune must keep all 5 generated RGB overviews.
	removed, err := enginedb.PruneRGBOverviews(ctx, dir, filepath.Join(dir, "m.db"), "")
	if err != nil || removed != 0 {
		t.Fatalf("PruneRGBOverviews removed=%d err=%v, want 0", removed, err)
	}

	// Simulate an empty (0-byte) Engine RGB file: BackfillStageRGBOverviews repairs it.
	brokenPath := filepath.Join(dir, "OverviewData", uuid, strconv.FormatInt(ids[0], 10)+".rgb")
	if err := os.WriteFile(brokenPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Trigger a re-export touch and verify backfill replaces the 0-byte file on publish.
	pdb := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	info, err := os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdb, info.ModTime().Add(10_000_000_000), info.ModTime().Add(10_000_000_000)); err != nil {
		t.Fatal(err)
	}
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("re-export mount: %+v %v", r, err)
	}
	if r, err := bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("re-export background: %+v %v", r, err)
	}
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatalf("re-export publish: %v", err)
	}
	b, err := os.ReadFile(brokenPath)
	if err != nil || len(b) <= 8 {
		t.Fatalf("0-byte RGB overview was not repaired on publish: len=%d err=%v", len(b), err)
	}
	if raw, err := enginedb.QUncompress(b, 1<<20); err != nil || !enginedb.ValidRGBOverview(raw) {
		t.Fatalf("repaired RGB overview invalid: err=%v", err)
	}
}
