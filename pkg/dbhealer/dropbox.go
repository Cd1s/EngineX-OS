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
	dropboxScanPageSize = 512
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
// It shares runMountTrackPass with HealMountDataOnlyContext and HealMount so
// standalone callers and the production mount entry point follow the exact
// same paged, context-aware discovery, cursor persistence, fileBytes size
// check, and #322 NFD->NFC safeguards.
func localizeDropboxTracks(ctx context.Context, db *sql.DB, mount string, r *Report) (int, error) {
	if ctx.Err() != nil {
		return 1, nil
	}
	dbUUID, ok, err := librarySchema302UUID(ctx, db)
	if err != nil {
		if isContextStop(ctx, err) {
			return 1, nil
		}
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	res, err := runMountTrackPass(ctx, db, mount, r, mountTrackPassOptions{
		runNFD:     false,
		runDropbox: true,
		dataOnly:   false,
		dbUUID:     dbUUID,
		pageSize:   dropboxScanPageSize,
	})
	if err != nil {
		return res.dropboxRemaining, err
	}
	if res.stoppedEarly && res.dropboxRemaining == 0 {
		return 1, nil
	}
	return res.dropboxRemaining, nil
}

func processDropboxCandidates(
	ctx context.Context,
	q sqlQuerier,
	outerTx *sql.Tx,
	mount string,
	r *Report,
	candidates []dropboxTrackCandidate,
	bridgeManaged map[int64]bool,
	safeDirs map[string]bool,
	claimedNFC map[string]int64,
) (remaining int, lastExaminedID int64, interrupted bool, err error) {
	db, _ := q.(*sql.DB)
	for start := 0; start < len(candidates); start += 64 {
		if ctx.Err() != nil {
			if outerTx != nil {
				return len(candidates) - start, lastExaminedID, true, ctx.Err()
			}
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
			if bridgeManaged[candidate.id] {
				if candidate.id > batchLastID {
					batchLastID = candidate.id
				}
				continue
			}
			upd, ok, err := resolveDropboxCandidate(ctx, q, mount, candidate, safeDirs, claimedNFC)
			if err != nil {
				if outerTx == nil && isContextStop(ctx, err) {
					next--
					break
				}
				return len(candidates) - start, lastExaminedID, false, err
			}
			if candidate.id > batchLastID {
				batchLastID = candidate.id
			}
			if ok {
				updates = append(updates, upd)
			}
		}

		if len(updates) > 0 {
			var fixed, normalized int
			if outerTx != nil {
				fixed, normalized, err = applyDropboxUpdatesTx(outerTx, updates)
			} else {
				fixed, normalized, err = applyDropboxUpdates(db, updates)
			}
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
			if outerTx != nil {
				return len(candidates) - next, lastExaminedID, true, ctx.Err()
			}
			return len(candidates) - next, lastExaminedID, true, nil
		}
		remaining = len(candidates) - end
		if ctx.Err() != nil && (outerTx != nil || remaining > 0) {
			if outerTx != nil {
				return remaining, lastExaminedID, true, ctx.Err()
			}
			return remaining, lastExaminedID, true, nil
		}
	}
	return 0, lastExaminedID, false, nil
}

func resolveDropboxCandidate(
	ctx context.Context,
	q sqlQuerier,
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
	err := q.QueryRowContext(ctx, `SELECT id FROM Track WHERE path=?`, nfcPath).Scan(&other)
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
	fixed, normalized, err = applyDropboxUpdatesTx(tx, updates)
	if err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return fixed, normalized, nil
}

func applyDropboxUpdatesTx(tx *sql.Tx, updates []dropboxResolvedUpdate) (fixed, normalized int, err error) {
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
			return 0, 0, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			fixed++
			if upd.renamePath {
				normalized++
			}
		}
	}
	return fixed, normalized, nil
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
