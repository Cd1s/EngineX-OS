package enginedb

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const bulkInsertBatch = 100

// bulkLoad is Options.Bulk: a private build loads its rows without maintaining Engine's
// secondary indexes and derived Track data row by row, then rebuilds them set-based (#301).
// A nil bulkLoad is an ordinary import.
type bulkLoad struct {
	saved      []string // Engine's own SQL of what was dropped, in schema order
	nextTrack  int64
	nextEntity int64
	nowSec     string
	seenPaths  map[string]struct{}
	trackCount int
	trackArgs  []any
	perfArgs   []any
	stateArgs  []any
	fastArgs   []any
}

// startBulk drops the secondary indexes on Track and PlaylistEntity and the two Track
// triggers that only fill derived data: its PerformanceData row and its origin columns.
// Unique constraints stay, so the import's lookups and its duplicate checks still hold.
func startBulk(ctx context.Context, tx *sql.Tx) (*bulkLoad, error) {
	rows, err := tx.QueryContext(ctx, `SELECT type,name,sql FROM sqlite_master WHERE sql IS NOT NULL AND (
		(type='index' AND tbl_name IN ('Track','PlaylistEntity')) OR
		(type='trigger' AND name IN ('trigger_after_insert_Track_fix_origin','trigger_after_insert_Track_insert_performance_data')))
		ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	b := &bulkLoad{}
	var drops []string
	for rows.Next() {
		var kind, name, text string
		if err = rows.Scan(&kind, &name, &text); err != nil {
			rows.Close()
			return nil, err
		}
		b.saved = append(b.saved, text)
		drops = append(drops, fmt.Sprintf(`DROP %s "%s"`, kind, name))
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	bRows, err := tx.QueryContext(ctx, `SELECT name,sql FROM bridge.sqlite_master WHERE type='index' AND tbl_name='BridgeState' AND sql LIKE 'CREATE INDEX %' ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	for bRows.Next() {
		var name, text string
		if err = bRows.Scan(&name, &text); err != nil {
			bRows.Close()
			return nil, err
		}
		b.saved = append(b.saved, strings.Replace(text, "CREATE INDEX ", "CREATE INDEX bridge.", 1))
		drops = append(drops, fmt.Sprintf(`DROP INDEX bridge."%s"`, name))
	}
	if err = bRows.Err(); err != nil {
		bRows.Close()
		return nil, err
	}
	bRows.Close()
	for _, d := range drops {
		if _, err = tx.ExecContext(ctx, d); err != nil {
			return nil, err
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT max(COALESCE((SELECT seq FROM sqlite_sequence WHERE name='Track'),0),COALESCE((SELECT max(id) FROM Track),0))`).Scan(&b.nextTrack); err != nil {
		return nil, err
	}
	b.nowSec = strconv.FormatInt(time.Now().Unix(), 10)
	b.seenPaths = make(map[string]struct{})
	err = tx.QueryRowContext(ctx, `SELECT max(COALESCE((SELECT seq FROM sqlite_sequence WHERE name='PlaylistEntity'),0),COALESCE((SELECT max(id) FROM PlaylistEntity),0))`).Scan(&b.nextEntity)
	return b, err
}

// finish fills the origin columns the way trigger_after_insert_Track_fix_origin does, in one
// statement, and recreates everything startBulk dropped from its own SQL. Each added track got
// its PerformanceData row as it was inserted.
func (b *bulkLoad) finish(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE Track SET originTrackId=id,originDatabaseUuid=(SELECT uuid FROM Information)
		WHERE IFNULL(originTrackId,0)=0 OR IFNULL(originDatabaseUuid,'')=''`); err != nil {
		return err
	}
	for _, text := range b.saved {
		if _, err := tx.ExecContext(ctx, text); err != nil {
			return fmt.Errorf("rebuild %q: %w", text, err)
		}
	}
	return nil
}

func execMultiRow(ctx context.Context, q sqlExecer, prefix, tuple string, cols int, args []any) error {
	if len(args) == 0 {
		return nil
	}
	var fullSQL string
	step := bulkInsertBatch * cols
	for len(args) >= step {
		if fullSQL == "" {
			fullSQL = prefix + strings.Repeat(","+tuple, bulkInsertBatch)[1:]
		}
		if _, err := q.ExecContext(ctx, fullSQL, args[:step]...); err != nil {
			return err
		}
		args = args[step:]
	}
	if rem := len(args) / cols; rem > 0 {
		remSQL := prefix + strings.Repeat(","+tuple, rem)[1:]
		if _, err := q.ExecContext(ctx, remSQL, args...); err != nil {
			return err
		}
	}
	return nil
}

const (
	trackInsertPrefix    = `INSERT INTO Track(id,length,bpm,year,path,filename,bitrate,bpmAnalyzed,albumArtId,fileBytes,title,artist,album,genre,comment,label,composer,remixer,key,rating,fileType,isAnalyzed,isAvailable,isMetadataImported,pdbImportKey,dateCreated,dateAdded) VALUES `
	trackInsertTuple     = `(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	perfInsertPrefix     = `INSERT INTO PerformanceData(trackId,quickCues,loops,activeOnLoadLoops,thirdPartySourceId) VALUES `
	stateInsertPrefix    = `INSERT INTO BridgeState(path,trackId,fingerprint) VALUES `
	fastBaseInsertPrefix = `INSERT OR REPLACE INTO BridgeFastBase(trackId,record,rating,quickCues,loops,active) VALUES `
	entityInsertPrefix   = `INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES `
)

func (b *bulkLoad) addTrack(id int64, args []any) {
	b.trackArgs = append(b.trackArgs, id)
	b.trackArgs = append(b.trackArgs, args...)
	b.trackArgs = append(b.trackArgs, b.nowSec, b.nowSec)
	b.trackCount++
}

func (b *bulkLoad) addPerf(id int64, cues, loops []byte, active, source any) {
	b.perfArgs = append(b.perfArgs, id, cues, loops, active, source)
}

func (b *bulkLoad) addState(path string, id int64, hash string) {
	b.stateArgs = append(b.stateArgs, path, id, hash)
}

func (b *bulkLoad) addFastBase(id int64, fingerprint string, fb fastBase) {
	b.fastArgs = append(b.fastArgs, id, recordCheck(fingerprint), fb.rating, fb.cues, fb.loops, fb.active)
}

func (b *bulkLoad) maybeFlushTracks(ctx context.Context, q sqlExecer) error {
	if b.trackCount < bulkInsertBatch && len(b.perfArgs) < bulkInsertBatch*5 && len(b.stateArgs) < bulkInsertBatch*3 {
		return nil
	}
	return b.flushTracks(ctx, q)
}

func (b *bulkLoad) flushTracks(ctx context.Context, q sqlExecer) error {
	if err := execMultiRow(ctx, q, trackInsertPrefix, trackInsertTuple, 27, b.trackArgs); err != nil {
		return err
	}
	b.trackArgs = b.trackArgs[:0]
	b.trackCount = 0
	if err := execMultiRow(ctx, q, perfInsertPrefix, "(?,?,?,?,?)", 5, b.perfArgs); err != nil {
		return err
	}
	b.perfArgs = b.perfArgs[:0]
	if err := execMultiRow(ctx, q, stateInsertPrefix, "(?,?,?)", 3, b.stateArgs); err != nil {
		return err
	}
	b.stateArgs = b.stateArgs[:0]
	if err := execMultiRow(ctx, q, fastBaseInsertPrefix, "(?,?,?,?,?,?)", 6, b.fastArgs); err != nil {
		return err
	}
	b.fastArgs = b.fastArgs[:0]
	return nil
}

// writeEntities is the package's writeEntities; a bulk load numbers the entities itself, so
// each is one insert with its link to the next instead of an insert and an update.
func (b *bulkLoad) writeEntities(ctx context.Context, q sqlExecer, list int64, tracks []uint32, mapping map[uint32]int64, databaseUUID string) error {
	if b == nil {
		return writeEntities(ctx, q, list, tracks, mapping, databaseUUID)
	}
	members, err := entityTracks(tracks, mapping)
	if err != nil {
		return err
	}
	args := make([]any, 0, min(len(members), bulkInsertBatch)*5)
	for i, track := range members {
		id, next := b.nextEntity+1, int64(0)
		if i < len(members)-1 {
			next = id + 1
		}
		b.nextEntity = id
		args = append(args, id, list, track, databaseUUID, next)
		if len(args) >= bulkInsertBatch*5 {
			if err = execMultiRow(ctx, q, entityInsertPrefix, "(?,?,?,?,?,0)", 5, args); err != nil {
				return err
			}
			args = args[:0]
		}
	}
	return execMultiRow(ctx, q, entityInsertPrefix, "(?,?,?,?,?,0)", 5, args)
}
