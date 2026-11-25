package enginedb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
	"time"
)

// The next library is a second m.db beside the one Engine has open (design 1.1 rule 1). The job
// makes it from the live one and writes the bridge's data into it; a window renames it into
// place. MirrorLibrary is what keeps it a copy of the live library in everything the bridge did
// not write: it reads the live file in short statements and never writes it.

// Mirror is what a next library remembers of the live one.
type Mirror struct {
	State  string            // LibraryState of the live file when its rows were last taken
	Tracks map[int64]uint64  // Track.id → fingerprint of the live row
	Tables map[string]uint64 // every other table → fingerprint of all its rows
	Art    int64             // the highest AlbumArt.id taken
	Arts   int64             // AlbumArt rows the live library had
	Top    int64             // the highest Track.id the live library had issued when the next one was made
	Rows   int               // rows of the live library the last call read: what the next call reads again
}

// MirrorChange is what MirrorLibrary took from the live library.
type MirrorChange struct {
	Tracks []int64 // Track rows taken again or removed: what the bridge wrote on them in the next library is gone
	Lists  bool    // Playlist and PlaylistEntity were taken again: the bridge's lists are to be written again
	Rows   int     // rows read from the live library
}

// mirrorGap separates the IDs the bridge gives new rows of the next library from those Engine
// gives in the live one meanwhile.
const mirrorGap = 1 << 16

// ErrMirrorConflict: the next library cannot take a row of the live one (Engine added a file the
// bridge added too, or used up the gap). It is to be made again.
var ErrMirrorConflict = errors.New("enginedb: the live library and the next one disagree")

// LibraryState names what a library file holds without opening it: SQLite's change counter, which every
// committed write of any process moves, and the file's size and time. "" when there is no file.
func LibraryState(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	var head [28]byte
	st, err := f.Stat()
	if _, e := io.ReadFull(f, head[:]); err != nil || e != nil {
		return ""
	}
	wal, _ := os.Stat(file + "-wal")
	state := fmt.Sprintf("%d/%d/%d", binary.BigEndian.Uint32(head[24:]), st.Size(), st.ModTime().UnixNano())
	if wal != nil {
		state += fmt.Sprintf("/%d/%d", wal.Size(), wal.ModTime().UnixNano())
	}
	return state
}

const mirrorFlushBytes = 16 << 20 // cap dirty page cache during fresh mirror to <= 16 MiB

var syncMirrorFile = syncFilePath

type mirroring struct {
	ctx        context.Context
	db         *sql.DB
	m          *Mirror
	pace       func(took time.Duration) error
	out        MirrorChange
	triggers   bool // the next library has Engine's triggers: they go before its first row is written
	nextPath   string
	fresh      bool
	syncedSize int64
}

// MirrorLibrary brings the file next up to the library live: rows of the live library that are new or
// changed since m saw them are copied over what the next library holds, rows that are gone are
// removed. With an empty m the next library is made. pace is called after every statement with
// the time it took (the job's pacing, and its stop).
func MirrorLibrary(ctx context.Context, live, next string, m *Mirror, pace func(took time.Duration) error) (MirrorChange, error) {
	if pace != nil {
		if err := pace(0); err != nil {
			return MirrorChange{}, err
		}
	}
	state := LibraryState(live)
	fresh := m.Tracks == nil
	dsn := (&url.URL{Scheme: "file", Path: next}).String() + "?_pragma=busy_timeout(5000)"
	if fresh {
		// Nothing reads the file before it is whole: no journal, one sync at the end.
		var head [18]byte
		f, err := os.Open(live)
		if err == nil {
			_, err = io.ReadFull(f, head[:])
			f.Close()
		}
		if err != nil {
			return MirrorChange{}, err
		}
		for _, suffix := range []string{"", "-journal"} {
			if err = os.Remove(next + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return MirrorChange{}, err
			}
		}
		*m = Mirror{Tracks: map[int64]uint64{}, Tables: map[string]uint64{}}
		dsn += fmt.Sprintf("&_pragma=page_size(%d)&_pragma=journal_mode(MEMORY)&_pragma=synchronous(OFF)", binary.BigEndian.Uint16(head[16:]))
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return MirrorChange{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `ATTACH DATABASE ? AS live`, (&url.URL{Scheme: "file", Path: live, RawQuery: "mode=ro"}).String()); err != nil {
		return MirrorChange{}, err
	}
	mr := &mirroring{ctx: ctx, db: db, m: m, pace: pace, triggers: !fresh, nextPath: next, fresh: fresh}
	if fresh {
		err = mr.objects(`type='table'`)
	}
	if err == nil {
		err = mr.tracks(fresh)
	}
	if err == nil {
		err = mr.covers()
	}
	var again []string
	if err == nil {
		again, err = mr.tables()
	}
	if err == nil && fresh {
		err = mr.sequences()
	}
	if err == nil {
		// Engine's indexes, views and triggers: all of them for a new file, the triggers again otherwise.
		err = mr.objects(`type IN ('index','view','trigger') AND sql IS NOT NULL`)
	}
	// With the triggers back, which keep the chains: no entry of a track the bridge removed.
	for i := 0; err == nil && i < len(again); i++ {
		if again[i] == "PlaylistEntity" || again[i] == "PreparelistEntity" {
			err = mr.exec(`DELETE FROM main.` + again[i] + ` WHERE trackId NOT IN (SELECT id FROM main.Track)`)
		}
	}
	if err == nil {
		if err = db.Close(); err == nil && fresh {
			if pace != nil {
				err = pace(0)
			}
			if err == nil {
				err = syncFilePath(next)
			}
		}
	} else {
		_ = db.Close()
	}
	if err != nil && fresh {
		for _, suffix := range []string{"", "-journal"} {
			_ = os.Remove(next + suffix)
		}
	}
	if err == nil {
		m.State, m.Rows = state, mr.out.Rows // the state read before the rows: what Engine writes meanwhile is taken by the next call
	}
	return mr.out, err
}

func (mr *mirroring) exec(q string, args ...any) error {
	if mr.pace != nil {
		if err := mr.pace(0); err != nil {
			return err
		}
	}
	start := time.Now()
	if _, err := mr.db.ExecContext(mr.ctx, q, args...); err != nil {
		if strings.Contains(err.Error(), "constraint failed") {
			return fmt.Errorf("%w: %v", ErrMirrorConflict, err)
		}
		return err
	}
	if mr.pace != nil {
		return mr.pace(time.Since(start))
	}
	return mr.ctx.Err()
}

// strings1 are the first column of a query's rows.
func (mr *mirroring) strings1(q string, args ...any) ([]string, error) {
	rows, err := mr.db.QueryContext(mr.ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// objects makes in the next library the live library's schema objects that match and that it
// lacks, in the order Engine made them, in one transaction (none when it lacks none: a commit
// each was thirty commits of a window on the stick).
func (mr *mirroring) objects(which string) error {
	texts, err := mr.strings1(`SELECT sql FROM live.sqlite_master l WHERE name NOT LIKE 'sqlite_%' AND (` + which + `)
		AND NOT EXISTS(SELECT 1 FROM main.sqlite_master m WHERE m.type=l.type AND m.name=l.name) ORDER BY rowid`)
	if err != nil || len(texts) == 0 {
		return err
	}
	return mr.transaction(func() error {
		for _, text := range texts {
			if err := mr.exec(text); err != nil {
				return err
			}
		}
		return nil
	})
}

// plain runs do in one transaction of the next library, without Engine's triggers: rows are
// copied with their own IDs and times. The triggers go before the first such transaction and
// objects puts them back at the end; a call that copies nothing leaves them alone, and a call
// that is cut leaves none: the next call ends with all.
func (mr *mirroring) plain(do func() error) error {
	if mr.triggers {
		names, err := mr.strings1(`SELECT name FROM main.sqlite_master WHERE type='trigger'`)
		if err == nil && len(names) > 0 {
			err = mr.transaction(func() error {
				for _, name := range names {
					if err := mr.exec(`DROP TRIGGER main."` + name + `"`); err != nil {
						return err
					}
				}
				return nil
			})
		}
		if err != nil {
			return err
		}
		mr.triggers = false
	}
	return mr.transaction(do)
}

func (mr *mirroring) transaction(do func() error) error {
	err := mr.exec(`BEGIN IMMEDIATE`)
	if err == nil {
		if err = do(); err == nil {
			err = mr.exec(`COMMIT`)
		}
		if err != nil {
			_, _ = mr.db.ExecContext(context.WithoutCancel(mr.ctx), `ROLLBACK`)
		}
	}
	if err == nil && mr.fresh && mr.nextPath != "" {
		if st, serr := os.Stat(mr.nextPath); serr == nil && st.Size()-mr.syncedSize >= mirrorFlushBytes {
			if mr.pace != nil {
				err = mr.pace(0)
			}
			if err == nil {
				s0 := time.Now()
				if err = syncMirrorFile(mr.nextPath); err == nil {
					mr.syncedSize = st.Size()
					if mr.pace != nil {
						err = mr.pace(time.Since(s0))
					}
				}
			}
		}
	}
	return err
}

// print is the SQL text a table's row is told apart by: every column, a blob by its length (which
// is in the row's header: its bytes are not read).
func (mr *mirroring) print(table string) (string, error) {
	rows, err := mr.db.QueryContext(mr.ctx, `SELECT name,upper(type) FROM pragma_table_info(?,'live') ORDER BY cid`, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var col, kind string
		if err = rows.Scan(&col, &kind); err != nil {
			return "", err
		}
		if strings.Contains(kind, "BLOB") {
			parts = append(parts, `ifnull(length("`+col+`"),-1)`)
		} else {
			parts = append(parts, `quote("`+col+`")`)
		}
	}
	return strings.Join(parts, `||'|'||`), rows.Err()
}

// scan reads (id, print) of the rows after an ID, at most n.
func (mr *mirroring) scan(table, print string, after int64, n int, each func(id int64, sum uint64)) (last int64, got int, err error) {
	start := time.Now()
	rows, err := mr.db.QueryContext(mr.ctx, `SELECT rowid,`+print+` FROM live."`+table+`" WHERE rowid>? ORDER BY rowid LIMIT ?`, after, n)
	if err != nil {
		return after, 0, err
	}
	for last = after; rows.Next(); got++ {
		var text string
		if err = rows.Scan(&last, &text); err != nil {
			rows.Close()
			return last, got, err
		}
		h := fnv.New64a()
		io.WriteString(h, text)
		each(last, h.Sum64())
	}
	if err = rows.Close(); err == nil && mr.pace != nil {
		err = mr.pace(time.Since(start))
	}
	mr.out.Rows += got
	return last, got, err
}

const (
	scanRows = 250 // fingerprints a statement reads from the live file
	blobRows = 8   // rows with waveforms or a cover a statement copies
)

// tracks takes the Track rows that are new or changed, each with its PerformanceData row, which is
// never scanned: its columns lie behind the waveforms, and Engine's triggers move
// Track.lastEditTime with every write of it. That time is in whole seconds: a row written in
// the second it is taken (or later, by the library's clock) is taken again by the next call.
func (mr *mirroring) tracks(fresh bool) error {
	print, err := mr.print("Track")
	if err != nil {
		return err
	}
	var top sql.NullInt64
	if err = mr.db.QueryRowContext(mr.ctx, `SELECT seq FROM live.sqlite_sequence WHERE name='Track'`).Scan(&top); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if fresh {
		mr.m.Top = top.Int64
	} else if top.Int64 >= mr.m.Top+mirrorGap {
		return fmt.Errorf("%w: Engine added %d tracks", ErrMirrorConflict, top.Int64-mr.m.Top)
	}
	seen := mr.m.Tracks
	live := make(map[int64]bool, len(seen))
	const unsettled = 0 // no fingerprint: the row is taken again
	// put copies the rows of ids, at most n a transaction; again: over the rows the next library has.
	put := func(ids []int64, n int, again bool, tables ...[2]string) error {
		for len(ids) > 0 {
			part := ids[:min(n, len(ids))]
			ids = ids[len(part):]
			list := strings.Trim(strings.Join(strings.Fields(fmt.Sprint(part)), ","), "[]")
			if err := mr.plain(func() error {
				for _, t := range tables {
					if again {
						if e := mr.exec(`DELETE FROM main.` + t[0] + ` WHERE ` + t[1] + ` IN (` + list + `)`); e != nil {
							return e
						}
					}
					if e := mr.exec(`INSERT INTO main.` + t[0] + ` SELECT * FROM live.` + t[0] + ` WHERE ` + t[1] + ` IN (` + list + `)`); e != nil {
						return e
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}
	track, perf := [2]string{"Track", "id"}, [2]string{"PerformanceData", "trackId"}
	var all []int64
	for after, got := int64(math.MinInt64), scanRows; got == scanRows; {
		var ids []int64
		if after, got, err = mr.scan("Track", print, after, scanRows, func(id int64, sum uint64) {
			live[id] = true
			if old, ok := seen[id]; !ok || old != sum {
				seen[id] = sum
				ids = append(ids, id)
			}
		}); err != nil {
			return err
		}
		if len(ids) > 0 {
			recent, err := mr.strings1(`SELECT id FROM live.Track WHERE id>=? AND id<=? AND lastEditTime>=CAST(strftime('%s','now') AS INTEGER)-1`, ids[0], ids[len(ids)-1])
			if err != nil {
				return err
			}
			for _, text := range recent {
				var id int64
				fmt.Sscan(text, &id)
				seen[id] = unsettled
			}
		}
		if fresh {
			// A new file gets every Track row before any waveform: the rows then lie together and in
			// order, and a scan of them is a few reads of the stick. Between each other's waveforms
			// it was a read a page: 7.9 s for 5,962 tracks on a USB 2.0 stick, against 0.2 s.
			err = put(ids, scanRows, false, track)
			all = append(all, ids...)
		} else {
			err = put(ids, blobRows, true, track, perf)
		}
		if err != nil {
			return err
		}
		mr.out.Tracks = append(mr.out.Tracks, ids...)
	}
	if err = put(all, blobRows, false, perf); err != nil {
		return err
	}
	var gone []int64
	for id := range seen {
		if !live[id] {
			gone = append(gone, id)
		}
	}
	if len(gone) == 0 {
		return nil
	}
	mr.out.Tracks = append(mr.out.Tracks, gone...)
	return mr.plain(func() error {
		for _, id := range gone {
			delete(seen, id)
			for _, q := range []string{`DELETE FROM main.PerformanceData WHERE trackId=?`, `DELETE FROM main.Track WHERE id=?`} {
				if e := mr.exec(q, id); e != nil {
					return e
				}
			}
		}
		return nil
	})
}

// covers takes the AlbumArt rows Engine added (it never rewrites one) and drops those it removed.
func (mr *mirroring) covers() error {
	var top, n int64
	if err := mr.db.QueryRowContext(mr.ctx, `SELECT ifnull(max(id),0),count(*) FROM live.AlbumArt`).Scan(&top, &n); err != nil {
		return err
	}
	added := 0
	for top > mr.m.Art {
		// Covers are read a few at a time, by their IDs: an index holds those, the rows hold the images.
		ids, err := mr.strings1(`SELECT id FROM live.AlbumArt WHERE id>? ORDER BY id LIMIT ?`, mr.m.Art, blobRows)
		if err != nil || len(ids) == 0 {
			return err
		}
		if err = mr.plain(func() error {
			return mr.exec(`INSERT INTO main.AlbumArt SELECT * FROM live.AlbumArt WHERE id>? AND id<=?`, mr.m.Art, ids[len(ids)-1])
		}); err != nil {
			return err
		}
		fmt.Sscan(ids[len(ids)-1], &mr.m.Art)
		added += len(ids)
	}
	if n != mr.m.Arts+int64(added) {
		if err := mr.plain(func() error {
			return mr.exec(`DELETE FROM main.AlbumArt WHERE id<=? AND id NOT IN (SELECT id FROM live.AlbumArt)`, mr.m.Art)
		}); err != nil {
			return err
		}
	}
	mr.m.Arts = n
	mr.out.Rows += added
	return nil
}

// tables takes every other table whole when one of its rows differs: they are small, and Engine's
// lists are chains that only make sense together.
func (mr *mirroring) tables() (again []string, err error) {
	names, err := mr.strings1(`SELECT name FROM live.sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'
		AND name NOT IN ('Track','PerformanceData','AlbumArt') ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		print, err := mr.print(name)
		if err != nil {
			return nil, err
		}
		h := fnv.New64a()
		for after, got := int64(math.MinInt64), scanRows; got == scanRows; {
			if after, got, err = mr.scan(name, print, after, scanRows, func(id int64, sum uint64) { fmt.Fprintf(h, "%d %d ", id, sum) }); err != nil {
				return nil, err
			}
		}
		old, known := mr.m.Tables[name]
		if known && old == h.Sum64() {
			continue
		}
		mr.m.Tables[name] = h.Sum64()
		if name == "Information" && known {
			// Engine gives the library a new played indicator every time it mounts the stick (real
			// Engine, sandbox 2026-11-25): one statement, and no trigger is on this table. The next
			// library keeps its own commit ID.
			if err = mr.transaction(func() error {
				return mr.exec(`UPDATE main.Information SET currentPlayedIndiciator=(SELECT currentPlayedIndiciator FROM live.Information)`)
			}); err != nil {
				return nil, err
			}
			continue
		}
		if err = mr.plain(func() error {
			if e := mr.exec(`DELETE FROM main."` + name + `"`); e != nil {
				return e
			}
			return mr.exec(`INSERT INTO main."` + name + `" SELECT * FROM live."` + name + `"`)
		}); err != nil {
			return nil, err
		}
		if known {
			again = append(again, name)
			mr.out.Lists = mr.out.Lists || name == "Playlist" || name == "PlaylistEntity"
		}
	}
	return again, nil
}

// sequences gives a new next library the live library's ID counters, moved up by the gap, and
// its two header fields Engine may read.
func (mr *mirroring) sequences() error {
	err := mr.exec(`DELETE FROM main.sqlite_sequence`)
	if err != nil {
		return err
	}
	// Every table with a counter gets one, so that a table Engine left empty is counted too.
	if err = mr.exec(`INSERT INTO main.sqlite_sequence(name,seq) SELECT m.name,ifnull((SELECT seq FROM live.sqlite_sequence s WHERE s.name=m.name),0)+?
		FROM live.sqlite_master m WHERE m.type='table' AND m.sql LIKE '%AUTOINCREMENT%'`, mirrorGap); err != nil {
		return err
	}
	for _, pragma := range []string{"user_version", "application_id"} {
		var v int64
		if err = mr.db.QueryRowContext(mr.ctx, `PRAGMA live.`+pragma).Scan(&v); err != nil {
			return err
		}
		if err = mr.exec(fmt.Sprintf(`PRAGMA main.%s=%d`, pragma, v)); err != nil {
			return err
		}
	}
	return nil
}
