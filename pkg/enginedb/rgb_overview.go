package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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
func PruneRGBOverviews(ctx context.Context, dir, library, previous string) (int, error) {
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
	var before *sql.DB
	if previous != "" {
		if _, err := os.Stat(previous); err == nil {
			if before, err = openReadOnly(previous); err != nil {
				return 0, err
			}
			defer before.Close()
		}
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		id, err := strconv.ParseInt(strings.TrimSuffix(name, ".rgb"), 10, 64)
		if !strings.HasSuffix(name, ".rgb") || err != nil || id <= 0 || !e.Type().IsRegular() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		path := filepath.Join(root, name)
		if stale, err := staleRGB(ctx, db, before, id, path); err != nil {
			return removed, err
		} else if stale {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
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
