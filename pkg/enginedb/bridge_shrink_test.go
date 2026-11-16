package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func createLegacyStateDBForShrinkTest(t *testing.T, dir string, count int, padBytes int) string {
	t.Helper()
	statePath := filepath.Join(dir, stateFile)
	db, err := openStateDB(context.Background(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(strings.ReplaceAll(bridgeSchema, "bridge.", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO BridgeLibrary(key,value) VALUES('version','1')`); err != nil {
		t.Fatal(err)
	}

	padding := strings.Repeat("x", padBytes)
	wv := make([]byte, 30)
	for i := range wv {
		wv[i] = byte(i + 1)
	}
	for i := 1; i <= count; i++ {
		payload := struct {
			Track    struct{ ID uint32 }
			Analysis *rekordbox.Analysis
		}{
			Track: struct{ ID uint32 }{ID: uint32(100 + i)},
			Analysis: &rekordbox.Analysis{
				Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: wv}},
			},
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		sourceBlob := raw
		if i%2 == 0 {
			sourceBlob, err = packAnalysis(raw)
			if err != nil {
				t.Fatal(err)
			}
		}
		audioBlob := []byte(`{"pad":"` + padding + `"}`)
		if _, err := db.Exec(`INSERT INTO BridgeAnalysis(trackId,sourceJSON,audioJSON) VALUES(?,?,?)`, i, sourceBlob, audioBlob); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO BridgeState(path,trackId,fingerprint) VALUES(?,?,?)`, "../track"+strconv.Itoa(i)+".mp3", i, "fp"); err != nil {
			t.Fatal(err)
		}
	}
	return statePath
}

func assertDBReadableAndIntact(t *testing.T, statePath string, wantRows int) {
	t.Helper()
	db, err := openStateDB(context.Background(), statePath)
	if err != nil {
		t.Fatalf("openStateDB: %v", err)
	}
	defer db.Close()

	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, err = %v", integrity, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM BridgeAnalysis`).Scan(&count); err != nil || count != wantRows {
		t.Fatalf("BridgeAnalysis count = %d (want %d), err = %v", count, wantRows, err)
	}
}

func TestShrinkBridgeAnalysisPullOutMidRewrite(t *testing.T) {
	dir := t.TempDir()
	statePath := createLegacyStateDBForShrinkTest(t, dir, 20, 4096)
	ctx := context.Background()

	// Expired deadline simulates a mount whose time budget is already spent:
	// no rows rewritten, DB stays intact.
	res0, err := ShrinkBridgeAnalysis(ctx, "", ShrinkOptions{
		DatabaseDir: dir,
		Deadline:    time.Now().Add(-time.Second),
		BatchSize:   5,
	})
	if err != nil {
		t.Fatalf("expired deadline shrink: %v", err)
	}
	if res0.Rewritten != 0 || res0.Done {
		t.Fatalf("expected 0 rewritten and Done=false on expired deadline, got %+v", res0)
	}
	assertDBReadableAndIntact(t, statePath, 20)

	// Simulate pull-out after 2 batches (10 rows): cursor is at trackId=10,
	// first 10 rows are RBA1, remaining 10 rows are still legacy JSON.
	db, err := openStateDB(ctx, statePath)
	if err != nil {
		t.Fatal(err)
	}
	emptyAudio, err := packAnalysis([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		var src []byte
		if err := db.QueryRow(`SELECT sourceJSON FROM BridgeAnalysis WHERE trackId=?`, i).Scan(&src); err != nil {
			t.Fatal(err)
		}
		plain, err := UnpackAnalysis(src, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Track    struct{ ID uint32 }
			Analysis *rekordbox.Analysis
		}
		if err := json.Unmarshal(plain, &parsed); err != nil {
			t.Fatal(err)
		}
		packed, err := packBridgeSource(parsed.Track.ID, parsed.Analysis)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE BridgeAnalysis SET sourceJSON=?, audioJSON=? WHERE trackId=?`, packed, emptyAudio, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO BridgeLibrary(key,value) VALUES('rba1_cursor','10')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	assertDBReadableAndIntact(t, statePath, 20)

	// Next mount resumes from rba1_cursor=10, rewrites remaining 10 rows, and compacts.
	res, err := ShrinkBridgeAnalysis(ctx, "", ShrinkOptions{
		DatabaseDir:         dir,
		MaxBridgeStateBytes: 32 << 10,
		BatchSize:           5,
		FreeSpace:           func(string) (int64, error) { return 64 << 20, nil },
	})
	if err != nil {
		t.Fatalf("resume shrink: %v", err)
	}
	if res.Rewritten != 10 || !res.Compacted || !res.Done {
		t.Fatalf("unexpected shrink result: %+v", res)
	}
	assertDBReadableAndIntact(t, statePath, 20)

	db2, err := openStateDB(ctx, statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var fmtVal string
	if err := db2.QueryRow(`SELECT value FROM BridgeLibrary WHERE key='analysis_format'`).Scan(&fmtVal); err != nil || fmtVal != "rba1" {
		t.Fatalf("analysis_format = %q, err = %v", fmtVal, err)
	}
	var cursorCount int
	if err := db2.QueryRow(`SELECT count(*) FROM BridgeLibrary WHERE key='rba1_cursor'`).Scan(&cursorCount); err != nil || cursorCount != 0 {
		t.Fatalf("expected rba1_cursor deleted, count=%d err=%v", cursorCount, err)
	}
	rows, err := db2.Query(`SELECT trackId, sourceJSON, audioJSON FROM BridgeAnalysis ORDER BY trackId`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var src, aud []byte
		if err := rows.Scan(&id, &src, &aud); err != nil {
			t.Fatal(err)
		}
		if !isCompactRBA1Row(src, aud) {
			t.Fatalf("track %d not compact RBA1 after resume", id)
		}
	}
}

func TestShrinkBridgeAnalysisPullOutMidVacuum(t *testing.T) {
	dir := t.TempDir()
	statePath := createLegacyStateDBForShrinkTest(t, dir, 20, 4096)
	tmpPath := statePath + ".compact"

	// Simulate an interrupted VACUUM INTO that left a truncated temp file.
	if err := os.WriteFile(tmpPath, []byte("partial-sqlite-garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertDBReadableAndIntact(t, statePath, 20)

	// Simulate context cancellation right before vacuum (FreeSpace cancels ctx):
	// original DB must remain readable and untouched, no .compact left behind.
	ctx, cancel := context.WithCancel(context.Background())
	res1, err := ShrinkBridgeAnalysis(ctx, "", ShrinkOptions{
		DatabaseDir:         dir,
		MaxBridgeStateBytes: 32 << 10,
		BatchSize:           10,
		FreeSpace: func(string) (int64, error) {
			cancel()
			return 64 << 20, nil
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("shrink with pre-vacuum cancel: %v", err)
	}
	if res1.Compacted {
		t.Fatalf("expected Compacted=false when cancelled before vacuum")
	}
	assertDBReadableAndIntact(t, statePath, 20)
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("expected leftover .compact to be removed, stat err = %v", err)
	}

	// Next mount finishes compaction cleanly.
	res2, err := ShrinkBridgeAnalysis(context.Background(), "", ShrinkOptions{
		DatabaseDir:         dir,
		MaxBridgeStateBytes: 32 << 10,
		FreeSpace:           func(string) (int64, error) { return 64 << 20, nil },
	})
	if err != nil {
		t.Fatalf("second shrink: %v", err)
	}
	if !res2.Compacted || !res2.Done {
		t.Fatalf("expected Compacted=true, Done=true, got %+v", res2)
	}
	assertDBReadableAndIntact(t, statePath, 20)
}

func TestShrinkBridgeAnalysisSkipsVacuumWhenFreeSpaceShort(t *testing.T) {
	dir := t.TempDir()
	statePath := createLegacyStateDBForShrinkTest(t, dir, 20, 4096)
	ctx := context.Background()

	beforeStat, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}

	res1, err := ShrinkBridgeAnalysis(ctx, "", ShrinkOptions{
		DatabaseDir:         dir,
		MaxBridgeStateBytes: 32 << 10,
		FreeSpace:           func(string) (int64, error) { return 512, nil },
	})
	if err != nil {
		t.Fatalf("shrink with short free space: %v", err)
	}
	if res1.Rewritten != 20 || res1.Compacted || res1.Done {
		t.Fatalf("expected Rewritten=20, Compacted=false, Done=false; got %+v", res1)
	}
	afterShortStat, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if afterShortStat.Size() < beforeStat.Size()/2 {
		t.Fatalf("expected DB not to be vacuumed when free space is short (before=%d, after=%d)", beforeStat.Size(), afterShortStat.Size())
	}
	if _, err := os.Stat(statePath + ".compact"); !os.IsNotExist(err) {
		t.Fatalf("expected no .compact file left behind, err=%v", err)
	}

	// Once free space is available, the next run compacts without rewriting again.
	res2, err := ShrinkBridgeAnalysis(ctx, "", ShrinkOptions{
		DatabaseDir:         dir,
		MaxBridgeStateBytes: 32 << 10,
		FreeSpace:           func(string) (int64, error) { return 64 << 20, nil },
	})
	if err != nil {
		t.Fatalf("shrink with enough free space: %v", err)
	}
	if res2.Rewritten != 0 || !res2.Compacted || !res2.Done {
		t.Fatalf("expected Rewritten=0, Compacted=true, Done=true; got %+v", res2)
	}
	afterVacuumStat, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if afterVacuumStat.Size() >= afterShortStat.Size()/2 {
		t.Fatalf("expected DB to shrink significantly after vacuum (before=%d, after=%d)", afterShortStat.Size(), afterVacuumStat.Size())
	}

	// Verify UnpackAnalysis still decodes a rewritten row after vacuum.
	db, err := openStateDB(ctx, statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var src []byte
	if err := db.QueryRow(`SELECT sourceJSON FROM BridgeAnalysis WHERE trackId=1`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	plain, err := UnpackAnalysis(src, 64<<20)
	if err != nil {
		t.Fatalf("UnpackAnalysis: %v", err)
	}
	if !bytes.Contains(plain, []byte(`"ID":101`)) {
		t.Fatalf("unexpected unpacked JSON: %s", plain)
	}
}

// Keep unused sql import check satisfied if needed.
var _ = sql.ErrNoRows
