package enginedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// PlaylistOnlyResult counts the imported playlist rows changed by a list-only update.
type PlaylistOnlyResult struct {
	Added, Updated, Removed int
}

var ErrPlaylistTrackSetChanged = errors.New("rekordbox track set changed")

type importedPlaylist struct {
	sourceID, listID, parentID int64
	title                      string
	entities                   []int64
}

// SyncPlaylistsOnly applies a re-export whose track identities are unchanged. It
// reads and writes only playlist tables in one transaction. Track and analysis
// rows, RGB files, and the bridge state remain untouched.
func SyncPlaylistsOnly(ctx context.Context, mount string, l *rekordbox.Library) (PlaylistOnlyResult, error) {
	var result PlaylistOnlyResult
	dir, err := compat.SafeOutputDir(mount, "Engine Library/Database2")
	if err != nil {
		return result, err
	}
	if OpenElsewhere(filepath.Join(dir, "m.db")) != 0 {
		return result, ErrLibraryInUse
	}
	lockPath := filepath.Join(dir, ".rb-bridge.lock")
	if err = safeFile(lockPath); err != nil {
		return result, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return result, fmt.Errorf("bridge already syncing: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	db, tx, err := open(ctx, filepath.Join(dir, "m.db"), false)
	if err != nil {
		return result, err
	}
	defer db.Close()
	defer tx.Rollback()
	q := newStmtCache(tx)
	defer q.Close()

	var databaseUUID string
	if err = tx.QueryRowContext(ctx, `SELECT uuid FROM Information`).Scan(&databaseUUID); err != nil {
		return result, err
	}
	trackIDs, err := playlistTrackMapping(ctx, tx)
	if err != nil {
		return result, err
	}
	for _, track := range l.Tracks {
		if _, ok := trackIDs[track.ID]; !ok {
			return result, ErrPlaylistTrackSetChanged
		}
	}

	existing, byList, err := importedPlaylists(ctx, tx)
	if err != nil {
		return result, err
	}
	desired := make(map[uint32]rekordbox.Playlist, len(l.Playlists))
	for _, playlist := range l.Playlists {
		desired[playlist.ID] = playlist
	}
	for {
		removed := false
		for source, current := range existing {
			if _, ok := desired[source]; ok {
				continue
			}
			if _, parentImported := byList[current.parentID]; parentImported {
				continue
			}
			if _, err = q.ExecContext(ctx, `DELETE FROM PlaylistEntity WHERE listId=?`, current.listID); err != nil {
				return result, err
			}
			if _, err = q.ExecContext(ctx, `DELETE FROM Playlist WHERE id=?`, current.listID); err != nil {
				return result, err
			}
			if _, err = q.ExecContext(ctx, `DELETE FROM BridgePlaylist WHERE sourceId=?`, source); err != nil {
				return result, err
			}
			delete(existing, source)
			delete(byList, current.listID)
			result.Removed++
			removed = true
		}
		if !removed {
			break
		}
	}

	resolved := make(map[uint32]int64, len(desired))
	var ensure func(uint32) (int64, error)
	ensure = func(source uint32) (int64, error) {
		if id, ok := resolved[source]; ok {
			return id, nil
		}
		playlist, ok := desired[source]
		if !ok {
			return 0, fmt.Errorf("playlist %d parent missing", source)
		}
		parent := int64(0)
		if playlist.ParentID != 0 {
			var err error
			parent, err = ensure(playlist.ParentID)
			if err != nil {
				return 0, err
			}
		}
		current, exists := existing[uint32(source)]
		if !exists {
			title, err := uniqueTitle(ctx, q, playlist.Name, parent)
			if err != nil {
				return 0, err
			}
			r, err := q.ExecContext(ctx, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES(?,?,1,0,1)`, title, parent)
			if err != nil {
				return 0, err
			}
			id, err := r.LastInsertId()
			if err != nil {
				return 0, err
			}
			if _, err = q.ExecContext(ctx, `INSERT INTO BridgePlaylist(sourceId,listId) VALUES(?,?)`, source, id); err != nil {
				return 0, err
			}
			current = importedPlaylist{sourceID: int64(source), listID: id, parentID: parent, title: title}
			existing[source] = current
			byList[id] = current
			result.Added++
		} else if current.parentID != parent || !playlistTitleMatches(current.title, playlist.Name) {
			if _, err := q.ExecContext(ctx, `UPDATE Playlist SET title=?,parentListId=? WHERE id=?`, playlist.Name, parent, current.listID); err != nil {
				return 0, err
			}
			current.parentID, current.title = parent, playlist.Name
			existing[source] = current
			byList[current.listID] = current
			result.Updated++
		}
		resolved[source] = current.listID
		return current.listID, nil
	}

	for _, playlist := range l.Playlists {
		_, existedBefore := existing[playlist.ID]
		listID, err := ensure(playlist.ID)
		if err != nil {
			return result, err
		}
		members, err := entityTracks(playlist.Tracks, trackIDs)
		if err != nil {
			return result, ErrPlaylistTrackSetChanged
		}
		current := existing[playlist.ID]
		if sameInt64s(current.entities, members) {
			continue
		}
		if _, err = q.ExecContext(ctx, `DELETE FROM PlaylistEntity WHERE listId=?`, listID); err != nil {
			return result, err
		}
		if err = writeEntities(ctx, q, listID, playlist.Tracks, trackIDs, databaseUUID); err != nil {
			return result, err
		}
		current.entities = members
		existing[playlist.ID] = current
		if existedBefore {
			result.Updated++
		}
	}
	if err = normalizePlaylistTails(ctx, tx); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func playlistTitleMatches(current, source string) bool {
	return current == source || strings.HasPrefix(current, source+" (rekordbox")
}

func playlistTrackMapping(ctx context.Context, tx *sql.Tx) (map[uint32]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT pdbImportKey,id FROM Track WHERE pdbImportKey BETWEEN 1 AND 4294967295`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mapping := make(map[uint32]int64)
	for rows.Next() {
		var source uint32
		var id int64
		if err = rows.Scan(&source, &id); err != nil {
			return nil, err
		}
		if _, exists := mapping[source]; exists {
			return nil, ErrPlaylistTrackSetChanged
		}
		mapping[source] = id
	}
	return mapping, rows.Err()
}

func importedPlaylists(ctx context.Context, tx *sql.Tx) (map[uint32]importedPlaylist, map[int64]importedPlaylist, error) {
	rows, err := tx.QueryContext(ctx, `SELECT b.sourceId,b.listId,p.parentListId,p.title FROM BridgePlaylist b JOIN Playlist p ON p.id=b.listId WHERE b.sourceId>=0`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	bySource := make(map[uint32]importedPlaylist)
	byList := make(map[int64]importedPlaylist)
	for rows.Next() {
		var source, list, parent int64
		var title string
		if err = rows.Scan(&source, &list, &parent, &title); err != nil {
			return nil, nil, err
		}
		p := importedPlaylist{sourceID: source, listID: list, parentID: parent, title: title}
		bySource[uint32(source)] = p
		byList[list] = p
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	entities, err := tx.QueryContext(ctx, `SELECT listId,trackId FROM PlaylistEntity WHERE listId IN (SELECT listId FROM BridgePlaylist WHERE sourceId>=0) ORDER BY listId,id`)
	if err != nil {
		return nil, nil, err
	}
	defer entities.Close()
	for entities.Next() {
		var list, track int64
		if err = entities.Scan(&list, &track); err != nil {
			return nil, nil, err
		}
		p, ok := byList[list]
		if ok {
			p.entities = append(p.entities, track)
			byList[list] = p
			bySource[uint32(p.sourceID)] = p
		}
	}
	return bySource, byList, entities.Err()
}

func sameInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
