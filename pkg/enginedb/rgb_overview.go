package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const rgbVerifiedName = ".rb-bridge-rgb-verified"

type rgbVerifiedEntry struct {
	Size    int64  `json:"s"`
	ModTime int64  `json:"m"`
	Path    string `json:"p"`
}

type rgbVerifiedCache struct {
	UUID       string                     `json:"uuid"`
	MDBSize    int64                      `json:"ms,omitempty"`
	MDBModTime int64                      `json:"mm,omitempty"`
	Files      map[int64]rgbVerifiedEntry `json:"files"`
}

func loadRGBVerifiedCacheFull(dir string) (rgbVerifiedCache, bool) {
	b, err := os.ReadFile(filepath.Join(dir, rgbVerifiedName))
	if err != nil {
		return rgbVerifiedCache{}, false
	}
	var c rgbVerifiedCache
	if err := json.Unmarshal(b, &c); err != nil || c.UUID == "" || strings.ContainsAny(c.UUID, `/\`) || c.UUID == "." || c.UUID == ".." || c.Files == nil {
		return rgbVerifiedCache{}, false
	}
	return c, true
}

func loadRGBVerifiedCache(dir, uuid string) map[int64]rgbVerifiedEntry {
	c, ok := loadRGBVerifiedCacheFull(dir)
	if !ok || c.UUID != uuid {
		return map[int64]rgbVerifiedEntry{}
	}
	return c.Files
}

func cleanMDBStat(library string) (size, modTime int64, ok bool) {
	if library == "" {
		return 0, 0, false
	}
	for _, side := range []string{"-wal", "-journal"} {
		if st, err := os.Stat(library + side); err == nil && st.Size() > 0 {
			return 0, 0, false
		}
	}
	st, err := os.Stat(library)
	if err != nil || !st.Mode().IsRegular() {
		return 0, 0, false
	}
	return st.Size(), st.ModTime().UnixNano(), true
}

func saveRGBVerifiedCache(dir, library, uuid string, files map[int64]rgbVerifiedEntry) {
	if uuid == "" || dir == "" {
		return
	}
	ms, mm, _ := cleanMDBStat(library)
	b, err := json.Marshal(rgbVerifiedCache{UUID: uuid, MDBSize: ms, MDBModTime: mm, Files: files})
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".rb-rgb-verified-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return
	}
	if err := f.Close(); err != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, rgbVerifiedName)); err != nil {
		return
	}
	ok = true
}

type rgbTrackMeta struct {
	path string
}

func loadRGBTrackMeta(ctx context.Context, db *sql.DB) (map[int64]rgbTrackMeta, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(path,'') FROM Track`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]rgbTrackMeta{}
	for rows.Next() {
		var id int64
		var m rgbTrackMeta
		if err := rows.Scan(&id, &m.path); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}

func loadTrackPaths(ctx context.Context, db *sql.DB) (map[int64]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(path,'') FROM Track`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

// Engine keeps each track's RGB overview outside m.db, in
// Database2/OverviewData/<library uuid>/<track id>.rgb (qCompress'd, the overview's
// 24-byte header and bins), and builds it only for a track that has none
// (GET_RGB_OVERVIEW_SIZE = 0). A file is never checked against its track again: one
// written for another track under the same id, or an empty one, is drawn for good
// (real stick 2026-11-19: Fons et Origo drew Centauri's, 26 of 123 files another
// song's, 140 empty). PruneRGBOverviews removes such files so Engine builds them anew
// at the next load. It runs only before Engine has the library.
//
// library is the m.db the volume has once this mount is done; previous, when not "",
// the m.db Engine wrote the files against (a publish replaces it): a file whose id
// names another audio file in library than in previous goes.
func pruneRGBVerifiedFast(ctx context.Context, dir, library string) (removed int, handled bool, err error) {
	ms, mm, ok := cleanMDBStat(library)
	if !ok || ms <= 0 {
		return 0, false, nil
	}
	c, ok := loadRGBVerifiedCacheFull(dir)
	if !ok || c.MDBSize != ms || c.MDBModTime != mm {
		return 0, false, nil
	}
	root := filepath.Join(dir, "OverviewData", c.UUID)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, nil
	}
	dirty := false
	seen := make(map[int64]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		id, perr := strconv.ParseInt(strings.TrimSuffix(name, ".rgb"), 10, 64)
		if !strings.HasSuffix(name, ".rgb") || perr != nil || id <= 0 || !e.Type().IsRegular() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, true, err
		}
		seen[id] = true
		p := filepath.Join(root, name)
		fi, infoErr := e.Info()
		if infoErr != nil {
			return removed, false, nil
		}
		if fi.Size() <= 8 {
			if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				return removed, true, rerr
			}
			delete(c.Files, id)
			dirty = true
			removed++
			continue
		}
		prev, exists := c.Files[id]
		if !exists || prev.Size != fi.Size() || prev.ModTime != fi.ModTime().UnixNano() {
			return removed, false, nil
		}
	}
	for id := range c.Files {
		if !seen[id] {
			delete(c.Files, id)
			dirty = true
		}
	}
	if dirty {
		saveRGBVerifiedCache(dir, library, c.UUID, c.Files)
	}
	return removed, true, nil
}

func PruneRGBOverviews(ctx context.Context, dir, library, previous string) (int, error) {
	if previous == "" {
		if removed, ok, err := pruneRGBVerifiedFast(ctx, dir, library); ok {
			return removed, err
		}
	}
	db, err := openReadOnly(library)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var uuid string
	if err = db.QueryRowContext(ctx, `SELECT uuid FROM Information LIMIT 1`).Scan(&uuid); err != nil {
		return 0, err
	}
	if uuid == "" || strings.ContainsAny(uuid, `/\`) || uuid == "." || uuid == ".." {
		return 0, errors.New("rgb overviews: bad library uuid")
	}
	root := filepath.Join(dir, "OverviewData", uuid)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, nil
	}
	tracks, err := loadRGBTrackMeta(ctx, db)
	if err != nil {
		return 0, err
	}
	var before *sql.DB
	var prevPaths map[int64]string
	if previous != "" {
		if _, err := os.Stat(previous); err == nil {
			if before, err = openReadOnly(previous); err != nil {
				return 0, err
			}
			defer before.Close()
			if prevPaths, err = loadTrackPaths(ctx, before); err != nil {
				return 0, err
			}
		}
	}
	cached := loadRGBVerifiedCache(dir, uuid)
	verified := make(map[int64]rgbVerifiedEntry, len(cached))
	for k, v := range cached {
		verified[k] = v
	}
	seen := make(map[int64]bool, len(entries))
	dirty := false
	removed := 0
	var runErr error
	for _, e := range entries {
		name := e.Name()
		id, err := strconv.ParseInt(strings.TrimSuffix(name, ".rgb"), 10, 64)
		if !strings.HasSuffix(name, ".rgb") || err != nil || id <= 0 || !e.Type().IsRegular() {
			continue
		}
		if err := ctx.Err(); err != nil {
			runErr = err
			break
		}
		seen[id] = true
		p := filepath.Join(root, name)
		meta, exists := tracks[id]
		if !exists {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				runErr = err
				break
			}
			delete(verified, id)
			dirty = true
			removed++
			continue
		}
		if prevPaths != nil {
			prevPath, hadPrev := prevPaths[id]
			if !hadPrev || prevPath != meta.path {
				if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					runErr = err
					break
				}
				delete(verified, id)
				dirty = true
				removed++
				continue
			}
		}
		fi, infoErr := e.Info()
		if infoErr == nil && fi.Size() <= 8 {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				runErr = err
				break
			}
			delete(verified, id)
			dirty = true
			removed++
			continue
		}
		if infoErr == nil {
			if prev, ok := verified[id]; ok && prev.Size == fi.Size() && prev.ModTime == fi.ModTime().UnixNano() && prev.Path == meta.path {
				continue
			}
		}
		if stale, err := staleRGB(ctx, db, before, id, p); err != nil {
			runErr = err
			break
		} else if stale {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				runErr = err
				break
			}
			delete(verified, id)
			dirty = true
			removed++
		} else if infoErr == nil {
			verified[id] = rgbVerifiedEntry{
				Size:    fi.Size(),
				ModTime: fi.ModTime().UnixNano(),
				Path:    meta.path,
			}
			dirty = true
		}
	}
	if runErr == nil {
		for id := range verified {
			if !seen[id] {
				delete(verified, id)
				dirty = true
			}
		}
		if len(verified) > 0 {
			saveRGBVerifiedCache(dir, library, uuid, verified)
		}
	} else if dirty && len(verified) > 0 {
		saveRGBVerifiedCache(dir, library, uuid, verified)
	}
	return removed, runErr
}

// RecordVerifiedRGBOverviews records newly published RGB overview files in the
// verified manifest so the next mount does not re-read and decompress them.
func RecordVerifiedRGBOverviews(ctx context.Context, dir, library string, ids []int64) {
	if len(ids) == 0 {
		return
	}
	db, err := openReadOnly(library)
	if err != nil {
		return
	}
	defer db.Close()
	var uuid string
	if err = db.QueryRowContext(ctx, `SELECT uuid FROM Information LIMIT 1`).Scan(&uuid); err != nil {
		return
	}
	if uuid == "" || strings.ContainsAny(uuid, `/\`) || uuid == "." || uuid == ".." {
		return
	}
	tracks, err := loadRGBTrackMeta(ctx, db)
	if err != nil {
		return
	}
	verified := loadRGBVerifiedCache(dir, uuid)
	root := filepath.Join(dir, "OverviewData", uuid)
	changed := false
	for _, id := range ids {
		meta, ok := tracks[id]
		if !ok {
			continue
		}
		fi, err := os.Stat(filepath.Join(root, strconv.FormatInt(id, 10)+".rgb"))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() <= 8 {
			continue
		}
		entry := rgbVerifiedEntry{
			Size:    fi.Size(),
			ModTime: fi.ModTime().UnixNano(),
			Path:    meta.path,
		}
		if prev, exists := verified[id]; !exists || prev != entry {
			verified[id] = entry
			changed = true
		}
	}
	if changed || len(verified) > 0 {
		saveRGBVerifiedCache(dir, library, uuid, verified)
	}
}

// RefreshRGBVerifiedStat updates the cached m.db size and mtime in .rb-bridge-rgb-verified
// after a stage publish that moved no new .rgb files.
func RefreshRGBVerifiedStat(dir, library string) {
	c, ok := loadRGBVerifiedCacheFull(dir)
	if !ok || len(c.Files) == 0 {
		return
	}
	saveRGBVerifiedCache(dir, library, c.UUID, c.Files)
}

// CountMissingRGBOverviews returns how many analyzed tracks in dir/m.db lack a
// non-empty OverviewData/<uuid>/<id>.rgb file, using directory metadata only.
func CountMissingRGBOverviews(ctx context.Context, dir string) (int, error) {
	db, err := openReadOnly(filepath.Join(dir, "m.db"))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var uuid string
	if err = db.QueryRowContext(ctx, `SELECT uuid FROM Information LIMIT 1`).Scan(&uuid); err != nil {
		return 0, err
	}
	if uuid == "" || strings.ContainsAny(uuid, `/\`) || uuid == "." || uuid == ".." {
		return 0, errors.New("rgb overviews: bad library uuid")
	}
	present := map[int64]bool{}
	if entries, err := os.ReadDir(filepath.Join(dir, "OverviewData", uuid)); err == nil {
		for _, e := range entries {
			name := e.Name()
			id, perr := strconv.ParseInt(strings.TrimSuffix(name, ".rgb"), 10, 64)
			if !strings.HasSuffix(name, ".rgb") || perr != nil || id <= 0 || !e.Type().IsRegular() {
				continue
			}
			if fi, serr := e.Info(); serr == nil && fi.Size() > 8 {
				present[id] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT t.id FROM Track t
		JOIN PerformanceData p ON p.trackId = t.id
		WHERE t.isAnalyzed = 1 AND p.overviewWaveFormData IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	missing := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		if !present[id] {
			missing++
		}
	}
	return missing, rows.Err()
}

func openReadOnly(p string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}).String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func staleRGB(ctx context.Context, db, before *sql.DB, id int64, path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if len(b) <= 8 { // qCompress of nothing: an overview Engine never got, drawn black
		return true, nil
	}
	var file sql.NullString
	var overview []byte
	err = db.QueryRowContext(ctx, `SELECT t.path, p.overviewWaveFormData FROM Track t LEFT JOIN PerformanceData p ON p.trackId = t.id WHERE t.id = ?`, id).Scan(&file, &overview)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // the id names no track any more
	}
	if err != nil {
		return false, err
	}
	if before != nil {
		var was sql.NullString
		err = before.QueryRowContext(ctx, `SELECT path FROM Track WHERE id = ?`, id).Scan(&was)
		if errors.Is(err, sql.ErrNoRows) || err == nil && was != file {
			return true, nil // Engine wrote it for another track of this id
		}
		if err != nil {
			return false, err
		}
	}
	// The old shadow and rekordbox replays answered Engine's RGB overview with the stored
	// 3-band one, which Engine kept and drew in the wrong colours (LEFT, 2026-11-20: 184 of
	// 207 shadow copies' files had exactly their row's bins; none of the 151 Engine made did).
	if a, b := overviewBins(b), overviewBins(overview); a != nil && bytes.Equal(a, b) {
		return true, nil
	}
	// Safety net for files gone stale before any of this: both overviews have 1024 bins,
	// so their samples per bin say how long a track they are of. What decoders trim
	// (< 16384 samples) moves it by at most 16; another song by far more.
	got, own := rgbSamplesPerBin(b), rgbSamplesPerBin(overview)
	return got > 0 && own > 0 && math.Abs(got-own) > max(16, own*0.002), nil
}

// overviewBins is an overview's bins without its header and band-maximum trailer, nil if none.
func overviewBins(blob []byte) []byte {
	raw, err := QUncompress(blob, 1<<20)
	if err != nil || len(raw) < 27 {
		return nil
	}
	return raw[24 : len(raw)-3]
}

// rgbSamplesPerBin reads an overview's samples per bin (its header's third field), 0 if none.
func rgbSamplesPerBin(blob []byte) float64 {
	raw, err := QUncompress(blob, 1<<20)
	if err != nil || len(raw) < 24 {
		return 0
	}
	v := math.Float64frombits(binary.BigEndian.Uint64(raw[16:24]))
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0
	}
	return v
}

// RGBOverview is the RGB overview Engine built for the track at rel (Track.path) of the
// library library, in dir/OverviewData, uncompressed: what a shadow copy replays so both
// units draw one picture. A file PruneRGBOverviews would remove is none.
func RGBOverview(ctx context.Context, dir, library, rel string) ([]byte, error) {
	db, err := openReadOnly(library)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var uuid string
	var id int64
	if err = db.QueryRowContext(ctx, `SELECT (SELECT uuid FROM Information LIMIT 1), id FROM Track WHERE path = ? ORDER BY id DESC LIMIT 1`, rel).Scan(&uuid, &id); err != nil {
		return nil, err
	}
	if uuid == "" || strings.ContainsAny(uuid, `/\`) || uuid == "." || uuid == ".." {
		return nil, errors.New("rgb overview: bad library uuid")
	}
	path := filepath.Join(dir, "OverviewData", uuid, strconv.FormatInt(id, 10)+".rgb")
	if stale, err := staleRGB(ctx, db, nil, id, path); err != nil {
		return nil, err
	} else if stale {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := QUncompress(b, 1<<20)
	if err != nil {
		return nil, err
	}
	if !ValidRGBOverview(raw) {
		return nil, errors.New("rgb overview: not an overview")
	}
	return raw, nil
}

// ValidRGBOverview: an uncompressed overview, its header's bin counts and length agreeing.
func ValidRGBOverview(raw []byte) bool {
	if len(raw) < 27 {
		return false
	}
	bins := binary.BigEndian.Uint64(raw[0:8])
	return bins > 0 && bins <= 1<<16 && binary.BigEndian.Uint64(raw[8:16]) == bins && uint64(len(raw)) == 24+3*bins+3
}

// hasColorOverviewWaveform reports whether a carries a rekordbox colour
// overview section (PWV6 in .2EX or PWV4 in .EXT).
func hasColorOverviewWaveform(a *rekordbox.Analysis) bool {
	if a == nil {
		return false
	}
	for i := range a.Waveforms {
		w := &a.Waveforms[i]
		if len(w.Data) == 0 || w.EntryBytes == 0 || uint64(len(w.Data))%uint64(w.EntryBytes) != 0 {
			continue
		}
		if (w.Tag == "PWV6" && w.EntryBytes >= 3) || (w.Tag == "PWV4" && w.EntryBytes >= 6) {
			return true
		}
	}
	return false
}

// selectRGBOverviewWaveform chooses the best ANLZ waveform for Engine's
// Database2/OverviewData/<uuid>/<id>.rgb sidecar: PWV6 (.2EX colour overview)
// first, PWV4 (.EXT colour overview) second, then remaining waveforms as
// fallback so every analysed track gets a valid non-empty mini waveform.
func selectRGBOverviewWaveform(a *rekordbox.Analysis) *rekordbox.Waveform {
	if a == nil {
		return nil
	}
	priority := map[string]int{
		"PWAV": 1, "PWV2": 1, "PWV3": 2,
		"PWV5": 3, "PWV7": 3,
		"PWV4": 4, "PWV6": 5,
	}
	var best *rekordbox.Waveform
	for i := range a.Waveforms {
		w := &a.Waveforms[i]
		if len(w.Data) == 0 || w.EntryBytes == 0 || uint64(len(w.Data))%uint64(w.EntryBytes) != 0 {
			continue
		}
		if best == nil || priority[w.Tag] > priority[best.Tag] {
			best = w
		}
	}
	return best
}

func overviewRGBEntry(w rekordbox.Waveform, i int) [3]byte {
	switch w.Tag {
	case "PWV6", "PWV4", "PWV5", "PWV7":
		return WaveRGB(w, i)
	default:
		// Monochrome blue preview (PWAV/PWV2/PWV3): preserve silhouette with a
		// rekordbox-blue tint so the RGB sidecar is both readable and distinct
		// from the grayscale 3-band fallback overviewWaveFormData.
		v := WaveRGB(w, i)[0]
		return [3]byte{v / 4, v / 2, v}
	}
}

// EncodeRGBOverview builds the qCompress'd 3,099-byte Engine RGB overview body
// (>QQd 1024, 1024, samplesPerBin + 1024 RGB triples + 3-byte per-channel
// maximum trailer) from rekordbox's colour waveform (PWV6 in .2EX, PWV4 in
// .EXT, or tinted fallback). storedOverview, when present, supplies the exact
// samplesPerBin and ensures the generated bins never equal the stored 3-band
// overview blob (which PruneRGBOverviews intentionally rejects).
func EncodeRGBOverview(t rekordbox.Track, a *rekordbox.Analysis, storedOverview []byte) ([]byte, error) {
	wave := selectRGBOverviewWaveform(a)
	if wave == nil {
		return nil, errors.New("rgb overview: no waveform in analysis")
	}
	const bins = 1024
	samplesPerBin := rgbSamplesPerBin(storedOverview)
	if samplesPerBin <= 0 {
		sr := t.SampleRate
		if sr == 0 {
			sr = 44100
		}
		sampleCount := t.SampleCount
		if sampleCount == 0 && t.Duration > 0 && !math.IsNaN(t.Duration) && !math.IsInf(t.Duration, 0) {
			sampleCount = uint64(math.Round(t.Duration * float64(sr)))
		}
		if sampleCount == 0 {
			sampleCount = bins
		}
		samplesPerBin = float64(sampleCount) / bins
	}

	n := uint64(len(wave.Data)) / uint64(wave.EntryBytes)
	if n == 0 {
		return nil, errors.New("rgb overview: empty waveform")
	}

	raw := make([]byte, 24+bins*3+3)
	binary.BigEndian.PutUint64(raw[0:8], bins)
	binary.BigEndian.PutUint64(raw[8:16], bins)
	binary.BigEndian.PutUint64(raw[16:24], math.Float64bits(samplesPerBin))

	var top [3]byte
	for i := 0; i < bins; i++ {
		lo, hi := uint64(i)*n/bins, uint64(i+1)*n/bins
		if hi <= lo {
			hi = lo + 1
		}
		var peak [3]byte
		for j := lo; j < hi && j < n; j++ {
			v := overviewRGBEntry(*wave, int(j))
			for c := 0; c < 3; c++ {
				peak[c] = max(peak[c], v[c])
				top[c] = max(top[c], v[c])
			}
		}
		off := 24 + i*3
		raw[off], raw[off+1], raw[off+2] = peak[0], peak[1], peak[2]
	}

	// Never emit bins byte-identical to PerformanceData.overviewWaveFormData,
	// because PruneRGBOverviews treats equal bins as an invalid 3-band replay.
	// This only occurs when an export lacks PWV6/PWV7/PWV5 and both paths read
	// the same PWV4 section; a 1-LSB adjustment on the peak bin is invisible and
	// preserves full fidelity.
	if storedBins := overviewBins(storedOverview); storedBins != nil && bytes.Equal(raw[24:24+bins*3], storedBins) {
		adjusted := false
		for i := 0; i < bins*3; i++ {
			if raw[24+i] > 0 {
				if raw[24+i] > 1 {
					raw[24+i]--
				} else {
					raw[24+i]++
				}
				adjusted = true
				break
			}
		}
		if !adjusted {
			raw[24+2] = 1
		}
		top = [3]byte{}
		for i := 0; i < bins; i++ {
			off := 24 + i*3
			for c := 0; c < 3; c++ {
				top[c] = max(top[c], raw[off+c])
			}
		}
	}
	copy(raw[24+bins*3:], top[:])
	return QCompress(raw)
}

// WriteRGBOverviewAtomic atomically writes a compressed RGB overview sidecar to
// <dir>/OverviewData/<uuid>/<trackID>.rgb.
func WriteRGBOverviewAtomic(dir, uuid string, trackID int64, compressed []byte) error {
	if uuid == "" || strings.ContainsAny(uuid, `/\`) || uuid == "." || uuid == ".." {
		return errors.New("rgb overview: bad library uuid")
	}
	if trackID <= 0 || len(compressed) <= 8 {
		return errors.New("rgb overview: invalid track id or payload")
	}
	outDir := filepath.Join(dir, "OverviewData", uuid)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(outDir, strconv.FormatInt(trackID, 10)+".rgb")
	if st, err := os.Lstat(dst); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("rgb overview: refusing symlink target")
	}
	f, err := os.CreateTemp(outDir, ".rgb-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(compressed); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

// readColorOverviewAnalysis reads the best available waveform needed to build
// an RGB sidecar, preferring small colour overview sections (PWV6 from .2EX,
// PWV4 from .EXT) to avoid reading large detail sections, and falling back to
// detail colour/monochrome waveforms (PWV7, PWV5, PWV3, .DAT preview) when an
// export omits overview sections.
func readColorOverviewAnalysis(res *compat.Resolver, analysisPath string) (*rekordbox.Analysis, error) {
	if analysisPath == "" {
		return nil, os.ErrNotExist
	}
	base := strings.TrimSuffix(analysisPath, path.Ext(analysisPath))
	resolved := map[string]string{}
	resolveExt := func(ext string) (string, bool) {
		if p, ok := resolved[ext]; ok {
			return p, p != ""
		}
		rel, err := res.Resolve(base + ext)
		if err != nil {
			resolved[ext] = ""
			return "", false
		}
		full := res.FullPath(rel)
		resolved[ext] = full
		return full, true
	}
	for _, spec := range []struct {
		ext  string
		keep string
	}{
		{".2EX", "PWV6"},
		{".EXT", "PWV4"},
		{".2EX", "PWV7"},
		{".EXT", "PWV5"},
		{".2EX", "PWV3"},
		{".EXT", "PWV3"},
		{".DAT", ""},
	} {
		full, ok := resolveExt(spec.ext)
		if !ok {
			continue
		}
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		var a *rekordbox.Analysis
		if spec.keep != "" {
			a, _, err = rekordbox.ReadAnalysisFile(f, func(tag string) bool { return tag != spec.keep })
		} else {
			a, _, err = rekordbox.ReadAnalysisFile(f, func(string) bool { return false })
		}
		_ = f.Close()
		if err == nil && selectRGBOverviewWaveform(a) != nil {
			return a, nil
		}
	}
	return nil, os.ErrNotExist
}

// BackfillStageRGBOverviews ensures every complete track in stageDir/m.db has a
// valid non-empty RGB overview sidecar either in stageDir/OverviewData/<uuid>
// or preserved in liveDir/OverviewData/<uuid> (for the same track path). Any
// missing or zero-byte sidecar is generated from the track's ANLZ colour
// waveform and written atomically into stageDir before publication.
func BackfillStageRGBOverviews(ctx context.Context, res *compat.Resolver, l *rekordbox.Library, stageDir, liveDir string) (int, error) {
	stageDB := filepath.Join(stageDir, "m.db")
	db, err := openReadOnly(stageDB)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var uuid string
	if err := db.QueryRowContext(ctx, `SELECT uuid FROM Information LIMIT 1`).Scan(&uuid); err != nil {
		return 0, err
	}
	if uuid == "" || strings.ContainsAny(uuid, `/\`) || uuid == "." || uuid == ".." {
		return 0, errors.New("rgb overview: bad library uuid")
	}

	livePaths := map[int64]string{}
	if liveDir != "" && liveDir != stageDir {
		if ldb, lerr := openReadOnly(filepath.Join(liveDir, "m.db")); lerr == nil {
			if rows, rerr := ldb.QueryContext(ctx, `SELECT id, COALESCE(path,'') FROM Track`); rerr == nil {
				for rows.Next() {
					var id int64
					var p string
					if rows.Scan(&id, &p) == nil {
						livePaths[id] = p
					}
				}
				rows.Close()
			}
			ldb.Close()
		}
	}

	byFold := make(map[string]rekordbox.Track, len(l.Tracks))
	for _, tr := range l.Tracks {
		if rel, e := compat.CleanRelative(tr.Path); e == nil {
			byFold[foldPreparedPath("../"+rel)] = tr
		}
	}

	rows, err := db.QueryContext(ctx, `SELECT t.id, COALESCE(t.path,''), p.overviewWaveFormData
		FROM Track t
		JOIN PerformanceData p ON p.trackId = t.id
		WHERE t.isAnalyzed = 1 AND p.overviewWaveFormData IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id       int64
		path     string
		overview []byte
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.path, &c.overview); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	written := 0
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		name := strconv.FormatInt(c.id, 10) + ".rgb"
		stagePath := filepath.Join(stageDir, "OverviewData", uuid, name)
		if st, err := os.Stat(stagePath); err == nil && st.Mode().IsRegular() && st.Size() > 8 {
			continue
		}
		if liveDir != "" && liveDir != stageDir && livePaths[c.id] == c.path {
			livePath := filepath.Join(liveDir, "OverviewData", uuid, name)
			if st, err := os.Stat(livePath); err == nil && st.Mode().IsRegular() && st.Size() > 8 {
				if stale, serr := staleRGB(ctx, db, nil, c.id, livePath); serr == nil && !stale {
					continue
				}
			}
		}
		tr, ok := byFold[foldPreparedPath(c.path)]
		if !ok || tr.AnalysisPath == "" {
			continue
		}
		a, aerr := readColorOverviewAnalysis(res, tr.AnalysisPath)
		if aerr != nil {
			continue
		}
		blob, eerr := EncodeRGBOverview(tr, a, c.overview)
		if eerr != nil {
			continue
		}
		if werr := WriteRGBOverviewAtomic(stageDir, uuid, c.id, blob); werr != nil {
			log.Printf("rb-bridge: stage RGB overview for track %d not written: %v", c.id, werr)
			continue
		}
		written++
	}
	return written, nil
}
