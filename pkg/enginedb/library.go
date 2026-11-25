package enginedb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"github.com/google/uuid"
)

// Library is a stick's m.db opened for one mount window, before Engine sees the stick: the only
// writer of m.db in the bridge (design §1.4). Rows are keyed by Engine's own IDs, which the
// snapshot keeps; nothing of the bridge is stored in m.db.
type Library struct {
	db    *sql.DB
	mount string // the volume: covers go to its Engine Library/Artwork
	uuid  string
	bulk  bool
}

// TrackRow is a track as the export describes it.
type TrackRow struct {
	Rel   string          // the path the row gets, volume-relative
	Track rekordbox.Track // the export's fields; Cues are the export's own cue rows, if it has any
}

type ListRow struct {
	Title  string
	Parent int64 // Playlist.id of its folder, 0 at the root
	Before int64 // Playlist.id of the sibling it goes in front of, 0 to be the last
}

// Outcome says what ApplyPerformance did with a row.
type Outcome uint8

const (
	PerfWritten Outcome = iota + 1 // the row is the staged data
	PerfSame                       // it already was
	PerfMerged                     // staged data with what the DJ did on a row the export alone had filled
	PerfKept                       // the deck's grid, cues and loops stay; waveforms and track data were written
)

// OpenLibrary opens dir/m.db for a window, made with Engine's schema when there is none.
// ErrLibraryInUse: another process has it open (Engine would write its own pages over ours).
func OpenLibrary(ctx context.Context, dir string) (*Library, error) {
	return openLibrary(ctx, filepath.Join(dir, "m.db"), filepath.Dir(filepath.Dir(dir)), false, "")
}

// OpenLibraryFile opens a library file of the volume mounted at mount that is not its m.db: the
// next library, which Engine does not have open.
func OpenLibraryFile(ctx context.Context, file, mount string) (*Library, error) {
	return openLibrary(ctx, file, mount, false, "")
}

func openLibrary(ctx context.Context, p, mount string, bulk bool, newUUID string) (*Library, error) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := safeFile(p + suffix); err != nil {
			return nil, err
		}
	}
	if !bulk && OpenElsewhere(p) != 0 {
		return nil, ErrLibraryInUse
	}
	dsn := (&url.URL{Scheme: "file", Path: p}).String() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	if bulk {
		// A private build that nothing opens before it is renamed into place.
		dsn += "&_pragma=journal_mode(MEMORY)&_pragma=synchronous(OFF)"
	} else {
		dsn += "&_pragma=cache_size(-16384)" // 16 MiB: a window's batch stays in memory until its commit
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if newUUID == "" {
		newUUID = uuid.NewString()
	}
	l := &Library{db: db, mount: mount, bulk: bulk, uuid: newUUID}
	if err = l.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Library) init(ctx context.Context) error {
	var tables int
	if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("engine schema: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO Information(id,uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch,currentPlayedIndiciator,lastRekordBoxLibraryImportReadCounter) VALUES(1,?,3,0,2,?,0)`, l.uuid, playedIndicator()); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	var major, minor, patch, rows int
	var id sql.NullInt64
	var libUUID sql.NullString
	var played sql.NullInt64
	err := l.db.QueryRowContext(ctx, `SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch,id,uuid,currentPlayedIndiciator,(SELECT count(*) FROM Information) FROM Information`).Scan(&major, &minor, &patch, &id, &libUUID, &played, &rows)
	if err != nil {
		return fmt.Errorf("engine library: %w", err)
	}
	if rows != 1 || major != 3 || minor != 0 || patch != 2 {
		return fmt.Errorf("unsupported Engine database schema %d.%d.%d", major, minor, patch)
	}
	// Engine treats a library as corrupt when Information.id is not 1 or its uuid is empty, and
	// initialises the played indicator itself; earlier builds left these unset.
	if id.Int64 != 1 || strings.TrimSpace(libUUID.String) == "" || played.Int64 == 0 {
		if _, err = l.db.ExecContext(ctx, `UPDATE Information SET id=1, uuid=CASE WHEN uuid IS NULL OR trim(uuid)='' THEN ? ELSE uuid END,
			currentPlayedIndiciator=CASE WHEN COALESCE(currentPlayedIndiciator,0)=0 THEN ? ELSE currentPlayedIndiciator END`, uuid.NewString(), playedIndicator()); err != nil {
			return err
		}
		if err = l.db.QueryRowContext(ctx, `SELECT uuid FROM Information`).Scan(&libUUID); err != nil {
			return err
		}
	}
	l.uuid = strings.TrimSpace(libUUID.String)
	return nil
}

func (l *Library) UUID() string { return l.uuid }

func (l *Library) Close() error { return l.db.Close() }

// Counts are the library's tracks and lists: whether the stick has a library to keep.
func (l *Library) Counts(ctx context.Context) (tracks, lists int, err error) {
	err = l.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM Track),(SELECT count(*) FROM Playlist)`).Scan(&tracks, &lists)
	return tracks, lists, err
}

type Batch struct {
	l          *Library
	tx         *sql.Tx
	stmts      map[string]*sql.Stmt
	saved      []string                  // Engine's own SQL of the indexes and triggers suspended for the batch
	nextTrack  int64                     // bulk: the next Track.id
	nextEntity int64                     // the next PlaylistEntity.id, 0 until a list is written
	lists      bool                      // a list was added: Engine's persist trigger is suspended
	covers     map[[sha1.Size]byte]int64 // AlbumArt.id of the images this batch has put
	now        int64
	commitID   int64
}

// CommitID is the library's CommitID once this batch is committed.
func (b *Batch) CommitID() int64 { return b.commitID }

// CommitID names the bridge's last committed transaction: Information.lastRekordBoxLibraryImportReadCounter (design §5).
func (l *Library) CommitID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := l.db.QueryRowContext(ctx, `SELECT lastRekordBoxLibraryImportReadCounter FROM Information`).Scan(&id)
	return id.Int64, err
}

// Row is a Track row's ID and path.
type Row struct {
	ID   int64
	Path string // as stored, "../…"
	Edit int64  // lastEditTime
}

// TrackIndex is every row by the key of its path (read from the unique path index, not the rows).
func (l *Library) TrackIndex(ctx context.Context) (map[string]Row, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id,path,COALESCE(lastEditTime,0) FROM Track WHERE path IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Row{}
	for rows.Next() {
		var r Row
		if err = rows.Scan(&r.ID, &r.Path, &r.Edit); err != nil {
			return nil, err
		}
		out[rbstate.FoldPath(r.Path)] = r
	}
	return out, rows.Err()
}

func (l *Library) ListIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id FROM Playlist`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Begin starts a transaction. In a bulk build it drops Engine's secondary indexes on Track and
// PlaylistEntity and the two Track triggers that only fill derived data; Commit rebuilds them.
func (l *Library) Begin(ctx context.Context) (*Batch, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	b := &Batch{l: l, tx: tx, stmts: map[string]*sql.Stmt{}, now: time.Now().Unix()}
	// Every transaction moves Engine's counter of rekordbox imports: the commit ID (design §5).
	if err = tx.QueryRowContext(ctx, `UPDATE Information SET lastRekordBoxLibraryImportReadCounter=COALESCE(lastRekordBoxLibraryImportReadCounter,0)+1 RETURNING lastRekordBoxLibraryImportReadCounter`).Scan(&b.commitID); err != nil {
		tx.Rollback()
		return nil, err
	}
	if l.bulk {
		if err = b.startBulk(ctx); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	return b, nil
}

func (b *Batch) startBulk(ctx context.Context) error {
	if err := b.suspend(ctx, `(type='index' AND tbl_name IN ('Track','PlaylistEntity')) OR
		(type='trigger' AND name IN ('trigger_after_insert_Track_fix_origin','trigger_after_insert_Track_insert_performance_data'))`); err != nil {
		return err
	}
	return b.tx.QueryRowContext(ctx, `SELECT max(COALESCE((SELECT seq FROM sqlite_sequence WHERE name='Track'),0),COALESCE((SELECT max(id) FROM Track),0))`).Scan(&b.nextTrack)
}

// suspend drops the indexes and triggers that match for the rest of the batch; Commit makes
// them again in the same transaction, so no other connection ever sees the library without them.
func (b *Batch) suspend(ctx context.Context, which string) error {
	rows, err := b.tx.QueryContext(ctx, `SELECT type,name,sql FROM sqlite_master WHERE sql IS NOT NULL AND (`+which+`) ORDER BY rowid`)
	if err != nil {
		return err
	}
	var drops []string
	for rows.Next() {
		var kind, name, text string
		if err = rows.Scan(&kind, &name, &text); err != nil {
			rows.Close()
			return err
		}
		b.saved = append(b.saved, text)
		drops = append(drops, fmt.Sprintf(`DROP %s "%s"`, kind, name))
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, d := range drops {
		if _, err = b.tx.ExecContext(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (b *Batch) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	st, err := b.stmt(ctx, query)
	if err != nil {
		return nil, err
	}
	return st.ExecContext(ctx, args...)
}

func (b *Batch) stmt(ctx context.Context, query string) (*sql.Stmt, error) {
	if st := b.stmts[query]; st != nil {
		return st, nil
	}
	st, err := b.tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	b.stmts[query] = st
	return st, nil
}

func (b *Batch) closeStmts() {
	for _, st := range b.stmts {
		st.Close()
	}
	b.stmts = nil
}

func (b *Batch) Rollback() {
	b.closeStmts()
	b.tx.Rollback()
}

func (b *Batch) Commit(ctx context.Context) error {
	if b.l.bulk {
		if _, err := b.tx.ExecContext(ctx, `UPDATE Track SET originTrackId=id,originDatabaseUuid=(SELECT uuid FROM Information)
			WHERE IFNULL(originTrackId,0)=0 OR IFNULL(originDatabaseUuid,'')=''`); err != nil {
			b.Rollback()
			return err
		}
	}
	for _, text := range b.saved {
		if _, err := b.tx.ExecContext(ctx, text); err != nil {
			b.Rollback()
			return fmt.Errorf("rebuild %q: %w", text, err)
		}
	}
	b.closeStmts()
	return b.tx.Commit()
}

const (
	libTrackColumns = `length,bpm,year,path,filename,bitrate,bpmAnalyzed,albumArtId,fileBytes,title,artist,album,genre,comment,label,composer,remixer,key,rating,fileType,isAnalyzed,isAvailable,isMetadataImported,pdbImportKey,dateCreated,dateAdded`
	libTrackTuple   = `(?,?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,?,?,?,?,?,0,1,1,?,?,?)`
)

// trackArgs are a row's values in libTrackColumns order, without the cover and the three flags.
func (b *Batch) trackArgs(r *TrackRow) []any {
	t := &r.Track
	bpm := t.BPM
	if bpm >= 499.5 { // rekordbox's analyser clamps a misparsed AIFF to 500 BPM
		bpm = 0
	}
	return []any{int(t.Duration + 0.5), int(bpm + 0.5), t.Year, "../" + r.Rel, path.Base(r.Rel), t.Bitrate, bpm, t.FileSize,
		t.Title, t.Artist, t.Album, t.Genre, t.Comment, t.Label, t.Composer, t.Remixer, EngineKey(t.Key), t.Rating,
		strings.TrimPrefix(strings.ToLower(path.Ext(r.Rel)), "."), int64(t.ID), b.now, b.now}
}

// InsertTracks adds rows. A path an Engine-made row holds is not added: native[i] is set and
// ids[i] is that row, which lists may name and the bridge never writes (#252). A new row starts
// with the export's own cue rows, marked as imported, so a track loaded before it is staged has
// its hot cues (Engine analyses it at that load).
func (b *Batch) InsertTracks(ctx context.Context, rows []TrackRow) (ids []int64, native []bool, err error) {
	ids, native = make([]int64, len(rows)), make([]bool, len(rows))
	if b.l.bulk {
		return ids, native, b.insertTracksBulk(ctx, rows, ids)
	}
	insert := `INSERT INTO Track(` + libTrackColumns + `) VALUES` + libTrackTuple
	for i := range rows {
		res, e := b.exec(ctx, insert, b.trackArgs(&rows[i])...)
		if e != nil {
			var id int64
			if qe := b.tx.QueryRowContext(ctx, `SELECT id FROM Track WHERE path=?`, "../"+rows[i].Rel).Scan(&id); qe == nil {
				ids[i], native[i] = id, true
				continue
			}
			return nil, nil, fmt.Errorf("insert track %d: %w", rows[i].Track.ID, e)
		}
		if ids[i], e = res.LastInsertId(); e != nil {
			return nil, nil, e
		}
		if base := fastBaseOf(rows[i].Track); base.cues != nil {
			if _, e = b.exec(ctx, `UPDATE PerformanceData SET quickCues=?,loops=?,activeOnLoadLoops=?,thirdPartySourceId=1 WHERE trackId=?`, base.cues, base.loops, base.active, ids[i]); e != nil {
				return nil, nil, e
			}
		}
	}
	return ids, native, nil
}

const libBatchRows = 100

func (b *Batch) insertTracksBulk(ctx context.Context, rows []TrackRow, ids []int64) error {
	const cols = 23
	var trackArgs, perfArgs []any
	flush := func() error {
		if n := len(trackArgs) / cols; n > 0 {
			q := `INSERT INTO Track(id,` + libTrackColumns + `) VALUES` + strings.Repeat(",(?,"+libTrackTuple[1:], n)[1:]
			if _, err := b.exec(ctx, q, trackArgs...); err != nil {
				return err
			}
			q = `INSERT INTO PerformanceData(trackId,quickCues,loops,activeOnLoadLoops,thirdPartySourceId) VALUES` + strings.Repeat(",(?,?,?,?,?)", n)[1:]
			if _, err := b.exec(ctx, q, perfArgs...); err != nil {
				return err
			}
		}
		trackArgs, perfArgs = trackArgs[:0], perfArgs[:0]
		return nil
	}
	for i := range rows {
		b.nextTrack++
		ids[i] = b.nextTrack
		trackArgs = append(append(trackArgs, ids[i]), b.trackArgs(&rows[i])...)
		var cues, loops []byte
		var active, mark any
		if base := fastBaseOf(rows[i].Track); base.cues != nil {
			cues, loops, active, mark = base.cues, base.loops, base.active, 1
		}
		perfArgs = append(perfArgs, ids[i], cues, loops, active, mark)
		if len(trackArgs) >= libBatchRows*cols {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// UpdateTrack gives the row the export's fields and says whether it held anything else. An
// analysed row keeps its own length, BPM, bitrate and size (ApplyPerformance's). A row that holds
// it all already is not written: a write fires Engine's triggers, 31 ms a row on a cold stick
// with a 313 MB library (64b0ea95). The rating stays the deck's when the deck changed it:
// oldRating is what the bridge last wrote (negative: unknown, the export's is taken).
func (b *Batch) UpdateTrack(ctx context.Context, id int64, r TrackRow, oldRating int) (changed bool, err error) {
	a := b.trackArgs(&r)
	const (
		rating = `CASE WHEN ?22<0 OR COALESCE(rating,0)=?22 THEN ?18 ELSE rating END`
		own    = `COALESCE(isAnalyzed,0)` // the figures are the audio's own
	)
	res, err := b.exec(ctx, `UPDATE Track SET length=CASE WHEN `+own+` THEN length ELSE ?1 END,bpm=CASE WHEN `+own+` THEN bpm ELSE ?2 END,
		bitrate=CASE WHEN `+own+` THEN bitrate ELSE ?6 END,bpmAnalyzed=CASE WHEN `+own+` THEN bpmAnalyzed ELSE ?7 END,
		fileBytes=CASE WHEN `+own+` THEN fileBytes ELSE ?8 END,year=?3,title=?9,artist=?10,album=?11,genre=?12,comment=?13,label=?14,composer=?15,
		remixer=?16,key=?17,rating=`+rating+`,pdbImportKey=?20
		WHERE id=?21 AND NOT ((`+own+` OR (length IS ?1 AND bpm IS ?2 AND bitrate IS ?6 AND bpmAnalyzed IS ?7 AND fileBytes IS ?8))
		AND year IS ?3 AND title IS ?9 AND artist IS ?10 AND album IS ?11 AND genre IS ?12 AND comment IS ?13 AND label IS ?14 AND composer IS ?15
		AND remixer IS ?16 AND key IS ?17 AND rating IS (`+rating+`) AND pdbImportKey IS ?20)`,
		append(append(a[:len(a)-2:len(a)-2], id), oldRating)...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MoveTracks gives rows new paths in one go; paths may be swapped among them (#4).
func (b *Batch) MoveTracks(ctx context.Context, moves map[int64]string) error {
	for id := range moves {
		if _, err := b.exec(ctx, `UPDATE Track SET path=? WHERE id=?`, fmt.Sprintf(".rb-bridge-moving-%d", id), id); err != nil {
			return err
		}
	}
	for id, rel := range moves {
		if _, err := b.exec(ctx, `UPDATE Track SET path=?,filename=?,fileType=? WHERE id=?`, "../"+rel, path.Base(rel), strings.TrimPrefix(strings.ToLower(path.Ext(rel)), "."), id); err != nil {
			return fmt.Errorf("move track %d: %w", id, err)
		}
	}
	return nil
}

// DeleteTrack removes a row the bridge owns, with its list entries and its performance data.
func (b *Batch) DeleteTrack(ctx context.Context, id int64) error {
	for _, q := range []string{`DELETE FROM PlaylistEntity WHERE trackId=?`, `DELETE FROM PreparelistEntity WHERE trackId=?`, `DELETE FROM Track WHERE id=?`} {
		if _, err := b.exec(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// uniqueTitle keeps Engine's UNIQUE(title,parentListId): a list of Engine's own, or a second
// rekordbox list of the same name in one folder, makes this one "<name> (rekordbox)".
func (b *Batch) uniqueTitle(ctx context.Context, name string, parent, self int64) (string, error) {
	title := name
	for n := 1; ; n++ {
		var exists int
		if err := b.tx.QueryRowContext(ctx, `SELECT count(*) FROM Playlist WHERE title=? AND parentListId=? AND id<>?`, title, parent, self).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			return title, nil
		}
		if n == 1 {
			title = name + " (rekordbox)"
		} else {
			title = fmt.Sprintf("%s (rekordbox %d)", name, n)
		}
	}
}

// InsertList adds a list; Engine's triggers link it into its folder in front of Before. Engine's
// trigger that marks a new list's folders persisted is suspended for the batch: it walks every
// list at each insert (506 lists took 12 s at deck speed), and the bridge's folders are persisted.
func (b *Batch) InsertList(ctx context.Context, row ListRow) (int64, error) {
	if !b.lists {
		if err := b.suspend(ctx, `type='trigger' AND name='trigger_after_insert_isPersist'`); err != nil {
			return 0, err
		}
		b.lists = true
	}
	title, err := b.uniqueTitle(ctx, row.Title, row.Parent, 0)
	if err != nil {
		return 0, err
	}
	res, err := b.exec(ctx, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES(?,?,1,?,1)`, title, row.Parent, row.Before)
	if err != nil {
		return 0, fmt.Errorf("insert list %q: %w", row.Title, err)
	}
	return res.LastInsertId()
}

// FindList is the list of a title in a folder that the DJ or Engine asked for by name, 0 when
// there is none.
func (b *Batch) FindList(ctx context.Context, title string, parent int64) (int64, error) {
	var id int64
	err := b.tx.QueryRowContext(ctx, `SELECT id FROM Playlist WHERE title=? AND parentListId=? AND isExplicitlyExported=1`, title, parent).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// OrderLists chains the given lists of one folder in this order. Lists of the folder that are
// not named (Engine's own) keep their order and follow them.
func (b *Batch) OrderLists(ctx context.Context, parent int64, ids []int64) error {
	named := make(map[int64]bool, len(ids))
	for _, id := range ids {
		named[id] = true
	}
	nodes, err := b.children(ctx, parent, func(id int64) bool { return named[id] })
	if err != nil {
		return err
	}
	have := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		have[n.id] = true
	}
	var order []int64
	for _, id := range ids {
		if have[id] {
			order = append(order, id)
		}
	}
	for _, n := range orderNativeChildren(nodes) {
		order = append(order, n.id)
	}
	// UNIQUE(parentListId,nextListId): park every link on a value of its own first.
	for _, id := range order {
		if _, err = b.exec(ctx, `UPDATE Playlist SET nextListId=? WHERE id=?`, -id-1, id); err != nil {
			return err
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		next := int64(0)
		if i+1 < len(order) {
			next = order[i+1]
		}
		if _, err = b.exec(ctx, `UPDATE Playlist SET nextListId=? WHERE id=?`, next, order[i]); err != nil {
			return err
		}
	}
	return nil
}

// children are a folder's lists by ID; owned marks the bridge's.
func (b *Batch) children(ctx context.Context, parent int64, owned func(int64) bool) ([]nativeChildNode, error) {
	rows, err := b.tx.QueryContext(ctx, `SELECT id,COALESCE(title,''),COALESCE(nextListId,0) FROM Playlist WHERE parentListId=? ORDER BY id`, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []nativeChildNode
	for rows.Next() {
		var n nativeChildNode
		if err = rows.Scan(&n.id, &n.title, &n.next); err != nil {
			return nil, err
		}
		n.owned = owned(n.id)
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// SetEntries makes a list hold exactly these tracks, in this order.
func (b *Batch) SetEntries(ctx context.Context, list int64, tracks []int64) error {
	if _, err := b.exec(ctx, `DELETE FROM PlaylistEntity WHERE listId=?`, list); err != nil {
		return err
	}
	if len(tracks) == 0 {
		return nil
	}
	if b.nextEntity == 0 {
		if err := b.tx.QueryRowContext(ctx, `SELECT max(COALESCE((SELECT seq FROM sqlite_sequence WHERE name='PlaylistEntity'),0),COALESCE((SELECT max(id) FROM PlaylistEntity),0))`).Scan(&b.nextEntity); err != nil {
			return err
		}
	}
	// Each entity with its link to the next: one insert a row, no update, 100 rows a statement.
	args := make([]any, 0, min(len(tracks), libBatchRows)*5)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		q := `INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES` + strings.Repeat(",(?,?,?,?,?,0)", len(args)/5)[1:]
		_, err := b.exec(ctx, q, args...)
		args = args[:0]
		return err
	}
	for i, track := range tracks {
		b.nextEntity++
		next := int64(0)
		if i < len(tracks)-1 {
			next = b.nextEntity + 1
		}
		args = append(args, b.nextEntity, list, track, b.l.uuid, next)
		if len(args) == libBatchRows*5 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// DeleteList removes a list the bridge owns. Engine's own lists inside it (not owned) move to the
// root first, in their order, as "<name> (n)" when the root has the name: the bridge never
// deletes a list the DJ made on a deck (#252).
func (b *Batch) DeleteList(ctx context.Context, id int64, owned func(int64) bool) error {
	nodes, err := b.children(ctx, id, owned)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.owned {
			if err = b.DeleteList(ctx, n.id, owned); err != nil {
				return err
			}
		}
	}
	for _, n := range orderNativeChildren(nodes) {
		title := n.title
		for k := 1; ; k++ {
			var taken int
			if err = b.tx.QueryRowContext(ctx, `SELECT count(*) FROM Playlist WHERE title=? AND parentListId=0 AND id<>?`, title, n.id).Scan(&taken); err != nil {
				return err
			}
			if taken == 0 {
				break
			}
			title = fmt.Sprintf("%s (%d)", n.title, k)
		}
		// To the end of the root's chain; the chain it leaves goes with its folder.
		if _, err = b.exec(ctx, `UPDATE Playlist SET parentListId=0,title=?,nextListId=? WHERE id=?`, title, -n.id-1, n.id); err != nil {
			return err
		}
		if _, err = b.exec(ctx, `UPDATE Playlist SET nextListId=? WHERE parentListId=0 AND nextListId=0 AND id<>?`, n.id, n.id); err != nil {
			return err
		}
		if _, err = b.exec(ctx, `UPDATE Playlist SET nextListId=0 WHERE id=?`, n.id); err != nil {
			return err
		}
	}
	if _, err = b.exec(ctx, `DELETE FROM PlaylistEntity WHERE listId=?`, id); err != nil {
		return err
	}
	_, err = b.exec(ctx, `DELETE FROM Playlist WHERE id=?`, id)
	return err
}

// PerfPrev is what the snapshot knows of a row before staged data is applied.
type PerfPrev struct {
	State   rbstate.PerfState
	Written []byte // the digest of what the bridge last wrote, nil for a row only the export filled
}

// asWritten is a row's quickCues as the bridge wrote them when only a load changed them: Engine
// puts its own figure into the last field, the main cue as imported, every time a deck loads the
// track (the bridge writes the main cue there). A main cue the DJ adjusted has its flag set and
// is an edit, as every other difference is.
func asWritten(cues []byte) []byte {
	raw, err := QUncompress(cues, 64<<10)
	if err != nil {
		return cues
	}
	at, ok := cuePositions(raw)
	if !ok || raw[at[8]+8] != 0 || bytes.Equal(raw[at[8]:at[8]+8], raw[at[9]:]) {
		return cues
	}
	copy(raw[at[9]:], raw[at[8]:at[8]+8])
	if out, err := QCompress(raw); err == nil {
		return out
	}
	return cues
}

// ApplyPerformance writes a track's staged data unless the deck changed the row since the bridge
// wrote it, and returns the digest of the grid, cues and loops the bridge wrote (nil: the row is
// the deck's from now on) and what it did. Four cases:
//   - the row holds the staged data already (a window cut after its commit): nothing is written;
//   - the row holds what the bridge wrote before: all of it is replaced;
//   - the export alone filled the row and the deck worked on it since: the DJ's hot cues, loops,
//     adjusted main cue, locked or adjusted grid and rating stay (#328, mergeFast);
//   - the deck changed a row the bridge wrote in full: its grid, cues and loops stay, moved from
//     an older offset rule to this one (#178, realignKept); waveforms and track data are staged.
func (b *Batch) ApplyPerformance(ctx context.Context, id int64, p Performance, f rbstate.Facts, prev PerfPrev, export rekordbox.Track) (digest []byte, outcome Outcome, err error) {
	var row deckRow
	var trackData, overview []byte
	st, err := b.stmt(ctx, `SELECT p.beatData,p.quickCues,p.loops,p.activeOnLoadLoops,p.thirdPartySourceId,p.trackData,p.overviewWaveFormData,COALESCE(t.isBeatGridLocked,0),t.rating
		FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE p.trackId=?`)
	if err != nil {
		return nil, 0, err
	}
	if err = st.QueryRowContext(ctx, id).Scan(&row.beats, &row.cues, &row.loops, &row.active, &row.mark, &trackData, &overview, &row.locked, &row.rating); err != nil {
		return nil, 0, fmt.Errorf("track %d: %w", id, err)
	}
	staged := p.Digest
	if staged == nil {
		staged = WrittenDigest(p.Beats, p.QuickCues, p.Loops)
	}
	// The row's own digest (a third of a window's CPU on a deck) only for a row that can hold
	// something else than the staged data or the export's cue rows.
	same := bytes.Equal(row.beats, p.Beats) && bytes.Equal(row.cues, p.QuickCues) && bytes.Equal(row.loops, p.Loops)
	var have []byte
	if !same && (prev.Written != nil || row.beats != nil) {
		have = WrittenDigest(row.beats, asWritten(row.cues), row.loops)
		same = bytes.Equal(have, staged)
	}
	bpm := f.BPM
	if bpm == 0 {
		bpm = export.BPM
	}
	if bpm >= 499.5 {
		bpm = 0
	}
	facts := func(keepGrid bool) error {
		// What the audio header says replaces the export's figures; a grid the DJ set keeps its BPM.
		_, e := b.exec(ctx, `UPDATE Track SET length=CASE WHEN ?1>0 THEN ?1 ELSE length END, bitrate=CASE WHEN ?2>0 THEN ?2 ELSE bitrate END,
			fileBytes=CASE WHEN ?3>0 THEN ?3 ELSE fileBytes END, bpm=CASE WHEN ?4 THEN bpm ELSE ?5 END, bpmAnalyzed=CASE WHEN ?4 THEN bpmAnalyzed ELSE ?6 END,
			isAnalyzed=?7 WHERE id=?8 AND NOT (length IS (CASE WHEN ?1>0 THEN ?1 ELSE length END) AND bitrate IS (CASE WHEN ?2>0 THEN ?2 ELSE bitrate END)
			AND fileBytes IS (CASE WHEN ?3>0 THEN ?3 ELSE fileBytes END) AND (?4 OR (bpm IS ?5 AND bpmAnalyzed IS ?6)) AND isAnalyzed IS ?7)`,
			int(f.Duration+0.5), f.Bitrate, f.FileBytes, keepGrid, int(bpm+0.5), bpm, p.Complete, id)
		return e
	}
	switch {
	case same && prev.State != rbstate.PerfDeck:
		if !bytes.Equal(trackData, p.TrackData) || !bytes.Equal(overview, p.Overview) || row.mark.Valid {
			if _, err = b.exec(ctx, `UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,thirdPartySourceId=NULL WHERE trackId=?`, p.TrackData, p.Overview, id); err != nil {
				return nil, 0, err
			}
		}
		return staged, PerfSame, facts(false)
	case prev.State == rbstate.PerfDeck || (prev.Written != nil && !bytes.Equal(have, prev.Written)):
		// The deck's since the bridge wrote it (realignKept, #178).
		beats, cues, loops, _ := realignKept(row.beats, row.cues, row.loops, p, row.locked)
		_, err = b.exec(ctx, `UPDATE PerformanceData SET trackData=?1,overviewWaveFormData=?2,beatData=COALESCE(?3,?6),quickCues=COALESCE(?4,?7),
			loops=COALESCE(?5,?8),thirdPartySourceId=NULL WHERE trackId=?9`, p.TrackData, p.Overview, beats, cues, loops, p.Beats, p.QuickCues, p.Loops, id)
		if err != nil {
			return nil, 0, err
		}
		return nil, PerfKept, facts(true)
	case prev.Written != nil:
		_, err = b.exec(ctx, `UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?,activeOnLoadLoops=?,thirdPartySourceId=NULL WHERE trackId=?`,
			p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops, p.ActiveLoops, id)
		if err != nil {
			return nil, 0, err
		}
		return staged, PerfWritten, facts(false)
	}
	// A row the export alone filled: rekordbox's analysis, with what the DJ did on it meanwhile.
	m := mergeFast(row, fastBaseOf(export), p)
	or := func(deck, staged []byte) []byte {
		if deck != nil {
			return deck
		}
		return staged
	}
	active := any(p.ActiveLoops)
	if m.active != nil {
		active = m.active
	}
	if _, err = b.exec(ctx, `UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?,activeOnLoadLoops=?,thirdPartySourceId=NULL WHERE trackId=?`,
		p.TrackData, p.Overview, or(m.beats, p.Beats), or(m.cues, p.QuickCues), or(m.loops, p.Loops), active, id); err != nil {
		return nil, 0, err
	}
	if err = facts(m.grid); err != nil {
		return nil, 0, err
	}
	if m.deck() {
		return nil, PerfMerged, nil
	}
	return staged, PerfWritten, nil
}

// PutArtwork gives a track its cover: the AlbumArt row, shared by the tracks of one image (a batch
// looks each up once), and the Engine Library/Artwork file Engine serves to other decks.
func (b *Batch) PutArtwork(ctx context.Context, id int64, image []byte) error {
	if len(image) == 0 {
		return errors.New("empty cover")
	}
	h := sha1.Sum(image)
	sum := h[:]
	art, known := b.covers[h]
	if !known {
		if artDir, err := compat.SafeOutputDir(b.l.mount, artworkDir); err == nil {
			_ = putArtwork(artDir, sum, image) // without the file the cover still shows on this deck
		}
		err := b.tx.QueryRowContext(ctx, `SELECT id FROM AlbumArt WHERE hash=? OR hash=? LIMIT 1`, sum, hex.EncodeToString(sum)).Scan(&art)
		if errors.Is(err, sql.ErrNoRows) {
			var res sql.Result
			if res, err = b.exec(ctx, `INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, sum, image); err == nil {
				art, err = res.LastInsertId()
			}
		}
		if err != nil {
			return err
		}
		// An earlier build's row has the hash as text, which Engine Link does not serve.
		if _, err = b.exec(ctx, `UPDATE AlbumArt SET hash=? WHERE id=? AND typeof(hash)='text'`, sum, art); err != nil {
			return err
		}
		if b.covers == nil {
			b.covers = map[[sha1.Size]byte]int64{}
		}
		b.covers[h] = art
	}
	_, err := b.exec(ctx, `UPDATE Track SET albumArtId=? WHERE id=? AND albumArtId IS NOT ?`, art, id, art)
	return err
}

// Synced is what cue write-back compares a row by: its lastEditTime (Engine's triggers move it
// with every write of the row) and SyncedDigest of its cues, loops and editable tags.
func (b *Batch) Synced(ctx context.Context, id int64) (edit int64, digest []byte, err error) {
	var cues, loops []byte
	var active int64
	var genre, comment, label string
	st, err := b.stmt(ctx, `SELECT COALESCE(t.lastEditTime,0),p.quickCues,p.loops,COALESCE(p.activeOnLoadLoops,0),COALESCE(t.genre,''),COALESCE(t.comment,''),COALESCE(t.label,'')
		FROM Track t LEFT JOIN PerformanceData p ON p.trackId=t.id WHERE t.id=?`)
	if err != nil {
		return 0, nil, err
	}
	if err = st.QueryRowContext(ctx, id).Scan(&edit, &cues, &loops, &active, &genre, &comment, &label); err != nil {
		return 0, nil, err
	}
	return edit, SyncedDigest(nil, cues, loops, active, genre, comment, label), nil
}

// BuildLibrary writes a new library in dir (beside the stick's; the caller renames it into place)
// from the rows and lists' lists: without Engine's secondary indexes until the end, the journal
// in memory, one sync. libraryID is the UUID of the skeleton it replaces, "" for a new one.
func BuildLibrary(ctx context.Context, dir, libraryID string, rows []TrackRow, lists func(b *Batch, ids []int64) error) (ids []int64, libraryUUID string, commitID int64, err error) {
	l, err := openLibrary(ctx, filepath.Join(dir, "m.db"), filepath.Dir(filepath.Dir(dir)), true, libraryID)
	if err != nil {
		return nil, "", 0, err
	}
	defer l.Close()
	b, err := l.Begin(ctx)
	if err != nil {
		return nil, "", 0, err
	}
	if ids, _, err = b.InsertTracks(ctx, rows); err != nil {
		b.Rollback()
		return nil, "", 0, err
	}
	if lists != nil {
		if err = lists(b, ids); err != nil {
			b.Rollback()
			return nil, "", 0, err
		}
	}
	commitID = b.CommitID()
	if err = b.Commit(ctx); err != nil {
		return nil, "", 0, err
	}
	if err = l.Close(); err != nil {
		return nil, "", 0, err
	}
	return ids, l.uuid, commitID, syncFilePath(filepath.Join(dir, "m.db"))
}

//go:embed schema.sql
var schema string

// safeFile refuses redirection into PIONEER through symlinks or hard links.
func safeFile(p string) error {
	st, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("unsafe database file %s", p)
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && s.Nlink != 1 {
		return fmt.Errorf("hard-linked database file %s", p)
	}
	return nil
}

// playedIndicator is a positive random 63-bit Information.currentPlayedIndiciator.
func playedIndicator() int64 {
	id := uuid.New()
	return int64(binary.BigEndian.Uint64(id[:8])>>1) | 1
}

func syncFilePath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
