package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type trackPathRow struct {
	id        int64
	path      string
	filename  string
	fileBytes int64
	streaming string
}

type scanTrackRow struct {
	id   int64
	path string
}

type pathDirEntry struct {
	name      string
	isDir     bool
	isRegular bool
	isSymlink bool
}

type pathMatchStatus int

const (
	pathMatchAbsent pathMatchStatus = iota
	pathMatchFound
	pathMatchAmbiguous
)

type fileIdentity struct {
	dev uint64
	ino uint64
	ok  bool
}

func statIdentity(info os.FileInfo) fileIdentity {
	if info == nil {
		return fileIdentity{}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}
	}
	return fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino), ok: true}
}

const (
	// cleanPathProbeCap bounds how long mount-time path healing spends probing
	// healthy/clean Track.path rows when the caller provided a deadline (e.g.
	// the 20 s edisksd mount hook), so healthy large libraries never stall
	// insert or starve base heal.
	cleanPathProbeCap = 500 * time.Millisecond

	pathScanChunkSize      = 512
	pathProbeCursorFile    = ".enginex-path-probe-cursor"
	pathProbeCursorVersion = "v1"
)

var normalizeTrackPathBatch func()

type mountDirCache struct {
	mount string
	dirs  map[string][]pathDirEntry
	exact map[string]map[string]pathDirEntry
}

func newMountDirCache(mount string) *mountDirCache {
	return &mountDirCache{
		mount: mount,
		dirs:  make(map[string][]pathDirEntry),
		exact: make(map[string]map[string]pathDirEntry),
	}
}

func (c *mountDirCache) readDir(ctx context.Context, relDir string) ([]pathDirEntry, map[string]pathDirEntry, bool, error) {
	if entries, ok := c.dirs[relDir]; ok {
		return entries, c.exact[relDir], entries != nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	full := c.mount
	if relDir != "" {
		full = filepath.Join(c.mount, filepath.FromSlash(relDir))
	}
	f, err := os.Open(full)
	if err != nil {
		c.dirs[relDir] = nil
		c.exact[relDir] = nil
		return nil, nil, false, nil
	}
	defer f.Close()

	var entries []pathDirEntry
	byName := make(map[string]pathDirEntry)
	for {
		if err := ctx.Err(); err != nil {
			// Never cache partial directory listings on cancellation.
			return nil, nil, false, err
		}
		chunk, rerr := f.ReadDir(256)
		for _, ent := range chunk {
			typ := ent.Type()
			item := pathDirEntry{
				name:      ent.Name(),
				isDir:     typ.IsDir(),
				isRegular: typ.IsRegular(),
				isSymlink: typ&fs.ModeSymlink != 0,
			}
			entries = append(entries, item)
			byName[item.name] = item
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			c.dirs[relDir] = nil
			c.exact[relDir] = nil
			return nil, nil, false, nil
		}
	}
	if entries == nil {
		entries = []pathDirEntry{}
	}
	c.dirs[relDir] = entries
	c.exact[relDir] = byName
	return entries, byName, true, nil
}

// resolveOnDisk walks mountRel component-by-component from mount root, matching
// missing segments created by macOS / exFAT / FAT32 sync mismatches while
// refusing symlinks, ambiguous matches, and NFC-to-NFD degradation.
func (c *mountDirCache) resolveOnDisk(ctx context.Context, parts []string) (string, pathMatchStatus, error) {
	resolved, status, err := c.resolveFrom(ctx, "", parts)
	if err != nil || status != pathMatchFound {
		return "", status, err
	}
	return strings.Join(resolved, "/"), pathMatchFound, nil
}

func (c *mountDirCache) resolveFrom(ctx context.Context, cur string, parts []string) ([]string, pathMatchStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, pathMatchAbsent, err
	}
	entries, byName, ok, err := c.readDir(ctx, cur)
	if err != nil {
		return nil, pathMatchAbsent, err
	}
	if !ok {
		return nil, pathMatchAbsent, nil
	}
	part := parts[0]
	isFile := len(parts) == 1
	if isFile {
		ent, status, err := matchPathComponent(ctx, part, entries, byName, true, false)
		if err != nil || status != pathMatchFound {
			return nil, status, err
		}
		return []string{ent.name}, pathMatchFound, nil
	}

	nextCur := func(name string) string {
		if cur == "" {
			return name
		}
		return cur + "/" + name
	}

	// Prefer exact directory component first; never bypass an exact-name
	// symlink. Allow sibling directory fallback ONLY when the exact parent
	// lookup returned pathMatchAbsent (never when child lookup was ambiguous).
	if ent, found := byName[part]; found {
		if ent.isSymlink {
			return nil, pathMatchAmbiguous, nil
		}
		if ent.isDir {
			tail, status, err := c.resolveFrom(ctx, nextCur(ent.name), parts[1:])
			if err != nil {
				return nil, pathMatchAbsent, err
			}
			switch status {
			case pathMatchFound:
				return append([]string{ent.name}, tail...), pathMatchFound, nil
			case pathMatchAmbiguous:
				return nil, pathMatchAmbiguous, nil
			}
		}
	}

	ent, status, err := matchPathComponent(ctx, part, entries, byName, false, true)
	if err != nil || status != pathMatchFound {
		return nil, status, err
	}
	tail, tailStatus, err := c.resolveFrom(ctx, nextCur(ent.name), parts[1:])
	if err != nil || tailStatus != pathMatchFound {
		return nil, tailStatus, err
	}
	return append([]string{ent.name}, tail...), pathMatchFound, nil
}

type ownerRowEntry struct {
	id        int64
	path      string
	streaming string
}

type pathOwnershipIndex struct {
	db             *sql.DB
	mount          string
	cache          *mountDirCache
	bridgeManaged  map[int64]bool
	built          bool
	singleChecks   int
	allRows        []ownerRowEntry
	fuzzyFirst     map[string]int32
	fuzzyExtra     map[string][]int32
	claimedByFile  map[fileIdentity]int64
	claimedByRel   map[string]int64
	conflictedFile map[fileIdentity]bool
	conflictedRel  map[string]bool
}

func newPathOwnershipIndex(db *sql.DB, mount string, cache *mountDirCache, bridgeManaged map[int64]bool) *pathOwnershipIndex {
	return &pathOwnershipIndex{
		db:             db,
		mount:          mount,
		cache:          cache,
		bridgeManaged:  bridgeManaged,
		claimedByFile:  make(map[fileIdentity]int64),
		claimedByRel:   make(map[string]int64),
		conflictedFile: make(map[fileIdentity]bool),
		conflictedRel:  make(map[string]bool),
	}
}

func (idx *pathOwnershipIndex) ensureFullIndex(ctx context.Context) error {
	if idx.built {
		return nil
	}
	var est int
	_ = idx.db.QueryRowContext(ctx, `SELECT count(*) FROM Track`).Scan(&est)
	if est < 16 {
		est = 16
	}
	rows, err := idx.db.QueryContext(ctx, `SELECT id, path, COALESCE(streamingSource, '') FROM Track WHERE path IS NOT NULL AND path <> '' ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	allRows := make([]ownerRowEntry, 0, est)
	fuzzyFirst := make(map[string]int32, est)
	fuzzyExtra := make(map[string][]int32)

	addBucket := func(fk string, rowIdx int32) {
		if prev, exists := fuzzyFirst[fk]; !exists {
			fuzzyFirst[fk] = rowIdx
		} else {
			if extra, ok := fuzzyExtra[fk]; !ok {
				fuzzyExtra[fk] = []int32{prev, rowIdx}
			} else {
				fuzzyExtra[fk] = append(extra, rowIdx)
			}
		}
	}

	seen := 0
	for rows.Next() {
		if seen&255 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		seen++
		var row ownerRowEntry
		if err := rows.Scan(&row.id, &row.path, &row.streaming); err != nil {
			return err
		}
		rowIdx := int32(len(allRows))
		allRows = append(allRows, row)

		var litKey string
		if litRel, ok := cleanLiteralMountRel(row.path); ok {
			if fk := fuzzyMountRelKey(litRel); fk != "" {
				litKey = fk
				addBucket(fk, rowIdx)
			}
		}
		if strings.ContainsRune(row.path, '\\') {
			if slashRel, ok := cleanSlashMountRel(row.path); ok {
				if fk := fuzzyMountRelKey(slashRel); fk != "" && fk != litKey {
					addBucket(fk, rowIdx)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	idx.allRows = allRows
	idx.fuzzyFirst = fuzzyFirst
	idx.fuzzyExtra = fuzzyExtra
	idx.built = true
	return nil
}

// collectBucketForSingleCheck streams (id, path) once without allocating full
// maps for the common case of a mount with only one track needing repair.
func (idx *pathOwnershipIndex) collectBucketForSingleCheck(ctx context.Context, targetKey string) ([]ownerRowEntry, error) {
	rows, err := idx.db.QueryContext(ctx, `SELECT id, path FROM Track WHERE path IS NOT NULL AND path <> '' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []ownerRowEntry
	seen := 0
	for rows.Next() {
		if seen&255 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		seen++
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		matched := false
		if litRel, ok := cleanLiteralMountRel(p); ok && fuzzyMountRelKey(litRel) == targetKey {
			matched = true
		} else if strings.ContainsRune(p, '\\') {
			if slashRel, ok := cleanSlashMountRel(p); ok && fuzzyMountRelKey(slashRel) == targetKey {
				matched = true
			}
		}
		if matched {
			matches = append(matches, ownerRowEntry{id: id, path: p})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range matches {
		if err := idx.db.QueryRowContext(ctx, `SELECT COALESCE(streamingSource, '') FROM Track WHERE id=?`, matches[i].id).Scan(&matches[i].streaming); err != nil {
			return nil, err
		}
	}
	return matches, nil
}

// canClaim verifies that resolvedRel (with candStat) is not already owned by
// another Track row (by raw path, canonical cleaned mount path, or on-disk
// file identity) and is not simultaneously claimed by another missing Track
// row with a distinct NFC spelling.
func (idx *pathOwnershipIndex) canClaim(
	ctx context.Context,
	row trackPathRow,
	resolvedRel string,
	newPath string,
	candStat os.FileInfo,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	candID := statIdentity(candStat)
	if idx.conflictedRel[resolvedRel] || (candID.ok && idx.conflictedFile[candID]) {
		return false, nil
	}
	if owner, taken := idx.claimedByRel[resolvedRel]; taken && owner != row.id {
		return false, nil
	}
	if candID.ok {
		if owner, taken := idx.claimedByFile[candID]; taken && owner != row.id {
			return false, nil
		}
	}
	// Fast O(log N) exact unique-path check using Track's C_path UNIQUE index.
	var existingID int64
	if err := idx.db.QueryRowContext(ctx, `SELECT id FROM Track WHERE path=? AND id<>? LIMIT 1`, newPath, row.id).Scan(&existingID); err == nil {
		idx.markConflicted(resolvedRel, candID)
		return false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	fk := fuzzyMountRelKey(resolvedRel)
	var candidates []ownerRowEntry
	if !idx.built && idx.singleChecks == 0 {
		idx.singleChecks++
		var err error
		candidates, err = idx.collectBucketForSingleCheck(ctx, fk)
		if err != nil {
			return false, err
		}
	} else {
		if err := idx.ensureFullIndex(ctx); err != nil {
			return false, err
		}
		if extra, ok := idx.fuzzyExtra[fk]; ok {
			candidates = make([]ownerRowEntry, len(extra))
			for i, rowIdx := range extra {
				candidates[i] = idx.allRows[rowIdx]
			}
		} else if first, ok := idx.fuzzyFirst[fk]; ok {
			candidates = []ownerRowEntry{idx.allRows[first]}
		}
	}

	hasEarlierCanonicalDup := false
	for _, other := range candidates {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if other.id == row.id {
			continue
		}
		if other.path == newPath {
			idx.markConflicted(resolvedRel, candID)
			return false, nil
		}
		if otherLitRel, ok := cleanLiteralMountRel(other.path); ok && otherLitRel == resolvedRel {
			idx.markConflicted(resolvedRel, candID)
			return false, nil
		}
		if other.streaming == "" {
			if otherStat, _, otherExists := literalRegularOrSymlink(idx.mount, other.path); otherExists && os.SameFile(otherStat, candStat) {
				idx.markConflicted(resolvedRel, candID)
				return false, nil
			}
			if !idx.bridgeManaged[other.id] {
				otherRow, ferr := loadTrackPathMeta(ctx, idx.db, other.id, other.path)
				if ferr != nil {
					return false, ferr
				}
				otherRel, otherStat, otherOK, err := resolveRowCandidate(ctx, idx.mount, idx.cache, otherRow)
				if err != nil {
					return false, err
				}
				if otherOK && (otherRel == resolvedRel || os.SameFile(otherStat, candStat)) {
					// Preserve #322 canonical Unicode duplicate semantics when
					// two non-NFC rows encode the exact same NFC target path;
					// reject ALL competing claims whenever distinct names
					// collapse onto the same file (e.g. Mix?.mp3 vs Mix*.mp3).
					if norm.NFC.String(row.path) == newPath && norm.NFC.String(other.path) == newPath {
						if other.id < row.id {
							hasEarlierCanonicalDup = true
						}
						continue
					}
					idx.markConflicted(resolvedRel, candID)
					return false, nil
				}
			}
		}
	}
	if hasEarlierCanonicalDup {
		return false, nil
	}

	idx.claimedByRel[resolvedRel] = row.id
	if candID.ok {
		idx.claimedByFile[candID] = row.id
	}
	return true, nil
}

func (idx *pathOwnershipIndex) markConflicted(resolvedRel string, candID fileIdentity) {
	idx.conflictedRel[resolvedRel] = true
	if candID.ok {
		idx.conflictedFile[candID] = true
	}
}

type pendingTrackPathUpdate struct {
	row     trackPathRow
	newPath string
	newName string
}

func commitTrackPathBatch(ctx context.Context, db *sql.DB, batch []pendingTrackPathUpdate, r *Report) (committed int, lastUncommittedID int64, err error) {
	if len(batch) == 0 {
		return 0, 0, nil
	}
	if ctx.Err() != nil {
		return 0, batch[0].row.id, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, batch[0].row.id, err
	}
	batchFixed := 0
	for i, upd := range batch {
		if ctx.Err() != nil {
			lastUncommittedID = upd.row.id
			break
		}
		if upd.row.filename == upd.newName {
			_, err = tx.Exec(`UPDATE Track SET path=? WHERE id=? AND path=?`, upd.newPath, upd.row.id, upd.row.path)
		} else {
			var lastEditType string
			var lastEditTime any
			if err = tx.QueryRow(
				`SELECT typeof(lastEditTime), CASE typeof(lastEditTime) WHEN 'text' THEN CAST(lastEditTime AS BLOB) ELSE lastEditTime END FROM Track WHERE id=? AND path=?`,
				upd.row.id, upd.row.path,
			).Scan(&lastEditType, &lastEditTime); err == nil {
				if b, ok := lastEditTime.([]byte); ok && lastEditType == "text" {
					lastEditTime = string(b)
				} else if b, ok := lastEditTime.([]byte); ok && lastEditType == "blob" && b == nil {
					lastEditTime = []byte{}
				}
				if _, err = tx.Exec(`UPDATE Track SET path=?, filename=? WHERE id=? AND path=?`, upd.newPath, upd.newName, upd.row.id, upd.row.path); err == nil {
					_, err = tx.Exec(`UPDATE Track SET lastEditTime=? WHERE id=?`, lastEditTime, upd.row.id)
				}
			}
		}
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				err = nil
				continue
			}
			_ = tx.Rollback()
			return 0, batch[i].row.id, err
		}
		batchFixed++
	}
	if batchFixed == 0 {
		_ = tx.Rollback()
		return 0, lastUncommittedID, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, batch[0].row.id, err
	}
	r.PathsNormalized += batchFixed
	if normalizeTrackPathBatch != nil {
		normalizeTrackPathBatch()
	}
	return batchFixed, lastUncommittedID, nil
}

func normalizeTrackPaths(ctx context.Context, db *sql.DB, mount string, r *Report) (int, error) {
	start := time.Now()
	libUUID := queryLibraryUUID(ctx, db)
	cursorID := readCleanProbeCursor(mount, libUUID)
	if ctx.Err() != nil {
		return countRemainingLocalTracksRange(db, cursorID, 0), nil
	}

	bridgeManaged, err := bridgeManagedTracks(ctx, mount)
	if err != nil {
		if ctx.Err() != nil {
			return countRemainingLocalTracksRange(db, cursorID, 0), nil
		}
		// Unreadable bridge state: conservatively leave all tracks untouched.
		return 0, nil
	}

	probeCtx := ctx
	var cancelProbe context.CancelFunc
	dl, hasDeadline := ctx.Deadline()
	if hasDeadline {
		left := time.Until(dl)
		if left <= 5*time.Millisecond {
			return countRemainingLocalTracksRange(db, cursorID, 0), nil
		}
		// Reserve a small tail margin before the outer deadline so the
		// remaining-count query and cursor save finish inside the caller budget.
		margin := min(25*time.Millisecond, left/4)
		budget := left - margin
		if budget <= 0 {
			return countRemainingLocalTracksRange(db, cursorID, 0), nil
		}
		probeCtx, cancelProbe = context.WithTimeout(ctx, budget)
		defer cancelProbe()
	}

	cache := newMountDirCache(mount)
	var ownerIdx *pathOwnershipIndex
	ensureOwnerIdx := func(opCtx context.Context) (*pathOwnershipIndex, error) {
		if ownerIdx == nil {
			ownerIdx = newPathOwnershipIndex(db, mount, cache, bridgeManaged)
		}
		return ownerIdx, nil
	}

	rem, lastSafeID, completed, err := runTrackPathSweep(
		ctx, probeCtx, db, mount, cache, bridgeManaged, ensureOwnerIdx,
		cursorID, 0, hasDeadline, start, r,
	)
	if err != nil {
		return rem, err
	}
	if completed {
		if !hasDeadline && cursorID > 0 && ctx.Err() == nil {
			// An unlimited pass verifies the earlier prefix once the resumed
			// tail completes, ensuring full-library coverage.
			prefixRem, _, _, perr := runTrackPathSweep(
				ctx, probeCtx, db, mount, cache, bridgeManaged, ensureOwnerIdx,
				0, cursorID, false, start, r,
			)
			if perr != nil {
				return prefixRem, perr
			}
			if prefixRem > 0 {
				return prefixRem, nil
			}
		}
		removeCleanProbeCursor(mount)
		return 0, nil
	}
	if lastSafeID > cursorID {
		writeCleanProbeCursor(mount, libUUID, lastSafeID)
	}
	return rem, nil
}

func runTrackPathSweep(
	outerCtx context.Context,
	probeCtx context.Context,
	db *sql.DB,
	mount string,
	cache *mountDirCache,
	bridgeManaged map[int64]bool,
	ensureOwnerIdx func(context.Context) (*pathOwnershipIndex, error),
	afterID int64,
	maxID int64,
	enforceIdleCap bool,
	start time.Time,
	r *Report,
) (remaining int, lastSafeID int64, completed bool, err error) {
	lastQueryID := afterID
	prevChunkMaxID := afterID
	lastSafeID = afterID
	lastRepairAt := start
	batch := make([]pendingTrackPathUpdate, 0, 64)

	idleCapExceeded := func() bool {
		if !enforceIdleCap {
			return false
		}
		now := time.Now()
		if r.PathsNormalized == 0 {
			return now.Sub(start) >= cleanPathProbeCap
		}
		// Keep a fast-repair mount well under db.go's 1 s base-heal deferral
		// threshold when only clean rows remain after a small batch of fixes.
		return now.Sub(lastRepairAt) >= cleanPathProbeCap && now.Sub(start) >= 750*time.Millisecond
	}

	flushOnSubBudget := func() (int64, error) {
		if len(batch) == 0 {
			return lastSafeID, nil
		}
		if outerCtx.Err() != nil {
			// Outer context expired: do not write after deadline; keep cursor
			// before the first uncommitted row.
			return lastSafeID, nil
		}
		lastIDInBatch := batch[len(batch)-1].row.id
		committed, uncommID, ferr := commitTrackPathBatch(outerCtx, db, batch, r)
		batch = batch[:0]
		if committed > 0 {
			lastRepairAt = time.Now()
		}
		if ferr != nil {
			return lastSafeID, ferr
		}
		if uncommID > 0 {
			return uncommID - 1, nil
		}
		return lastIDInBatch, nil
	}

	for {
		if probeCtx.Err() != nil || idleCapExceeded() {
			safe, ferr := flushOnSubBudget()
			return countRemainingLocalTracksRange(db, safe, maxID), safe, false, ferr
		}

		var rows *sql.Rows
		var qerr error
		if maxID > 0 {
			rows, qerr = db.QueryContext(probeCtx,
				`SELECT id, path FROM Track WHERE id > ? AND id <= ? AND path IS NOT NULL AND path <> '' AND COALESCE(streamingSource, '') = '' ORDER BY id LIMIT ?`,
				lastQueryID, maxID, pathScanChunkSize)
		} else {
			rows, qerr = db.QueryContext(probeCtx,
				`SELECT id, path FROM Track WHERE id > ? AND path IS NOT NULL AND path <> '' AND COALESCE(streamingSource, '') = '' ORDER BY id LIMIT ?`,
				lastQueryID, pathScanChunkSize)
		}
		if qerr != nil {
			if probeCtx.Err() != nil {
				safe, ferr := flushOnSubBudget()
				return countRemainingLocalTracksRange(db, safe, maxID), safe, false, ferr
			}
			return 0, lastSafeID, false, qerr
		}

		chunk := make([]scanTrackRow, 0, pathScanChunkSize)
		prevChunkMaxID = lastQueryID
		for rows.Next() {
			var sr scanTrackRow
			if err := rows.Scan(&sr.id, &sr.path); err != nil {
				rows.Close()
				return 0, lastSafeID, false, err
			}
			lastQueryID = sr.id
			chunk = append(chunk, sr)
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			if probeCtx.Err() != nil {
				safe, ferr := flushOnSubBudget()
				return countRemainingLocalTracksRange(db, safe, maxID), safe, false, ferr
			}
			return 0, lastSafeID, false, rerr
		}
		if len(chunk) == 0 {
			break
		}
		eof := len(chunk) < pathScanChunkSize

		for i, sr := range chunk {
			if probeCtx.Err() != nil || (i&63 == 0 && idleCapExceeded()) {
				safe, ferr := flushOnSubBudget()
				return remainingInChunkOrDB(db, chunk, i, eof && safe >= prevChunkMaxID, safe, maxID), safe, false, ferr
			}
			if bridgeManaged[sr.id] {
				if len(batch) == 0 {
					lastSafeID = sr.id
				}
				continue
			}

			// Fast path for every already-playable literal regular file (zero
			// ReadDir calls, zero SQLite writes, preserves literal backslashes
			// and healthy suspect-punctuation names like curly quotes).
			if _, isSym, exists := literalRegularOrSymlink(mount, sr.path); exists || isSym {
				if len(batch) == 0 {
					lastSafeID = sr.id
				}
				continue
			}

			row, ferr := loadTrackPathMeta(probeCtx, db, sr.id, sr.path)
			if ferr != nil {
				if probeCtx.Err() != nil {
					safe, cerr := flushOnSubBudget()
					return remainingInChunkOrDB(db, chunk, i, eof && safe >= prevChunkMaxID, safe, maxID), safe, false, cerr
				}
				return 0, lastSafeID, false, ferr
			}
			resolvedRel, candStat, ok, ferr := resolveRowCandidate(probeCtx, mount, cache, row)
			if ferr != nil {
				if probeCtx.Err() != nil {
					safe, cerr := flushOnSubBudget()
					return remainingInChunkOrDB(db, chunk, i, eof && safe >= prevChunkMaxID, safe, maxID), safe, false, cerr
				}
				return 0, lastSafeID, false, ferr
			}
			if !ok {
				if len(batch) == 0 {
					lastSafeID = sr.id
				}
				continue
			}
			newPath := mountRelToTrackPath(resolvedRel)
			ownerIdx, ferr := ensureOwnerIdx(probeCtx)
			if ferr != nil {
				if probeCtx.Err() != nil {
					safe, cerr := flushOnSubBudget()
					return remainingInChunkOrDB(db, chunk, i, eof && safe >= prevChunkMaxID, safe, maxID), safe, false, cerr
				}
				return 0, lastSafeID, false, ferr
			}
			allowed, ferr := ownerIdx.canClaim(probeCtx, row, resolvedRel, newPath, candStat)
			if ferr != nil {
				if probeCtx.Err() != nil {
					safe, cerr := flushOnSubBudget()
					return remainingInChunkOrDB(db, chunk, i, eof && safe >= prevChunkMaxID, safe, maxID), safe, false, cerr
				}
				return 0, lastSafeID, false, ferr
			}
			if !allowed {
				if len(batch) == 0 {
					lastSafeID = sr.id
				}
				continue
			}

			batch = append(batch, pendingTrackPathUpdate{
				row:     row,
				newPath: newPath,
				newName: path.Base(newPath),
			})
			if len(batch) == 64 {
				committed, uncommID, ferr := commitTrackPathBatch(outerCtx, db, batch, r)
				batch = batch[:0]
				if committed > 0 {
					lastRepairAt = time.Now()
				}
				if ferr != nil {
					return remainingInChunkOrDB(db, chunk, i+1, eof && lastSafeID >= prevChunkMaxID, lastSafeID, maxID), lastSafeID, false, ferr
				}
				if uncommID > 0 {
					lastSafeID = uncommID - 1
					return countRemainingLocalTracksRange(db, lastSafeID, maxID), lastSafeID, false, nil
				}
				lastSafeID = sr.id
				if probeCtx.Err() != nil {
					return remainingInChunkOrDB(db, chunk, i+1, eof && lastSafeID >= prevChunkMaxID, lastSafeID, maxID), lastSafeID, false, nil
				}
			}
		}

		if eof {
			break
		}
	}

	if len(batch) > 0 {
		safe, ferr := flushOnSubBudget()
		if ferr != nil {
			return countRemainingLocalTracksRange(db, safe, maxID), safe, false, ferr
		}
		if outerCtx.Err() != nil {
			return countRemainingLocalTracksRange(db, safe, maxID), safe, false, nil
		}
		lastSafeID = safe
	}
	if probeCtx.Err() != nil {
		return countRemainingLocalTracksRange(db, lastSafeID, maxID), lastSafeID, false, nil
	}
	return 0, lastSafeID, true, nil
}

func loadTrackPathMeta(ctx context.Context, db *sql.DB, id int64, p string) (trackPathRow, error) {
	row := trackPathRow{id: id, path: p}
	err := db.QueryRowContext(ctx, `SELECT COALESCE(filename, ''), COALESCE(fileBytes, 0) FROM Track WHERE id=?`, id).Scan(&row.filename, &row.fileBytes)
	return row, err
}

func remainingInChunkOrDB(
	db *sql.DB,
	chunk []scanTrackRow,
	nextIdx int,
	eof bool,
	lastSafeID int64,
	maxID int64,
) int {
	if eof {
		n := 0
		for _, row := range chunk {
			if row.id > lastSafeID {
				n++
			}
		}
		if n > 0 || nextIdx >= len(chunk) {
			return n
		}
	}
	return countRemainingLocalTracksRange(db, lastSafeID, maxID)
}

func countRemainingLocalTracksRange(db *sql.DB, afterID, maxID int64) int {
	qctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var total int
	var err error
	if maxID > 0 {
		err = db.QueryRowContext(qctx,
			`SELECT count(*) FROM Track WHERE id > ? AND id <= ?`,
			afterID, maxID,
		).Scan(&total)
	} else {
		err = db.QueryRowContext(qctx,
			`SELECT count(*) FROM Track WHERE id > ?`,
			afterID,
		).Scan(&total)
	}
	if err != nil {
		return 1
	}
	return total
}

func queryLibraryUUID(ctx context.Context, db *sql.DB) string {
	var u string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(uuid, '') FROM Information LIMIT 1`).Scan(&u); err != nil {
		return ""
	}
	return strings.TrimSpace(u)
}

func cleanProbeCursorPath(mount string) string {
	return filepath.Join(mount, "Engine Library", "Database2", pathProbeCursorFile)
}

func readCleanProbeCursor(mount, wantUUID string) int64 {
	p := cleanProbeCursorPath(mount)
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 || fields[0] != pathProbeCursorVersion {
		return 0
	}
	var lastID int64
	var gotUUID string
	for _, f := range fields[1:] {
		if v, ok := strings.CutPrefix(f, "last_id="); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return 0
			}
			lastID = n
		} else if v, ok := strings.CutPrefix(f, "uuid="); ok {
			gotUUID = v
		}
	}
	if wantUUID != "" && gotUUID != "" && gotUUID != wantUUID {
		return 0
	}
	return lastID
}

func writeCleanProbeCursor(mount, libUUID string, lastID int64) {
	if lastID <= 0 {
		return
	}
	p := cleanProbeCursorPath(mount)
	if st, err := os.Lstat(p); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		return
	}
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, ".enginex-path-probe-cursor-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	content := fmt.Sprintf("%s last_id=%d", pathProbeCursorVersion, lastID)
	if libUUID != "" && !strings.ContainsAny(libUUID, " \t\r\n") {
		content += " uuid=" + libUUID
	}
	content += "\n"
	if _, err = tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return
	}
	if err = tmp.Close(); err != nil {
		return
	}
	_ = os.Chmod(tmpName, 0o644)
	_ = os.Rename(tmpName, p)
}

func removeCleanProbeCursor(mount string) {
	p := cleanProbeCursorPath(mount)
	if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
		_ = os.Remove(p)
	}
}

// resolveRowCandidate checks whether row's original literal path is already
// playable (or a symlink), and if missing resolves a unique on-disk regular
// file matching Track.fileBytes (when positive).
func resolveRowCandidate(
	ctx context.Context,
	mount string,
	cache *mountDirCache,
	row trackPathRow,
) (resolvedRel string, candStat os.FileInfo, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", nil, false, err
	}
	if _, isSym, exists := literalRegularOrSymlink(mount, row.path); exists || isSym {
		return "", nil, false, nil
	}

	hasBackslash := strings.ContainsRune(row.path, '\\')
	if !hasBackslash {
		litParts, litOK := splitLiteralTrackMountParts(row.path)
		if !litOK {
			return "", nil, false, nil
		}
		rel, status, rerr := cache.resolveOnDisk(ctx, litParts)
		if rerr != nil || status != pathMatchFound {
			return "", nil, false, rerr
		}
		resolvedRel = rel
	} else {
		var litRel, slashRel string
		var litStatus, slashStatus pathMatchStatus
		if litParts, litOK := splitLiteralTrackMountParts(row.path); litOK {
			var rerr error
			litRel, litStatus, rerr = cache.resolveOnDisk(ctx, litParts)
			if rerr != nil {
				return "", nil, false, rerr
			}
			if litStatus == pathMatchAmbiguous {
				return "", nil, false, nil
			}
		}
		if slashParts, slashOK := splitSlashTrackMountParts(row.path); slashOK {
			var rerr error
			slashRel, slashStatus, rerr = cache.resolveOnDisk(ctx, slashParts)
			if rerr != nil {
				return "", nil, false, rerr
			}
			if slashStatus == pathMatchAmbiguous {
				return "", nil, false, nil
			}
		}
		switch {
		case litStatus == pathMatchFound && slashStatus == pathMatchFound:
			if litRel != slashRel {
				return "", nil, false, nil
			}
			resolvedRel = litRel
		case litStatus == pathMatchFound:
			resolvedRel = litRel
		case slashStatus == pathMatchFound:
			resolvedRel = slashRel
		default:
			return "", nil, false, nil
		}
	}

	newPath := mountRelToTrackPath(resolvedRel)
	if newPath == row.path {
		return "", nil, false, nil
	}
	fullCand, pathOK := trackPath(mount, newPath)
	if !pathOK {
		return "", nil, false, nil
	}
	st, serr := os.Lstat(fullCand)
	if serr != nil || !st.Mode().IsRegular() {
		return "", nil, false, nil
	}
	if row.fileBytes > 0 && st.Size() != row.fileBytes {
		return "", nil, false, nil
	}
	return resolvedRel, st, true, nil
}

// literalRegularOrSymlink checks the original literal path inside mount
// (preserving any literal '\' characters) with the existing mount path-safety
// checks.
func literalRegularOrSymlink(mount, raw string) (info os.FileInfo, isSymlink bool, isRegular bool) {
	if raw == "" || raw == "." {
		return nil, false, false
	}
	if rel, ok := fastRelativeMountPath(raw); ok {
		full := mount + string(filepath.Separator) + rel
		st, err := os.Lstat(full)
		if err != nil {
			return nil, false, false
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return st, true, false
		}
		return st, false, st.Mode().IsRegular()
	}

	litRel, ok := cleanLiteralMountRel(raw)
	if !ok {
		return nil, false, false
	}
	full := filepath.Join(mount, filepath.FromSlash(litRel))
	rel, err := filepath.Rel(mount, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false, false
	}
	st, err := os.Lstat(full)
	if err != nil {
		return nil, false, false
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		return st, true, false
	}
	return st, false, st.Mode().IsRegular()
}

// fastRelativeMountPath returns the mount-relative path for standard clean
// "../Music/..." strings without allocating slices or calling path.Clean.
func fastRelativeMountPath(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "../") {
		return "", false
	}
	rel := raw[3:]
	if rel == "" || rel[0] == '/' || rel == "." ||
		strings.HasPrefix(rel, "./") ||
		strings.HasSuffix(rel, "/") ||
		strings.HasSuffix(rel, "/.") ||
		strings.Contains(rel, "..") ||
		strings.Contains(rel, "//") ||
		strings.Contains(rel, "/./") ||
		strings.ContainsRune(rel, '\\') ||
		strings.ContainsRune(rel, 0) {
		return "", false
	}
	return rel, true
}

// splitStemExt splits a filename into its stem and extension text WITHOUT the
// dot separator, plus hasDot. Dotfiles like ".hidden" have hasDot == false.
func splitStemExt(name string) (stem, extText string, hasDot bool) {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return name, "", false
	}
	return name[:dot], name[dot+1:], true
}

func isTrimSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\u00a0' || unicode.IsSpace(r)
}

func isFATIllegalRune(r rune) bool {
	switch r {
	case ':', '*', '?', '"', '<', '>', '|', '\\':
		return true
	}
	return false
}

func containsIllegalOrSmartChar(s string) bool {
	for _, r := range s {
		if isFATIllegalRune(r) {
			return true
		}
		switch r {
		case '‘', '’', '“', '”':
			return true
		}
	}
	return false
}

func hasWindowsDrivePrefix(s string) bool {
	return len(s) >= 2 && s[1] == ':' && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z'))
}

func cleanLiteralMountRel(raw string) (string, bool) {
	if rel, ok := fastRelativeMountPath(raw); ok {
		return rel, true
	}
	if raw == "" || raw == "." || strings.ContainsRune(raw, 0) {
		return "", false
	}
	if strings.HasPrefix(raw, "/") || hasWindowsDrivePrefix(raw) {
		return "", false
	}
	cleaned := path.Clean("Engine Library/" + raw)
	if cleaned == "." || cleaned == ".." || cleaned == "Engine Library" || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

func cleanSlashMountRel(raw string) (string, bool) {
	if raw == "" || raw == "." || strings.ContainsRune(raw, 0) {
		return "", false
	}
	slash := strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(slash, "/") || hasWindowsDrivePrefix(slash) {
		return "", false
	}
	cleaned := path.Clean("Engine Library/" + slash)
	if cleaned == "." || cleaned == ".." || cleaned == "Engine Library" || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

// splitLiteralTrackMountParts validates and cleans raw relative to
// "Engine Library/" WITHOUT rewriting literal backslashes to slashes.
func splitLiteralTrackMountParts(raw string) ([]string, bool) {
	cleaned, ok := cleanLiteralMountRel(raw)
	if !ok {
		return nil, false
	}
	return strings.Split(cleaned, "/"), true
}

// splitSlashTrackMountParts interprets Windows backslashes as '/' separators
// and validates that the cleaned path stays inside the mount.
func splitSlashTrackMountParts(raw string) ([]string, bool) {
	cleaned, ok := cleanSlashMountRel(raw)
	if !ok {
		return nil, false
	}
	return strings.Split(cleaned, "/"), true
}

func mountRelToTrackPath(mountRel string) string {
	if strings.HasPrefix(mountRel, "Engine Library/") {
		return strings.TrimPrefix(mountRel, "Engine Library/")
	}
	return "../" + mountRel
}

func trimPathComponent(name string, isFile bool) string {
	s := norm.NFC.String(name)
	s = strings.TrimLeftFunc(s, isTrimSpace)
	s = strings.TrimRightFunc(s, func(r rune) bool { return isTrimSpace(r) || r == '.' })
	if isFile {
		stem, extText, hasDot := splitStemExt(s)
		stem = strings.TrimRightFunc(stem, func(r rune) bool { return isTrimSpace(r) || r == '.' })
		if stem == "" {
			return ""
		}
		if !hasDot {
			return stem
		}
		extText = strings.TrimFunc(extText, isTrimSpace)
		if extText == "" {
			return stem
		}
		return stem + "." + extText
	}
	return s
}

func foldPathComponent(s string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(s)))
}

// fuzzyMountRelKey maps a cleaned mount-relative path ("Music/Artist/Song.mp3")
// to a coarse canonical key shared by every Track row that could own or
// fuzzy-match the same on-disk file, using an ASCII fast path to stay fast on
// 100k+ track libraries.
func fuzzyMountRelKey(mountRel string) string {
	if mountRel == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(mountRel))
	start := 0
	for {
		slash := strings.IndexByte(mountRel[start:], '/')
		var end int
		var isFile bool
		if slash < 0 {
			end = len(mountRel)
			isFile = true
		} else {
			end = start + slash
		}
		if start > 0 {
			b.WriteByte(0)
		}
		appendFuzzyComponentKey(&b, mountRel[start:end], isFile)
		if isFile {
			break
		}
		start = end + 1
	}
	return b.String()
}

func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func trimASCIIPathComponent(s string, isFile bool) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	j := len(s)
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r' || s[j-1] == '\v' || s[j-1] == '\f' || s[j-1] == '.') {
		j--
	}
	s = s[i:j]
	if !isFile {
		return s
	}
	stem, extText, hasDot := splitStemExt(s)
	k := len(stem)
	for k > 0 && (stem[k-1] == ' ' || stem[k-1] == '\t' || stem[k-1] == '\n' || stem[k-1] == '\r' || stem[k-1] == '\v' || stem[k-1] == '\f' || stem[k-1] == '.') {
		k--
	}
	stem = stem[:k]
	if stem == "" {
		return ""
	}
	if !hasDot {
		return stem
	}
	a, c := 0, len(extText)
	for a < c && (extText[a] == ' ' || extText[a] == '\t' || extText[a] == '\n' || extText[a] == '\r' || extText[a] == '\v' || extText[a] == '\f') {
		a++
	}
	for c > a && (extText[c-1] == ' ' || extText[c-1] == '\t' || extText[c-1] == '\n' || extText[c-1] == '\r' || extText[c-1] == '\v' || extText[c-1] == '\f') {
		c--
	}
	extText = extText[a:c]
	if extText == "" {
		return stem
	}
	if len(stem)+1+len(extText) == len(s) && stem == s[:len(stem)] && extText == s[len(stem)+1:] {
		return s
	}
	return stem + "." + extText
}

func appendFuzzyComponentKey(dst *strings.Builder, name string, isFile bool) {
	if isASCIIString(name) {
		s := trimASCIIPathComponent(name, isFile)
		if s == "" {
			return
		}
		var buf [128]byte
		out := buf[:0]
		if len(s) > len(buf) {
			out = make([]byte, 0, len(s))
		}
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch c {
			case ':', '*', '?', '"', '<', '>', '|', '\\', '_', '-':
				continue
			default:
				if c >= 'A' && c <= 'Z' {
					c += 'a' - 'A'
				}
				out = append(out, c)
			}
		}
		dst.WriteString(trimASCIIPathComponent(string(out), isFile))
		return
	}

	s := trimPathComponent(name, isFile)
	if s == "" {
		return
	}
	s = foldPathComponent(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '‘', '’':
			b.WriteByte('\'')
		case '“', '”', '_', '-':
			continue
		default:
			if isFATIllegalRune(r) {
				continue
			}
			b.WriteRune(r)
		}
	}
	dst.WriteString(trimPathComponent(b.String(), isFile))
}

func sanitizePathForms(name string, isFile bool) (underscoreForm, dashForm, strippedForm string) {
	nfc := norm.NFC.String(name)
	var bUnder, bDash, bStrip strings.Builder
	bUnder.Grow(len(nfc))
	bDash.Grow(len(nfc))
	bStrip.Grow(len(nfc))
	for _, r := range nfc {
		switch r {
		case '‘', '’':
			bUnder.WriteByte('\'')
			bDash.WriteByte('\'')
			bStrip.WriteByte('\'')
		case '“', '”':
			bUnder.WriteByte('_')
			bDash.WriteByte('-')
			// stripped omits double-quote
		default:
			if isFATIllegalRune(r) {
				bUnder.WriteByte('_')
				bDash.WriteByte('-')
			} else {
				bUnder.WriteRune(r)
				bDash.WriteRune(r)
				bStrip.WriteRune(r)
			}
		}
	}
	if uTrim := trimPathComponent(bUnder.String(), isFile); uTrim != "" {
		underscoreForm = foldPathComponent(uTrim)
	}
	if dTrim := trimPathComponent(bDash.String(), isFile); dTrim != "" {
		dashForm = foldPathComponent(dTrim)
	}
	if sTrim := trimPathComponent(bStrip.String(), isFile); sTrim != "" {
		strippedForm = foldPathComponent(sTrim)
	}
	return underscoreForm, dashForm, strippedForm
}

func matchPathComponent(
	ctx context.Context,
	want string,
	entries []pathDirEntry,
	byName map[string]pathDirEntry,
	isFile, skipExact bool,
) (pathDirEntry, pathMatchStatus, error) {
	if err := ctx.Err(); err != nil {
		return pathDirEntry{}, pathMatchAbsent, err
	}
	typeOK := func(ent pathDirEntry) bool {
		if ent.isSymlink {
			return false
		}
		if isFile {
			return ent.isRegular
		}
		return ent.isDir
	}
	candOK := func(ent pathDirEntry) bool {
		if skipExact && ent.name == want {
			return false
		}
		return typeOK(ent)
	}

	// Tier 0: exact byte match. Never bypass an exact-name symlink.
	if !skipExact {
		if ent, ok := byName[want]; ok {
			if ent.isSymlink {
				return pathDirEntry{}, pathMatchAmbiguous, nil
			}
			if typeOK(ent) {
				return ent, pathMatchFound, nil
			}
		}
	}

	wantIsNFC := norm.NFC.IsNormalString(want)
	nfcOK := func(ent pathDirEntry) bool {
		// Never degrade an already-NFC DB component to an NFD disk entry
		// (preserves the #322 NFC contract).
		if wantIsNFC && !norm.NFC.IsNormalString(ent.name) {
			return false
		}
		return true
	}

	// Tier 1: exact NFC match for non-NFC DB entries (#322).
	if !wantIsNFC {
		wantNFC := norm.NFC.String(want)
		if ent, ok := byName[wantNFC]; ok {
			if ent.isSymlink && (!skipExact || ent.name != want) {
				return pathDirEntry{}, pathMatchAmbiguous, nil
			}
			if candOK(ent) {
				return ent, pathMatchFound, nil
			}
		}
	}

	wantTrim := trimPathComponent(want, isFile)
	if wantTrim == "" {
		return pathDirEntry{}, pathMatchAbsent, nil
	}
	wantFold := foldPathComponent(wantTrim)
	_, wantExt, _ := splitStemExt(wantFold)

	// Tier 2: trimmed NFC match (leading/trailing spaces and trailing dots).
	var t2 []pathDirEntry
	for i, ent := range entries {
		if i&255 == 0 && ctx.Err() != nil {
			return pathDirEntry{}, pathMatchAbsent, ctx.Err()
		}
		if !candOK(ent) || !nfcOK(ent) {
			continue
		}
		if trimPathComponent(ent.name, isFile) == wantTrim {
			t2 = append(t2, ent)
		}
	}
	if len(t2) == 1 {
		return t2[0], pathMatchFound, nil
	}
	if len(t2) > 1 {
		return pathDirEntry{}, pathMatchAmbiguous, nil
	}

	// Tier 3: case-folded trimmed NFC match.
	var t3 []pathDirEntry
	for i, ent := range entries {
		if i&255 == 0 && ctx.Err() != nil {
			return pathDirEntry{}, pathMatchAbsent, ctx.Err()
		}
		if !candOK(ent) || !nfcOK(ent) {
			continue
		}
		entTrim := trimPathComponent(ent.name, isFile)
		if entTrim != "" && foldPathComponent(entTrim) == wantFold {
			t3 = append(t3, ent)
		}
	}
	if len(t3) == 1 {
		return t3[0], pathMatchFound, nil
	}
	if len(t3) > 1 {
		return pathDirEntry{}, pathMatchAmbiguous, nil
	}

	// Tier 4: FAT32/exFAT sanitized character match (underscore, dash, or
	// stripped, plus smart quotes normalized) — all variants share one tier so
	// if both "Track_.mp3" and "Track-.mp3" (or "Track.mp3") exist for
	// "Track?.mp3", the match is rejected as ambiguous.
	if !containsIllegalOrSmartChar(want) {
		return pathDirEntry{}, pathMatchAbsent, nil
	}
	wantUnder, wantDash, wantStrip := sanitizePathForms(want, isFile)
	var t4 []pathDirEntry
	for i, ent := range entries {
		if i&255 == 0 && ctx.Err() != nil {
			return pathDirEntry{}, pathMatchAbsent, ctx.Err()
		}
		if !candOK(ent) || !nfcOK(ent) {
			continue
		}
		entTrim := trimPathComponent(ent.name, isFile)
		if entTrim == "" {
			continue
		}
		entFold := foldPathComponent(entTrim)
		if isFile {
			_, entExt, _ := splitStemExt(entFold)
			if entExt != wantExt {
				continue
			}
		}
		if (wantUnder != "" && entFold == wantUnder) ||
			(wantDash != "" && entFold == wantDash) ||
			(wantStrip != "" && entFold == wantStrip) {
			t4 = append(t4, ent)
		}
	}
	if len(t4) == 1 {
		return t4[0], pathMatchFound, nil
	}
	if len(t4) > 1 {
		return pathDirEntry{}, pathMatchAmbiguous, nil
	}
	return pathDirEntry{}, pathMatchAbsent, nil
}

func bridgeManagedTracks(ctx context.Context, mount string) (map[int64]bool, error) {
	managed := map[int64]bool{}
	if err := ctx.Err(); err != nil {
		return managed, err
	}
	p := filepath.Join(mount, "Engine Library", "Database2", ".rb-bridge-state.db")
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return managed, nil
	} else if err != nil {
		return managed, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p}).String()+"?mode=ro")
	if err != nil {
		return managed, err
	}
	defer db.Close()
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='BridgeState'`).Scan(&exists); err != nil {
		return managed, err
	}
	if exists == 0 {
		return managed, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT trackId FROM BridgeState`)
	if err != nil {
		return managed, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return managed, err
		}
		managed[id] = true
	}
	return managed, rows.Err()
}
