package rekordbox

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// NearestCueColorCode maps an RGB color to the closest 1-based index in
// rekordboxCuePalette (1..62). Returns 0 if rgb has no alpha/color set.
func NearestCueColorCode(rgb [4]byte) byte {
	if rgb == [4]byte{} {
		return 0
	}
	best, distance := byte(1), int(^uint(0)>>1)
	for i, p := range rekordboxCuePalette {
		d := 0
		for j := range p {
			delta := int(p[j]) - int(rgb[j+1])
			d += delta * delta
		}
		if d < distance {
			best, distance = byte(i+1), d
		}
	}
	return best
}

// UpdateOneLibraryCues updates the cue table in a rekordbox 7 OneLibrary
// database (SQLCipher 4 encrypted or plaintext SQLite) for the specified
// content_id -> cues map, writing the resulting database to dstPath.
func UpdateOneLibraryCues(ctx context.Context, dbPath string, updates map[uint32][]Cue, dstPath string) error {
	if len(updates) == 0 {
		return nil
	}
	encrypted, err := isEncryptedSQLite(dbPath)
	if err != nil {
		return err
	}

	var salt []byte
	plainPath := dbPath
	if encrypted {
		salt, err = ReadSQLCipherSalt(dbPath)
		if err != nil {
			return err
		}
		plainPath, err = decryptSQLCipher4ToTemp(ctx, dbPath, []byte(oneLibraryPassphrase))
		if err != nil {
			return err
		}
		defer os.Remove(plainPath)
	} else {
		// Work on a temp copy so dbPath is never mutated in place.
		tmpPlain, err := copyToOneLibraryTemp(dbPath)
		if err != nil {
			return err
		}
		defer os.Remove(tmpPlain)
		plainPath = tmpPlain
	}

	if err := applyCueUpdatesSQLite(ctx, plainPath, updates); err != nil {
		return err
	}

	if !encrypted {
		return copyFileTo(plainPath, dstPath)
	}

	in, err := os.Open(plainPath)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dstPath)
		}
	}()
	bw := bufio.NewWriterSize(out, 64*1024)
	if err := EncryptSQLCipher4(ctx, io.LimitReader(in, st.Size()), st.Size(), bw, salt, []byte(oneLibraryPassphrase)); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func applyCueUpdatesSQLite(ctx context.Context, plainPath string, updates map[uint32][]Cue) error {
	u := url.URL{Scheme: "file", Path: plainPath}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=DELETE;`); err != nil {
		return fmt.Errorf("onelibrary: set journal_mode: %w", err)
	}

	cols, err := cueTableColumns(ctx, db)
	if err != nil {
		return err
	}
	hasBeatLoop := cols["beatloopnumerator"] && cols["beatloopdenominator"]

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for contentID, cues := range updates {
		if _, err := tx.ExecContext(ctx, `DELETE FROM cue WHERE content_id = ?`, contentID); err != nil {
			return fmt.Errorf("onelibrary: delete cues for %d: %w", contentID, err)
		}
		for _, c := range cues {
			colorIdx := int(c.ColorCode)
			if colorIdx == 0 && c.Color != [4]byte{} {
				colorIdx = int(NearestCueColorCode(c.Color))
			}
			active := 0
			if c.Active {
				active = 1
			}
			inUsec := int64(c.TimeMS) * 1000
			outUsec := int64(0)
			if c.Loop && c.EndMS > c.TimeMS {
				outUsec = int64(c.EndMS) * 1000
			}
			if hasBeatLoop {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO cue(content_id, kind, colorTableIndex, cueComment, isActiveLoop, beatLoopNumerator, beatLoopDenominator, inUsec, outUsec) VALUES(?,?,?,?,?,?,?,?,?)`,
					contentID, c.HotCue, colorIdx, c.Name, active, c.LoopNumerator, c.LoopDenominator, inUsec, outUsec,
				); err != nil {
					return fmt.Errorf("onelibrary: insert cue for %d: %w", contentID, err)
				}
			} else {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO cue(content_id, kind, colorTableIndex, cueComment, isActiveLoop, inUsec, outUsec) VALUES(?,?,?,?,?,?,?)`,
					contentID, c.HotCue, colorIdx, c.Name, active, inUsec, outUsec,
				); err != nil {
					return fmt.Errorf("onelibrary: insert cue for %d: %w", contentID, err)
				}
			}
		}
	}
	return tx.Commit()
}

func cueTableColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(cue)`)
	if err != nil {
		return nil, fmt.Errorf("onelibrary: inspect cue table: %w", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !cols["content_id"] || !cols["kind"] || !cols["inusec"] {
		return nil, fmt.Errorf("onelibrary: unexpected cue table schema")
	}
	return cols, nil
}

func copyToOneLibraryTemp(srcPath string) (string, error) {
	dir := oneLibraryTempDir()
	if err := prepareOneLibraryTempDir(dir); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, fmt.Sprintf("%s%d-*.db", oneLibraryTempPrefix, os.Getpid()))
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	if err := copyFileTo(srcPath, tmpName); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}

func copyFileTo(srcPath, dstPath string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dstPath)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
