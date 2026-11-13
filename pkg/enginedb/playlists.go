package enginedb

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

type nativeChildNode struct {
	id    int64
	title string
	next  int64
}

func orderNativeChildren(nodes []nativeChildNode) []nativeChildNode {
	byID := make(map[int64]nativeChildNode, len(nodes))
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
	out := make([]nativeChildNode, 0, len(nodes))
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

func reparentNativeChildren(ctx context.Context, q sqlExecer, owned string) error {
	rows, err := q.QueryContext(ctx, `SELECT id, COALESCE(title,''), parentListId, COALESCE(nextListId,0) FROM Playlist WHERE parentListId IN (`+owned+`) AND id NOT IN (SELECT listId FROM BridgePlaylist) ORDER BY parentListId, id`)
	if err != nil {
		return err
	}
	byParent := map[int64][]nativeChildNode{}
	for rows.Next() {
		var parent int64
		var n nativeChildNode
		if err = rows.Scan(&n.id, &n.title, &parent, &n.next); err != nil {
			rows.Close()
			return err
		}
		byParent[parent] = append(byParent[parent], n)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(byParent) == 0 {
		return nil
	}

	parents := make([]int64, 0, len(byParent))
	for p := range byParent {
		parents = append(parents, p)
	}
	sort.Slice(parents, func(i, j int) bool { return parents[i] < parents[j] })

	for _, p := range parents {
		nodes := orderNativeChildren(byParent[p])
		for i, n := range nodes {
			title, err := uniqueNativeTitle(ctx, q, n.title, n.id)
			if err != nil {
				return err
			}
			var nextVal any
			if i+1 < len(nodes) && n.next == nodes[i+1].id {
				nextVal = nodes[i+1].id
			}
			if _, err = q.ExecContext(ctx, `UPDATE Playlist SET parentListId=0, title=?, nextListId=? WHERE id=?`, title, nextVal, n.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func uniqueNativeTitle(ctx context.Context, q sqlExecer, name string, id int64) (string, error) {
	title := name
	for n := 1; ; n++ {
		var count int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM Playlist WHERE title=? AND parentListId=0 AND id<>?`, title, id).Scan(&count); err != nil {
			return "", err
		}
		if count == 0 {
			return title, nil
		}
		title = fmt.Sprintf("%s (%d)", name, n)
	}
}

// normalizePlaylistTails joins disconnected NULL-terminated native chains before
// converting NULL to zero. Converting several sibling tails directly to zero
// violates Engine's UNIQUE(parentListId,nextListId) constraint.
func normalizePlaylistTails(ctx context.Context, tx *sql.Tx) error {
	type node struct {
		id   int64
		next sql.NullInt64
	}
	groups := map[sql.NullInt64][]node{}
	rows, err := tx.QueryContext(ctx, `SELECT id,parentListId,nextListId FROM Playlist ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n node
		var parent sql.NullInt64
		if err = rows.Scan(&n.id, &parent, &n.next); err != nil {
			rows.Close()
			return err
		}
		groups[parent] = append(groups[parent], n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, nodes := range groups {
		previous := map[int64]int64{}
		var nulls []int64
		var tail int64
		hasTail := false
		for _, n := range nodes {
			if !n.next.Valid {
				nulls = append(nulls, n.id)
			}
		}
		if len(nulls) == 0 {
			continue
		}
		for _, n := range nodes {
			if !n.next.Valid {
				continue
			}
			if n.next.Int64 == 0 {
				tail = n.id
				hasTail = true
				continue
			}
			if _, exists := previous[n.next.Int64]; exists {
				return fmt.Errorf("ambiguous native playlist chain")
			}
			previous[n.next.Int64] = n.id
		}
		for _, id := range nulls {
			head := id
			for hops := 0; ; hops++ {
				predecessor, exists := previous[head]
				if !exists {
					break
				}
				if hops >= len(nodes) {
					return fmt.Errorf("cyclic native playlist chain")
				}
				head = predecessor
			}
			if hasTail {
				if _, err = tx.ExecContext(ctx, `UPDATE Playlist SET nextListId=? WHERE id=?`, head, tail); err != nil {
					return err
				}
			}
			tail = id
			hasTail = true
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE Playlist SET nextListId=0 WHERE nextListId IS NULL`)
	return err
}
