package enginedb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// CopyBridgeStateCompact writes the bridge state file state to dst without the waveform copies
// earlier builds kept in it and without its free pages (#253): an update copied the whole file
// first, 2.7 GB on the maintainer's 6000-track stick, at USB speed. A row that holds a waveform
// becomes the reference this build writes: its source ID and the ANLZ files refs names for it, which
// the caller checked to give the display a waveform (DetailServes). Those rows are not read: SQLite
// reads a blob's overflow pages for any part of it, but not for length(). Their source ID is the
// track's pdbImportKey in mdb, a consistent copy of the library the state belongs to, when no
// other bridge track has that key. A row whose key is shared (libraries of early builds) or for
// which refs names no file is copied as it is: a reference never replaces the only copy of a
// waveform the display can find, also when the export names files that are missing, truncated or
// renamed on the stick (#253 reviews).
func CopyBridgeStateCompact(ctx context.Context, state, mdb, dst string, refs map[uint32][]string) error {
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	ro := func(p string) string { return (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String() }
	// A private file nothing opens before it is complete and renamed: no journal, one sync.
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dst}).String()+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) error {
		db.Close()
		os.Remove(dst)
		return err
	}
	for _, a := range [][2]string{{ro(state), "old"}, {ro(mdb), "lib"}} {
		if _, err = db.ExecContext(ctx, `ATTACH DATABASE ? AS `+a[1], a[0]); err != nil {
			return fail(err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT type,name,sql FROM old.sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY type='index'`)
	if err != nil {
		return fail(err)
	}
	var tables, schema []string
	for rows.Next() {
		var typ, name, q string
		if err = rows.Scan(&typ, &name, &q); err != nil {
			rows.Close()
			return fail(err)
		}
		schema = append(schema, q)
		if typ == "table" && name != "BridgeAnalysis" {
			tables = append(tables, name)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fail(err)
	}
	for _, q := range schema {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fail(fmt.Errorf("bridge state schema: %w", err))
		}
	}
	for _, t := range tables {
		name := `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		if _, err = tx.ExecContext(ctx, `INSERT INTO main.`+name+` SELECT * FROM old.`+name); err != nil {
			return fail(err)
		}
	}
	const small = `length(a.sourceJSON)<=` + waveformRowBytes
	if _, err = tx.ExecContext(ctx, `INSERT INTO main.BridgeAnalysis SELECT a.* FROM old.BridgeAnalysis a WHERE `+small); err != nil {
		return fail(err)
	}
	keys := map[int64]int{}
	if rows, err = tx.QueryContext(ctx, `SELECT t.pdbImportKey,count(*) FROM old.BridgeState s JOIN lib.Track t ON t.id=s.trackId WHERE t.pdbImportKey>0 GROUP BY 1`); err != nil {
		return fail(err)
	}
	for rows.Next() {
		var key int64
		var n int
		if err = rows.Scan(&key, &n); err != nil {
			rows.Close()
			return fail(err)
		}
		keys[key] = n
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fail(err)
	}
	type big struct{ id, key int64 }
	var bigs []big
	if rows, err = tx.QueryContext(ctx, `SELECT a.trackId,COALESCE(t.pdbImportKey,0) FROM old.BridgeAnalysis a LEFT JOIN lib.Track t ON t.id=a.trackId WHERE NOT `+small); err != nil {
		return fail(err)
	}
	for rows.Next() {
		var b big
		if err = rows.Scan(&b.id, &b.key); err != nil {
			rows.Close()
			return fail(err)
		}
		bigs = append(bigs, b)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fail(err)
	}
	emptyAudio, err := packAnalysis([]byte("{}"))
	if err != nil {
		return fail(err)
	}
	for _, b := range bigs {
		if files := refs[uint32(b.key)]; b.key > 0 && b.key <= 0xffffffff && keys[b.key] == 1 && len(files) > 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO main.BridgeAnalysis(trackId,sourceJSON,audioJSON) VALUES(?,?,?)`, b.id, packBridgeRef(uint32(b.key), files), emptyAudio)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO main.BridgeAnalysis SELECT * FROM old.BridgeAnalysis WHERE trackId=?`, b.id)
		}
		if err != nil {
			return fail(err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	if err = db.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	if err = syncFilePath(dst); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// waveformRowBytes: a state row longer than this still holds a waveform (RBA1 or JSON); a
// reference is about 100 bytes.
const waveformRowBytes = "4096"

// WaveformRowKeys are the pdbImportKeys, in the library mdb, of the tracks whose row in the bridge
// state file state still holds a waveform: the rows CopyBridgeStateCompact may replace by a
// reference. Read with length() only, so no waveform is read.
func WaveformRowKeys(ctx context.Context, state, mdb string) (map[uint32]bool, error) {
	ro := func(p string) string { return (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String() }
	db, err := sql.Open("sqlite", ro(state))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `ATTACH DATABASE ? AS lib`, ro(mdb)); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT t.pdbImportKey FROM BridgeAnalysis a JOIN lib.Track t ON t.id=a.trackId
		WHERE length(a.sourceJSON)>`+waveformRowBytes+` AND t.pdbImportKey>0 AND t.pdbImportKey<=4294967295`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[uint32]bool{}
	for rows.Next() {
		var key int64
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys[uint32(key)] = true
	}
	return keys, rows.Err()
}

// ExportRefs are the ANLZ files of the display's detail of each track of l, by its ID, as the
// export names them. Unchecked: CopyBridgeStateCompact gets only those DetailServes confirms.
func ExportRefs(l *rekordbox.Library) map[uint32][]string {
	refs := make(map[uint32][]string, len(l.Tracks))
	for _, t := range l.Tracks {
		if files := exportDetailFiles(t.AnalysisPath); len(files) > 0 {
			refs[t.ID] = files
		}
	}
	return refs
}
