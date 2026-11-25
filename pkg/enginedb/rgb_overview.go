package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Engine keeps each track's RGB overview outside m.db, in Database2/OverviewData/<uuid>/<id>.rgb
// (qCompress'd: a 24-byte header and the bins), builds it only for a track that has none and never
// checks it against its track again: one written for another track under the same id, or an empty
// one, is drawn for good (real stick 2026-11-19: 26 of 123 files another song's, 140 empty).
// PruneRGBOverviews removes such files from a library Engine does not have open (the guard runs it
// on /data's library at boot), so that Engine builds them anew. .rb-bridge-rgb-verified names the
// files it checked and m.db's size and time: when neither changed, m.db is not opened.

const rgbVerifiedName = ".rb-bridge-rgb-verified"

// HasRGBOverviews reports whether a library UUID already has per-track overview files.
// An empty m.db with such files is an old populated library whose Track IDs must not restart.
func HasRGBOverviews(dir, uuid string) bool {
	if badUUID(uuid) {
		return false
	}
	entries, err := os.ReadDir(filepath.Join(dir, "OverviewData", uuid))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if rgbFileID(entry) > 0 {
			return true
		}
	}
	return false
}

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

func badUUID(uuid string) bool {
	return uuid == "" || strings.ContainsAny(uuid, `/\`) || uuid == "." || uuid == ".."
}

func loadRGBVerifiedCache(dir string) (rgbVerifiedCache, bool) {
	b, err := os.ReadFile(filepath.Join(dir, rgbVerifiedName))
	if err != nil {
		return rgbVerifiedCache{}, false
	}
	var c rgbVerifiedCache
	if err := json.Unmarshal(b, &c); err != nil || badUUID(c.UUID) || c.Files == nil {
		return rgbVerifiedCache{}, false
	}
	return c, true
}

// cleanMDBStat is m.db's size and time when no journal or WAL holds part of it.
func cleanMDBStat(library string) (size, modTime int64, ok bool) {
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
	ms, mm, _ := cleanMDBStat(library)
	if b, err := json.Marshal(rgbVerifiedCache{UUID: uuid, MDBSize: ms, MDBModTime: mm, Files: files}); err == nil {
		_ = writeRenamed(dir, rgbVerifiedName, b)
	}
}

// writeRenamed replaces dir/name with data, mode 0644, through a temp file and a rename.
func writeRenamed(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".rb-tmp-*")
	if err != nil {
		return err
	}
	err = f.Chmod(0o644)
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// rgbFileID is the track ID of an OverviewData entry, 0 for anything else.
func rgbFileID(e os.DirEntry) int64 {
	id, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".rgb"), 10, 64)
	if !strings.HasSuffix(e.Name(), ".rgb") || err != nil || id <= 0 || !e.Type().IsRegular() {
		return 0
	}
	return id
}

// pruneRGBVerifiedFast is the prune of an m.db unchanged since the cache was written: empty files
// go, and every other must be a file the cache names, as it was. handled is false otherwise.
func pruneRGBVerifiedFast(ctx context.Context, dir, library string) (removed int, handled bool, err error) {
	ms, mm, ok := cleanMDBStat(library)
	if !ok || ms <= 0 {
		return 0, false, nil
	}
	c, ok := loadRGBVerifiedCache(dir)
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
		id := rgbFileID(e)
		if id == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, true, err
		}
		seen[id] = true
		fi, err := e.Info()
		if err != nil {
			return removed, false, nil
		}
		if fi.Size() <= 8 {
			if err := os.Remove(filepath.Join(root, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, true, err
			}
			delete(c.Files, id)
			dirty = true
			removed++
		} else if prev, ok := c.Files[id]; !ok || prev.Size != fi.Size() || prev.ModTime != fi.ModTime().UnixNano() {
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

// PruneRGBOverviews removes the OverviewData files of library (an m.db Engine does not have open)
// that are not its tracks' own, and returns how many.
func PruneRGBOverviews(ctx context.Context, dir, library string) (int, error) {
	if removed, ok, err := pruneRGBVerifiedFast(ctx, dir, library); ok {
		return removed, err
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
	if badUUID(uuid) {
		return 0, errors.New("rgb overviews: bad library uuid")
	}
	root := filepath.Join(dir, "OverviewData", uuid)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) || err == nil && len(entries) == 0 {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	paths := map[int64]string{}
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(path,'') FROM Track`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		var p string
		if err = rows.Scan(&id, &p); err != nil {
			rows.Close()
			return 0, err
		}
		paths[id] = p
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	verified := map[int64]rgbVerifiedEntry{}
	if c, ok := loadRGBVerifiedCache(dir); ok && c.UUID == uuid {
		verified = c.Files
	}
	seen := make(map[int64]bool, len(entries))
	dirty, removed := false, 0
	var runErr error
	for _, e := range entries {
		id := rgbFileID(e)
		if id == 0 {
			continue
		}
		if runErr = ctx.Err(); runErr != nil {
			break
		}
		seen[id] = true
		p := filepath.Join(root, e.Name())
		path, stale := paths[id]
		stale = !stale
		if !stale {
			fi, infoErr := e.Info()
			if infoErr == nil && fi.Size() <= 8 {
				stale = true
			} else {
				if prev, ok := verified[id]; infoErr == nil && ok && prev.Size == fi.Size() && prev.ModTime == fi.ModTime().UnixNano() && prev.Path == path {
					continue
				}
				if stale, runErr = staleRGB(ctx, db, id, p); runErr != nil {
					break
				}
				if !stale && infoErr == nil {
					verified[id] = rgbVerifiedEntry{Size: fi.Size(), ModTime: fi.ModTime().UnixNano(), Path: path}
					dirty = true
				}
			}
		}
		if stale {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				runErr = err
				break
			}
			delete(verified, id)
			dirty = true
			removed++
		}
	}
	if runErr == nil {
		for id := range verified {
			if !seen[id] {
				delete(verified, id)
			}
		}
	}
	if len(verified) > 0 && (runErr == nil || dirty) {
		saveRGBVerifiedCache(dir, library, uuid, verified)
	}
	return removed, runErr
}

func openReadOnly(p string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}).String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

// staleRGB reports a file that is not the track's own overview.
func staleRGB(ctx context.Context, db *sql.DB, id int64, path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if len(b) <= 8 { // qCompress of nothing: an overview Engine never got, drawn black
		return true, nil
	}
	var overview []byte
	err = db.QueryRowContext(ctx, `SELECT p.overviewWaveFormData FROM Track t LEFT JOIN PerformanceData p ON p.trackId = t.id WHERE t.id = ?`, id).Scan(&overview)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // the id names no track any more
	}
	if err != nil {
		return false, err
	}
	// The old shadow and rekordbox replays answered Engine with the stored 3-band overview, which
	// Engine kept and drew in the wrong colours (2026-11-20: 184 of 207 shadow copies' files).
	if a, b := overviewBins(b), overviewBins(overview); a != nil && bytes.Equal(a, b) {
		return true, nil
	}
	// Both overviews have 1024 bins, so their samples per bin say how long a track they are of:
	// what decoders trim moves it by at most 16, another song by far more.
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
	if badUUID(uuid) {
		return nil, errors.New("rgb overview: bad library uuid")
	}
	path := filepath.Join(dir, "OverviewData", uuid, strconv.FormatInt(id, 10)+".rgb")
	if stale, err := staleRGB(ctx, db, id, path); err != nil {
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

// hasColorOverviewWaveform: a has a colour overview (PWV6 of a .2EX, PWV4 of an .EXT).
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

// selectRGBOverviewWaveform is the waveform the .rgb file is made of: PWV6, then PWV4, then any.
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
		// A monochrome preview (PWAV/PWV2/PWV3), tinted rekordbox blue.
		v := WaveRGB(w, i)[0]
		return [3]byte{v / 4, v / 2, v}
	}
}

// EncodeRGBOverview is the qCompress'd .rgb body (>QQd 1024, 1024, samplesPerBin; 1024 RGB
// triples; the channels' maxima) from rekordbox's colour waveform. storedOverview, the row's
// 3-band overview, gives samplesPerBin; the bins never equal its bins (PruneRGBOverviews' rule).
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

	// Equal bins (both made of one PWV4) would be pruned: one LSB of the first lit bin moves.
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

// WriteRGBOverviewAtomic replaces <dir>/OverviewData/<uuid>/<trackID>.rgb with compressed.
func WriteRGBOverviewAtomic(dir, uuid string, trackID int64, compressed []byte) error {
	if badUUID(uuid) {
		return errors.New("rgb overview: bad library uuid")
	}
	if trackID <= 0 || len(compressed) <= 8 {
		return errors.New("rgb overview: invalid track id or payload")
	}
	outDir := filepath.Join(dir, "OverviewData", uuid)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	name := strconv.FormatInt(trackID, 10) + ".rgb"
	if st, err := os.Lstat(filepath.Join(outDir, name)); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("rgb overview: refusing symlink target")
	}
	return writeRenamed(outDir, name, compressed)
}
