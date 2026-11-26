package enginedb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestDeleteListReparentsNativeChildAfterNullRootTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := b.InsertList(ctx, ListRow{Title: "Imported"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.InsertList(ctx, ListRow{Title: "Native"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := b.InsertList(ctx, ListRow{Title: "Native", Parent: folder})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE Playlist SET nextListId=NULL WHERE id=?`, root); err != nil {
		t.Fatal(err)
	}
	if err = b.DeleteList(ctx, folder, func(id int64) bool { return id == folder }); err != nil {
		t.Fatal(err)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var parent, next, rootNext int64
	var title string
	if err = lib.db.QueryRowContext(ctx, `SELECT parentListId,COALESCE(nextListId,0),title FROM Playlist WHERE id=?`, child).Scan(&parent, &next, &title); err != nil {
		t.Fatal(err)
	}
	if err = lib.db.QueryRowContext(ctx, `SELECT COALESCE(nextListId,0) FROM Playlist WHERE id=?`, root).Scan(&rootNext); err != nil {
		t.Fatal(err)
	}
	if parent != 0 || next != 0 || rootNext != child || title != "Native (1)" {
		t.Fatalf("native child was not reparented: parent=%d next=%d rootNext=%d title=%q", parent, next, rootNext, title)
	}
}

func TestOpenLibraryMigratesLegacyArtworkHashes(t *testing.T) {
	ctx := context.Background()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	art := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	legacyHash := "00112233445566778899aabbccddeeff00112233"
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, legacyHash, art); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// Opening alone must not mutate legacy artwork hashes.
	lib, err := OpenLibraryFile(ctx, filepath.Join(dir, "m.db"), mount)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var kind string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt LIMIT 1`).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "text" {
		t.Fatalf("OpenLibraryFile unexpectedly migrated legacy hash: kind=%q", kind)
	}
	sum := sha1.Sum(art)
	artPath := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")
	if _, err = os.Stat(artPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artwork file existed before migration: err=%v", err)
	}

	if err = lib.MigrateLegacyArtwork(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	var gotHash []byte
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash),hash FROM AlbumArt LIMIT 1`).Scan(&kind, &gotHash); err != nil {
		t.Fatal(err)
	}
	if kind != "blob" || !bytes.Equal(gotHash, sum[:]) {
		t.Fatalf("legacy artwork hash type=%q hash=%x want blob %x", kind, gotHash, sum[:])
	}
	data, err := os.ReadFile(artPath)
	if err != nil || !bytes.Equal(data, art) {
		t.Fatalf("legacy artwork file: err=%v data=%x", err, data)
	}
}

func TestMigrateLegacyArtworkCommitsPartialProgressWithinBudget(t *testing.T) {
	ctx := context.Background()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	art1 := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 1, 1}
	art2 := []byte{0xff, 0xd8, 0xff, 0xe0, 2, 2, 2}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES('1111111111111111111111111111111111111111',?),('2222222222222222222222222222222222222222',?)`, art1, art2); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// Advance clock past the 1s budget right after row 1 finishes writing so row 1 commits cleanly
	// and row 2 stops cleanly on budget expiry.
	base := time.Unix(1700000000, 0)
	var expired atomic.Bool
	origNow := migrateNow
	migrateNow = func() time.Time {
		if expired.Load() {
			return base.Add(2 * time.Second)
		}
		return base
	}
	t.Cleanup(func() { migrateNow = origNow })
	origPut := migratePutArtwork
	var puts int
	migratePutArtwork = func(dir string, sum, data []byte, stop func() error) error {
		puts++
		err := origPut(dir, sum, data, stop)
		if err == nil && puts == 1 {
			expired.Store(true)
		}
		return err
	}
	t.Cleanup(func() { migratePutArtwork = origPut })

	lib, err := OpenLibraryFile(ctx, filepath.Join(dir, "m.db"), mount)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if err = lib.MigrateLegacyArtwork(ctx, time.Second); err != nil {
		t.Fatalf("MigrateLegacyArtwork failed on budget expiry: %v", err)
	}
	var kind1, kind2 string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 0`).Scan(&kind1); err != nil {
		t.Fatal(err)
	}
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 1`).Scan(&kind2); err != nil {
		t.Fatal(err)
	}
	if kind1 != "blob" || kind2 != "text" {
		t.Fatalf("partial budget progress not committed: row1=%q row2=%q, want blob/text", kind1, kind2)
	}
	sum1 := sha1.Sum(art1)
	p1 := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum1[:])+".jpg")
	if _, err = os.Stat(p1); err != nil {
		t.Fatalf("first migrated artwork file missing: %v", err)
	}
}

func TestMigrateLegacyArtworkSkipsCorruptCoverAndChecksOpenElsewhere(t *testing.T) {
	ctx := context.Background()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	badPNG := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0}
	goodJPG := []byte{0xff, 0xd8, 0xff, 0xe0, 9, 8, 7}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',?),('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',?)`, badPNG, goodJPG); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	lib, err := OpenLibraryFile(ctx, filepath.Join(dir, "m.db"), mount)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	if err = lib.MigrateLegacyArtwork(ctx, time.Second); err != nil {
		t.Fatalf("corrupt PNG aborted migration: %v", err)
	}
	var badKind, goodKind string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 0`).Scan(&badKind); err != nil {
		t.Fatal(err)
	}
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 1`).Scan(&goodKind); err != nil {
		t.Fatal(err)
	}
	if badKind != "text" || goodKind != "blob" {
		t.Fatalf("corrupt-skip mismatch: bad=%q good=%q, want text/blob", badKind, goodKind)
	}

	// OpenElsewhere before commit must roll back without converting the new TEXT row.
	good2 := []byte{0xff, 0xd8, 0xff, 0xe0, 4, 5, 6}
	if _, err = lib.db.ExecContext(ctx, `INSERT INTO AlbumArt(hash,albumArt) VALUES('cccccccccccccccccccccccccccccccccccccccc',?)`, good2); err != nil {
		t.Fatal(err)
	}
	stop := holdOpen(t, filepath.Join(dir, "m.db"))
	defer stop()
	if err = lib.MigrateLegacyArtwork(ctx, time.Second); !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("OpenElsewhere migration err=%v, want ErrLibraryInUse", err)
	}
	var thirdKind string
	if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash) FROM AlbumArt ORDER BY id LIMIT 1 OFFSET 2`).Scan(&thirdKind); err != nil {
		t.Fatal(err)
	}
	if thirdKind != "text" {
		t.Fatalf("OpenElsewhere rollback failed: row3=%q, want text", thirdKind)
	}
}

func TestMigrateLegacyArtworkUsesHashIndex(t *testing.T) {
	ctx := context.Background()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibraryFile(ctx, filepath.Join(dir, "m.db"), mount)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()

	for _, querySpec := range []struct {
		name string
		sql  string
		args []any
	}{
		{"first_page", legacyArtworkFirstPageQuery, []any{legacyArtworkBatch}},
		{"next_page", legacyArtworkNextPageQuery, []any{"", "", int64(0), legacyArtworkBatch}},
	} {
		rows, err := lib.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+querySpec.sql, querySpec.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plans []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err = rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plans = append(plans, detail)
		}
		rows.Close()
		joined := strings.Join(plans, "; ")
		if !strings.Contains(joined, "COVERING INDEX index_AlbumArt_hash") ||
			strings.Contains(joined, "SCAN AlbumArt") ||
			strings.Contains(joined, "TEMP B-TREE") {
			t.Fatalf("%s must use covering index_AlbumArt_hash without SCAN/TEMP B-TREE, got plan: %s", querySpec.name, joined)
		}
	}

	jpg1 := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 1, 1}
	jpg2 := []byte{0xff, 0xd8, 0xff, 0xe0, 2, 2, 2}
	jpg3 := []byte{0xff, 0xd8, 0xff, 0xe0, 3, 3, 3}
	badPNG := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0}
	if _, err = lib.db.ExecContext(ctx, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES
		(1, NULL, ?),
		(2, X'', ?),
		(3, X'00', ?),
		(4, X'ff', ?),
		(5, '', ?),
		(6, 'aaa', ?),
		(7, 'aaa', ?),
		(8, 'bad', ?),
		(9, 'bbb', ?),
		(10, 'ccc', ?),
		(11, 'ddd', ?),
		(12, 'eee', ?),
		(13, ?, ?)`, jpg1, jpg1, jpg1, jpg1, jpg1, jpg2, jpg3, badPNG, jpg1, jpg2, jpg3, jpg1, "\U0010ffff", jpg3); err != nil {
		t.Fatal(err)
	}
	if err = lib.MigrateLegacyArtwork(ctx, time.Second); err != nil {
		t.Fatalf("MigrateLegacyArtwork: %v", err)
	}
	wantKinds := map[int64]string{
		1: "null", 2: "blob", 3: "blob", 4: "blob",
		5: "blob", 6: "blob", 7: "blob", 8: "text",
		9: "blob", 10: "blob", 11: "blob", 12: "blob", 13: "blob",
	}
	for id, want := range wantKinds {
		var kind string
		var hash []byte
		if err = lib.db.QueryRowContext(ctx, `SELECT typeof(hash), hash FROM AlbumArt WHERE id=?`, id).Scan(&kind, &hash); err != nil {
			t.Fatalf("id %d: %v", id, err)
		}
		if kind != want {
			t.Fatalf("id %d: typeof(hash)=%q, want %q", id, kind, want)
		}
		if id == 2 && len(hash) != 0 {
			t.Fatalf("id 2 empty blob was altered: %x", hash)
		}
		if id == 3 && !bytes.Equal(hash, []byte{0x00}) {
			t.Fatalf("id 3 blob 0x00 was altered: %x", hash)
		}
		if id == 4 && !bytes.Equal(hash, []byte{0xff}) {
			t.Fatalf("id 4 blob 0xff was altered: %x", hash)
		}
		if want == "blob" && id >= 5 && len(hash) != sha1.Size {
			t.Fatalf("id %d text hash was not migrated to sha1 blob: len=%d", id, len(hash))
		}
	}
	for _, img := range [][]byte{jpg1, jpg2, jpg3} {
		sum := sha1.Sum(img)
		p := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")
		if _, err = os.Stat(p); err != nil {
			t.Fatalf("migrated artwork file %s missing: %v", p, err)
		}
	}
}

func openLegacyArtTestDB(t *testing.T, count int, payloadFn func(i int) []byte) (*Library, string, string) {
	t.Helper()
	ctx := context.Background()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "m.db")
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= count; i++ {
		h := fmt.Sprintf("legacy-%04d", i)
		if _, err = db.Exec(`INSERT INTO AlbumArt(id,hash,albumArt) VALUES(?,?,?)`, i, h, payloadFn(i)); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	lib, err := OpenLibraryFile(ctx, dbPath, mount)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lib.Close() })
	return lib, mount, dbPath
}

func TestMigrateLegacyArtworkBoundedPagesAndCancelKeepsCommittedBatches(t *testing.T) {
	jpg := func(i int) []byte { return []byte{0xff, 0xd8, 0xff, 0xe0, byte(i), 1, 2} }

	t.Run("BatchCommitsBeforeNextBatchForKillSafety", func(t *testing.T) {
		lib, mount, dbPath := openLegacyArtTestDB(t, 6, jpg)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		origPut := migratePutArtwork
		t.Cleanup(func() { migratePutArtwork = origPut })
		var calls int
		var batch1CommittedBeforeBatch2 bool
		migratePutArtwork = func(dir string, sum, data []byte, stop func() error) error {
			calls++
			if calls == 3 {
				// Before row 3 starts, batch 1 (rows 1..2) must already be durably committed
				// on a separate connection so a process kill / Eject cannot roll it back.
				ro, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
				if err != nil {
					return err
				}
				var blobs int
				err = ro.QueryRow(`SELECT count(*) FROM AlbumArt WHERE id<=2 AND typeof(hash)='blob'`).Scan(&blobs)
				ro.Close()
				if err != nil {
					return err
				}
				batch1CommittedBeforeBatch2 = blobs == 2
				cancel()
			}
			return origPut(dir, sum, data, stop)
		}

		err := lib.MigrateLegacyArtworkWithOptions(ctx, LegacyArtworkOptions{BatchSize: 2})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want context.Canceled", err)
		}
		if !batch1CommittedBeforeBatch2 {
			t.Fatal("batch 1 was not durably committed before batch 2 started")
		}
		for i := 1; i <= 6; i++ {
			var kind string
			if err = lib.db.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=?`, i).Scan(&kind); err != nil {
				t.Fatal(err)
			}
			want := "text"
			if i <= 2 {
				want = "blob"
			}
			if kind != want {
				t.Fatalf("id %d: kind=%q, want %q", i, kind, want)
			}
			sum := sha1.Sum(jpg(i))
			p := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")
			_, statErr := os.Stat(p)
			if i <= 2 && statErr != nil {
				t.Fatalf("id %d committed artwork missing: %v", i, statErr)
			}
			if i > 2 && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("id %d canceled artwork should not exist: err=%v", i, statErr)
			}
		}
	})

	t.Run("CancelMidBatchCommitsCompletedRow", func(t *testing.T) {
		lib, mount, _ := openLegacyArtTestDB(t, 4, jpg)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		origPut := migratePutArtwork
		t.Cleanup(func() { migratePutArtwork = origPut })
		var calls int
		migratePutArtwork = func(dir string, sum, data []byte, stop func() error) error {
			calls++
			if calls == 2 {
				cancel()
			}
			return origPut(dir, sum, data, stop)
		}

		err := lib.MigrateLegacyArtworkWithOptions(ctx, LegacyArtworkOptions{BatchSize: 4})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want context.Canceled", err)
		}
		for i := 1; i <= 4; i++ {
			var kind string
			if err = lib.db.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=?`, i).Scan(&kind); err != nil {
				t.Fatal(err)
			}
			want := "text"
			if i == 1 {
				want = "blob"
			}
			if kind != want {
				t.Fatalf("id %d: kind=%q, want %q", i, kind, want)
			}
		}
		sum1 := sha1.Sum(jpg(1))
		p1 := filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum1[:])+".jpg")
		if _, err = os.Stat(p1); err != nil {
			t.Fatalf("row 1 completed before mid-batch cancel is missing: %v", err)
		}
	})

	t.Run("BudgetExpiryDuringEnumerationStopsEarly", func(t *testing.T) {
		lib, _, _ := openLegacyArtTestDB(t, 6, jpg)
		var nowCalls atomic.Int32
		var callsBeforeFirstPut int32
		base := time.Unix(1700000000, 0)
		origNow := migrateNow
		migrateNow = func() time.Time {
			nowCalls.Add(1)
			return base
		}
		origPut := migratePutArtwork
		migratePutArtwork = func(dir string, sum, data []byte, stop func() error) error {
			if callsBeforeFirstPut == 0 {
				callsBeforeFirstPut = nowCalls.Load()
			}
			return origPut(dir, sum, data, stop)
		}
		if err := lib.MigrateLegacyArtworkWithOptions(context.Background(), LegacyArtworkOptions{Budget: time.Second, BatchSize: 6}); err != nil {
			migrateNow = origNow
			migratePutArtwork = origPut
			t.Fatalf("unexpected error: %v", err)
		}
		migrateNow = origNow
		migratePutArtwork = origPut
		// 6 scanned rows must each check stop() during rows.Next() before row 1 starts processing.
		if callsBeforeFirstPut < 12 {
			t.Fatalf("rows.Next() skipped budget/stop checks during enumeration: nowCalls before 1st put = %d, want >= 12", callsBeforeFirstPut)
		}

		// Also verify immediate stop when deadline expires inside rows.Next().
		lib2, _, _ := openLegacyArtTestDB(t, 6, jpg)
		nowCalls.Store(0)
		migrateNow = func() time.Time {
			if nowCalls.Add(1) >= 4 {
				return base.Add(2 * time.Second)
			}
			return base
		}
		t.Cleanup(func() { migrateNow = origNow })
		var puts int
		migratePutArtwork = func(dir string, sum, data []byte, stop func() error) error {
			puts++
			return origPut(dir, sum, data, stop)
		}
		t.Cleanup(func() { migratePutArtwork = origPut })
		if err := lib2.MigrateLegacyArtworkWithOptions(context.Background(), LegacyArtworkOptions{Budget: time.Second, BatchSize: 6}); err != nil {
			t.Fatalf("unexpected error on enumeration timeout: %v", err)
		}
		if puts != 0 {
			t.Fatalf("expected 0 artwork writes when budget expires during rows.Next(), got %d", puts)
		}
	})
}

func TestMigrateLegacyArtworkBoundsPerImageConversionAndWrite(t *testing.T) {
	t.Run("ChunkedWriteAbortsMidImageOnBudgetExpiry", func(t *testing.T) {
		origChunk := legacyArtworkWriteChunk
		legacyArtworkWriteChunk = 1024
		t.Cleanup(func() { legacyArtworkWriteChunk = origChunk })

		largeJPG := append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0x42}, 8192)...)
		lib, mount, _ := openLegacyArtTestDB(t, 1, func(int) []byte { return largeJPG })

		base := time.Unix(1700000000, 0)
		var expired atomic.Bool
		origNow := migrateNow
		migrateNow = func() time.Time {
			if expired.Load() {
				return base.Add(2 * time.Second)
			}
			return base
		}
		t.Cleanup(func() { migrateNow = origNow })

		origPut := migratePutArtwork
		migratePutArtwork = func(dir string, sum, data []byte, stop func() error) error {
			var inner atomic.Int32
			return origPut(dir, sum, data, func() error {
				if inner.Add(1) >= 5 {
					expired.Store(true)
				}
				return stop()
			})
		}
		t.Cleanup(func() { migratePutArtwork = origPut })

		if err := lib.MigrateLegacyArtworkWithOptions(context.Background(), LegacyArtworkOptions{Budget: time.Second}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var kind string
		if err := lib.db.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=1`).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != "text" {
			t.Fatalf("aborted image hash mutated to %q, want text", kind)
		}
		artDir := filepath.Join(mount, "Engine Library", "Artwork")
		entries, err := os.ReadDir(artDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("aborted chunked write left files in Artwork: %v", entries)
		}
	})

	t.Run("PNGConversionAbortsMidDecodeOrEncodeOnBudgetExpiry", func(t *testing.T) {
		origStep := legacyArtworkEncodeStep
		origChunk := legacyArtworkWriteChunk
		legacyArtworkEncodeStep = 1024
		legacyArtworkWriteChunk = 1 << 20
		t.Cleanup(func() {
			legacyArtworkEncodeStep = origStep
			legacyArtworkWriteChunk = origChunk
		})

		img := image.NewRGBA(image.Rect(0, 0, 256, 256))
		for y := 0; y < 256; y++ {
			for x := 0; x < 256; x++ {
				v := uint8((x*37 ^ y*91) + (x+y)*13)
				img.SetRGBA(x, y, color.RGBA{R: v, G: v ^ 0x55, B: uint8(x * y), A: 255})
			}
		}
		var pngBuf bytes.Buffer
		if err := png.Encode(&pngBuf, img); err != nil {
			t.Fatal(err)
		}
		pngData := pngBuf.Bytes()
		if len(pngData) < 20<<10 {
			t.Fatalf("synthetic PNG too small (%d bytes) to exercise chunked decode/encode bounds", len(pngData))
		}
		lib, mount, _ := openLegacyArtTestDB(t, 1, func(int) []byte { return pngData })

		base := time.Unix(1700000000, 0)
		var expired atomic.Bool
		origNow := migrateNow
		migrateNow = func() time.Time {
			if expired.Load() {
				return base.Add(2 * time.Second)
			}
			return base
		}
		t.Cleanup(func() { migrateNow = origNow })

		origPut := migratePutArtwork
		migratePutArtwork = func(dir string, sum, data []byte, stop func() error) error {
			var inner atomic.Int32
			return origPut(dir, sum, data, func() error {
				if inner.Add(1) >= 12 {
					expired.Store(true)
				}
				return stop()
			})
		}
		t.Cleanup(func() { migratePutArtwork = origPut })

		if err := lib.MigrateLegacyArtworkWithOptions(context.Background(), LegacyArtworkOptions{Budget: time.Second}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var kind string
		if err := lib.db.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=1`).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != "text" {
			t.Fatalf("aborted PNG conversion hash=%q, want text", kind)
		}
		artDir := filepath.Join(mount, "Engine Library", "Artwork")
		entries, err := os.ReadDir(artDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("aborted PNG conversion left files in Artwork: %v", entries)
		}
	})

	t.Run("OversizedBlobSkippedWithoutAborting", func(t *testing.T) {
		origMax := maxLegacyArtworkBytes
		maxLegacyArtworkBytes = 2048
		t.Cleanup(func() { maxLegacyArtworkBytes = origMax })

		bigJPG := append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0x11}, 4096)...)
		smallJPG := []byte{0xff, 0xd8, 0xff, 0xe0, 9, 8, 7}
		lib, _, _ := openLegacyArtTestDB(t, 2, func(i int) []byte {
			if i == 1 {
				return bigJPG
			}
			return smallJPG
		})
		if err := lib.MigrateLegacyArtworkWithOptions(context.Background(), LegacyArtworkOptions{Budget: time.Second}); err != nil {
			t.Fatalf("oversized artwork aborted migration: %v", err)
		}
		var k1, k2 string
		if err := lib.db.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=1`).Scan(&k1); err != nil {
			t.Fatal(err)
		}
		if err := lib.db.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=2`).Scan(&k2); err != nil {
			t.Fatal(err)
		}
		if k1 != "text" || k2 != "blob" {
			t.Fatalf("oversized skip mismatch: k1=%q k2=%q, want text/blob", k1, k2)
		}
	})
}

func TestMigrateLegacyArtworkRespectsBusyAndPace(t *testing.T) {
	jpg := func(i int) []byte { return []byte{0xff, 0xd8, 0xff, 0xe0, byte(i), 3, 4} }

	t.Run("BusyBeforeStartDoesZeroWrites", func(t *testing.T) {
		lib, mount, _ := openLegacyArtTestDB(t, 3, jpg)
		if err := lib.MigrateLegacyArtworkWithOptions(context.Background(), LegacyArtworkOptions{
			Busy: func() bool { return true },
		}); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		var textCount int
		if err := lib.db.QueryRow(`SELECT count(*) FROM AlbumArt WHERE typeof(hash)='text'`).Scan(&textCount); err != nil {
			t.Fatal(err)
		}
		if textCount != 3 {
			t.Fatalf("migrated rows while deck busy: remaining text=%d, want 3", textCount)
		}
		entries, err := os.ReadDir(filepath.Join(mount, "Engine Library", "Artwork"))
		if err == nil && len(entries) != 0 {
			t.Fatalf("wrote Artwork files while deck busy: %v", entries)
		}
	})

	t.Run("BusyMidRunPreservesBatch1AndSkipsBatch2Commit", func(t *testing.T) {
		lib, _, _ := openLegacyArtTestDB(t, 6, jpg)
		var busy atomic.Bool
		var puts int
		origPut := migratePutArtwork
		t.Cleanup(func() { migratePutArtwork = origPut })
		migratePutArtwork = func(dir string, sum, data []byte, stop func() error) error {
			puts++
			err := origPut(dir, sum, data, stop)
			// Batch 1 = rows 1..2 commits; row 3 finishes in batch 2, then deck starts playing before batch 2 commits.
			if puts == 3 {
				busy.Store(true)
			}
			return err
		}

		if err := lib.MigrateLegacyArtworkWithOptions(context.Background(), LegacyArtworkOptions{
			BatchSize: 2,
			Busy:      busy.Load,
		}); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		for i := 1; i <= 6; i++ {
			var kind string
			if err := lib.db.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=?`, i).Scan(&kind); err != nil {
				t.Fatal(err)
			}
			want := "text"
			if i <= 2 {
				want = "blob"
			}
			if kind != want {
				t.Fatalf("id %d: kind=%q, want %q (batch 2 must roll back on busy before commit)", i, kind, want)
			}
		}
	})

	t.Run("PaceCalledPerCommittedBatch", func(t *testing.T) {
		lib, _, _ := openLegacyArtTestDB(t, 5, jpg)
		var paceCalls int
		err := lib.MigrateLegacyArtworkWithOptions(context.Background(), LegacyArtworkOptions{
			BatchSize: 2,
			Pace: func(took time.Duration) error {
				paceCalls++
				if took < 0 {
					return fmt.Errorf("negative batch duration: %v", took)
				}
				return nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if paceCalls != 3 {
			t.Fatalf("Pace called %d times for 5 rows in batch 2, want 3", paceCalls)
		}
	})
}

func makeQuickCuesWithMain(t *testing.T, base []byte, main float64, adj byte) []byte {
	t.Helper()
	raw, err := QUncompress(base, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := cuePositions(raw)
	if !ok {
		t.Fatal("cue layout not recognized")
	}
	big.PutUint64(raw[at[8]:], math.Float64bits(main))
	raw[at[8]+8] = adj
	big.PutUint64(raw[at[9]:], math.Float64bits(main))
	out, err := QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readMainCueFromDB(t *testing.T, lib *Library, id int64) (float64, byte) {
	t.Helper()
	var blob []byte
	if err := lib.db.QueryRowContext(context.Background(), `SELECT quickCues FROM PerformanceData WHERE trackId=?`, id).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	raw, err := QUncompress(blob, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := cuePositions(raw)
	if !ok {
		t.Fatal("cue layout not recognized")
	}
	return math.Float64frombits(big.Uint64(raw[at[8]:])), raw[at[8]+8]
}

func TestApplyPerformanceAcceptsRawLegacyWrittenDigest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	track := rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", SampleRate: 44100, Duration: 10}
	ids, _, _, err := BuildLibrary(ctx, dir, "", []TrackRow{{Rel: "Contents/a.mp3", Track: track}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	memPerf, err := EncodePerformance(track, &rekordbox.Analysis{Cues: []rekordbox.Cue{{HotCue: 0, TimeMS: 225}}})
	if err != nil {
		t.Fatal(err)
	}
	noMemPerf, err := EncodePerformance(track, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy v1 / 9f2c0770 wrote adj=0 with the memory cue position and stored RawWrittenDigest.
	legacyCues := makeQuickCuesWithMain(t, memPerf.QuickCues, 9920, 0)
	legacyRawDigest := RawWrittenDigest(memPerf.Beats, legacyCues, memPerf.Loops)

	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()

	// Case 1 (NEW_REG1): untouched legacy row (adj=0, main=9920) upgraded to adj=1 -> PerfWritten, not PerfKept.
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, memPerf.Beats, legacyCues, memPerf.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	digest, outcome, err := b.ApplyPerformance(ctx, ids[0], memPerf, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{State: rbstate.PerfApplied, Written: legacyRawDigest}, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfWritten || digest == nil {
		t.Fatalf("NEW_REG1: untouched legacy row became outcome=%d digestNil=%v, want PerfWritten", outcome, digest == nil)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, adj := readMainCueFromDB(t, lib, ids[0]); adj != 1 {
		t.Fatalf("NEW_REG1: upgraded row adj=%d, want 1", adj)
	}

	// Case 2 (NEW_REG4): untouched pre-fix adj=0 row (main=9920) whose memory cue is deleted -> PerfWritten (rewrites to -1, not PerfSame).
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, noMemPerf.Beats, legacyCues, noMemPerf.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	digest, outcome, err = b.ApplyPerformance(ctx, ids[0], noMemPerf, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{State: rbstate.PerfApplied, Written: legacyRawDigest}, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfWritten || digest == nil {
		t.Fatalf("NEW_REG4: deleted memory cue on pre-fix row became outcome=%d digestNil=%v, want PerfWritten", outcome, digest == nil)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if main, adj := readMainCueFromDB(t, lib, ids[0]); main != -1 || adj != 0 {
		t.Fatalf("NEW_REG4: stale main cue kept in DB: main=%v adj=%d, want -1, 0", main, adj)
	}

	// Case 3: legacy row (adj=0, main=9920) loaded on deck before upgrade (Engine rewrote to -1105, adj=0),
	// now upgraded to adj=1 -> PerfWritten (restores rekordbox main cue with adj=1).
	loadedLegacyCues := makeQuickCuesWithMain(t, memPerf.QuickCues, -1105, 0)
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, memPerf.Beats, loadedLegacyCues, memPerf.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	mem9920 := memPerf
	mem9920.QuickCues = makeQuickCuesWithMain(t, memPerf.QuickCues, 9920, 1)
	mem9920.Digest = WrittenDigest(mem9920.Beats, mem9920.QuickCues, mem9920.Loops)
	digest, outcome, err = b.ApplyPerformance(ctx, ids[0], mem9920, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{State: rbstate.PerfApplied, Written: legacyRawDigest}, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfWritten || digest == nil {
		t.Fatalf("loaded legacy row became outcome=%d digestNil=%v, want PerfWritten", outcome, digest == nil)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if main, adj := readMainCueFromDB(t, lib, ids[0]); main != 9920 || adj != 1 {
		t.Fatalf("loaded legacy row did not restore main cue: main=%v adj=%d, want 9920, 1", main, adj)
	}

	// Case 4: real DJ edit on a legacy row (DJ moved main cue -> adj=1) -> must stay PerfKept.
	djEditedCues := makeQuickCuesWithMain(t, memPerf.QuickCues, 4410, 1)
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=? WHERE trackId=?`, memPerf.Beats, djEditedCues, memPerf.Loops, ids[0]); err != nil {
		t.Fatal(err)
	}
	digest, outcome, err = b.ApplyPerformance(ctx, ids[0], mem9920, rbstate.Facts{Duration: 10, SampleRate: 44100}, PerfPrev{State: rbstate.PerfApplied, Written: legacyRawDigest}, track)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != PerfKept || digest != nil {
		t.Fatalf("DJ edit on legacy row became outcome=%d digest=%x, want PerfKept/nil", outcome, digest)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestBatchCommitRollsBackWhenLibraryOpensElsewhere(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.exec(ctx, `UPDATE Information SET currentPlayedIndiciator=999`); err != nil {
		t.Fatal(err)
	}
	stop := holdOpen(t, filepath.Join(dir, "m.db"))
	defer stop()
	if err = b.Commit(ctx); !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("commit error=%v, want ErrLibraryInUse", err)
	}
	var indicator int64
	if err = lib.db.QueryRowContext(ctx, `SELECT currentPlayedIndiciator FROM Information`).Scan(&indicator); err != nil {
		t.Fatal(err)
	}
	if indicator == 999 {
		t.Fatal("failed commit left its transaction applied")
	}
}

func TestSetEntriesChainsEntitiesAndClampsFiveHundredBPM(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rows := []TrackRow{{Rel: "Contents/a.mp3", Track: rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", BPM: 500}}, {Rel: "Contents/b.mp3", Track: rekordbox.Track{ID: 2, Path: "/Contents/b.mp3", BPM: 120}}}
	ids, _, _, err := BuildLibrary(ctx, dir, "", rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var bpm float64
	if err = lib.db.QueryRowContext(ctx, `SELECT bpm FROM Track WHERE id=?`, ids[0]).Scan(&bpm); err != nil {
		t.Fatal(err)
	}
	if bpm != 0 {
		t.Fatalf("500 BPM was not clamped: %v", bpm)
	}
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list, err := b.InsertList(ctx, ListRow{Title: "Set"})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.SetEntries(ctx, list, ids); err != nil {
		t.Fatal(err)
	}
	if err = b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var firstNext, secondNext int64
	if err = lib.db.QueryRowContext(ctx, `SELECT nextEntityId FROM PlaylistEntity WHERE trackId=?`, ids[0]).Scan(&firstNext); err != nil {
		t.Fatal(err)
	}
	if err = lib.db.QueryRowContext(ctx, `SELECT nextEntityId FROM PlaylistEntity WHERE trackId=?`, ids[1]).Scan(&secondNext); err != nil {
		t.Fatal(err)
	}
	if firstNext == 0 || secondNext != 0 {
		t.Fatalf("entity chain: first=%d second=%d", firstNext, secondNext)
	}
}

func TestOpenLibraryRepairsGL010Information(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE Information SET id=2, uuid='', currentPlayedIndiciator=0`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var id, played int64
	var gotUUID string
	if err = lib.db.QueryRowContext(ctx, `SELECT id,uuid,currentPlayedIndiciator FROM Information`).Scan(&id, &gotUUID, &played); err != nil {
		t.Fatal(err)
	}
	if id != 1 || gotUUID == "" || played == 0 {
		t.Fatalf("GL-010 Information repair failed: id=%d uuid=%q played=%d", id, gotUUID, played)
	}
}

func TestLibraryUsesImmediateWriteLock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, _, _, err := BuildLibrary(ctx, dir, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Hold a RESERVED write lock on a separate connection so a BEGIN IMMEDIATE fails at BeginTx.
	holder, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err = holder.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer holder.ExecContext(ctx, `ROLLBACK`)

	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	shortCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err = lib.db.BeginTx(shortCtx, nil); err == nil {
		t.Fatal("BeginTx succeeded despite active write transaction; _txlock=immediate not active")
	}
}

func TestSetEntriesSuspendsAndRestoresPlaylistEntityDeleteTrigger(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rows := []TrackRow{
		{Rel: "Contents/a.mp3", Track: rekordbox.Track{ID: 1, Path: "/Contents/a.mp3", Title: "A"}},
		{Rel: "Contents/b.mp3", Track: rekordbox.Track{ID: 2, Path: "/Contents/b.mp3", Title: "B"}},
		{Rel: "Contents/c.mp3", Track: rekordbox.Track{ID: 3, Path: "/Contents/c.mp3", Title: "C"}},
	}
	ids, _, _, err := BuildLibrary(ctx, dir, "", rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()

	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list1, err := b.InsertList(ctx, ListRow{Title: "List1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetEntries(ctx, list1, ids); err != nil {
		t.Fatal(err)
	}
	list2, err := b.InsertList(ctx, ListRow{Title: "List2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetEntries(ctx, list2, ids); err != nil {
		t.Fatal(err)
	}
	// DeleteTrack after SetEntries in the same batch must restore the trigger and relink list2.
	if err := b.DeleteTrack(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var trigCount int
	if err := lib.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='trigger_before_delete_PlaylistEntity'`).Scan(&trigCount); err != nil {
		t.Fatal(err)
	}
	if trigCount != 1 {
		t.Fatalf("trigger_before_delete_PlaylistEntity count = %d, want 1 after Commit", trigCount)
	}
	var e1Next, e3ID int64
	if err := lib.db.QueryRowContext(ctx, `SELECT nextEntityId FROM PlaylistEntity WHERE listId=? AND trackId=?`, list2, ids[0]).Scan(&e1Next); err != nil {
		t.Fatal(err)
	}
	if err := lib.db.QueryRowContext(ctx, `SELECT id FROM PlaylistEntity WHERE listId=? AND trackId=?`, list2, ids[2]).Scan(&e3ID); err != nil {
		t.Fatal(err)
	}
	if e1Next != e3ID {
		t.Fatalf("list2 entity relink after DeleteTrack: nextEntityId=%d, want %d", e1Next, e3ID)
	}
}

func TestUpdateTrackZeroWritesWhenUnchangedAndMinimalColumnUpdate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	artHash := sha1.Sum([]byte("rb:/PIONEER/Artwork/00001/a1.jpg"))
	rows := []TrackRow{{
		Rel: "Contents/track.mp3",
		Track: rekordbox.Track{
			ID:       10,
			Path:     "/Contents/track.mp3",
			Title:    "Song A",
			Artist:   "Artist A",
			Album:    "Album A",
			Genre:    "Techno",
			Comment:  "Note A",
			Label:    "Label A",
			Composer: "Comp A",
			Remixer:  "Rem A",
			Key:      "8A",
			Year:     2025,
			Rating:   60,
			BPM:      128,
			Duration: 240,
			Bitrate:  320,
			FileSize: 123456,
		},
		DirectArtHash: artHash[:],
	}}
	ids, _, _, err := BuildLibrary(ctx, dir, "", rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()

	if _, err := lib.db.ExecContext(ctx, `UPDATE Track SET lastEditTime=111 WHERE id=?`, ids[0]); err != nil {
		t.Fatal(err)
	}

	// Read-only plan and Batch.UpdateTrack + SetDirectCover must agree on no-op.
	needTrack, needCover, err := lib.TrackUpdatePlan(ctx, ids[0], rows[0], 60, artHash[:])
	if err != nil || needTrack || needCover {
		t.Fatalf("TrackUpdatePlan no-op = (%v, %v, %v), want (false, false, nil)", needTrack, needCover, err)
	}
	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := b.UpdateTrack(ctx, ids[0], rows[0], 60)
	if err != nil || changed {
		b.Rollback()
		t.Fatalf("UpdateTrack no-op = (%v, %v), want (false, nil)", changed, err)
	}
	covChanged, err := b.SetDirectCover(ctx, ids[0], artHash[:])
	if err != nil || covChanged {
		b.Rollback()
		t.Fatalf("SetDirectCover no-op = (%v, %v), want (false, nil)", covChanged, err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var editTime int64
	if err := lib.db.QueryRowContext(ctx, `SELECT lastEditTime FROM Track WHERE id=?`, ids[0]).Scan(&editTime); err != nil {
		t.Fatal(err)
	}
	if editTime != 111 {
		t.Fatalf("no-op update moved lastEditTime to %d, want 111", editTime)
	}

	// Changing only pdbImportKey updates ONLY pdbImportKey without firing Track timestamp trigger.
	changedRow := rows[0]
	changedRow.Track.ID = 42
	needTrack, needCover, err = lib.TrackUpdatePlan(ctx, ids[0], changedRow, 60, artHash[:])
	if err != nil || !needTrack || needCover {
		t.Fatalf("TrackUpdatePlan pdbImportKey change = (%v, %v, %v), want (true, false, nil)", needTrack, needCover, err)
	}
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = b.UpdateTrack(ctx, ids[0], changedRow, 60)
	if err != nil || !changed {
		b.Rollback()
		t.Fatalf("UpdateTrack pdbImportKey = (%v, %v), want (true, nil)", changed, err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var gotImportKey int64
	var gotTitle string
	if err := lib.db.QueryRowContext(ctx, `SELECT pdbImportKey,title,lastEditTime FROM Track WHERE id=?`, ids[0]).Scan(&gotImportKey, &gotTitle, &editTime); err != nil {
		t.Fatal(err)
	}
	if gotImportKey != 42 || gotTitle != "Song A" || editTime != 111 {
		t.Fatalf("pdbImportKey-only update got key=%d title=%q lastEditTime=%d, want 42/Song A/111", gotImportKey, gotTitle, editTime)
	}

	// Analyzed row keeps Engine-owned length/bpm/bitrate/fileBytes even when export differs, while updating title.
	if _, err := lib.db.ExecContext(ctx, `UPDATE Track SET isAnalyzed=1,length=300 WHERE id=?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	analyzedRow := changedRow
	analyzedRow.Track.Title = "Song A v2"
	analyzedRow.Track.Duration = 123
	needTrack, needCover, err = lib.TrackUpdatePlan(ctx, ids[0], analyzedRow, 60, artHash[:])
	if err != nil || !needTrack || needCover {
		t.Fatalf("TrackUpdatePlan analyzed title update = (%v, %v, %v), want (true, false, nil)", needTrack, needCover, err)
	}
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := b.UpdateTrack(ctx, ids[0], analyzedRow, 60); err != nil || !changed {
		b.Rollback()
		t.Fatalf("UpdateTrack analyzed = (%v, %v), want (true, nil)", changed, err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var gotLen int
	if err := lib.db.QueryRowContext(ctx, `SELECT title,length FROM Track WHERE id=?`, ids[0]).Scan(&gotTitle, &gotLen); err != nil {
		t.Fatal(err)
	}
	if gotTitle != "Song A v2" || gotLen != 300 {
		t.Fatalf("analyzed UpdateTrack got title=%q length=%d, want Song A v2 / 300", gotTitle, gotLen)
	}
}

func TestSetDirectCoverPreservesEmbeddedBlobWithoutOrphanAlbumArtRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rows := []TrackRow{
		{Rel: "Contents/blob.mp3", Track: rekordbox.Track{ID: 1, Path: "/Contents/blob.mp3", Title: "Blob"}},
		{Rel: "Contents/nullart.mp3", Track: rekordbox.Track{ID: 2, Path: "/Contents/nullart.mp3", Title: "NullArt"}},
		{Rel: "Contents/zeroart.mp3", Track: rekordbox.Track{ID: 3, Path: "/Contents/zeroart.mp3", Title: "ZeroArt"}},
		{Rel: "Contents/stalehash.mp3", Track: rekordbox.Track{ID: 4, Path: "/Contents/stalehash.mp3", Title: "StaleHash"}},
	}
	ids, _, _, err := BuildLibrary(ctx, dir, "", rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Seed legacy invalid albumArtId=0 on track 3 with foreign_keys=OFF before OpenLibrary enables them.
	rawDB, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `UPDATE Track SET albumArtId=0 WHERE id=?`, ids[2]); err != nil {
		rawDB.Close()
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	lib, err := OpenLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()

	// Link track 1 to a real embedded AlbumArt BLOB; track 2 stays NULL; track 3 has 0; track 4 has stale hash-only art.
	if _, err := lib.db.ExecContext(ctx, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(10, X'0102030405060708090a0b0c0d0e0f1011121314', X'ffd8ffe0'),(11, X'1112131415161718191a1b1c1d1e1f2021222324', NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.db.ExecContext(ctx, `UPDATE Track SET albumArtId=10 WHERE id=?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.db.ExecContext(ctx, `UPDATE Track SET albumArtId=11 WHERE id=?`, ids[3]); err != nil {
		t.Fatal(err)
	}

	// Verify EXPLAIN QUERY PLAN for MissingAlbumArtIDs uses covering index_Track_albumArtId.
	qrows, err := lib.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM Track WHERE albumArtId IS NULL OR albumArtId <= 0`)
	if err != nil {
		t.Fatal(err)
	}
	var plans []string
	for qrows.Next() {
		var a, b, c int
		var d string
		if err := qrows.Scan(&a, &b, &c, &d); err != nil {
			qrows.Close()
			t.Fatal(err)
		}
		plans = append(plans, d)
	}
	qrows.Close()
	if !strings.Contains(strings.Join(plans, "; "), "COVERING INDEX index_Track_albumArtId") {
		t.Fatalf("MissingAlbumArtIDs query plan = %v, want COVERING INDEX index_Track_albumArtId", plans)
	}

	missing, err := lib.MissingAlbumArtIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if missing[ids[0]] || !missing[ids[1]] || !missing[ids[2]] || missing[ids[3]] || len(missing) != 2 {
		t.Fatalf("MissingAlbumArtIDs = %v, want {%d, %d}", missing, ids[1], ids[2])
	}
	missingRO, err := MissingAlbumArtIDsRO(ctx, filepath.Join(dir, "m.db"))
	if err != nil || len(missingRO) != 2 || !missingRO[ids[1]] || !missingRO[ids[2]] {
		t.Fatalf("MissingAlbumArtIDsRO = %v, err=%v", missingRO, err)
	}

	newHash := sha1.Sum([]byte("rb:/PIONEER/Artwork/00001/new.jpg"))
	needTrack, needCover, err := lib.TrackUpdatePlan(ctx, ids[0], rows[0], -1, newHash[:])
	if err != nil || needTrack || needCover {
		t.Fatalf("TrackUpdatePlan embedded blob track = (%v, %v, %v), want false/false/nil", needTrack, needCover, err)
	}
	needTrack, needCover, err = lib.TrackUpdatePlan(ctx, ids[3], rows[3], -1, newHash[:])
	if err != nil || needTrack || !needCover {
		t.Fatalf("TrackUpdatePlan stale hash-only track = (%v, %v, %v), want false/true/nil", needTrack, needCover, err)
	}

	b, err := lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := b.SetDirectCover(ctx, ids[0], newHash[:])
	if err != nil || changed {
		b.Rollback()
		t.Fatalf("SetDirectCover on embedded blob = (%v, %v), want (false, nil)", changed, err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var artRows int
	if err := lib.db.QueryRowContext(ctx, `SELECT count(*) FROM AlbumArt`).Scan(&artRows); err != nil {
		t.Fatal(err)
	}
	if artRows != 2 {
		t.Fatalf("SetDirectCover created orphan AlbumArt row on embedded cover: count=%d, want 2", artRows)
	}

	// Track 2, 3 (NULL / <=0) and track 4 (stale hash-only) must be repaired, clearing MissingAlbumArtIDs.
	b, err = lib.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{ids[1], ids[2], ids[3]} {
		if ch, err := b.SetDirectCover(ctx, id, newHash[:]); err != nil || !ch {
			b.Rollback()
			t.Fatalf("SetDirectCover row %d = (%v, %v), want (true, nil)", id, ch, err)
		}
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	missingAfter, err := lib.MissingAlbumArtIDs(ctx)
	if err != nil || len(missingAfter) != 0 {
		t.Fatalf("MissingAlbumArtIDs after repair = %v, err=%v, want empty", missingAfter, err)
	}
}
