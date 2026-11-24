package enginedb

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssue233ArtworkDeadlineCommitsProgressAndRetries(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE AlbumArt(id INTEGER PRIMARY KEY,hash,albumArt BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		data := []byte{0xff, 0xd8, 0xff, byte(i)}
		sum := sha1.Sum(data)
		if _, err := tx.Exec(`INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, hex.EncodeToString(sum[:]), data); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	calls := 0
	now := func() time.Time {
		calls++
		if calls > 12 {
			return start.Add(time.Hour)
		}
		return start
	}
	dir := t.TempDir()
	n, err := migrateArtwork(context.Background(), tx, dir, start.Add(time.Minute), now)
	if err != nil || n <= 0 || n >= 100 {
		t.Fatalf("partial artwork migration: %d %v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rest, err := migrateArtwork(context.Background(), tx, dir, time.Time{}, time.Now)
	if err != nil || n+rest != 100 {
		t.Fatalf("artwork retry: %d + %d %v", n, rest, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateArtworkUsesCoveringHashIndex(t *testing.T) {
	mount := t.TempDir()
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	artOut := filepath.Join(mount, "Engine Library", "Artwork")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(artOut, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE AlbumArt (id INTEGER PRIMARY KEY AUTOINCREMENT, hash TEXT, albumArt BLOB);
CREATE INDEX index_AlbumArt_hash ON AlbumArt (hash);`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("EXPLAIN QUERY PLAN " + selectAlbumArtIndexQuery)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	rows.Close()
	if !strings.Contains(plan.String(), "COVERING INDEX index_AlbumArt_hash") {
		t.Fatalf("expected covering index scan on index_AlbumArt_hash, got plan:\n%s", plan.String())
	}

	jpeg1 := []byte{0xff, 0xd8, 0xff, 0xd9}
	sum1 := sha1.Sum(jpeg1)
	if _, err := db.Exec(`INSERT INTO AlbumArt(hash, albumArt) VALUES(?, ?)`, sum1[:], jpeg1); err != nil {
		t.Fatal(err)
	}
	jpg1 := filepath.Join(artOut, base64.RawURLEncoding.EncodeToString(sum1[:])+".jpg")
	if err := os.WriteFile(jpg1, jpeg1, 0o644); err != nil {
		t.Fatal(err)
	}
	db.Close()

	beforeBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	n, err := MigrateArtworkBefore(context.Background(), mount, time.Now().Add(time.Minute))
	if err != nil || n != 0 {
		t.Fatalf("already-migrated MigrateArtworkBefore = (%d, %v), want (0, nil)", n, err)
	}
	afterBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeBytes[:100]) != string(afterBytes[:100]) {
		t.Fatal("MigrateArtworkBefore modified SQLite header when 0 covers needed migration")
	}

	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	jpeg2 := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x02, 0xff, 0xd9}
	sum2 := sha1.Sum(jpeg2)
	if _, err := db2.Exec(`INSERT INTO AlbumArt(hash, albumArt) VALUES(?, ?)`, hex.EncodeToString(sum2[:]), jpeg2); err != nil {
		db2.Close()
		t.Fatal(err)
	}
	db2.Close()

	n, err = MigrateArtworkBefore(context.Background(), mount, time.Now().Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("legacy MigrateArtworkBefore = (%d, %v), want (1, nil)", n, err)
	}
	jpg2 := filepath.Join(artOut, base64.RawURLEncoding.EncodeToString(sum2[:])+".jpg")
	if _, err := os.Stat(jpg2); err != nil {
		t.Fatalf("migrated artwork file missing: %v", err)
	}
}
