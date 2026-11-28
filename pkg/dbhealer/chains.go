package dbhealer

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

func fixSchema(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
 CREATE INDEX IF NOT EXISTS index_PlaylistEntity_listId_nextEntityId ON PlaylistEntity(listId,nextEntityId);
 CREATE INDEX IF NOT EXISTS index_PlaylistEntity_trackId ON PlaylistEntity(trackId);
 DROP TRIGGER IF EXISTS trigger_before_delete_PlaylistEntity;
 CREATE TRIGGER trigger_before_delete_PlaylistEntity BEFORE DELETE ON PlaylistEntity BEGIN
 UPDATE PlaylistEntity SET nextEntityId=CASE WHEN OLD.nextEntityId=OLD.id THEN 0 ELSE OLD.nextEntityId END
 WHERE nextEntityId=OLD.id AND listId IS OLD.listId AND id<>OLD.id; END;
 DROP TRIGGER IF EXISTS trigger_PerformanceData_after_update_Track_timestamp;
 CREATE TRIGGER trigger_PerformanceData_after_update_Track_timestamp
 AFTER UPDATE OF trackData,overviewWaveFormData,beatData,quickCues,loops,activeOnLoadLoops ON PerformanceData
 BEGIN UPDATE Track SET lastEditTime=strftime('%s') WHERE id=NEW.trackId; END;`)
	return err
}

type link struct {
	id, group, next      int64
	title                string
	parentNull, nextNull bool
}

// order preserves each surviving chain, visits heads by ID, then breaks remaining
// cycles at their smallest ID. Each row appears exactly once, including orphans.
func order(nodes []link) []link {
	byID := make(map[int64]link, len(nodes))
	incoming := make(map[int64]bool, len(nodes))
	for _, n := range nodes {
		byID[n.id] = n
	}
	for _, n := range nodes {
		if _, ok := byID[n.next]; ok {
			incoming[n.next] = true
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].id < nodes[j].id })
	out := make([]link, 0, len(nodes))
	seen := make(map[int64]bool, len(nodes))
	walk := func(id int64) {
		for !seen[id] {
			n, ok := byID[id]
			if !ok {
				break
			}
			seen[id] = true
			out = append(out, n)
			id = n.next
		}
	}
	for _, n := range nodes {
		if !incoming[n.id] {
			walk(n.id)
		}
	}
	for _, n := range nodes {
		walk(n.id)
	}
	return out
}

func HealPlaylistEntities(db *sql.DB) error {
	ctx := context.Background()
	return transactionContext(ctx, db, func(tx *sql.Tx) error {
		if err := fixSchema(ctx, tx); err != nil {
			return err
		}
		if _, _, err := healPlaylistEntityIntegrity(ctx, tx); err != nil {
			return err
		}
		if _, err := healChains(ctx, tx, false); err != nil {
			return err
		}
		return syncSqliteSequence(ctx, tx)
	})
}

const uuidTrimCutset = " \t\n\r\v\f"

// healPlaylistEntityIntegrity removes orphaned PlaylistEntity rows whose listId
// no longer exists in Playlist, backfills missing databaseUuid only for provably
// local tracks, and deduplicates colliding known-source (listId, databaseUuid, trackId)
// rows while preserving first-occurrence playlist order and direct membership.
// Healthy mounts exit via indexed pre-checks without materializing playlists.
func healPlaylistEntityIntegrity(ctx context.Context, tx *sql.Tx) (duplicates, orphans int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	var hasPlaylist int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_master WHERE type='table' AND name='Playlist'").Scan(&hasPlaylist); err != nil {
		return 0, 0, err
	}
	if hasPlaylist > 0 {
		var hasOrphans int
		if err := tx.QueryRowContext(ctx, `
			WITH RECURSIVE distinct_lists(lid) AS (
				SELECT MIN(listId) FROM PlaylistEntity WHERE listId IS NOT NULL
				UNION ALL
				SELECT (SELECT MIN(listId) FROM PlaylistEntity WHERE listId > lid)
				FROM distinct_lists
				WHERE lid IS NOT NULL
			)
			SELECT EXISTS(SELECT 1 FROM PlaylistEntity WHERE listId IS NULL)
				OR EXISTS(
					SELECT 1 FROM distinct_lists d
					WHERE d.lid IS NOT NULL
						AND NOT EXISTS (SELECT 1 FROM Playlist p WHERE p.id = d.lid)
				)`).Scan(&hasOrphans); err != nil {
			return 0, 0, err
		}
		if hasOrphans != 0 {
			res, err := tx.ExecContext(ctx, "DELETE FROM PlaylistEntity WHERE listId IS NULL OR listId NOT IN (SELECT id FROM Playlist)")
			if err != nil {
				return 0, 0, err
			}
			if n, err := res.RowsAffected(); err == nil {
				orphans = int(n)
			}
		}
	}

	var infoUUID string
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(uuid,'') FROM Information LIMIT 1").Scan(&infoUUID); err != nil && err != sql.ErrNoRows {
		return 0, orphans, err
	}
	infoUUID = strings.Trim(infoUUID, uuidTrimCutset)

	// Cheap indexed pre-check over distinct listIds using C_NAME_UNIQUE_FOR_LIST
	// (listId, databaseUuid, trackId). Healthy playlists whose entries already carry
	// canonical infoUUID require only O(log N) boundary seeks and zero Track probes.
	dirtyRows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE distinct_lists(lid) AS (
			SELECT MIN(listId) FROM PlaylistEntity WHERE listId IS NOT NULL
			UNION ALL
			SELECT (SELECT MIN(listId) FROM PlaylistEntity WHERE listId > lid)
			FROM distinct_lists
			WHERE lid IS NOT NULL
		)
		SELECT d.lid
		FROM distinct_lists d
		WHERE d.lid IS NOT NULL
			AND (
				(? <> '' AND EXISTS(
					SELECT 1 FROM PlaylistEntity pe
					WHERE pe.listId = d.lid AND pe.databaseUuid IS NULL AND pe.trackId > 0
						AND EXISTS(SELECT 1 FROM Track t WHERE t.id = pe.trackId)
				))
				OR EXISTS(
					SELECT 1 FROM PlaylistEntity pe
					WHERE pe.listId = d.lid AND pe.databaseUuid < ? AND pe.trackId > 0
						AND (
							(? <> '' AND trim(pe.databaseUuid, ' ' || char(9,10,11,12,13)) = '' AND EXISTS(SELECT 1 FROM Track t WHERE t.id = pe.trackId))
							OR (trim(pe.databaseUuid, ' ' || char(9,10,11,12,13)) <> '' AND pe.databaseUuid <> trim(pe.databaseUuid, ' ' || char(9,10,11,12,13)))
						)
				)
				OR EXISTS(
					SELECT 1 FROM PlaylistEntity pe
					WHERE pe.listId = d.lid AND pe.databaseUuid > ? AND pe.trackId > 0
						AND (
							(? <> '' AND trim(pe.databaseUuid, ' ' || char(9,10,11,12,13)) = '' AND EXISTS(SELECT 1 FROM Track t WHERE t.id = pe.trackId))
							OR (trim(pe.databaseUuid, ' ' || char(9,10,11,12,13)) <> '' AND pe.databaseUuid <> trim(pe.databaseUuid, ' ' || char(9,10,11,12,13)))
						)
				)
			)
		ORDER BY d.lid`, infoUUID, infoUUID, infoUUID, infoUUID, infoUUID)
	if err != nil {
		return 0, orphans, err
	}
	var dirtyListIDs []int64
	for dirtyRows.Next() {
		var lid int64
		if err := dirtyRows.Scan(&lid); err != nil {
			dirtyRows.Close()
			return 0, orphans, err
		}
		dirtyListIDs = append(dirtyListIDs, lid)
	}
	if err := dirtyRows.Err(); err != nil {
		dirtyRows.Close()
		return 0, orphans, err
	}
	dirtyRows.Close()
	if len(dirtyListIDs) == 0 {
		return 0, orphans, nil
	}

	type entityRow struct {
		id         int64
		trackID    int64
		dbUUID     string
		nextID     int64
		mr         sql.NullInt64
		localTrack bool
	}
	type dedupKey struct {
		listID  int64
		uuid    string
		trackID int64
	}
	type keptUpdate struct {
		id         int64
		dbUUID     string
		updateUUID bool
		mr         sql.NullInt64
		updateMR   bool
	}

	var toDelete []int64
	var toUpdate []keptUpdate
	for _, lid := range dirtyListIDs {
		if err := ctx.Err(); err != nil {
			return 0, orphans, err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT pe.id, COALESCE(pe.trackId,0), COALESCE(pe.databaseUuid,''),
				COALESCE(pe.nextEntityId,0), pe.membershipReference,
				CASE WHEN pe.trackId > 0 AND (pe.databaseUuid IS NULL OR trim(pe.databaseUuid, ' ' || char(9,10,11,12,13)) = '')
					THEN EXISTS(SELECT 1 FROM Track WHERE id=pe.trackId)
					ELSE 0 END
			FROM PlaylistEntity pe
			WHERE pe.listId = ?
			ORDER BY pe.id`, lid)
		if err != nil {
			return 0, orphans, err
		}
		var group []entityRow
		for rows.Next() {
			var e entityRow
			if err := rows.Scan(&e.id, &e.trackID, &e.dbUUID, &e.nextID, &e.mr, &e.localTrack); err != nil {
				rows.Close()
				return 0, orphans, err
			}
			group = append(group, e)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, orphans, err
		}
		rows.Close()

		byID := make(map[int64]entityRow, len(group))
		links := make([]link, 0, len(group))
		for _, e := range group {
			byID[e.id] = e
			links = append(links, link{id: e.id, group: lid, next: e.nextID})
		}
		ordered := order(links)
		kept := map[dedupKey]int{}
		updates := make([]keptUpdate, 0, len(ordered))
		for _, l := range ordered {
			e := byID[l.id]
			if e.trackID <= 0 {
				continue
			}
			effUUID := strings.Trim(e.dbUUID, uuidTrimCutset)
			needsUUID := false
			if effUUID == "" {
				if infoUUID != "" && e.localTrack {
					effUUID = infoUUID
					needsUUID = true
				} else {
					// Unknown source (non-local track or absent Information.uuid):
					// preserve row, original NULL/blank databaseUuid, and chain position.
					continue
				}
			} else {
				needsUUID = effUUID != e.dbUUID
			}
			k := dedupKey{listID: lid, uuid: effUUID, trackID: e.trackID}
			if idx, exists := kept[k]; exists {
				// Always keep the first occurrence in playlist order and drop the later
				// duplicate before normalizing databaseUuid. Merge nullable membershipReference:
				// explicit direct (0) beats non-zero and NULL; explicit non-zero beats NULL;
				// (NULL, NULL) stays NULL.
				prev := &updates[idx]
				toDelete = append(toDelete, e.id)
				if e.mr.Valid {
					if !prev.mr.Valid || (e.mr.Int64 == 0 && prev.mr.Int64 != 0) {
						prev.mr = e.mr
						prev.updateMR = true
					}
				}
				continue
			}
			kept[k] = len(updates)
			updates = append(updates, keptUpdate{
				id:         e.id,
				dbUUID:     effUUID,
				updateUUID: needsUUID,
				mr:         e.mr,
			})
		}
		for _, u := range updates {
			if u.updateUUID || u.updateMR {
				toUpdate = append(toUpdate, u)
			}
		}
	}

	for _, id := range toDelete {
		if err := ctx.Err(); err != nil {
			return duplicates, orphans, err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM PlaylistEntity WHERE id=?", id)
		if err != nil {
			return duplicates, orphans, err
		}
		if n, err := res.RowsAffected(); err == nil {
			duplicates += int(n)
		}
	}
	for _, u := range toUpdate {
		if err := ctx.Err(); err != nil {
			return duplicates, orphans, err
		}
		if u.updateUUID && u.updateMR {
			if _, err := tx.ExecContext(ctx, "UPDATE PlaylistEntity SET databaseUuid=?, membershipReference=? WHERE id=?", u.dbUUID, u.mr, u.id); err != nil {
				return duplicates, orphans, err
			}
		} else if u.updateUUID {
			if _, err := tx.ExecContext(ctx, "UPDATE PlaylistEntity SET databaseUuid=? WHERE id=?", u.dbUUID, u.id); err != nil {
				return duplicates, orphans, err
			}
		} else if u.updateMR {
			if _, err := tx.ExecContext(ctx, "UPDATE PlaylistEntity SET membershipReference=? WHERE id=?", u.mr, u.id); err != nil {
				return duplicates, orphans, err
			}
		}
	}
	return duplicates, orphans, nil
}

// syncSqliteSequence advances NULL, non-integer, lagging, or lower conflicting
// sqlite_sequence entries to at least max(highest_valid_seq, MAX(id)) for Engine's
// five target AUTOINCREMENT library tables without ever creating sqlite_sequence,
// touching unrelated tables, or lowering an existing higher counter.
// Empty tables are skipped only when they have no existing sqlite_sequence row to
// reconcile, so removing the last orphan never lets conflicting lower counters reuse
// a previously retired ID while preserving the healthy empty-table fast path.
func syncSqliteSequence(ctx context.Context, tx *sql.Tx) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tables := []string{"PlaylistEntity", "Playlist", "Track", "PreparelistEntity", "AlbumArt"}
	mrows, err := tx.QueryContext(ctx, `
		SELECT name, upper(COALESCE(sql,''))
		FROM main.sqlite_master
		WHERE type='table'
			AND name IN ('sqlite_sequence', 'PlaylistEntity', 'Playlist', 'Track', 'PreparelistEntity', 'AlbumArt')`)
	if err != nil {
		return err
	}
	hasSeqTable := false
	autoTables := map[string]bool{}
	for mrows.Next() {
		var name, ddl string
		if err := mrows.Scan(&name, &ddl); err != nil {
			mrows.Close()
			return err
		}
		if name == "sqlite_sequence" {
			hasSeqTable = true
		} else if strings.Contains(ddl, "AUTOINCREMENT") {
			autoTables[name] = true
		}
	}
	if err := mrows.Err(); err != nil {
		mrows.Close()
		return err
	}
	mrows.Close()
	if !hasSeqTable || len(autoTables) == 0 {
		return nil
	}

	activeTables := make([]string, 0, len(tables))
	placeholders := make([]string, 0, len(tables))
	args := make([]any, 0, len(tables))
	for _, table := range tables {
		if autoTables[table] {
			activeTables = append(activeTables, table)
			placeholders = append(placeholders, "?")
			args = append(args, table)
		}
	}

	type seqState struct {
		present    bool
		hasInvalid bool
		hasValid   bool
		minSeq     int64
		maxSeq     int64
	}
	states := map[string]seqState{}
	// Filter strictly to the active target tables in SQL before scanning seq so
	// unrelated tables (e.g. Pack) with non-integer seq values are never touched.
	srows, err := tx.QueryContext(ctx,
		"SELECT name, CASE WHEN typeof(seq)='integer' THEN seq ELSE NULL END FROM main.sqlite_sequence WHERE name IN ("+strings.Join(placeholders, ",")+")",
		args...,
	)
	if err != nil {
		return err
	}
	for srows.Next() {
		var name string
		var seq sql.NullInt64
		if err := srows.Scan(&name, &seq); err != nil {
			srows.Close()
			return err
		}
		st := states[name]
		st.present = true
		if !seq.Valid {
			st.hasInvalid = true
		} else if !st.hasValid {
			st.hasValid = true
			st.minSeq = seq.Int64
			st.maxSeq = seq.Int64
		} else {
			if seq.Int64 < st.minSeq {
				st.minSeq = seq.Int64
			}
			if seq.Int64 > st.maxSeq {
				st.maxSeq = seq.Int64
			}
		}
		states[name] = st
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		return err
	}
	srows.Close()

	for _, table := range activeTables {
		if err := ctx.Err(); err != nil {
			return err
		}
		var maxID int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM "+table).Scan(&maxID); err != nil {
			return err
		}
		st := states[table]
		if maxID <= 0 && !st.present {
			continue
		}
		if !st.present {
			if _, err := tx.ExecContext(ctx, "INSERT INTO main.sqlite_sequence(name, seq) VALUES(?, ?)", table, maxID); err != nil {
				return err
			}
			continue
		}
		target := maxID
		if target < 0 {
			target = 0
		}
		if st.hasValid && st.maxSeq > target {
			target = st.maxSeq
		}
		if st.hasInvalid || !st.hasValid || st.minSeq < target {
			if _, err := tx.ExecContext(ctx,
				"UPDATE main.sqlite_sequence SET seq=? WHERE name=? AND (seq IS NULL OR typeof(seq)<>'integer' OR seq<?)",
				target, table, target,
			); err != nil {
				return err
			}
		}
	}
	return nil
}
func HealPlaylistTree(db *sql.DB) error {
	ctx := context.Background()
	return transactionContext(ctx, db, func(tx *sql.Tx) error {
		_, err := healChains(ctx, tx, true)
		if err != nil {
			return err
		}
		return materialize(ctx, tx)
	})
}

func healChains(ctx context.Context, tx *sql.Tx, tree bool) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	table, groupCol, nextCol := "PlaylistEntity", "listId", "nextEntityId"
	if tree {
		table, groupCol, nextCol = "Playlist", "parentListId", "nextListId"
	}
	query := fmt.Sprintf("SELECT id, COALESCE(%s,0), COALESCE(%s,0)", groupCol, nextCol)
	query += ", " + nextCol + " IS NULL"
	if tree {
		query += ", COALESCE(title,''), parentListId IS NULL"
	}
	query += " FROM " + table + " ORDER BY id"
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	var all []link
	for rows.Next() {
		var n link
		var e error
		if tree {
			e = rows.Scan(&n.id, &n.group, &n.next, &n.nextNull, &n.title, &n.parentNull)
		} else {
			e = rows.Scan(&n.id, &n.group, &n.next, &n.nextNull)
		}
		if e != nil {
			rows.Close()
			return 0, e
		}
		all = append(all, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	// Break parent cycles and dangling parents too, before building paths. A
	// temporary NULL next value avoids UNIQUE(parentListId,nextListId) collisions.
	changedParents := make(map[int64]bool)
	if tree {
		parents := map[int64]int64{}
		for _, n := range all {
			parents[n.id] = n.group
		}
		done := map[int64]bool{}
		for _, n := range all {
			path := map[int64]bool{}
			id := n.id
			for id != 0 && !done[id] {
				if path[id] {
					parents[id] = 0
					changedParents[id] = true
					break
				}
				path[id] = true
				p, ok := parents[id]
				if !ok {
					break
				}
				if p != 0 {
					if _, ok = parents[p]; !ok {
						parents[id] = 0
						changedParents[id] = true
						break
					}
				}
				id = p
			}
			for id := range path {
				done[id] = true
			}
		}
		for i := range all {
			all[i].group = parents[all[i].id]
		}
	}
	groups := map[int64][]link{}
	for _, n := range all {
		groups[n.group] = append(groups[n.group], n)
	}
	var changes []link
	keys := make([]int64, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		seq := order(groups[k])
		for i, n := range seq {
			next := int64(0)
			if i+1 < len(seq) {
				next = seq[i+1].id
			}
			if next != n.next || n.nextNull || changedParents[n.id] {
				n.next = next
				changes = append(changes, n)
			}
		}
	}
	if len(changes) == 0 {
		return 0, nil
	}
	if tree {
		// Clear every sibling pointer before assigning new parents or links. NULL is
		// temporary and stays invisible outside this transaction.
		if _, err = tx.ExecContext(ctx, "UPDATE Playlist SET nextListId=NULL"); err != nil {
			return 0, err
		}
		// The stock parent-persistence trigger traverses the entire recursive
		// ancestry view. Suspend it while parent cycles are being broken.
		var parentTrigger string
		if len(changedParents) > 0 {
			err = tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trigger_after_update_isPersistParent'").Scan(&parentTrigger)
			if err != nil && err != sql.ErrNoRows {
				return 0, err
			}
			if _, err = tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS trigger_after_update_isPersistParent"); err != nil {
				return 0, err
			}
		}
		for _, n := range all {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if changedParents[n.id] {
				title := n.title
				// Preserve every node when an orphan's title collides at the root.
				for suffix := 0; ; suffix++ {
					var count int
					if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM Playlist WHERE parentListId=0 AND title=? AND id<>?", title, n.id).Scan(&count); err != nil {
						return 0, err
					}
					if count == 0 {
						break
					}
					title = fmt.Sprintf("%s [recovered %d-%d]", n.title, n.id, suffix)
				}
				if _, err = tx.ExecContext(ctx, "UPDATE Playlist SET parentListId=0,title=? WHERE id=?", title, n.id); err != nil {
					return 0, err
				}
			}
		}
		if parentTrigger != "" {
			if _, err = tx.ExecContext(ctx, parentTrigger); err != nil {
				return 0, err
			}
		}
		for _, k := range keys {
			seq := order(groups[k])
			for i, n := range seq {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				next := int64(0)
				if i+1 < len(seq) {
					next = seq[i+1].id
				}
				if _, err = tx.ExecContext(ctx, "UPDATE Playlist SET nextListId=? WHERE id=?", next, n.id); err != nil {
					return 0, err
				}
			}
		}
	} else {
		for _, n := range changes {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE PlaylistEntity SET nextEntityId=? WHERE id=?", n.next, n.id); err != nil {
				return 0, err
			}
		}
	}
	return len(changes), nil
}

func materialize(ctx context.Context, tx *sql.Tx) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Preserve the stock view as a correctness fallback while edits have dirtied
	// the cache. Existing Engine queries transparently use the fast snapshot.
	var originalCount int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='view' AND name='GuardianOriginalPlaylistPath'").Scan(&originalCount); err != nil {
		return err
	}
	if originalCount == 0 {
		var ddl string
		if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='view' AND name='PlaylistPath'").Scan(&ddl); err != nil {
			return err
		}
		upper := strings.ToUpper(ddl)
		nameIdx := strings.Index(upper, "PLAYLISTPATH")
		as := -1
		if nameIdx >= 0 {
			rest := upper[nameIdx+len("PLAYLISTPATH"):]
			if rel := strings.Index(rest, "AS"); rel >= 0 {
				as = nameIdx + len("PLAYLISTPATH") + rel
			}
		}
		if as < 0 {
			return fmt.Errorf("unsupported PlaylistPath definition")
		}
		if _, err := tx.ExecContext(ctx, "CREATE VIEW GuardianOriginalPlaylistPath AS "+ddl[as+2:]); err != nil {
			return err
		}
	}
	// Dirty tracking is O(1) for Engine's own edits; refresh at the next guardian
	// pass. Reads retain a consistent snapshot between refreshes.
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GuardianCacheState(id INTEGER PRIMARY KEY CHECK(id=1),dirty INTEGER NOT NULL);
 INSERT OR IGNORE INTO GuardianCacheState VALUES(1,1);
 CREATE TABLE IF NOT EXISTS MaterializedPlaylistPath(id INTEGER PRIMARY KEY,parentListId INTEGER,nextListId INTEGER,position INTEGER,path TEXT);
 CREATE INDEX IF NOT EXISTS index_MaterializedPlaylistPath_parent_position ON MaterializedPlaylistPath(parentListId,position);`); err != nil {
		return err
	}
	// Preserve the vendor depth-descending, chain-tail-first position order.
	// Build paths once instead of running an ancestry subquery for every row.
	var liveDDL string
	err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='view' AND name='GuardianLivePlaylistPath'").Scan(&liveDDL)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	// NULL-terminated chains have no vendor OrderedList row: their distance
	// stays NULL. Equal ranks follow the captured parent/next covering index.
	if !strings.Contains(liveDDL, "p.depth DESC,r.distance,r.parentListId,r.nextListId,p.id") {
		if _, err = tx.ExecContext(ctx, `DROP VIEW IF EXISTS GuardianLivePlaylistPath;
 CREATE VIEW GuardianLivePlaylistPath AS
 WITH RECURSIVE ReverseOrder(id,parentListId,nextListId,title,distance) AS (
   SELECT id,parentListId,nextListId,title,
     CASE WHEN nextListId IS NULL THEN NULL ELSE 0 END FROM Playlist WHERE nextListId=0 OR nextListId IS NULL
   UNION ALL
   SELECT p.id,p.parentListId,p.nextListId,p.title,r.distance+1
   FROM Playlist p JOIN ReverseOrder r ON p.nextListId=r.id AND COALESCE(p.parentListId,0)=COALESCE(r.parentListId,0)
 ), Paths(id,path,depth) AS (
   SELECT id,CASE WHEN title IS NULL THEN '' ELSE title||';' END,1 FROM ReverseOrder WHERE COALESCE(parentListId,0)=0
   UNION ALL
   SELECT c.id,p.path||CASE WHEN c.title IS NULL THEN '' ELSE c.title||';' END,p.depth+1
   FROM ReverseOrder c JOIN Paths p ON c.parentListId=p.id
 ) SELECT p.id,NULLIF(p.path,'') AS path,
   ROW_NUMBER() OVER(ORDER BY p.depth DESC,r.distance,r.parentListId,r.nextListId,p.id) AS position
 FROM Paths p JOIN ReverseOrder r ON r.id=p.id;
 UPDATE GuardianCacheState SET dirty=1 WHERE id=1;`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP VIEW IF EXISTS PlaylistPath;
 CREATE VIEW PlaylistPath AS
 SELECT id,path,position FROM MaterializedPlaylistPath WHERE (SELECT dirty FROM GuardianCacheState WHERE id=1)=0
 UNION ALL
 SELECT id,path,position FROM GuardianLivePlaylistPath WHERE (SELECT dirty FROM GuardianCacheState WHERE id=1)<>0;`); err != nil {
		return err
	}
	for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS guardian_playlist_%s AFTER %s ON Playlist BEGIN UPDATE GuardianCacheState SET dirty=1 WHERE id=1; END`, strings.ToLower(event), event)); err != nil {
			return err
		}
	}
	var dirty int
	if err := tx.QueryRowContext(ctx, "SELECT dirty FROM GuardianCacheState WHERE id=1").Scan(&dirty); err != nil {
		return err
	}
	if dirty == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM MaterializedPlaylistPath;
 INSERT INTO MaterializedPlaylistPath(id,parentListId,nextListId,position,path)
 SELECT p.id,COALESCE(p.parentListId,0),COALESCE(p.nextListId,0),v.position,v.path
 FROM Playlist p JOIN GuardianLivePlaylistPath v ON v.id=p.id;`); err != nil {
		return err
	}
	var total, cached int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM Playlist),(SELECT count(*) FROM MaterializedPlaylistPath)`).Scan(&total, &cached); err != nil {
		return err
	}
	if total != cached {
		return fmt.Errorf("playlist tree is not rooted: %d of %d cached", cached, total)
	}
	_, err = tx.ExecContext(ctx, "UPDATE GuardianCacheState SET dirty=0 WHERE id=1")
	return err
}
