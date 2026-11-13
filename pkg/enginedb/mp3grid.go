package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/audioheader"
	_ "modernc.org/sqlite"
)

const (
	// MP3GridStampFile records that pre-5.3.0.4 EngineX MP3 grids in this
	// Engine Library have been shifted by their file start offset (#246).
	MP3GridStampFile = ".enginex-mp3-grid-stamp"

	mp3GridStampVersion = "v1"
	defaultMP3BatchSize = 25
)

type MP3GridOptions struct {
	// Budget bounds how long FixMP3Grids spends inspecting audio files before
	// stopping cleanly (without writing the stamp) so large libraries resume on
	// the next boot or insert. Zero means unlimited.
	Budget    time.Duration
	BatchSize int
	Now       func() time.Time
}

type MP3GridReport struct {
	Candidates  int  `json:"candidates"`
	Corrected   int  `json:"corrected"`
	Unchanged   int  `json:"unchanged"`
	Skipped     int  `json:"skipped"`
	Bypassed    bool `json:"bypassed,omitempty"`
	Interrupted bool `json:"interrupted,omitempty"`
}

var engineXMP3CommentRe = regexp.MustCompile(`^\[\d+k\] MP3 \d+ Hz; FLAC unavailable$`)

func isEngineXMP3Path(p string) bool {
	for _, prefix := range []string{"../Music/Stream/", "Music/Stream/", "../Stream/", "Stream/", "../Wi-Fi Drop/", "Wi-Fi Drop/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func isEngineXMP3Proxy(trackPath, comment string) bool {
	return isEngineXMP3Path(trackPath) || engineXMP3CommentRe.MatchString(comment)
}

func pathWritable(path string) (bool, error) {
	s, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(filepath.Dir(path), &fs); err != nil {
		return false, err
	}
	d, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	return fs.Flags&1 == 0 && s.Mode().Perm()&0o222 != 0 && d.Mode().Perm()&0o222 != 0, nil
}

func hasMP3GridStamp(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(b))
	return s == mp3GridStampVersion || strings.HasPrefix(s, mp3GridStampVersion+" ")
}

func writeMP3GridStamp(path string, r MP3GridReport) error {
	content := fmt.Sprintf("%s candidates=%d corrected=%d unchanged=%d skipped=%d\n",
		mp3GridStampVersion, r.Candidates, r.Corrected, r.Unchanged, r.Skipped)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".enginex-mp3-grid-stamp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(content); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		_ = os.Chmod(tmp.Name(), 0o644)
		err = os.Rename(tmp.Name(), path)
	}
	return err
}

func bridgeOwnedTracks(ctx context.Context, dbDir string) map[int64]bool {
	statePath := filepath.Join(dbDir, stateFile)
	if st, err := os.Stat(statePath); err != nil || !st.Mode().IsRegular() || safeFile(statePath) != nil {
		return nil
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: statePath}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT trackId FROM BridgeState`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	owned := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			owned[id] = true
		}
	}
	return owned
}

func resolveTrackAudioPath(root, rootReal, trackPath string) (string, error) {
	if trackPath == "" || filepath.IsAbs(filepath.FromSlash(trackPath)) {
		return "", errors.New("invalid track path")
	}
	engineLibDir := filepath.Join(root, "Engine Library")
	abs := filepath.Clean(filepath.Join(engineLibDir, filepath.FromSlash(trackPath)))
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("track path escapes library root")
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("track path is not a regular file")
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	relReal, err := filepath.Rel(rootReal, real)
	if err != nil || relReal == "." || relReal == ".." || strings.HasPrefix(relReal, ".."+string(filepath.Separator)) {
		return "", errors.New("track realpath escapes library root")
	}
	return abs, nil
}

func finiteFloat(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// parsePreR4AnalyzerBeatData accepts only the exact 130-byte beatData written by
// pkg/analyzer.PerformanceBlobs when the grid has not been edited by the user or
// overwritten/re-analyzed by stock Engine.
func parsePreR4AnalyzerBeatData(blob []byte) (raw []byte, rate, sampleLen float64, ok bool) {
	raw, err := QUncompress(blob, 8<<20)
	if err != nil || len(raw) != 130 {
		return nil, 0, 0, false
	}
	if raw[16] != 1 || raw[129] != 0 {
		return nil, 0, 0, false
	}
	rate = math.Float64frombits(big.Uint64(raw[0:8]))
	sampleLen = math.Float64frombits(big.Uint64(raw[8:16]))
	if !finiteFloat(rate) || rate < 8000 || rate > 384000 || !finiteFloat(sampleLen) || sampleLen <= 0 {
		return nil, 0, 0, false
	}
	// default_grid (17..73) and adjusted_grid (73..129) must both hold 2 markers
	// and match byte-for-byte; any user grid edit alters adjusted_grid or counts/tail.
	if big.Uint64(raw[17:25]) != 2 || big.Uint64(raw[73:81]) != 2 || !bytes.Equal(raw[17:73], raw[73:129]) {
		return nil, 0, 0, false
	}
	pos0 := math.Float64frombits(little.Uint64(raw[25:33]))
	beat0 := int64(little.Uint64(raw[33:41]))
	num0 := little.Uint32(raw[41:45])
	unk0 := little.Uint32(raw[45:49])

	pos1 := math.Float64frombits(little.Uint64(raw[49:57]))
	beat1 := int64(little.Uint64(raw[57:65]))
	num1 := little.Uint32(raw[65:69])
	unk1 := little.Uint32(raw[69:73])

	if !finiteFloat(pos0) || !finiteFloat(pos1) || pos1 <= pos0 {
		return nil, 0, 0, false
	}
	if beat0 != -4 || num0 == 0 || beat1 != int64(num0)-4 || num1 != 0 || unk0 != 0 || unk1 != 0 {
		return nil, 0, 0, false
	}
	return raw, rate, sampleLen, true
}

func shiftBeatData(raw []byte, shift float64) ([]byte, error) {
	out := append([]byte(nil), raw...)
	for _, off := range []int{25, 49, 81, 105} {
		pos := math.Float64frombits(little.Uint64(out[off : off+8]))
		little.PutUint64(out[off:off+8], math.Float64bits(pos-shift))
	}
	return QCompress(out)
}

func shiftQuickCues(blob []byte, shift float64) ([]byte, bool, error) {
	if len(blob) == 0 {
		return nil, false, nil
	}
	raw, err := QUncompress(blob, 64<<10)
	if err != nil || len(raw) < 8+17 {
		return blob, false, nil
	}
	slots := big.Uint64(raw[:8])
	if slots > 8 {
		return blob, false, nil
	}
	out := append([]byte(nil), raw...)
	off := 8
	changed := false
	for i := uint64(0); i < slots; i++ {
		if off >= len(out)-17 {
			return blob, false, nil
		}
		lLen := int(out[off])
		off++
		if len(out)-17-off < lLen+12 {
			return blob, false, nil
		}
		off += lLen
		pos := math.Float64frombits(big.Uint64(out[off : off+8]))
		if finiteFloat(pos) && pos != -1 {
			next := max(0.0, pos-shift)
			if next != pos {
				big.PutUint64(out[off:off+8], math.Float64bits(next))
				changed = true
			}
		}
		off += 12
	}
	if off+17 != len(out) {
		return blob, false, nil
	}
	for _, cueOff := range []int{off, off + 9} {
		pos := math.Float64frombits(big.Uint64(out[cueOff : cueOff+8]))
		if finiteFloat(pos) && pos != -1 {
			next := max(0.0, pos-shift)
			if next != pos {
				big.PutUint64(out[cueOff:cueOff+8], math.Float64bits(next))
				changed = true
			}
		}
	}
	if !changed {
		return blob, false, nil
	}
	enc, err := QCompress(out)
	return enc, true, err
}

func shiftLoops(blob []byte, shift, sampleLen float64) ([]byte, bool) {
	if len(blob) == 0 {
		return nil, false
	}
	if len(blob) < 8 || blob[0] > 8 {
		return blob, false
	}
	for _, b := range blob[1:8] {
		if b != 0 {
			return blob, false
		}
	}
	out := append([]byte(nil), blob...)
	off := 8
	changed := false
	for i := 0; i < int(blob[0]); i++ {
		if off >= len(out) {
			return blob, false
		}
		lLen := int(out[off])
		off++
		if len(out)-off < lLen+22 {
			return blob, false
		}
		off += lLen
		start := math.Float64frombits(little.Uint64(out[off : off+8]))
		end := math.Float64frombits(little.Uint64(out[off+8 : off+16]))
		startSet, endSet := out[off+16], out[off+17]
		if startSet > 1 || endSet > 1 {
			return blob, false
		}
		newStart, newEnd := start, end
		if startSet == 1 && finiteFloat(start) && start != -1 {
			newStart = max(0.0, start-shift)
		}
		if endSet == 1 && finiteFloat(end) && end != -1 {
			newEnd = max(0.0, end-shift)
		}
		if startSet == 1 && endSet == 1 && finiteFloat(start) && finiteFloat(end) && end > start && newEnd <= newStart {
			newEnd = newStart + max(1.0, end-start)
			if sampleLen > 0 && newStart < sampleLen {
				newEnd = min(sampleLen, newEnd)
			}
		}
		if startSet == 1 && newStart != start {
			little.PutUint64(out[off:off+8], math.Float64bits(newStart))
			changed = true
		}
		if endSet == 1 && newEnd != end {
			little.PutUint64(out[off+8:off+16], math.Float64bits(newEnd))
			changed = true
		}
		off += 22
	}
	if off != len(out) || !changed {
		return blob, false
	}
	return out, true
}

// FixMP3Grids performs a one-time correction of beat grids, cues and loops for
// pre-5.3.0.4 EngineX-analyzed MP3 tracks in root/Engine Library/Database2/m.db
// (#246). It refuses to write while another process has m.db open, commits in
// small batches so interrupted runs resume cleanly, skips missing/unreadable
// audio files without retrying forever, and records a per-library stamp once
// every candidate track has been processed.
func FixMP3Grids(ctx context.Context, root string, opts MP3GridOptions) (MP3GridReport, error) {
	var r MP3GridReport
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	dbPath := filepath.Join(dbDir, "m.db")
	stampPath := filepath.Join(dbDir, MP3GridStampFile)

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-journal", dbPath + "-shm", stampPath} {
		if err := safeFile(p); err != nil {
			return r, err
		}
	}
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		r.Bypassed = true
		return r, nil
	} else if err != nil {
		return r, err
	}
	rw, err := pathWritable(dbPath)
	if err != nil {
		return r, err
	}
	if !rw {
		r.Bypassed = true
		return r, nil
	}
	if hasMP3GridStamp(stampPath) {
		r.Bypassed = true
		return r, nil
	}
	if LibraryInUse(root) {
		r.Interrupted = true
		return r, ErrLibraryInUse
	}

	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	var deadline time.Time
	if opts.Budget > 0 {
		deadline = nowFn().Add(opts.Budget)
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultMP3BatchSize
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return r, err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		rootReal = rootAbs
	}

	u := url.URL{Scheme: "file", Path: dbPath}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(3000)")
	if err != nil {
		return r, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var major, minor, patch int
	if err := db.QueryRowContext(ctx, `SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1`).Scan(&major, &minor, &patch); err != nil || major != 3 || minor != 0 || patch != 2 {
		r.Bypassed = true
		return r, nil
	}
	var hasTables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('Track','PerformanceData')`).Scan(&hasTables); err != nil || hasTables != 2 {
		r.Bypassed = true
		return r, nil
	}

	bridgeOwned := bridgeOwnedTracks(ctx, dbDir)

	type candidate struct {
		id       int64
		path     string
		comment  string
		track    []byte
		overview []byte
		beat     []byte
		cues     []byte
		loops    []byte
	}
	type update struct {
		id    int64
		track []byte
		beat  []byte
		cues  any
		loops any
	}

	var lastID int64 = math.MinInt64
	for {
		if ctx.Err() != nil || (!deadline.IsZero() && nowFn().After(deadline)) {
			r.Interrupted = true
			return r, ctx.Err()
		}
		if LibraryInUse(root) {
			r.Interrupted = true
			return r, ErrLibraryInUse
		}

		rows, err := db.QueryContext(ctx, `
			SELECT t.id, coalesce(t.path,''), coalesce(t.comment,''),
			       substr(p.trackData,1,33554433),
			       substr(p.overviewWaveFormData,1,33554433),
			       substr(p.beatData,1,33554433),
			       substr(p.quickCues,1,33554433),
			       substr(p.loops,1,33554433)
			FROM Track t
			JOIN PerformanceData p ON p.trackId = t.id
			WHERE t.id > ?
			  AND coalesce(t.isAnalyzed, 0) = 1
			  AND coalesce(t.isBeatGridLocked, 0) = 0
			  AND t.pdbImportKey IS NULL
			  AND coalesce(p.thirdPartySourceId, 0) = 0
			  AND length(p.trackData) > 4
			  AND length(p.beatData) > 4
			  AND (lower(coalesce(t.fileType, '')) = 'mp3' OR lower(t.path) LIKE '%.mp3')
			  AND (
			    t.path LIKE '../Music/Stream/%' OR t.path LIKE 'Music/Stream/%' OR
			    t.path LIKE '../Stream/%'       OR t.path LIKE 'Stream/%'       OR
			    t.path LIKE '../Wi-Fi Drop/%'   OR t.path LIKE 'Wi-Fi Drop/%'   OR
			    t.comment LIKE '[%k] MP3 % Hz; FLAC unavailable'
			  )
			ORDER BY t.id
			LIMIT ?`, lastID, batchSize)
		if err != nil {
			return r, err
		}
		var page []candidate
		for rows.Next() {
			var c candidate
			if scanErr := rows.Scan(&c.id, &c.path, &c.comment, &c.track, &c.overview, &c.beat, &c.cues, &c.loops); scanErr != nil {
				rows.Close()
				return r, scanErr
			}
			page = append(page, c)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return r, err
		}
		rows.Close()
		if len(page) == 0 {
			break
		}

		var updates []update
		var interruptedErr error
		for _, c := range page {
			lastID = c.id
			if bridgeOwned[c.id] || !isEngineXMP3Proxy(c.path, c.comment) {
				continue
			}
			rawTrack, err := QUncompress(c.track, 4096)
			if err != nil || len(rawTrack) != 44 {
				continue
			}
			rawBeat, beatRate, sampleLen, ok := parsePreR4AnalyzerBeatData(c.beat)
			if !ok {
				continue
			}

			// Budget/in-use checks happen before opening audio files so an
			// interrupted run never discards already-prepared updates in this batch
			// without committing them first (below).
			if ctx.Err() != nil {
				r.Interrupted = true
				interruptedErr = ctx.Err()
				break
			}
			if !deadline.IsZero() && nowFn().After(deadline) {
				r.Interrupted = true
				break
			}
			if LibraryInUse(root) {
				r.Interrupted = true
				interruptedErr = ErrLibraryInUse
				break
			}

			r.Candidates++
			audioPath, err := resolveTrackAudioPath(rootAbs, rootReal, c.path)
			if err != nil {
				r.Skipped++
				log.Printf("enginedb: mp3 grid fix: skipped track %d (%s): %v", c.id, c.path, err)
				continue
			}
			info, err := audioheader.OpenMP3(audioPath)
			if err != nil {
				r.Skipped++
				log.Printf("enginedb: mp3 grid fix: skipped track %d (%s): %v", c.id, c.path, err)
				continue
			}
			if info.StartOffsetSamples <= 0 {
				r.Unchanged++
				continue
			}

			shift := float64(info.StartOffsetSamples)
			if info.SampleRate > 0 && beatRate > 0 && float64(info.SampleRate) != beatRate {
				shift = shift * beatRate / float64(info.SampleRate)
			}

			var rawOverview []byte
			if len(c.overview) > 0 {
				if ov, oerr := QUncompress(c.overview, 8<<20); oerr == nil {
					rawOverview = ov
				}
			}
			newTrack, err := QCompress(TrackDataPeaks(rawTrack, rawOverview))
			if err != nil {
				return r, err
			}
			newBeat, err := shiftBeatData(rawBeat, shift)
			if err != nil {
				return r, err
			}
			var cuesArg any
			if c.cues != nil {
				if newCues, changed, cerr := shiftQuickCues(c.cues, shift); cerr != nil {
					return r, cerr
				} else if changed {
					cuesArg = newCues
				} else {
					cuesArg = c.cues
				}
			}
			var loopsArg any
			if c.loops != nil {
				if newLoops, changed := shiftLoops(c.loops, shift, sampleLen); changed {
					loopsArg = newLoops
				} else {
					loopsArg = c.loops
				}
			}
			updates = append(updates, update{
				id:    c.id,
				track: newTrack,
				beat:  newBeat,
				cues:  cuesArg,
				loops: loopsArg,
			})
		}

		if len(updates) > 0 {
			if LibraryInUse(root) {
				r.Interrupted = true
				return r, ErrLibraryInUse
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return r, err
			}
			for _, u := range updates {
				if _, err := tx.ExecContext(ctx,
					`UPDATE PerformanceData SET trackData=?, beatData=?, quickCues=?, loops=? WHERE trackId=?`,
					u.track, u.beat, u.cues, u.loops, u.id,
				); err != nil {
					tx.Rollback()
					return r, err
				}
			}
			if err := tx.Commit(); err != nil {
				return r, err
			}
			r.Corrected += len(updates)
		}

		if r.Interrupted {
			return r, interruptedErr
		}
	}

	if r.Candidates > 0 {
		if err := writeMP3GridStamp(stampPath, r); err != nil {
			return r, err
		}
		if r.Corrected > 0 || r.Skipped > 0 {
			log.Printf("enginedb: mp3 grid fix on %s: corrected=%d unchanged=%d skipped=%d", root, r.Corrected, r.Unchanged, r.Skipped)
		}
	}
	return r, nil
}
