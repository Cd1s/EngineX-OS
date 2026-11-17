package enginedb

import (
	"context"
	"database/sql"
	"fmt"
)

// bulkLoad is Options.Bulk: a private build loads its rows without maintaining Engine's
// secondary indexes and derived Track data row by row, then rebuilds them set-based (#301).
// A nil bulkLoad is an ordinary import.
type bulkLoad struct {
	saved      []string // Engine's own SQL of what was dropped, in schema order
	nextEntity int64
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
	for _, d := range drops {
		if _, err = tx.ExecContext(ctx, d); err != nil {
			return nil, err
		}
	}
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
	for i, track := range members {
		id, next := b.nextEntity+1, int64(0)
		if i < len(members)-1 {
			next = id + 1
		}
		if _, err = q.ExecContext(ctx, `INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,?,?,?,?,0)`, id, list, track, databaseUUID, next); err != nil {
			return err
		}
		b.nextEntity = id
	}
	return nil
}
