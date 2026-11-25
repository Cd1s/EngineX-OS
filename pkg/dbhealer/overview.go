package dbhealer

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const overviewSampleCap = 16

var overviewMigrateBatch func()

type overviewTrackInfo struct {
	hasSPB bool
	spb    float64
}

type overviewFileEntry struct {
	id   int64
	name string
}

type overviewCandidate struct {
	name       string
	dir        string
	files      []overviewFileEntry
	clean      bool
	matchCount int
}

func validOverviewDirName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.HasPrefix(name, ".")
}

func validOverviewHeaderSPB(raw []byte) (float64, bool) {
	if len(raw) < 27 {
		return 0, false
	}
	bins := binary.BigEndian.Uint64(raw[0:8])
	if bins == 0 || bins > 1<<16 || binary.BigEndian.Uint64(raw[8:16]) != bins || uint64(len(raw)) != 24+3*bins+3 {
		return 0, false
	}
	spb := beFloat(raw[16:24])
	if !finite(spb) || spb <= 0 {
		return 0, false
	}
	return spb, true
}

func blobSamplesPerBin(blob []byte) (float64, bool) {
	if len(blob) <= 8 {
		return 0, false
	}
	raw, err := DecodeQCompress(blob)
	if err != nil {
		return 0, false
	}
	return validOverviewHeaderSPB(raw)
}

func readRGBSamplesPerBin(path string) (float64, bool) {
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Size() <= 8 || st.Size() > maxBlobSize {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return blobSamplesPerBin(b)
}

func parseRGBFileID(name string) (int64, bool) {
	if !strings.HasSuffix(name, ".rgb") {
		return 0, false
	}
	stem := strings.TrimSuffix(name, ".rgb")
	id, err := strconv.ParseInt(stem, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != stem {
		return 0, false
	}
	return id, true
}

func siblingDBUUIDs(ctx context.Context, db2Dir string) map[string]bool {
	uuids := map[string]bool{}
	entries, err := os.ReadDir(db2Dir)
	if err != nil {
		return uuids
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.Type()&os.ModeSymlink != 0 || !ent.Type().IsRegular() || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.EqualFold(name, "m.db") || !strings.HasSuffix(strings.ToLower(name), ".db") {
			continue
		}
		sdb, err := sql.Open("sqlite", fileURI(filepath.Join(db2Dir, name))+"?mode=ro&immutable=1&_pragma=query_only(1)")
		if err != nil {
			continue
		}
		sdb.SetMaxOpenConns(1)
		rows, qerr := sdb.QueryContext(ctx, `SELECT COALESCE(uuid,'') FROM Information`)
		if qerr == nil {
			for rows.Next() {
				var u string
				if rows.Scan(&u) == nil && validOverviewDirName(u) {
					uuids[u] = true
				}
			}
			_ = rows.Close()
		}
		_ = sdb.Close()
	}
	return uuids
}

func loadOverviewTrackState(ctx context.Context, db *sql.DB, curUUID string) (map[int64]overviewTrackInfo, map[string]bool, error) {
	tracks := map[int64]overviewTrackInfo{}
	provenance := map[string]bool{}

	rows, err := db.QueryContext(ctx, `SELECT t.id, p.overviewWaveFormData, COALESCE(t.originDatabaseUuid,'')
		FROM Track t
		LEFT JOIN PerformanceData p ON p.trackId = t.id
		WHERE t.isAnalyzed = 1 OR (p.overviewWaveFormData IS NOT NULL AND length(p.overviewWaveFormData) > 0)`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id int64
		var overview []byte
		var origin string
		if err := rows.Scan(&id, &overview, &origin); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		if validOverviewDirName(origin) && origin != curUUID {
			provenance[origin] = true
		}
		spb, ok := blobSamplesPerBin(overview)
		tracks[id] = overviewTrackInfo{hasSPB: ok, spb: spb}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, err
	}
	_ = rows.Close()

	if orows, oerr := db.QueryContext(ctx, `SELECT DISTINCT COALESCE(originDatabaseUuid,'') FROM Track WHERE originDatabaseUuid IS NOT NULL AND originDatabaseUuid <> ''`); oerr == nil {
		for orows.Next() {
			var u string
			if orows.Scan(&u) == nil && validOverviewDirName(u) && u != curUUID {
				provenance[u] = true
			}
		}
		_ = orows.Close()
	}
	if prows, perr := db.QueryContext(ctx, `SELECT DISTINCT COALESCE(databaseUuid,'') FROM PlaylistEntity WHERE databaseUuid IS NOT NULL AND databaseUuid <> ''`); perr == nil {
		for prows.Next() {
			var u string
			if prows.Scan(&u) == nil && validOverviewDirName(u) && u != curUUID {
				provenance[u] = true
			}
		}
		_ = prows.Close()
	}
	return tracks, provenance, nil
}

func existingCurRGB(curDir string) (present map[int64]bool, exists, empty, ok bool) {
	st, err := os.Lstat(curDir)
	if errors.Is(err, os.ErrNotExist) {
		return map[int64]bool{}, false, true, true
	}
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return nil, false, false, false
	}
	entries, err := os.ReadDir(curDir)
	if err != nil {
		return nil, true, false, false
	}
	if len(entries) == 0 {
		return map[int64]bool{}, true, true, true
	}
	present = make(map[int64]bool, len(entries))
	for _, ent := range entries {
		if ent.Type()&os.ModeSymlink != 0 || !ent.Type().IsRegular() {
			continue
		}
		id, valid := parseRGBFileID(ent.Name())
		if !valid {
			continue
		}
		if info, ierr := ent.Info(); ierr == nil && info.Mode().IsRegular() && info.Size() > 8 {
			present[id] = true
		}
	}
	return present, true, false, true
}

func validateCandidateDir(ctx context.Context, candName, candDir string, tracks map[int64]overviewTrackInfo, existingCur map[int64]bool) (overviewCandidate, bool) {
	st, err := os.Lstat(candDir)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return overviewCandidate{}, false
	}
	entries, err := os.ReadDir(candDir)
	if err != nil || len(entries) == 0 {
		return overviewCandidate{}, false
	}

	candClean := true
	var files []overviewFileEntry
	var withSPB []overviewFileEntry
	suppliesMissing := 0

	for _, ent := range entries {
		if ent.Type()&os.ModeSymlink != 0 || !ent.Type().IsRegular() {
			candClean = false
			continue
		}
		id, validID := parseRGBFileID(ent.Name())
		if !validID {
			candClean = false
			continue
		}
		info, ierr := ent.Info()
		if ierr != nil || !info.Mode().IsRegular() || info.Size() <= 8 {
			candClean = false
			continue
		}
		meta, known := tracks[id]
		if !known {
			candClean = false
			continue
		}
		fe := overviewFileEntry{id: id, name: ent.Name()}
		files = append(files, fe)
		if !existingCur[id] {
			suppliesMissing++
		}
		if meta.hasSPB {
			withSPB = append(withSPB, fe)
		}
	}
	if suppliesMissing == 0 || len(withSPB) == 0 {
		return overviewCandidate{}, false
	}

	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })
	sort.Slice(withSPB, func(i, j int) bool { return withSPB[i].id < withSPB[j].id })

	sampleCount := len(withSPB)
	if sampleCount > overviewSampleCap {
		sampleCount = overviewSampleCap
	}
	for i := 0; i < sampleCount; i++ {
		if ctx.Err() != nil {
			return overviewCandidate{}, false
		}
		idx := i
		if len(withSPB) > sampleCount {
			idx = i * (len(withSPB) - 1) / (sampleCount - 1)
		}
		f := withSPB[idx]
		gotSPB, ok := readRGBSamplesPerBin(filepath.Join(candDir, f.name))
		if !ok {
			return overviewCandidate{}, false
		}
		ownSPB := tracks[f.id].spb
		tol := math.Max(16, ownSPB*0.002)
		if math.Abs(gotSPB-ownSPB) > tol {
			return overviewCandidate{}, false
		}
	}

	return overviewCandidate{
		name:       candName,
		dir:        candDir,
		files:      files,
		clean:      candClean,
		matchCount: len(files),
	}, true
}

func selectOverviewWinner(valid []overviewCandidate, provenance map[string]bool, r *Report) (overviewCandidate, bool) {
	if len(valid) == 0 {
		return overviewCandidate{}, false
	}
	var prov []overviewCandidate
	for _, c := range valid {
		if provenance[c.name] {
			prov = append(prov, c)
		}
	}
	if len(prov) == 1 {
		return prov[0], true
	}
	pool := valid
	if len(prov) > 1 {
		pool = prov
	}
	best := 0
	for _, c := range pool {
		if c.matchCount > best {
			best = c.matchCount
		}
	}
	var leaders []overviewCandidate
	for _, c := range pool {
		if c.matchCount == best {
			leaders = append(leaders, c)
		}
	}
	if len(leaders) != 1 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("OverviewData: %d candidate directories tie at %d matching tracks; skipped migration", len(leaders), best))
		return overviewCandidate{}, false
	}
	return leaders[0], true
}

// reconcileOverviewDataDir recovers orphaned OverviewData/<oldUuid>/*.rgb
// sidecars created when Engine's Database::resetUuid changed Information.uuid
// without renaming OverviewData/<oldUuid> -> OverviewData/<curUuid>.
func reconcileOverviewDataDir(ctx context.Context, db *sql.DB, db2Dir string, r *Report) {
	if ctx.Err() != nil {
		return
	}
	var curUUID string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(uuid,'') FROM Information ORDER BY id LIMIT 1`).Scan(&curUUID); err != nil || !validOverviewDirName(curUUID) {
		return
	}

	overviewRoot := filepath.Join(db2Dir, "OverviewData")
	rootSt, err := os.Lstat(overviewRoot)
	if err != nil || rootSt.Mode()&os.ModeSymlink != 0 || !rootSt.IsDir() {
		return
	}
	entries, err := os.ReadDir(overviewRoot)
	if err != nil {
		return
	}
	var candidates []string
	for _, ent := range entries {
		name := ent.Name()
		if ent.Type()&os.ModeSymlink != 0 || !ent.IsDir() || !validOverviewDirName(name) || name == curUUID {
			continue
		}
		candidates = append(candidates, name)
	}
	// Fast O(1) path: no non-current UUID subdirs exist.
	if len(candidates) == 0 {
		return
	}

	siblings := siblingDBUUIDs(ctx, db2Dir)
	filtered := candidates[:0]
	for _, name := range candidates {
		if !siblings[name] {
			filtered = append(filtered, name)
		}
	}
	candidates = filtered
	if len(candidates) == 0 {
		return
	}

	curDir := filepath.Join(overviewRoot, curUUID)
	existingCur, curExists, curEmpty, curOK := existingCurRGB(curDir)
	if !curOK {
		return
	}

	tracks, provenance, err := loadOverviewTrackState(ctx, db, curUUID)
	if err != nil || len(tracks) == 0 {
		return
	}

	missing := 0
	for id := range tracks {
		if !existingCur[id] {
			missing++
		}
	}
	if missing == 0 {
		return
	}

	var valid []overviewCandidate
	for _, candName := range candidates {
		if ctx.Err() != nil {
			return
		}
		if c, ok := validateCandidateDir(ctx, candName, filepath.Join(overviewRoot, candName), tracks, existingCur); ok {
			valid = append(valid, c)
		}
	}

	winner, ok := selectOverviewWinner(valid, provenance, r)
	if !ok || ctx.Err() != nil {
		return
	}

	if curEmpty && winner.clean {
		removedEmpty := !curExists
		if curExists {
			if err := os.Remove(curDir); err == nil {
				removedEmpty = true
			}
		}
		if removedEmpty {
			if err := os.Rename(winner.dir, curDir); err == nil {
				r.RGBOverviewsMigrated += len(winner.files)
				log.Printf("dbhealer: migrated OverviewData/%s -> OverviewData/%s (%d files)", winner.name, curUUID, len(winner.files))
				return
			}
		}
	}

	if err := os.MkdirAll(curDir, 0o755); err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("OverviewData: mkdir %s: %v", curUUID, err))
		return
	}
	migrated := 0
	for i, f := range winner.files {
		if i%64 == 0 && ctx.Err() != nil {
			break
		}
		dst := filepath.Join(curDir, f.name)
		if _, err := os.Lstat(dst); err == nil || !errors.Is(err, os.ErrNotExist) {
			continue
		}
		src := filepath.Join(winner.dir, f.name)
		st, err := os.Lstat(src)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("OverviewData: rename %s: %v", f.name, err))
			continue
		}
		migrated++
		if i%64 == 63 && overviewMigrateBatch != nil {
			overviewMigrateBatch()
		}
	}
	r.RGBOverviewsMigrated += migrated
	if rem, err := os.ReadDir(winner.dir); err == nil && len(rem) == 0 {
		_ = os.Remove(winner.dir)
	}
	if migrated > 0 {
		log.Printf("dbhealer: merged %d OverviewData files from %s into %s", migrated, winner.name, curUUID)
	}
}
