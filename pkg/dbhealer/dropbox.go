package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const (
	dropboxCursorFile   = ".enginex-dropbox-cursor"
	dropboxCursorPrefix = "v1"
	dropboxScanPageSize = 256
)

type dropboxTrackCandidate struct {
	id        int64
	path      string
	fileBytes int64
}

type dropboxResolvedUpdate struct {
	id         int64
	oldPath    string
	newPath    string
	renamePath bool
}

var localizeDropboxTrackBatch func()

// localizeDropboxTracks clears Dropbox streaming metadata on Track rows whose
// complete, unencrypted audio file already exists on the mounted drive. Stock
// Engine OS 5.3.0 routes any Track with streamingSource="Dropbox" or a
// "streaming://" URI through StreamingInterfaceController::canLoadToDeck
// (requiring a live Dropbox login and internet connection) even when Sync
// Manager or a manual copy placed the audio file locally on the SD card or USB
// drive.
//
// Discovery is paged by Track.id and context-aware so large libraries without
// Dropbox rows stop cleanly at caller deadlines and resume from the last
// examined row across mounts via a per-library cursor file beside m.db (removed
// on completion, leaving stock m.db schema byte-identical).
//
// Paid streaming providers (TIDAL, Beatport, Beatsource, SoundCloud, Amazon
// Music, Apple Music, OfflineLibrary), online-only Dropbox rows without a local
// file, truncated/mismatched copies when Track.fileBytes > 0, 0-byte files,
// directories, symlinks, and paths escaping mount are never modified.
func localizeDropboxTracks(ctx context.Context, db *sql.DB, mount string, r *Report) (int, error) {
	if ctx.Err() != nil {
		return 1, nil
	}
	dbUUID, ok, err := librarySchema302UUID(ctx, db)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 1, nil
		}
		return 0, err
	}
	if !ok {
		return 0, nil
	}

	afterID, hadCursor := readDropboxCursor(mount, dbUUID)
	lastExaminedID := afterID
	var bridgeManaged map[int64]bool
	bridgeLoaded := false
	safeDirs := map[string]bool{}
	claimedNFC := map[string]int64{}

	pageStmt, err := db.PrepareContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(id), 0),
		        COALESCE(SUM(CASE WHEN path IS NOT NULL AND path <> ''
		            AND ((streamingSource IS NOT NULL AND streamingSource <> '') OR (uri IS NOT NULL AND uri <> ''))
		        THEN 1 ELSE 0 END), 0)
		 FROM (SELECT id, path, streamingSource, uri FROM Track WHERE id > ? ORDER BY id LIMIT ?)`,
	)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 1, nil
		}
		return 0, err
	}
	defer pageStmt.Close()

	var detailStmt *sql.Stmt
	defer func() {
		if detailStmt != nil {
			_ = detailStmt.Close()
		}
	}()

	pageAfter := afterID
	for {
		if ctx.Err() != nil {
			if lastExaminedID > afterID {
				writeDropboxCursor(mount, dbUUID, lastExaminedID)
			}
			return 1, nil
		}
		var count, pageLastID, hits int64
		if err := pageStmt.QueryRowContext(ctx, pageAfter, dropboxScanPageSize).Scan(&count, &pageLastID, &hits); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if lastExaminedID > afterID {
					writeDropboxCursor(mount, dbUUID, lastExaminedID)
				}
				return 1, nil
			}
			return 0, err
		}
		if count == 0 {
			if hadCursor {
				removeDropboxCursor(mount)
			}
			return 0, nil
		}
		eof := count < dropboxScanPageSize
		var page []dropboxTrackCandidate
		if hits > 0 {
			if detailStmt == nil {
				detailStmt, err = db.PrepareContext(ctx,
					`SELECT id, COALESCE(path,''), COALESCE(streamingSource,''), COALESCE(uri,''), COALESCE(fileBytes,0)
					 FROM Track
					 WHERE id > ? AND id <= ?
					   AND path IS NOT NULL AND path <> ''
					   AND ((streamingSource IS NOT NULL AND streamingSource <> '') OR (uri IS NOT NULL AND uri <> ''))
					 ORDER BY id`,
				)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						if lastExaminedID > afterID {
							writeDropboxCursor(mount, dbUUID, lastExaminedID)
						}
						return 1, nil
					}
					return 0, err
				}
			}
			page, err = queryDropboxPageDetails(ctx, detailStmt, pageAfter, pageLastID)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					if lastExaminedID > afterID {
						writeDropboxCursor(mount, dbUUID, lastExaminedID)
					}
					return 1, nil
				}
				return 0, err
			}
		}
		if len(page) > 0 {
			if !bridgeLoaded {
				bridgeLoaded = true
				bridgeManaged, err = loadBridgeManagedForDropbox(ctx, mount)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						if lastExaminedID > afterID {
							writeDropboxCursor(mount, dbUUID, lastExaminedID)
						}
						return 1, nil
					}
					// Conservative fallback matching #322: unreadable bridge state skips updates.
					bridgeManaged = nil
					if hadCursor {
						removeDropboxCursor(mount)
					}
					return 0, nil
				}
			}
			rem, examinedID, interrupted, err := processDropboxCandidates(
				ctx, db, mount, r, page, bridgeManaged, safeDirs, claimedNFC,
			)
			if examinedID > lastExaminedID {
				lastExaminedID = examinedID
			}
			if err != nil {
				return rem, err
			}
			if interrupted {
				if !eof || rem > 0 {
					if lastExaminedID > afterID {
						writeDropboxCursor(mount, dbUUID, lastExaminedID)
					}
					if rem > 0 {
						return rem, nil
					}
					return 1, nil
				}
			}
		}
		if pageLastID > lastExaminedID {
			lastExaminedID = pageLastID
		}
		pageAfter = pageLastID
		if eof {
			if hadCursor {
				removeDropboxCursor(mount)
			}
			return 0, nil
		}
	}
}

// localizeDropboxCandidates processes Dropbox candidates already collected by
// healMountContext's unified Track scan (avoiding a second full-table scan on
// normal mounts).
func localizeDropboxCandidates(
	ctx context.Context,
	db *sql.DB,
	mount string,
	r *Report,
	dbUUID string,
	afterID int64,
	hadCursor bool,
	candidates []dropboxTrackCandidate,
	lastScannedID int64,
	eof bool,
) (int, error) {
	if len(candidates) == 0 {
		if eof {
			if hadCursor {
				removeDropboxCursor(mount)
			}
			return 0, nil
		}
		if lastScannedID > afterID {
			writeDropboxCursor(mount, dbUUID, lastScannedID)
		}
		return 1, nil
	}
	if ctx.Err() != nil {
		return len(candidates), nil
	}
	bridgeManaged, err := loadBridgeManagedForDropbox(ctx, mount)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return len(candidates), nil
		}
		// Conservative fallback matching #322: unreadable bridge state skips updates.
		return 0, nil
	}
	safeDirs := map[string]bool{}
	claimedNFC := map[string]int64{}
	rem, examinedID, interrupted, err := processDropboxCandidates(
		ctx, db, mount, r, candidates, bridgeManaged, safeDirs, claimedNFC,
	)
	if err != nil {
		return rem, err
	}
	if interrupted || rem > 0 || !eof {
		cursorID := examinedID
		if rem == 0 && lastScannedID > cursorID {
			cursorID = lastScannedID
		}
		if cursorID > afterID {
			writeDropboxCursor(mount, dbUUID, cursorID)
		}
		if rem > 0 {
			return rem, nil
		}
		if !eof {
			return 1, nil
		}
	}
	if hadCursor {
		removeDropboxCursor(mount)
	}
	return 0, nil
}

func processDropboxCandidates(
	ctx context.Context,
	db *sql.DB,
	mount string,
	r *Report,
	candidates []dropboxTrackCandidate,
	bridgeManaged map[int64]bool,
	safeDirs map[string]bool,
	claimedNFC map[string]int64,
) (remaining int, lastExaminedID int64, interrupted bool, err error) {
	for start := 0; start < len(candidates); start += 64 {
		if ctx.Err() != nil {
			return len(candidates) - start, lastExaminedID, true, nil
		}
		end := min(start+64, len(candidates))
		updates := make([]dropboxResolvedUpdate, 0, end-start)
		next := start
		batchLastID := lastExaminedID
		for _, candidate := range candidates[start:end] {
			if ctx.Err() != nil {
				break
			}
			next++
			if candidate.id > batchLastID {
				batchLastID = candidate.id
			}
			if bridgeManaged[candidate.id] {
				continue
			}
			upd, ok, err := resolveDropboxCandidate(ctx, db, mount, candidate, safeDirs, claimedNFC)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					next--
					break
				}
				return len(candidates) - start, lastExaminedID, false, err
			}
			if ok {
				updates = append(updates, upd)
			}
		}

		if len(updates) > 0 {
			fixed, normalized, err := applyDropboxUpdates(db, updates)
			if err != nil {
				return len(candidates) - start, lastExaminedID, false, err
			}
			r.DropboxLocalized += fixed
			r.PathsNormalized += normalized
		}
		if batchLastID > lastExaminedID {
			lastExaminedID = batchLastID
		}
		if localizeDropboxTrackBatch != nil {
			localizeDropboxTrackBatch()
		}
		if next < end {
			return len(candidates) - next, lastExaminedID, true, nil
		}
		remaining = len(candidates) - end
		if ctx.Err() != nil && remaining > 0 {
			return remaining, lastExaminedID, true, nil
		}
	}
	return 0, lastExaminedID, false, nil
}

func resolveDropboxCandidate(
	ctx context.Context,
	db *sql.DB,
	mount string,
	candidate dropboxTrackCandidate,
	safeDirs map[string]bool,
	claimedNFC map[string]int64,
) (dropboxResolvedUpdate, bool, error) {
	if matchLocalAudioFile(mount, candidate.path, candidate.fileBytes, safeDirs) {
		return dropboxResolvedUpdate{
			id:      candidate.id,
			oldPath: candidate.path,
			newPath: candidate.path,
		}, true, nil
	}
	if norm.NFC.IsNormalString(candidate.path) {
		return dropboxResolvedUpdate{}, false, nil
	}
	nfcPath := norm.NFC.String(candidate.path)
	if !matchLocalAudioFile(mount, nfcPath, candidate.fileBytes, safeDirs) {
		return dropboxResolvedUpdate{}, false, nil
	}
	if other, collision := claimedNFC[nfcPath]; collision && other != candidate.id {
		return dropboxResolvedUpdate{}, false, nil
	}
	var other int64
	err := db.QueryRowContext(ctx, `SELECT id FROM Track WHERE path=?`, nfcPath).Scan(&other)
	if err == nil && other != candidate.id {
		return dropboxResolvedUpdate{}, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return dropboxResolvedUpdate{}, false, err
	}
	claimedNFC[nfcPath] = candidate.id
	return dropboxResolvedUpdate{
		id:         candidate.id,
		oldPath:    candidate.path,
		newPath:    nfcPath,
		renamePath: true,
	}, true, nil
}

func applyDropboxUpdates(db *sql.DB, updates []dropboxResolvedUpdate) (fixed, normalized int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	for _, upd := range updates {
		var res sql.Result
		if !upd.renamePath {
			// Clearing streamingSource, uri, and streamingFlags does not touch
			// any column watched by trigger_after_update_only_Track_timestamp,
			// so lastEditTime, originDatabaseUuid, originTrackId, cues, loops,
			// beat grids, and playlists remain intact.
			res, err = tx.Exec(
				`UPDATE Track SET streamingSource=NULL, uri=NULL, streamingFlags=0 WHERE id=? AND path=?`,
				upd.id, upd.oldPath,
			)
		} else {
			oldFilename := filepath.Base(upd.oldPath)
			newFilename := filepath.Base(upd.newPath)
			if oldFilename == newFilename {
				res, err = tx.Exec(
					`UPDATE Track SET path=?, streamingSource=NULL, uri=NULL, streamingFlags=0 WHERE id=? AND path=?`,
					upd.newPath, upd.id, upd.oldPath,
				)
			} else {
				var lastEditType string
				var lastEditTime any
				if err = tx.QueryRow(
					`SELECT typeof(lastEditTime), CASE typeof(lastEditTime) WHEN 'text' THEN CAST(lastEditTime AS BLOB) ELSE lastEditTime END FROM Track WHERE id=? AND path=?`,
					upd.id, upd.oldPath,
				).Scan(&lastEditType, &lastEditTime); err == nil {
					if b, ok := lastEditTime.([]byte); ok && lastEditType == "text" {
						lastEditTime = string(b)
					} else if b, ok := lastEditTime.([]byte); ok && lastEditType == "blob" && b == nil {
						lastEditTime = []byte{}
					}
					if res, err = tx.Exec(
						`UPDATE Track SET path=?, filename=?, streamingSource=NULL, uri=NULL, streamingFlags=0 WHERE id=? AND path=?`,
						upd.newPath, newFilename, upd.id, upd.oldPath,
					); err == nil {
						_, err = tx.Exec(`UPDATE Track SET lastEditTime=? WHERE id=?`, lastEditTime, upd.id)
					}
				}
			}
		}
		if err != nil {
			if upd.renamePath && strings.Contains(err.Error(), "UNIQUE constraint failed") {
				continue
			}
			_ = tx.Rollback()
			return 0, 0, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			fixed++
			if upd.renamePath {
				normalized++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return fixed, normalized, nil
}

func queryDropboxPageDetails(ctx context.Context, stmt *sql.Stmt, afterID, maxID int64) ([]dropboxTrackCandidate, error) {
	rows, err := stmt.QueryContext(ctx, afterID, maxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []dropboxTrackCandidate
	for rows.Next() {
		var id, fileBytes int64
		var path, src, uri string
		if err := rows.Scan(&id, &path, &src, &uri, &fileBytes); err != nil {
			return nil, err
		}
		if path != "" && isDropboxLocalizableTag(src, uri) {
			candidates = append(candidates, dropboxTrackCandidate{id: id, path: path, fileBytes: fileBytes})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func librarySchema302UUID(ctx context.Context, db *sql.DB) (string, bool, error) {
	var dbUUID string
	var major, minor, patch int
	err := db.QueryRowContext(
		ctx,
		`SELECT COALESCE(uuid,''), schemaVersionMajor, schemaVersionMinor, schemaVersionPatch FROM Information LIMIT 1`,
	).Scan(&dbUUID, &major, &minor, &patch)
	if err != nil {
		return "", false, err
	}
	if major != 3 || minor != 0 || patch != 2 {
		return "", false, nil
	}
	return strings.TrimSpace(dbUUID), true, nil
}

func loadBridgeManagedForDropbox(ctx context.Context, mount string) (map[int64]bool, error) {
	return bridgeManagedTracksContext(ctx, mount)
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

// matchLocalAudioFile reports whether relTrackPath resolves strictly inside
// mount without crossing any symlink inside mount and names a non-empty regular
// file whose byte size matches expectedBytes whenever expectedBytes > 0.
func matchLocalAudioFile(mount, relTrackPath string, expectedBytes int64, safeDirs map[string]bool) bool {
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
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return false
	}
	if expectedBytes > 0 && info.Size() != expectedBytes {
		return false
	}
	return true
}

func dropboxCursorPath(mount string) string {
	return filepath.Join(mount, "Engine Library", "Database2", dropboxCursorFile)
}

func readDropboxCursor(mount, dbUUID string) (int64, bool) {
	p := dropboxCursorPath(mount)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 256 {
		return 0, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, true
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 || fields[0] != dropboxCursorPrefix {
		return 0, true
	}
	id, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, true
	}
	storedUUID := ""
	if len(fields) >= 3 {
		storedUUID = fields[2]
	}
	if storedUUID != dbUUID {
		return 0, true
	}
	return id, true
}

func writeDropboxCursor(mount, dbUUID string, lastID int64) {
	if lastID <= 0 {
		return
	}
	p := dropboxCursorPath(mount)
	dir := filepath.Dir(p)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() {
		return
	}
	tmp, err := os.CreateTemp(dir, dropboxCursorFile+"-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	content := fmt.Sprintf("%s %d %s\n", dropboxCursorPrefix, lastID, dbUUID)
	if dbUUID == "" {
		content = fmt.Sprintf("%s %d\n", dropboxCursorPrefix, lastID)
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
	}
}

func removeDropboxCursor(mount string) {
	p := dropboxCursorPath(mount)
	if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() {
		_ = os.Remove(p)
	}
}
