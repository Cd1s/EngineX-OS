package dbhealer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
)

type dropboxTrackCandidate struct {
	id   int64
	path string
}

var localizeDropboxTrackBatch func()

// localizeDropboxTracks clears Dropbox streaming metadata on Track rows whose
// unencrypted audio file already exists on the mounted drive. Stock Engine OS
// 5.3.0 routes any Track with streamingSource="Dropbox" or a "streaming://" URI
// through StreamingInterfaceController::canLoadToDeck (requiring a live Dropbox
// login and internet connection) even when Sync Manager or a manual copy placed
// the audio file locally on the SD card or USB drive.
//
// Paid streaming providers (TIDAL, Beatport, Beatsource, SoundCloud, Amazon
// Music, Apple Music, OfflineLibrary), online-only Dropbox rows without a local
// file, 0-byte files, directories, symlinks, and paths escaping mount are never
// modified.
func localizeDropboxTracks(ctx context.Context, db *sql.DB, mount string, r *Report) (int, error) {
	candidates, bridgeManaged, err := collectDropboxTrackCandidates(db, mount)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if !bridgeManaged[candidate.id] {
			filtered = append(filtered, candidate)
		}
	}
	candidates = filtered
	if len(candidates) == 0 {
		return 0, nil
	}

	safeDirs := map[string]bool{}
	remaining := 0
	for start := 0; start < len(candidates); start += 64 {
		if err := ctx.Err(); err != nil {
			return len(candidates) - start, nil
		}
		end := min(start+64, len(candidates))
		tx, err := db.Begin()
		if err != nil {
			return remaining, err
		}
		next := start
		batchFixed := 0
		for _, candidate := range candidates[start:end] {
			if ctx.Err() != nil {
				break
			}
			next++
			if !localAudioFileExists(mount, candidate.path, safeDirs) {
				continue
			}
			// Clearing streamingSource, uri, and streamingFlags does not touch
			// any column watched by trigger_after_update_only_Track_timestamp,
			// so lastEditTime, originDatabaseUuid, originTrackId, cues, loops,
			// beat grids, and playlists remain intact.
			res, err := tx.Exec(`UPDATE Track SET streamingSource=NULL, uri=NULL, streamingFlags=0 WHERE id=? AND path=?`, candidate.id, candidate.path)
			if err != nil {
				_ = tx.Rollback()
				return remaining, err
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				batchFixed++
			}
		}
		if err := tx.Commit(); err != nil {
			return remaining, err
		}
		r.DropboxLocalized += batchFixed
		if localizeDropboxTrackBatch != nil {
			localizeDropboxTrackBatch()
		}
		if next < end {
			return len(candidates) - next, nil
		}
		remaining = len(candidates) - end
	}
	return remaining, nil
}

func collectDropboxTrackCandidates(db *sql.DB, mount string) ([]dropboxTrackCandidate, map[int64]bool, error) {
	rows, err := db.Query(`SELECT id, COALESCE(path,''), COALESCE(streamingSource,''), COALESCE(uri,'')
		FROM Track
		WHERE COALESCE(path,'') <> ''
		  AND (lower(COALESCE(streamingSource,'')) = 'dropbox' OR lower(COALESCE(uri,'')) LIKE 'streaming://dropbox/%')`)
	if err != nil {
		return nil, nil, err
	}
	var candidates []dropboxTrackCandidate
	for rows.Next() {
		var id int64
		var path, src, uri string
		if err := rows.Scan(&id, &path, &src, &uri); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		if isDropboxLocalizableTag(src, uri) {
			candidates = append(candidates, dropboxTrackCandidate{id: id, path: path})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	// Do not mutate an unrecognized future schema; let heal() report it.
	var major, minor, patch int
	if err := db.QueryRow("SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
		return nil, nil, err
	}
	if major != 3 || minor != 0 || patch != 2 {
		return nil, nil, nil
	}
	bridgeManaged, err := bridgeManagedTracks(mount)
	if err != nil {
		for _, candidate := range candidates {
			bridgeManaged[candidate.id] = true
		}
		return candidates, bridgeManaged, nil
	}
	return candidates, bridgeManaged, nil
}

func isDropboxLocalizableTag(streamingSource, uri string) bool {
	src := strings.TrimSpace(streamingSource)
	u := strings.TrimSpace(uri)
	if src != "" && !strings.EqualFold(src, "dropbox") {
		return false
	}
	if u != "" && !hasFoldPrefix(u, "streaming://dropbox/") {
		return false
	}
	return strings.EqualFold(src, "dropbox") || hasFoldPrefix(u, "streaming://dropbox/")
}

func hasFoldPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// localAudioFileExists reports whether relTrackPath resolves strictly inside
// mount without crossing any symlink inside mount and names a non-empty regular
// file.
func localAudioFileExists(mount, relTrackPath string, safeDirs map[string]bool) bool {
	full, ok := trackPath(mount, relTrackPath)
	if !ok {
		return false
	}
	rel, err := filepath.Rel(mount, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 {
		return false
	}
	dirRel := filepath.Dir(rel)
	if dirRel != "." {
		if ok, cached := safeDirs[dirRel]; cached {
			if !ok {
				return false
			}
		} else {
			cur := mount
			dirOK := true
			for _, seg := range parts[:len(parts)-1] {
				cur = filepath.Join(cur, seg)
				info, err := os.Lstat(cur)
				if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					dirOK = false
					break
				}
			}
			safeDirs[dirRel] = dirOK
			if !dirOK {
				return false
			}
		}
	}
	info, err := os.Lstat(full)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Size() > 0
}
