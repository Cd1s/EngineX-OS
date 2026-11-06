package enginedb

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
)

// Engine 5.3.0 answers GET_ALBUM_ART(hash) and GET_ALBUM_ART_SIZE(hash), which
// serve covers to other decks over Engine Link, only for a 20-byte BLOB hash
// (the raw SHA-1), reading Engine Library/Artwork/<base64url(hash)>.jpg. A
// 40-character TEXT hash yields nothing, so remote decks show no cover.
const artworkDir = "Engine Library/Artwork"

// putArtwork writes data as the file Engine looks up for sum, unless it exists.
func putArtwork(dir string, sum, data []byte) error {
	p := filepath.Join(dir, base64.RawURLEncoding.EncodeToString(sum)+".jpg")
	if _, e := os.Lstat(p); e == nil {
		return nil
	}
	if e := os.WriteFile(p+".tmp", data, 0o644); e != nil {
		return e
	}
	return os.Rename(p+".tmp", p)
}

// migrateArtwork turns earlier builds' TEXT hashes into BLOB hashes plus an
// Artwork file. A row whose file cannot be written keeps its TEXT hash, so
// the next run retries it.
func migrateArtwork(ctx context.Context, tx *sql.Tx, dir string) (migrated int, err error) {
	var last int64
	for {
		var id int64
		var text string
		var data []byte
		e := tx.QueryRowContext(ctx, `SELECT id,hash,albumArt FROM AlbumArt WHERE id>? AND typeof(hash)='text' AND length(hash)=40 AND albumArt IS NOT NULL ORDER BY id LIMIT 1`, last).Scan(&id, &text, &data)
		if e == sql.ErrNoRows {
			return migrated, nil
		}
		if e != nil {
			return migrated, e
		}
		last = id
		sum, e := hex.DecodeString(text)
		if e != nil {
			continue
		}
		if e = putArtwork(dir, sum, data); e != nil {
			log.Printf("rb-bridge: artwork %d left as is: %v", id, e)
			continue
		}
		if _, e = tx.ExecContext(ctx, `UPDATE AlbumArt SET hash=? WHERE id=?`, sum, id); e != nil {
			return migrated, e
		}
		migrated++
	}
}

// MigrateArtwork upgrades the library under mount in place. Engine must not
// have m.db open: bridge only ever writes it before Engine attaches the volume.
func MigrateArtwork(ctx context.Context, mount string) (int, error) {
	dir, err := compat.SafeOutputDir(mount, artworkDir)
	if err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(mount, "Engine Library/Database2/m.db")}).String()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := migrateArtwork(ctx, tx, dir)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
