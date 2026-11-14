package enginedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"github.com/google/uuid"
)

type preparedTrack struct {
	track       rekordbox.Track
	rel, hash   string
	info        os.FileInfo
	files       []analysisFile
	art         *analysisFileExport
	audio       *compat.AudioInfo
	audioErr    error
	uninspected bool
	// broken is why the track's analysis or artwork could not be found (#53).
	broken error
	// late tracks could not be read within this import (cut-off or file error):
	// imported as Fast imports them and left for the next import.
	late bool
}

// Seams for slow-media tests: the audio file stat and MP3 header read.
var (
	statFile    = os.Stat
	audioHeader = compat.AudioHeader
)

// prepareTracks stats and fingerprints every track. A track whose Engine path is
// in done keeps that fingerprint unread, so the import leaves it unchanged.
// Past cutoff (zero: none) the tracks left are not stat'ed or opened but marked
// late, after at least one track has been prepared: on a slow stick the stats
// and MP3 header reads of thousands of tracks used up the whole mount budget,
// and Sync then deferred every track on every mount (r3 review).
func prepareTracks(ctx context.Context, res *compat.Resolver, l *rekordbox.Library, cutoff time.Time, now func() time.Time, done map[string]string) ([]preparedTrack, error) {
	prepared := make([]preparedTrack, 0, len(l.Tracks))
	paths := map[string]uint32{}
	ready, late := 0, false
	for _, t := range l.Tracks {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		// A finished track is found under the export's own spelling without a
		// directory lookup: on a slow stick resolving thousands of finished
		// tracks used up the time meant for the others.
		guess, e := compat.CleanRelative(t.Path)
		if e != nil {
			return nil, fmt.Errorf("track %d: %w", t.ID, e)
		}
		if done != nil {
			if fp, ok := done["../"+guess]; ok {
				if other, ok := paths[guess]; ok {
					return nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", other, t.ID, guess)
				}
				paths[guess] = t.ID
				prepared = append(prepared, preparedTrack{track: t, rel: guess, hash: fp, info: exportFileInfo{size: t.FileSize}})
				continue
			}
		}
		var p preparedTrack
		rel, e := res.ResolveContext(ctx, t.Path)
		if e != nil {
			// A failed directory lookup cannot verify the path's identity.
			if !errors.Is(e, os.ErrNotExist) {
				return nil, fmt.Errorf("track %d: %w", t.ID, e)
			}
			if p, e = unreadableAudio(t, guess, e); e != nil {
				return nil, fmt.Errorf("track %d: %w", t.ID, e)
			}
			rel = guess
		}
		if other, ok := paths[rel]; ok {
			return nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", other, t.ID, rel)
		}
		paths[rel] = t.ID
		if fp, ok := done["../"+rel]; ok {
			prepared = append(prepared, preparedTrack{track: t, rel: rel, hash: fp, info: exportFileInfo{size: t.FileSize}})
			continue
		}
		if p.audioErr != nil {
			prepared = append(prepared, p)
			continue
		}
		// Once past the cut-off, every track left is late: no clock reading per track.
		if late = late || ready > 0 && !cutoff.IsZero() && now().After(cutoff); late {
			prepared = append(prepared, preparedTrack{track: t, rel: rel, hash: "fast:" + t.RecordHash, info: exportFileInfo{size: t.FileSize}, late: true})
			continue
		}
		p, e = prepareOne(res, t, rel, cutoff, now, ready > 0)
		if e != nil {
			return nil, e
		}
		if !p.late {
			ready++
		}
		prepared = append(prepared, p)
	}
	return prepared, nil
}

// Keep an unreadable file export-only and retry it on a later mount. Path
// validation, ambiguous names and cancellation still abort the import.
func unreadableAudio(t rekordbox.Track, rel string, err error) (preparedTrack, error) {
	var fileErr *os.PathError
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		!errors.Is(err, os.ErrNotExist) && !errors.As(err, &fileErr) {
		return preparedTrack{}, err
	}
	return preparedTrack{track: t, rel: rel, hash: "fast:" + t.RecordHash, info: exportFileInfo{size: t.FileSize}, audioErr: err, broken: err, late: true}, nil
}

// prepareOne stats and fingerprints one track. Past cutoff, when mayDefer, an
// MP3 is left uninspected instead of its header being read.
func prepareOne(res *compat.Resolver, t rekordbox.Track, rel string, cutoff time.Time, now func() time.Time, mayDefer bool) (preparedTrack, error) {
	full := res.FullPath(rel)
	st, e := statFile(full)
	if e != nil {
		return unreadableAudio(t, rel, e)
	}
	if !st.Mode().IsRegular() {
		return preparedTrack{}, fmt.Errorf("audio not regular: %s", rel)
	}
	files, art, broken := sourceFiles(res, t)
	fp := fingerprint{Encoding: performanceEncoding, Record: t.RecordHash + ":" + rekordbox.RecordHash(t), ResolvedPath: rel, Size: st.Size(), MTime: st.ModTime().UnixNano(), Artwork: art}
	for _, f := range files {
		fp.Analysis = append(fp.Analysis, analysisFileExport{f.rel, f.size, f.mtime})
	}
	var (
		audio       *compat.AudioInfo
		audioErr    error
		uninspected bool
	)
	if strings.EqualFold(path.Ext(rel), ".mp3") {
		if mayDefer && !cutoff.IsZero() && now().After(cutoff) {
			uninspected = true
		} else if info, err := audioHeader(full); err == nil {
			fp.MP3Offset = info.OffsetClass
			audio = &info
		} else {
			if p, e := unreadableAudio(t, rel, err); e == nil {
				return p, nil
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return preparedTrack{}, err
			}
			audioErr = err
		}
	}
	return preparedTrack{
		track: t, rel: rel, hash: digest(fp), info: st,
		files: files, art: art, audio: audio, audioErr: audioErr,
		uninspected: uninspected, broken: broken,
	}, nil
}

// exportFileInfo stands in for a stat in fast mode, from what the export recorded.
type exportFileInfo struct {
	os.FileInfo
	size int64
}

func (f exportFileInfo) Size() int64        { return f.size }
func (f exportFileInfo) ModTime() time.Time { return time.Time{} }

// prepareFast trusts the export's metadata without reading audio, but stores the
// spelling the volume actually uses: case and NFC/NFD differences are resolved
// from directory entries, as in a full import (#6). A path that cannot be
// resolved (missing file) keeps the export spelling; one that tries to leave
// the volume is dropped.
func prepareFast(ctx context.Context, res *compat.Resolver, l *rekordbox.Library) ([]preparedTrack, error) {
	prepared := make([]preparedTrack, 0, len(l.Tracks))
	paths := map[string]bool{}
	for _, t := range l.Tracks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel := strings.TrimLeft(path.Clean("/"+t.Path), "/")
		if rel == "" || t.Path == "" || strings.HasPrefix(rel, "PIONEER/") || strings.HasPrefix(rel, ".PIONEER/") {
			continue
		}
		if resolved, e := res.ResolveContext(ctx, t.Path); e == nil {
			rel = resolved
		}
		if paths[rel] {
			continue
		}
		paths[rel] = true
		prepared = append(prepared, preparedTrack{track: t, rel: rel, hash: "fast:" + t.RecordHash, info: exportFileInfo{size: t.FileSize}})
	}
	return prepared, ctx.Err()
}

// Stage every moved owned row first, so even two source tracks swapping paths
// can retain their Engine IDs without violating Track's immediate UNIQUE(path).
// Both temporary paths and the final rename are in the import transaction.
func stageOwnedMoves(ctx context.Context, tx *sql.Tx, prepared []preparedTrack, stagedOnly bool) (map[uint32]int64, error) {
	targets := map[uint32]*preparedTrack{}
	byPath := map[string]*preparedTrack{}
	for i := range prepared {
		p := &prepared[i]
		targets[p.track.ID] = p
		byPath["../"+p.rel] = p
	}
	// Legacy rows can share one import-wide pdbImportKey value. Our saved
	// source record retains the per-track ID, including across moves/swaps.
	rows, e := tx.QueryContext(ctx, `SELECT s.trackId,t.pdbImportKey,s.path,s.fingerprint,CASE WHEN json_valid(CAST(a.sourceJSON AS TEXT)) THEN json_extract(CAST(a.sourceJSON AS TEXT),'$.Track.ID') END FROM BridgeState s JOIN Track t ON t.id=s.trackId LEFT JOIN BridgeAnalysis a ON a.trackId=s.trackId`)
	if e != nil {
		return nil, e
	}
	type move struct {
		id     int64
		source uint32
	}
	var moves []move
	var removed []int64
	owned := map[uint32]int64{}
	type ownedTrack struct {
		id          int64
		source      uint32
		oldPath, fp string
		recorded    sql.NullInt64
	}
	var tracks []ownedTrack
	counts := map[uint32]int{}
	for rows.Next() {
		var t ownedTrack
		if e = rows.Scan(&t.id, &t.source, &t.oldPath, &t.fp, &t.recorded); e != nil {
			rows.Close()
			return nil, e
		}
		if t.recorded.Valid {
			t.source = uint32(t.recorded.Int64)
		}
		tracks = append(tracks, t)
		counts[t.source]++
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for _, t := range tracks {
		id, source, oldPath, fp := t.id, t.source, t.oldPath, t.fp
		if stagedOnly && source < 1<<24 && targets[source] == nil && byPath[oldPath] == nil {
			continue // a remote-only import must not identify or remove absent local sources
		}
		// Fast/capped or damaged state may have no saved source ID. A unique
		// owned path still identifies a row whose shared import key does not.
		if !t.recorded.Valid && (counts[source] > 1 || targets[source] == nil) {
			if p := byPath[oldPath]; p != nil {
				source = p.track.ID
			} else if counts[source] > 1 {
				return nil, fmt.Errorf("cannot identify bridge track %d with shared source ID %d", id, source)
			}
		}
		p, ok := targets[source]
		if !ok {
			if stagedOnly && source < 1<<24 {
				continue
			}
			removed = append(removed, id)
			continue
		}
		if _, duplicate := owned[source]; duplicate {
			rows.Close()
			return nil, fmt.Errorf("ambiguous bridge source ID %d", source)
		}
		owned[source] = id
		// An unreadable re-export must not park a completed row and lose its
		// fingerprint (and deck edits). Its source ID identifies the old row.
		if p.late && p.audioErr != nil && fp != "" && !strings.HasPrefix(fp, "fast:") {
			p.rel, p.hash, p.late = strings.TrimPrefix(oldPath, "../"), fp, false
			p.uninspected = true
		}
		target := "../" + p.rel
		if target != oldPath {
			moves = append(moves, move{id, source})
		}
	}
	paths := map[string]uint32{}
	for _, p := range prepared {
		source, target := p.track.ID, "../"+p.rel
		if other, ok := paths[target]; ok {
			return nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", other, source, strings.TrimPrefix(target, "../"))
		}
		paths[target] = source
	}
	// Release obsolete paths before moving or inserting their replacements.
	for _, id := range removed {
		for _, query := range []string{
			`DELETE FROM PlaylistEntity WHERE trackId=? AND (databaseUuid IS NULL OR databaseUuid='' OR databaseUuid=(SELECT uuid FROM Information))`,
			`DELETE FROM BridgeState WHERE trackId=?`,
			`DELETE FROM BridgeAnalysis WHERE trackId=?`,
			`DELETE FROM Track WHERE id=?`,
		} {
			if _, e = tx.ExecContext(ctx, query, id); e != nil {
				return nil, e
			}
		}
	}
	prefix := ".rb-bridge-pending-" + uuid.NewString() + "/"
	for _, m := range moves {
		temporary := prefix + strconv.FormatInt(m.id, 10)
		if _, e = tx.ExecContext(ctx, `UPDATE Track SET path=? WHERE id=?`, temporary, m.id); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE BridgeState SET path=?,fingerprint='' WHERE trackId=?`, temporary, m.id); e != nil {
			return nil, e
		}
	}
	return owned, nil
}
