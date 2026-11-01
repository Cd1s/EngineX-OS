package dbhealer

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

func fixSchema(tx *sql.Tx) error {
	_, err := tx.Exec(`
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
	return transaction(db, func(tx *sql.Tx) error {
		if err := fixSchema(tx); err != nil {
			return err
		}
		_, err := healChains(tx, false)
		return err
	})
}
func HealPlaylistTree(db *sql.DB) error {
	return transaction(db, func(tx *sql.Tx) error {
		_, err := healChains(tx, true)
		if err != nil {
			return err
		}
		return materialize(tx)
	})
}

func healChains(tx *sql.Tx, tree bool) (int, error) {
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
	rows, err := tx.Query(query)
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
		if _, err = tx.Exec("UPDATE Playlist SET nextListId=NULL"); err != nil {
			return 0, err
		}
		// The stock parent-persistence trigger traverses the entire recursive
		// ancestry view. Suspend it while parent cycles are being broken.
		var parentTrigger string
		if len(changedParents) > 0 {
			err = tx.QueryRow("SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trigger_after_update_isPersistParent'").Scan(&parentTrigger)
			if err != nil && err != sql.ErrNoRows {
				return 0, err
			}
			if _, err = tx.Exec("DROP TRIGGER IF EXISTS trigger_after_update_isPersistParent"); err != nil {
				return 0, err
			}
		}
		for _, n := range all {
			if changedParents[n.id] {
				title := n.title
				// Preserve every node when an orphan's title collides at the root.
				for suffix := 0; ; suffix++ {
					var count int
					if err = tx.QueryRow("SELECT count(*) FROM Playlist WHERE parentListId=0 AND title=? AND id<>?", title, n.id).Scan(&count); err != nil {
						return 0, err
					}
					if count == 0 {
						break
					}
					title = fmt.Sprintf("%s [recovered %d-%d]", n.title, n.id, suffix)
				}
				if _, err = tx.Exec("UPDATE Playlist SET parentListId=0,title=? WHERE id=?", title, n.id); err != nil {
					return 0, err
				}
			}
		}
		if parentTrigger != "" {
			if _, err = tx.Exec(parentTrigger); err != nil {
				return 0, err
			}
		}
		for _, k := range keys {
			seq := order(groups[k])
			for i, n := range seq {
				next := int64(0)
				if i+1 < len(seq) {
					next = seq[i+1].id
				}
				if _, err = tx.Exec("UPDATE Playlist SET nextListId=? WHERE id=?", next, n.id); err != nil {
					return 0, err
				}
			}
		}
	} else {
		for _, n := range changes {
			if _, err = tx.Exec("UPDATE PlaylistEntity SET nextEntityId=? WHERE id=?", n.next, n.id); err != nil {
				return 0, err
			}
		}
	}
	return len(changes), nil
}

func materialize(tx *sql.Tx) error {
	// Preserve the stock view as a correctness fallback while edits have dirtied
	// the cache. Existing Engine queries transparently use the fast snapshot.
	var originalCount int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='view' AND name='GuardianOriginalPlaylistPath'").Scan(&originalCount); err != nil {
		return err
	}
	if originalCount == 0 {
		var ddl string
		if err := tx.QueryRow("SELECT sql FROM sqlite_master WHERE type='view' AND name='PlaylistPath'").Scan(&ddl); err != nil {
			return err
		}
		as := strings.Index(strings.ToUpper(ddl), " AS ")
		if as < 0 {
			return fmt.Errorf("unsupported PlaylistPath definition")
		}
		if _, err := tx.Exec("CREATE VIEW GuardianOriginalPlaylistPath AS " + ddl[as+4:]); err != nil {
			return err
		}
	}
	// Dirty tracking is O(1) for Engine's own edits; refresh at the next guardian
	// pass. Reads retain a consistent snapshot between refreshes.
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS GuardianCacheState(id INTEGER PRIMARY KEY CHECK(id=1),dirty INTEGER NOT NULL);
 INSERT OR IGNORE INTO GuardianCacheState VALUES(1,1);
 CREATE TABLE IF NOT EXISTS MaterializedPlaylistPath(id INTEGER PRIMARY KEY,parentListId INTEGER,nextListId INTEGER,position INTEGER,path TEXT);
 CREATE INDEX IF NOT EXISTS index_MaterializedPlaylistPath_parent_position ON MaterializedPlaylistPath(parentListId,position);`); err != nil {
		return err
	}
	// The vendor GROUP_CONCAT ordering is not stable across SQLite versions.
	// Use a linear traversal plus one sort while the materialized cache is dirty.
	if _, err := tx.Exec(`CREATE VIEW IF NOT EXISTS GuardianLivePlaylistPath AS
    WITH RECURSIVE ReverseOrder(id,parentListId,nextListId,title,distance) AS (
      SELECT id,COALESCE(parentListId,0),COALESCE(nextListId,0),COALESCE(title,''),0 FROM Playlist WHERE COALESCE(nextListId,0)=0
      UNION ALL
      SELECT p.id,COALESCE(p.parentListId,0),p.nextListId,COALESCE(p.title,''),r.distance+1
      FROM Playlist p JOIN ReverseOrder r ON p.nextListId=r.id AND COALESCE(p.parentListId,0)=r.parentListId
    ), Ranked AS (
      SELECT *,ROW_NUMBER() OVER(PARTITION BY parentListId ORDER BY distance DESC,id) AS sibling FROM ReverseOrder
    ), Paths(id,path,depth,sortKey) AS (
      SELECT id,title||';',1,printf('%016x',sibling) FROM Ranked WHERE parentListId=0
      UNION ALL
      SELECT c.id,p.path||c.title||';',p.depth+1,p.sortKey||'.'||printf('%016x',c.sibling)
      FROM Ranked c JOIN Paths p ON c.parentListId=p.id
    ) SELECT id,path,ROW_NUMBER() OVER(ORDER BY depth,sortKey) AS position FROM Paths;`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP VIEW IF EXISTS PlaylistPath;
 CREATE VIEW PlaylistPath AS
 SELECT id,path,position FROM MaterializedPlaylistPath WHERE (SELECT dirty FROM GuardianCacheState WHERE id=1)=0
 UNION ALL
 SELECT id,path,position FROM GuardianLivePlaylistPath WHERE (SELECT dirty FROM GuardianCacheState WHERE id=1)<>0;`); err != nil {
		return err
	}
	for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
		if _, err := tx.Exec(fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS guardian_playlist_%s AFTER %s ON Playlist BEGIN UPDATE GuardianCacheState SET dirty=1 WHERE id=1; END`, strings.ToLower(event), event)); err != nil {
			return err
		}
	}
	var dirty int
	if err := tx.QueryRow("SELECT dirty FROM GuardianCacheState WHERE id=1").Scan(&dirty); err != nil {
		return err
	}
	if dirty == 0 {
		return nil
	}
	rows, err := tx.Query("SELECT id,COALESCE(parentListId,0),COALESCE(nextListId,0),COALESCE(title,'') FROM Playlist")
	if err != nil {
		return err
	}
	byID := map[int64]link{}
	groups := map[int64][]link{}
	for rows.Next() {
		var n link
		if err = rows.Scan(&n.id, &n.group, &n.next, &n.title); err != nil {
			rows.Close()
			return err
		}
		byID[n.id] = n
		groups[n.group] = append(groups[n.group], n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM MaterializedPlaylistPath"); err != nil {
		return err
	}
	type pending struct {
		id     int64
		prefix string
	}
	queue := []pending{{0, ""}}
	position := 0
	// Breadth-first traversal uses the repaired sibling topology and emits each
	// path once, avoiding PlaylistPath's recursive per-row subqueries.
	for i := 0; i < len(queue); i++ {
		p := queue[i]
		for _, n := range order(groups[p.id]) {
			position++
			path := p.prefix + n.title + ";"
			if _, err = tx.Exec("INSERT INTO MaterializedPlaylistPath VALUES(?,?,?,?,?)", n.id, n.group, n.next, position, path); err != nil {
				return err
			}
			queue = append(queue, pending{n.id, path})
		}
	}
	if position != len(byID) {
		return fmt.Errorf("playlist tree is not rooted: %d of %d cached", position, len(byID))
	}
	_, err = tx.Exec("UPDATE GuardianCacheState SET dirty=0 WHERE id=1")
	return err
}
