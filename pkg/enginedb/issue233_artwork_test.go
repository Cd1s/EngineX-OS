package enginedb

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
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
