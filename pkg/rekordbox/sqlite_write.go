package rekordbox

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
)

var rekordboxMemoryColors = [8][3]byte{
	{255, 0, 127}, // 1 Pink
	{255, 0, 0},   // 2 Red
	{255, 127, 0}, // 3 Orange
	{255, 255, 0}, // 4 Yellow
	{0, 255, 0},   // 5 Green
	{0, 255, 255}, // 6 Aqua
	{0, 0, 255},   // 7 Blue
	{127, 0, 255}, // 8 Purple
}

// NearestMemoryColorID maps an RGB color to the closest 1-based index in
// rekordbox's 8-entry memory cue color table (1..8). Returns 0 if rgb is zero.
func NearestMemoryColorID(rgb [4]byte) byte {
	if rgb == [4]byte{} {
		return 0
	}
	best, distance := byte(1), int(^uint(0)>>1)
	for i, p := range rekordboxMemoryColors {
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
	return rewriteOneLibrary(ctx, dbPath, dstPath, func(plainPath string) error {
		return applyCueUpdatesSQLite(ctx, plainPath, updates)
	})
}

// UpdateOneLibraryMetadata updates editable track metadata in a rekordbox 7
// OneLibrary database (SQLCipher 4 encrypted or plaintext SQLite).
func UpdateOneLibraryMetadata(ctx context.Context, dbPath string, updates map[uint32]Metadata, dstPath string) error {
	if len(updates) == 0 {
		return nil
	}
	return rewriteOneLibrary(ctx, dbPath, dstPath, func(plainPath string) error {
		return applyMetadataUpdatesSQLite(ctx, plainPath, updates)
	})
}

// UpdateOneLibraryEdits applies cue and metadata updates to one decrypted or
// copied SQLite working file before writing the final export.
func UpdateOneLibraryEdits(ctx context.Context, dbPath string, cues map[uint32][]Cue, metadata map[uint32]Metadata, dstPath string) error {
	if len(cues) == 0 && len(metadata) == 0 {
		return nil
	}
	return rewriteOneLibrary(ctx, dbPath, dstPath, func(plainPath string) error {
		if len(cues) > 0 {
			if err := applyCueUpdatesSQLite(ctx, plainPath, cues); err != nil {
				return err
			}
		}
		if len(metadata) > 0 {
			if err := applyMetadataUpdatesSQLite(ctx, plainPath, metadata); err != nil {
				return err
			}
		}
		return nil
	})
}

func rewriteOneLibrary(ctx context.Context, dbPath, dstPath string, apply func(string) error) error {
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

	if err := apply(plainPath); err != nil {
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
	contentCols, _ := tableColumns(ctx, db, "content")
	hasBeatLoop := cols["beatloopnumerator"] && cols["beatloopdenominator"]
	has150FPS := cols["in150framepersec"] && cols["out150framepersec"]

	colNames := []string{"content_id", "kind", "colorTableIndex", "cueComment", "isActiveLoop"}
	if hasBeatLoop {
		colNames = append(colNames, "beatLoopNumerator", "beatLoopDenominator")
	}
	colNames = append(colNames, "inUsec", "outUsec")
	if has150FPS {
		colNames = append(colNames, "in150FramePerSec", "out150FramePerSec")
	}
	placeholders := make([]string, len(colNames))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	insertSQL := fmt.Sprintf("INSERT INTO cue(%s) VALUES(%s)", strings.Join(colNames, ", "), strings.Join(placeholders, ", "))

	var contentSets []string
	if contentCols["hasmodified"] {
		contentSets = append(contentSets, "hasModified = 1")
	}
	if contentCols["cueupdatecount"] {
		contentSets = append(contentSets, "cueUpdateCount = COALESCE(cueUpdateCount, 0) + 1")
	}
	contentUpdateSQL := ""
	if len(contentSets) > 0 && contentCols["content_id"] {
		contentUpdateSQL = "UPDATE content SET " + strings.Join(contentSets, ", ") + " WHERE content_id = ?"
	}

	contentIDs := make([]uint32, 0, len(updates))
	for id := range updates {
		contentIDs = append(contentIDs, id)
	}
	sort.Slice(contentIDs, func(i, j int) bool { return contentIDs[i] < contentIDs[j] })

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, contentID := range contentIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM cue WHERE content_id = ?`, contentID); err != nil {
			return fmt.Errorf("onelibrary: delete cues for %d: %w", contentID, err)
		}
		cues := make([]Cue, len(updates[contentID]))
		copy(cues, updates[contentID])
		sort.SliceStable(cues, func(i, j int) bool {
			if cues[i].HotCue != cues[j].HotCue {
				return cues[i].HotCue < cues[j].HotCue
			}
			if cues[i].TimeMS != cues[j].TimeMS {
				return cues[i].TimeMS < cues[j].TimeMS
			}
			if cues[i].Loop != cues[j].Loop {
				return !cues[i].Loop
			}
			return cues[i].EndMS < cues[j].EndMS
		})
		for _, c := range cues {
			var colorIdx int
			if c.HotCue == 0 {
				colorIdx = int(c.ColorID)
				if colorIdx == 0 && c.Color != [4]byte{} {
					colorIdx = int(NearestMemoryColorID(c.Color))
				}
				if colorIdx > 8 {
					colorIdx = 0
				}
			} else {
				colorIdx = int(c.ColorCode)
				if colorIdx == 0 && c.Color != [4]byte{} {
					colorIdx = int(NearestCueColorCode(c.Color))
				}
			}
			active := 0
			if c.Loop && c.Active {
				active = 1
			}
			inUsec := int64(c.TimeMS) * 1000
			outUsec := int64(-1)
			in150 := (int64(c.TimeMS)*150 + 500) / 1000
			out150 := int64(-1)
			if c.Loop && c.EndMS > c.TimeMS {
				outUsec = int64(c.EndMS) * 1000
				out150 = (int64(c.EndMS)*150 + 500) / 1000
			}
			args := []any{contentID, c.HotCue, colorIdx, c.Name, active}
			if hasBeatLoop {
				args = append(args, c.LoopNumerator, c.LoopDenominator)
			}
			args = append(args, inUsec, outUsec)
			if has150FPS {
				args = append(args, in150, out150)
			}
			if _, err := tx.ExecContext(ctx, insertSQL, args...); err != nil {
				return fmt.Errorf("onelibrary: insert cue for %d: %w", contentID, err)
			}
		}
		if contentUpdateSQL != "" {
			if _, err := tx.ExecContext(ctx, contentUpdateSQL, contentID); err != nil {
				return fmt.Errorf("onelibrary: update content change tracking for %d: %w", contentID, err)
			}
		}
	}
	return tx.Commit()
}

func applyMetadataUpdatesSQLite(ctx context.Context, plainPath string, updates map[uint32]Metadata) error {
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

	contentTable, err := findSQLiteTable(ctx, db, "content", "djmdcontent", "track")
	if err != nil {
		return fmt.Errorf("onelibrary: content table: %w", err)
	}
	contentCols, err := sqliteTableColumns(ctx, db, contentTable)
	if err != nil {
		return err
	}
	idCol := firstSQLiteColumn(contentCols, "content_id", "id", "track_id")
	if idCol == "" {
		return fmt.Errorf("onelibrary: content table has no id column")
	}
	genreIDCol := firstSQLiteColumn(contentCols, "genre_id")
	labelIDCol := firstSQLiteColumn(contentCols, "label_id")
	genreCol := firstSQLiteColumn(contentCols, "genre")
	labelCol := firstSQLiteColumn(contentCols, "label")
	commentCol := firstSQLiteColumn(contentCols, "djcomment", "commnt", "comment")
	if commentCol == "" && genreIDCol == "" && labelIDCol == "" && genreCol == "" && labelCol == "" {
		return fmt.Errorf("onelibrary: content table has no editable metadata columns")
	}
	genreTable, _ := findSQLiteTable(ctx, db, "genre", "djmdgenre")
	labelTable, _ := findSQLiteTable(ctx, db, "label", "djmdlabel")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for contentID, m := range updates {
		sets := []string{}
		args := []any{}
		if commentCol != "" {
			sets = append(sets, quoteSQLiteIdentifier(commentCol)+"=?")
			args = append(args, m.Comment)
		}
		if genreIDCol != "" {
			var value any
			if m.Genre != "" {
				if genreTable == "" {
					return fmt.Errorf("onelibrary: genre table missing")
				}
				id, err := lookupSQLiteID(ctx, tx, genreTable, m.Genre)
				if err != nil {
					return err
				}
				value = id
			}
			sets = append(sets, quoteSQLiteIdentifier(genreIDCol)+"=?")
			args = append(args, value)
		} else if genreCol != "" {
			sets = append(sets, quoteSQLiteIdentifier(genreCol)+"=?")
			args = append(args, m.Genre)
		}
		if labelIDCol != "" {
			var value any
			if m.Label != "" {
				if labelTable == "" {
					return fmt.Errorf("onelibrary: label table missing")
				}
				id, err := lookupSQLiteID(ctx, tx, labelTable, m.Label)
				if err != nil {
					return err
				}
				value = id
			}
			sets = append(sets, quoteSQLiteIdentifier(labelIDCol)+"=?")
			args = append(args, value)
		} else if labelCol != "" {
			sets = append(sets, quoteSQLiteIdentifier(labelCol)+"=?")
			args = append(args, m.Label)
		}
		if len(sets) == 0 {
			continue
		}
		args = append(args, contentID)
		q := `UPDATE ` + quoteSQLiteIdentifier(contentTable) + ` SET ` + strings.Join(sets, ", ") + ` WHERE ` + quoteSQLiteIdentifier(idCol) + `=?`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("onelibrary: update metadata for %d: %w", contentID, err)
		}
	}
	return tx.Commit()
}

func findSQLiteTable(ctx context.Context, db *sql.DB, names ...string) (string, error) {
	want := map[string]bool{}
	for _, name := range names {
		want[normalized(name)] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		if want[normalized(name)] {
			return name, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", sql.ErrNoRows
}

func sqliteTableColumns(ctx context.Context, db *sql.DB, table string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("onelibrary: inspect %s: %w", table, err)
	}
	defer rows.Close()
	cols := map[string]string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[normalized(name)] = name
	}
	return cols, rows.Err()
}

func firstSQLiteColumn(cols map[string]string, names ...string) string {
	for _, name := range names {
		if col := cols[normalized(name)]; col != "" {
			return col
		}
	}
	return ""
}

func quoteSQLiteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func lookupSQLiteID(ctx context.Context, tx *sql.Tx, table, name string) (int64, error) {
	cols, err := sqliteTableColumnsTx(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	idCol := firstSQLiteColumn(cols, "genre_id", "label_id", "id")
	nameCol := firstSQLiteColumn(cols, "name", "title")
	if idCol == "" || nameCol == "" {
		return 0, fmt.Errorf("onelibrary: lookup table %s has no id/name columns", table)
	}
	var id int64
	q := `SELECT ` + quoteSQLiteIdentifier(idCol) + ` FROM ` + quoteSQLiteIdentifier(table) + ` WHERE ` + quoteSQLiteIdentifier(nameCol) + `=? LIMIT 1`
	if err := tx.QueryRowContext(ctx, q, name).Scan(&id); err == nil {
		return id, nil
	} else if err != sql.ErrNoRows {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(`+quoteSQLiteIdentifier(idCol)+`),0)+1 FROM `+quoteSQLiteIdentifier(table)).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+quoteSQLiteIdentifier(table)+` (`+quoteSQLiteIdentifier(idCol)+`, `+quoteSQLiteIdentifier(nameCol)+`) VALUES (?, ?)`, id, name); err != nil {
		return 0, fmt.Errorf("onelibrary: insert lookup %s: %w", table, err)
	}
	return id, nil
}

func sqliteTableColumnsTx(ctx context.Context, tx *sql.Tx, table string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[normalized(name)] = name
	}
	return cols, rows.Err()
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, fmt.Errorf("onelibrary: inspect %s table: %w", table, err)
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
	return cols, nil
}

func cueTableColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	cols, err := tableColumns(ctx, db, "cue")
	if err != nil {
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
