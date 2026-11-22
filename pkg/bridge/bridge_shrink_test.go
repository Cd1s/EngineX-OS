package bridge_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestIssue272LegacyCappedStateDBShrinksAndBackfills(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const totalTracks = 40
	const legacyKept = 25
	const capBytes int64 = 64 << 10

	testfixture.LargeExport(t, root, totalTracks)
	ctx := context.Background()

	r0, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true)
	if err != nil {
		t.Fatalf("initial import: %v", err)
	}
	if r0.Added != totalTracks {
		t.Fatalf("initial import added %d, want %d", r0.Added, totalTracks)
	}

	dir := filepath.Join(root, "Engine Library", "Database2")
	statePath := filepath.Join(dir, ".rb-bridge-state.db")
	stateDB, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}

	// Downgrade the fixture DB to pre-RBA1 state at/above capBytes:
	// first 25 tracks have legacy plain/gzip JSON + large audioJSON,
	// remaining 15 tracks have no BridgeAnalysis row (capped on old build).
	if _, err := stateDB.Exec(`DELETE FROM BridgeLibrary WHERE key IN ('analysis_format','rba1_backfill_checked')`); err != nil {
		t.Fatal(err)
	}
	rows, err := stateDB.Query(`SELECT trackId, sourceJSON FROM BridgeAnalysis ORDER BY trackId`)
	if err != nil {
		t.Fatal(err)
	}
	type rowItem struct {
		id  int64
		src []byte
	}
	var all []rowItem
	for rows.Next() {
		var it rowItem
		if err := rows.Scan(&it.id, &it.src); err != nil {
			t.Fatal(err)
		}
		all = append(all, it)
	}
	rows.Close()
	if len(all) != totalTracks {
		t.Fatalf("expected %d BridgeAnalysis rows, got %d", totalTracks, len(all))
	}

	largeAudio := []byte(`{"pad":"` + strings.Repeat("a", 4096) + `"}`)
	for i, it := range all {
		if i >= legacyKept {
			if _, err := stateDB.Exec(`DELETE FROM BridgeAnalysis WHERE trackId=?`, it.id); err != nil {
				t.Fatal(err)
			}
			continue
		}
		plain, err := enginedb.UnpackAnalysis(it.src, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		srcBlob := plain
		if i%2 == 1 {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			if _, err := gz.Write(plain); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			srcBlob = buf.Bytes()
		}
		if _, err := stateDB.Exec(`UPDATE BridgeAnalysis SET sourceJSON=?, audioJSON=? WHERE trackId=?`, srcBlob, largeAudio, it.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := stateDB.Close(); err != nil {
		t.Fatal(err)
	}

	stBefore, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if stBefore.Size() < capBytes {
		t.Fatalf("expected downgraded DB size >= %d, got %d", capBytes, stBefore.Size())
	}

	// Run up to N=5 mount conversions:
	// - conversion 1 rewrites legacy rows to RBA1, vacuums below cap, and marks capped tracks;
	// - following conversion(s) backfill shadow detail for the capped tracks.
	const maxConversions = 5
	opts := enginedb.Options{MaxBridgeStateBytes: capBytes}
	completed := false
	for step := 1; step <= maxConversions; step++ {
		if _, err := bridge.ConvertAtMount(ctx, root, opts, false); err != nil {
			t.Fatalf("conversion %d: %v", step, err)
		}
		checkDB, err := sql.Open("sqlite", statePath)
		if err != nil {
			t.Fatal(err)
		}
		var analysisCount, fastCount int
		_ = checkDB.QueryRow(`SELECT count(*) FROM BridgeAnalysis`).Scan(&analysisCount)
		_ = checkDB.QueryRow(`SELECT count(*) FROM BridgeState WHERE fingerprint LIKE 'fast:%'`).Scan(&fastCount)
		checkDB.Close()

		stNow, err := os.Stat(statePath)
		if err != nil {
			t.Fatal(err)
		}
		if analysisCount == totalTracks && fastCount == 0 && stNow.Size() < capBytes {
			completed = true
			break
		}
	}
	if !completed {
		t.Fatalf("DB did not shrink below cap and complete all %d tracks within %d conversions", totalTracks, maxConversions)
	}

	stAfter, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if stAfter.Size() >= capBytes {
		t.Fatalf("expected final state DB < %d, got %d", capBytes, stAfter.Size())
	}

	finalStateDB, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer finalStateDB.Close()

	rows2, err := finalStateDB.Query(`SELECT trackId, sourceJSON FROM BridgeAnalysis`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows2.Close()
	verified := 0
	for rows2.Next() {
		var id int64
		var src []byte
		if err := rows2.Scan(&id, &src); err != nil {
			t.Fatal(err)
		}
		if _, err := enginedb.UnpackAnalysis(src, 64<<20); err != nil {
			t.Fatalf("track %d UnpackAnalysis after shrink/backfill: %v", id, err)
		}
		verified++
	}
	if verified != totalTracks {
		t.Fatalf("verified %d BridgeAnalysis rows, want %d", verified, totalTracks)
	}
}
