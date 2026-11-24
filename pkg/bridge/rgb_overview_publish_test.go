package bridge_test

import (
	"bytes"
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

	// Simulate an empty (0-byte) Engine RGB file on ids[0] (repaired on publish)
	// and a valid pre-existing Engine RGB sidecar on ids[1] (must never be replaced,
	// even when the stage also holds a generated <id>.rgb for ids[1]).
	brokenPath := filepath.Join(dir, "OverviewData", uuid, strconv.FormatInt(ids[0], 10)+".rgb")
	if err := os.WriteFile(brokenPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	preservedPath := filepath.Join(dir, "OverviewData", uuid, strconv.FormatInt(ids[1], 10)+".rgb")
	origB, err := os.ReadFile(preservedPath)
	if err != nil {
		t.Fatal(err)
	}
	origRaw, err := enginedb.QUncompress(origB, 1<<20)
	if err != nil || !enginedb.ValidRGBOverview(origRaw) {
		t.Fatalf("initial RGB overview invalid: %v", err)
	}
	makeValidRGB := func(marker byte) []byte {
		buf := append([]byte(nil), origRaw[:24]...)
		buf = append(buf, bytes.Repeat([]byte{marker}, len(origRaw)-24)...)
		comp, cerr := enginedb.QCompress(buf)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return comp
	}
	engineRGB := makeValidRGB(0x42)
	stagedRGB := makeValidRGB(0x19)
	if err := os.WriteFile(preservedPath, engineRGB, 0o644); err != nil {
		t.Fatal(err)
	}
	// Trigger a re-export touch and verify backfill repairs the 0-byte file while
	// publish preserves the valid pre-existing destination sidecar on ids[1].
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
	stageRGBDir := filepath.Join(dir, ".rb-bridge-import", "OverviewData", uuid)
	if err := os.MkdirAll(stageRGBDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageRGBDir, strconv.FormatInt(ids[1], 10)+".rgb"), stagedRGB, 0o644); err != nil {
		t.Fatal(err)
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
	gotPreserved, err := os.ReadFile(preservedPath)
	if err != nil {
		t.Fatalf("read preserved RGB overview: %v", err)
	}
	if !bytes.Equal(gotPreserved, engineRGB) {
		t.Fatalf("publish replaced valid destination RGB overview on track %d", ids[1])
	}
}
