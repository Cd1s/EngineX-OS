package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type trackPathRow struct {
	id       int64
	path     string
	filename string
}

type pathDirEntry struct {
	name      string
	isDir     bool
	isRegular bool
	isSymlink bool
}

// cleanPathProbeCap bounds how long mount-time path healing spends probing
// clean-looking Track.path rows when the caller provided a deadline (e.g. the
// 20 s edisksd mount hook), so healthy large libraries never stall insert or
// starve base heal.
const cleanPathProbeCap = 500 * time.Millisecond

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

func (c *mountDirCache) readDir(relDir string) ([]pathDirEntry, map[string]pathDirEntry, bool) {
	if entries, ok := c.dirs[relDir]; ok {
		return entries, c.exact[relDir], entries != nil
	}
	full := c.mount
	if relDir != "" {
		full = filepath.Join(c.mount, filepath.FromSlash(relDir))
	}
	raw, err := os.ReadDir(full)
	if err != nil {
		c.dirs[relDir] = nil
		c.exact[relDir] = nil
		return nil, nil, false
	}
	entries := make([]pathDirEntry, 0, len(raw))
	byName := make(map[string]pathDirEntry, len(raw))
	for _, ent := range raw {
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
	c.dirs[relDir] = entries
	c.exact[relDir] = byName
	return entries, byName, true
}

// exactRegularOnDisk checks whether every component of mountRel exists
// byte-for-byte in directory entries with no symlinks along the path.
func (c *mountDirCache) exactRegularOnDisk(parts []string) bool {
	cur := ""
	for i, part := range parts {
		_, byName, ok := c.readDir(cur)
		if !ok {
			return false
		}
		ent, found := byName[part]
		if !found || ent.isSymlink {
			return false
		}
		isFile := i == len(parts)-1
		if isFile {
			return ent.isRegular
		}
		if !ent.isDir {
			return false
		}
		if cur == "" {
			cur = ent.name
		} else {
			cur = cur + "/" + ent.name
		}
	}
	return false
}

// resolveOnDisk walks mountRel component-by-component from mount root, matching
// missing segments created by macOS / exFAT / FAT32 sync mismatches while
// refusing symlinks, ambiguous matches, and NFC-to-NFD degradation.
func (c *mountDirCache) resolveOnDisk(parts []string) (string, bool) {
	resolved, ok := c.resolveFrom("", parts)
	if !ok {
		return "", false
	}
	return strings.Join(resolved, "/"), true
}

func (c *mountDirCache) resolveFrom(cur string, parts []string) ([]string, bool) {
	entries, byName, ok := c.readDir(cur)
	if !ok {
		return nil, false
	}
	part := parts[0]
	isFile := len(parts) == 1
	if isFile {
		ent, matched := matchPathComponent(part, entries, byName, true, false)
		if !matched {
			return nil, false
		}
		return []string{ent.name}, true
	}

	nextCur := func(name string) string {
		if cur == "" {
			return name
		}
		return cur + "/" + name
	}

	// Prefer exact directory component first; never bypass an exact-name
	// symlink. If an exact real directory does not contain the target file
	// (e.g. an empty "Artist " folder sits beside synced "Artist"), fall back
	// to a unique non-exact directory match.
	if ent, found := byName[part]; found {
		if ent.isSymlink {
			return nil, false
		}
		if ent.isDir {
			if tail, ok := c.resolveFrom(nextCur(ent.name), parts[1:]); ok {
				return append([]string{ent.name}, tail...), true
			}
		}
	}
	ent, matched := matchPathComponent(part, entries, byName, false, true)
	if !matched {
		return nil, false
	}
	tail, ok := c.resolveFrom(nextCur(ent.name), parts[1:])
	if !ok {
		return nil, false
	}
	return append([]string{ent.name}, tail...), true
}

func normalizeTrackPaths(ctx context.Context, db *sql.DB, mount string, r *Report) (int, error) {
	start := time.Now()
	priority, clean, existingPaths, err := collectTrackPathRows(db)
	if err != nil {
		return 0, err
	}
	if len(priority) == 0 && len(clean) == 0 {
		return 0, nil
	}
	bridgeManaged, err := bridgeManagedTracks(mount)
	if err != nil {
		// Unreadable bridge state: conservatively leave all tracks untouched.
		return 0, nil
	}

	cache := newMountDirCache(mount)
	remaining, err := applyTrackPathPass(ctx, db, mount, cache, priority, bridgeManaged, existingPaths, true, r)
	if err != nil || remaining > 0 || ctx.Err() != nil || len(clean) == 0 {
		return remaining, err
	}

	probeCtx := ctx
	var cancelProbe context.CancelFunc
	if dl, ok := ctx.Deadline(); ok {
		left := time.Until(dl)
		if left <= 0 {
			return 0, nil
		}
		budget := left / 4
		if budget > cleanPathProbeCap {
			budget = cleanPathProbeCap
		}
		if r.PathsNormalized > 0 {
			// Keep a fast priority pass plus clean probe well under db.go's
			// 1 s base-heal deferral threshold.
			if headroom := 750*time.Millisecond - time.Since(start); headroom < budget {
				budget = headroom
			}
		}
		if budget <= 0 {
			return 0, nil
		}
		probeCtx, cancelProbe = context.WithTimeout(ctx, budget)
		defer cancelProbe()
	}

	_, err = applyTrackPathPass(probeCtx, db, mount, cache, clean, bridgeManaged, existingPaths, false, r)
	return 0, err
}

func collectTrackPathRows(db *sql.DB) (priority, clean []trackPathRow, existingPaths map[string]int64, err error) {
	rows, err := db.Query(`SELECT id, path, COALESCE(filename, ''), COALESCE(streamingSource, '') FROM Track WHERE path IS NOT NULL AND path <> '' ORDER BY id`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()

	existingPaths = make(map[string]int64)
	for rows.Next() {
		var row trackPathRow
		var streaming string
		if err := rows.Scan(&row.id, &row.path, &row.filename, &streaming); err != nil {
			return nil, nil, nil, err
		}
		if prev, dup := existingPaths[row.path]; dup && prev != row.id {
			existingPaths[row.path] = -1
		} else {
			existingPaths[row.path] = row.id
		}
		if streaming != "" {
			continue
		}
		if isSuspectTrackPath(row.path) {
			priority = append(priority, row)
		} else {
			clean = append(clean, row)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	return priority, clean, existingPaths, nil
}

func applyTrackPathPass(
	ctx context.Context,
	db *sql.DB,
	mount string,
	cache *mountDirCache,
	rows []trackPathRow,
	bridgeManaged map[int64]bool,
	existingPaths map[string]int64,
	suspectPass bool,
	r *Report,
) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	type pendingUpdate struct {
		row     trackPathRow
		newPath string
		newName string
	}

	commitBatch := func(batch []pendingUpdate) error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		batchFixed := 0
		for _, upd := range batch {
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
					continue
				}
				_ = tx.Rollback()
				return err
			}
			batchFixed++
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		r.PathsNormalized += batchFixed
		if batchFixed > 0 && normalizeTrackPathBatch != nil {
			normalizeTrackPathBatch()
		}
		return nil
	}

	batch := make([]pendingUpdate, 0, 64)
	for i, row := range rows {
		if ctx.Err() != nil {
			if err := commitBatch(batch); err != nil {
				return len(rows) - i, err
			}
			return len(rows) - i, nil
		}
		if bridgeManaged[row.id] {
			continue
		}
		parts, hasBackslash, ok := splitTrackMountParts(row.path)
		if !ok {
			continue
		}
		if !suspectPass && !hasBackslash {
			// Fast path for clean-looking rows: if the kernel already resolves
			// the path to a regular file (including case-insensitive exFAT/vfat),
			// Engine can open it without any ReadDir or m.db write.
			full := filepath.Join(mount, filepath.FromSlash(strings.Join(parts, "/")))
			if st, err := os.Lstat(full); err == nil && st.Mode().IsRegular() {
				continue
			}
		} else if !hasBackslash && cache.exactRegularOnDisk(parts) {
			continue
		}

		resolvedMountRel, found := cache.resolveOnDisk(parts)
		if !found {
			continue
		}
		newPath := mountRelToTrackPath(resolvedMountRel)
		if newPath == row.path {
			continue
		}
		if _, ok := trackPath(mount, newPath); !ok {
			continue
		}
		if other, taken := existingPaths[newPath]; taken && other != row.id {
			continue
		}
		existingPaths[newPath] = row.id

		batch = append(batch, pendingUpdate{
			row:     row,
			newPath: newPath,
			newName: path.Base(newPath),
		})
		if len(batch) == 64 {
			if err := commitBatch(batch); err != nil {
				return len(rows) - (i + 1), err
			}
			batch = batch[:0]
			if ctx.Err() != nil {
				return len(rows) - (i + 1), nil
			}
		}
	}
	if err := commitBatch(batch); err != nil {
		return 0, err
	}
	return 0, nil
}

// isSuspectTrackPath reports in memory (zero syscalls) whether a stored
// Track.path has markers of macOS / Windows / exFAT / FAT32 sync mismatch:
// non-NFC Unicode (#322), Windows backslashes, leading/trailing spaces or
// trailing periods on directories or filename stems, FAT/exFAT disallowed
// characters, or curly quotes.
func isSuspectTrackPath(p string) bool {
	if !norm.NFC.IsNormalString(p) {
		return true
	}
	if strings.ContainsRune(p, '\\') {
		return true
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		isFile := i == len(parts)-1
		if componentHasEdgeSpaceOrDot(part, isFile) || containsIllegalOrSmartChar(part) {
			return true
		}
	}
	return false
}

func componentHasEdgeSpaceOrDot(name string, isFile bool) bool {
	if hasLeadingSpace(name) || hasTrailingSpaceOrDot(name) {
		return true
	}
	if isFile {
		stem, ext := splitStemExt(name)
		return hasTrailingSpaceOrDot(stem) || hasLeadingSpace(ext)
	}
	return false
}

func splitStemExt(name string) (stem, ext string) {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return name, ""
	}
	return name[:dot], name[dot:]
}

func isTrimSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == ' ' || unicode.IsSpace(r)
}

func hasLeadingSpace(s string) bool {
	for _, r := range s {
		return isTrimSpace(r)
	}
	return false
}

func hasTrailingSpaceOrDot(s string) bool {
	var last rune
	has := false
	for _, r := range s {
		last = r
		has = true
	}
	return has && (isTrimSpace(last) || last == '.')
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

func splitTrackMountParts(raw string) (parts []string, hasBackslash bool, ok bool) {
	if raw == "" || raw == "." || strings.ContainsRune(raw, 0) {
		return nil, false, false
	}
	hasBackslash = strings.ContainsRune(raw, '\\')
	slash := strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(slash, "/") {
		return nil, hasBackslash, false
	}
	if len(slash) >= 2 && slash[1] == ':' && ((slash[0] >= 'a' && slash[0] <= 'z') || (slash[0] >= 'A' && slash[0] <= 'Z')) {
		return nil, hasBackslash, false
	}
	cleaned := path.Clean("Engine Library/" + slash)
	if cleaned == "." || cleaned == ".." || cleaned == "Engine Library" || strings.HasPrefix(cleaned, "../") {
		return nil, hasBackslash, false
	}
	return strings.Split(cleaned, "/"), hasBackslash, true
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
		stem, ext := splitStemExt(s)
		stem = strings.TrimRightFunc(stem, func(r rune) bool { return isTrimSpace(r) || r == '.' })
		if stem == "" {
			return ""
		}
		ext = strings.TrimFunc(ext, isTrimSpace)
		return stem + ext
	}
	return s
}

func foldPathComponent(s string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(s)))
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

func matchPathComponent(want string, entries []pathDirEntry, byName map[string]pathDirEntry, isFile, skipExact bool) (pathDirEntry, bool) {
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

	// Tier 0: exact byte match.
	if !skipExact {
		if ent, ok := byName[want]; ok && typeOK(ent) {
			return ent, true
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
		if ent, ok := byName[wantNFC]; ok && candOK(ent) {
			return ent, true
		}
	}

	wantTrim := trimPathComponent(want, isFile)
	if wantTrim == "" {
		return pathDirEntry{}, false
	}
	wantFold := foldPathComponent(wantTrim)
	_, wantExt := splitStemExt(wantFold)

	// Tier 2: trimmed NFC match (leading/trailing spaces and trailing dots).
	var t2 []pathDirEntry
	for _, ent := range entries {
		if !candOK(ent) || !nfcOK(ent) {
			continue
		}
		if trimPathComponent(ent.name, isFile) == wantTrim {
			t2 = append(t2, ent)
		}
	}
	if len(t2) == 1 {
		return t2[0], true
	}
	if len(t2) > 1 {
		return pathDirEntry{}, false
	}

	// Tier 3: case-folded trimmed NFC match.
	var t3 []pathDirEntry
	for _, ent := range entries {
		if !candOK(ent) || !nfcOK(ent) {
			continue
		}
		entTrim := trimPathComponent(ent.name, isFile)
		if entTrim != "" && foldPathComponent(entTrim) == wantFold {
			t3 = append(t3, ent)
		}
	}
	if len(t3) == 1 {
		return t3[0], true
	}
	if len(t3) > 1 {
		return pathDirEntry{}, false
	}

	// Tier 4: FAT32/exFAT sanitized character match (underscore, dash, or
	// stripped, plus smart quotes normalized) — all variants share one tier so
	// if both "Track_.mp3" and "Track.mp3" exist for "Track?.mp3", the match
	// is rejected as ambiguous.
	if !containsIllegalOrSmartChar(want) {
		return pathDirEntry{}, false
	}
	wantUnder, wantDash, wantStrip := sanitizePathForms(want, isFile)
	var t4 []pathDirEntry
	for _, ent := range entries {
		if !candOK(ent) || !nfcOK(ent) {
			continue
		}
		entTrim := trimPathComponent(ent.name, isFile)
		if entTrim == "" {
			continue
		}
		entFold := foldPathComponent(entTrim)
		if isFile {
			_, entExt := splitStemExt(entFold)
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
		return t4[0], true
	}
	return pathDirEntry{}, false
}

func bridgeManagedTracks(mount string) (map[int64]bool, error) {
	managed := map[int64]bool{}
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
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='BridgeState'`).Scan(&exists); err != nil {
		return managed, err
	}
	if exists == 0 {
		return managed, nil
	}
	rows, err := db.Query(`SELECT trackId FROM BridgeState`)
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
