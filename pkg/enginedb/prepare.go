package enginedb

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
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

// orderTracksForPrepare puts tracks matching priorityPaths first (in priority
// order) and, when sortByPath is true, sorts the remaining tracks by
// normalized volume-relative path without mutating l.Tracks.
func orderTracksForPrepare(tracks []rekordbox.Track, priorityPaths []string, sortByPath bool) []rekordbox.Track {
	if len(tracks) <= 1 || (len(priorityPaths) == 0 && !sortByPath) {
		return tracks
	}
	sortPath := func(s []rekordbox.Track) {
		if !sortByPath || len(s) <= 1 {
			return
		}
		type keyed struct {
			track rekordbox.Track
			key   string
		}
		items := make([]keyed, len(s))
		for i, t := range s {
			guess, err := compat.CleanRelative(t.Path)
			if err != nil {
				guess = t.Path
			}
			items[i] = keyed{track: t, key: foldPreparedPath(guess)}
		}
		slices.SortFunc(items, func(a, b keyed) int {
			if c := cmp.Compare(a.key, b.key); c != 0 {
				return c
			}
			return cmp.Compare(a.track.ID, b.track.ID)
		})
		for i, it := range items {
			s[i] = it.track
		}
	}
	rank := make(map[string]int, len(priorityPaths))
	for _, p := range priorityPaths {
		norm := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"), "../")
		norm = strings.TrimLeft(path.Clean("/"+norm), "/")
		if norm == "" || norm == "." {
			continue
		}
		k := foldPreparedPath("../" + norm)
		if _, exists := rank[k]; !exists {
			rank[k] = len(rank)
		}
	}
	if len(rank) == 0 {
		out := append([]rekordbox.Track(nil), tracks...)
		sortPath(out)
		return out
	}
	buckets := make([][]rekordbox.Track, len(rank))
	rest := make([]rekordbox.Track, 0, len(tracks))
	matched := 0
	for _, t := range tracks {
		guess, err := compat.CleanRelative(t.Path)
		if err == nil {
			if idx, ok := rank[foldPreparedPath("../"+guess)]; ok {
				buckets[idx] = append(buckets[idx], t)
				matched++
				continue
			}
		}
		rest = append(rest, t)
	}
	sortPath(rest)
	if matched == 0 {
		return rest
	}
	ordered := make([]rekordbox.Track, 0, len(tracks))
	for _, b := range buckets {
		ordered = append(ordered, b...)
	}
	return append(ordered, rest...)
}

// prepareTracks stats and fingerprints every track. A track whose Engine path is
// in done keeps that fingerprint unread, so the import leaves it unchanged.
// Past cutoff (zero: none), maxTracks (zero: unlimited), or shouldYield, the
// tracks left are not stat'ed or opened but marked late, after at least one
// track has been prepared: on a slow stick the stats and MP3 header reads of
// thousands of tracks used up the whole mount budget, and Sync then deferred
// every track on every mount (r3 review).
func prepareTracks(ctx context.Context, res *compat.Resolver, l *rekordbox.Library, cutoff time.Time, now func() time.Time, done map[string]string) ([]preparedTrack, error) {
	return prepareTracksOpts(ctx, res, l, cutoff, now, done, nil, false, 0, nil)
}

func prepareTracksOpts(ctx context.Context, res *compat.Resolver, l *rekordbox.Library, cutoff time.Time, now func() time.Time, done map[string]string, priorityPaths []string, sortByPath bool, maxTracks int, shouldYield func() bool) ([]preparedTrack, error) {
	tracks := orderTracksForPrepare(l.Tracks, priorityPaths, sortByPath)
	prepared := make([]preparedTrack, 0, len(tracks))
	paths := map[string]uint32{}
	ready, late := 0, false
	for _, t := range tracks {
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
		// Once past the cut-off, maxTracks, or an incoming yield signal, every
		// track left is late: no clock reading per track.
		if !late && ready > 0 {
			if (!cutoff.IsZero() && now().After(cutoff)) ||
				(maxTracks > 0 && ready >= maxTracks) ||
				(shouldYield != nil && shouldYield()) {
				late = true
			}
		}
		if late {
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
	if strings.EqualFold(path.Ext(rel), ".mp3") || compat.IsMP4AudioPath(rel) {
		if mayDefer && !cutoff.IsZero() && now().After(cutoff) {
			uninspected = true
		} else if info, err := audioHeader(full); err == nil {
			fp.MP3Offset, fp.OffsetRule = info.OffsetClass, offsetRule
			if info.Format == "aac" && info.StartOffsetSamples > 0 {
				fp.AACOffset = strconv.Itoa(info.StartOffsetSamples)
			}
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
func prepareFast(ctx context.Context, res *compat.Resolver, l *rekordbox.Library, trust bool) ([]preparedTrack, error) {
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
		if !trust {
			if resolved, e := res.ResolveContext(ctx, t.Path); e == nil {
				rel = resolved
			}
		}
		if paths[rel] {
			continue
		}
		paths[rel] = true
		prepared = append(prepared, preparedTrack{track: t, rel: rel, hash: "fast:" + t.RecordHash, info: exportFileInfo{size: t.FileSize}})
	}
	return prepared, ctx.Err()
}

type existingState struct {
	id        int64
	fp        string
	storedBPM float64
}

// Stage every moved owned row first, so even two source tracks swapping paths
// can retain their Engine IDs without violating Track's immediate UNIQUE(path).
// Both temporary paths and the final rename are in the import transaction.
func stageOwnedMoves(ctx context.Context, tx *sql.Tx, prepared []preparedTrack, stagedOnly bool) (map[uint32]int64, map[int64]existingState, map[string]existingState, error) {
	targets := map[uint32]*preparedTrack{}
	byPath := map[string]*preparedTrack{}
	for i := range prepared {
		p := &prepared[i]
		targets[p.track.ID] = p
		byPath[foldPreparedPath("../"+p.rel)] = p
	}
	// Legacy rows can share one import-wide pdbImportKey value. Our saved
	// source record retains the per-track ID, including across moves/swaps.
	rows, e := tx.QueryContext(ctx, `SELECT s.trackId,COALESCE(t.pdbImportKey,0),s.path,s.fingerprint,COALESCE(t.bpmAnalyzed,0) FROM BridgeState s JOIN Track t ON t.id=s.trackId`)
	if e != nil {
		return nil, nil, nil, e
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
		storedBPM   float64
		recorded    sql.NullInt64
	}
	var tracks []ownedTrack
	counts := map[uint32]int{}
	existingByPath := map[string]int64{}
	for rows.Next() {
		var t ownedTrack
		if e = rows.Scan(&t.id, &t.source, &t.oldPath, &t.fp, &t.storedBPM); e != nil {
			rows.Close()
			return nil, nil, nil, e
		}
		tracks = append(tracks, t)
		counts[t.source]++
		key := foldPreparedPath(t.oldPath)
		if other, ok := existingByPath[key]; ok && other != t.id {
			return nil, nil, nil, fmt.Errorf("owned tracks %d and %d share audio path %q", other, t.id, t.oldPath)
		}
		existingByPath[key] = t.id
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, nil, nil, e
	}
	var needAnalysis []int
	for i := range tracks {
		t := &tracks[i]
		// A unique source ID and matching owned path already establish identity.
		// Read saved waveforms only to recover an ambiguous/moved source, not
		// on every insert of a partially prepared large library (#253).
		if p := targets[t.source]; p != nil && counts[t.source] == 1 && "../"+p.rel == t.oldPath {
			continue
		}
		needAnalysis = append(needAnalysis, i)
	}
	var bulkAnalysis map[int64][]byte
	if len(needAnalysis) > 16 {
		arows, aerr := tx.QueryContext(ctx, `SELECT trackId, substr(sourceJSON,1,4096) FROM BridgeAnalysis`)
		if aerr != nil {
			return nil, nil, nil, aerr
		}
		bulkAnalysis = make(map[int64][]byte, len(needAnalysis))
		for arows.Next() {
			var tid int64
			var packed []byte
			if aerr = arows.Scan(&tid, &packed); aerr != nil {
				arows.Close()
				return nil, nil, nil, aerr
			}
			bulkAnalysis[tid] = append([]byte(nil), packed...)
		}
		aerr = arows.Err()
		arows.Close()
		if aerr != nil {
			return nil, nil, nil, aerr
		}
	}
	for _, idx := range needAnalysis {
		t := &tracks[idx]
		var packed []byte
		if bulkAnalysis != nil {
			packed = bulkAnalysis[t.id]
		} else if e = tx.QueryRowContext(ctx, `SELECT substr(sourceJSON,1,4096) FROM BridgeAnalysis WHERE trackId=?`, t.id).Scan(&packed); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, nil, nil, e
		}
		source, err := analysisSourceID(packed)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("source record for track %d: %w", t.id, err)
		}
		if source != 0 {
			t.recorded = sql.NullInt64{Int64: int64(source), Valid: true}
		}
		if t.recorded.Valid {
			t.source = uint32(t.recorded.Int64)
		}
	}
	clear(counts)
	for _, t := range tracks {
		counts[t.source]++
	}
	sourceOldPath := map[uint32]string{}
	for _, t := range tracks {
		if _, exists := sourceOldPath[t.source]; !exists {
			sourceOldPath[t.source] = t.oldPath
		}
	}
	var nativeTrackIDs map[int64]bool
	if len(tracks) > 0 {
		var hasNativePlaylist bool
		if nerr := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM Playlist p WHERE NOT EXISTS (SELECT 1 FROM BridgePlaylist b WHERE b.listId=p.id))`).Scan(&hasNativePlaylist); nerr != nil {
			return nil, nil, nil, nerr
		}
		if hasNativePlaylist {
			nrows, nerr := tx.QueryContext(ctx, `SELECT DISTINCT e.trackId FROM PlaylistEntity e WHERE e.trackId IS NOT NULL AND (e.databaseUuid IS NULL OR e.databaseUuid='' OR e.databaseUuid=(SELECT uuid FROM Information)) AND NOT EXISTS (SELECT 1 FROM BridgePlaylist b WHERE b.listId=e.listId)`)
			if nerr != nil {
				return nil, nil, nil, nerr
			}
			nativeTrackIDs = map[int64]bool{}
			for nrows.Next() {
				var tid int64
				if err := nrows.Scan(&tid); err != nil {
					nrows.Close()
					return nil, nil, nil, err
				}
				nativeTrackIDs[tid] = true
			}
			if err := nrows.Err(); err != nil {
				nrows.Close()
				return nil, nil, nil, err
			}
			nrows.Close()
		}
	}
	rawPathTargets := make([]*preparedTrack, len(tracks))
	pathTargets := make([]*preparedTrack, len(tracks))
	pathCycles := make([]bool, len(tracks))
	for i, t := range tracks {
		raw := byPath[foldPreparedPath(t.oldPath)]
		rawPathTargets[i] = raw
		pt := raw
		if pt != nil && nativeTrackIDs[existingByPath[foldPreparedPath(t.oldPath)]] {
			// A native Engine playlist owns this row. Reusing it for a different
			// export source would silently retarget the DJ's membership.
			pt = nil
		}
		if pt != nil {
			candidate := pt.track.ID
			if otherOldPath, ok := sourceOldPath[candidate]; ok {
				if back := byPath[foldPreparedPath(otherOldPath)]; back != nil && back.track.ID == t.source {
					pathCycles[i] = true
				}
			}
		}
		pathTargets[i] = pt
	}
	// Pass 1: normalized path identity wins before any source-ID fallback, so a
	// re-export that removes one track and shifts its numeric ID onto another
	// surviving track keeps the surviving track's Engine ID and cleanly removes
	// the deleted track regardless of row order.
	claimedByPath := map[uint32]int64{}
	retainedUnreadable := map[int64]bool{}
	handled := make([]bool, len(tracks))
	for i, t := range tracks {
		pt := pathTargets[i]
		if pt == nil || pathCycles[i] {
			continue
		}
		source := pt.track.ID
		if source != t.source {
			if oldSelf := targets[t.source]; oldSelf != nil && oldSelf.late && oldSelf.audioErr != nil && t.fp != "" && !strings.HasPrefix(t.fp, "fast:") {
				retainedUnreadable[t.id] = true
			}
		}
		if _, duplicate := owned[source]; duplicate {
			if other, exists := existingByPath[foldPreparedPath("../"+pt.rel)]; exists && other != t.id {
				return nil, nil, nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", t.id, other, pt.rel)
			}
			return nil, nil, nil, fmt.Errorf("ambiguous bridge source ID %d", source)
		}
		owned[source] = t.id
		claimedByPath[source] = t.id
		handled[i] = true
		if pt.late && pt.audioErr != nil && t.fp != "" && !strings.HasPrefix(t.fp, "fast:") {
			pt.rel, pt.hash, pt.late = strings.TrimPrefix(t.oldPath, "../"), t.fp, false
			pt.uninspected = true
		}
		if "../"+pt.rel != t.oldPath {
			moves = append(moves, move{t.id, source})
		}
	}
	// Pass 2: handle path-swap cycles, genuine file moves, native-protected rows,
	// and removals.
	for i, t := range tracks {
		if handled[i] {
			continue
		}
		id, source, oldPath, fp := t.id, t.source, t.oldPath, t.fp
		pathTarget, pathCycle := pathTargets[i], pathCycles[i]
		if stagedOnly && source < 1<<24 && targets[source] == nil && pathTarget == nil {
			continue // a remote-only import must not identify or remove absent local sources
		}
		// If this track's own path was deleted from the export and its old numeric
		// source ID was reassigned to a surviving track that kept its path in Pass 1,
		// this row is removed rather than moved onto the surviving track's path.
		if owner := claimedByPath[source]; rawPathTargets[i] == nil && owner != 0 && !retainedUnreadable[owner] {
			if stagedOnly && source < 1<<24 {
				continue
			}
			removed = append(removed, id)
			continue
		}
		// Fast/capped or damaged state may have no saved source ID. A unique
		// owned path still identifies a row whose shared import key does not.
		if !t.recorded.Valid && (counts[source] > 1 || targets[source] == nil) {
			if p := pathTarget; p != nil {
				source = p.track.ID
			} else if counts[source] > 1 {
				if target := targets[source]; target != nil {
					if other, exists := existingByPath[foldPreparedPath("../"+target.rel)]; exists && other != id {
						return nil, nil, nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", id, other, target.rel)
					}
				}
				return nil, nil, nil, fmt.Errorf("cannot identify bridge track %d with shared source ID %d", id, source)
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
			if other, exists := existingByPath[foldPreparedPath("../"+p.rel)]; exists && other != id {
				return nil, nil, nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", id, other, p.rel)
			}
			return nil, nil, nil, fmt.Errorf("ambiguous bridge source ID %d", source)
		}
		owned[source] = id
		targetPath := foldPreparedPath("../" + p.rel)
		if other, ok := existingByPath[targetPath]; ok && other != id && !pathCycle && pathTarget == nil {
			return nil, nil, nil, fmt.Errorf("track %d would move onto path already owned by track %d", id, other)
		}
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
			return nil, nil, nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", other, source, strings.TrimPrefix(target, "../"))
		}
		paths[target] = source
	}
	// Release obsolete paths before moving or inserting their replacements.
	removedSet := make(map[int64]bool, len(removed))
	for _, id := range removed {
		removedSet[id] = true
		for _, query := range []string{
			`DELETE FROM PlaylistEntity WHERE trackId=? AND (databaseUuid IS NULL OR databaseUuid='' OR databaseUuid=(SELECT uuid FROM Information))`,
			`DELETE FROM BridgeState WHERE trackId=?`,
			`DELETE FROM BridgeAnalysis WHERE trackId=?`,
			`DELETE FROM BridgeWritten WHERE trackId=?`,
			`DELETE FROM Track WHERE id=?`,
		} {
			if _, e = tx.ExecContext(ctx, query, id); e != nil {
				return nil, nil, nil, e
			}
		}
	}
	prefix := ".rb-bridge-pending-" + uuid.NewString() + "/"
	movedTemp := make(map[int64]string, len(moves))
	for _, m := range moves {
		temporary := prefix + strconv.FormatInt(m.id, 10)
		movedTemp[m.id] = temporary
		if _, e = tx.ExecContext(ctx, `UPDATE Track SET path=? WHERE id=?`, temporary, m.id); e != nil {
			return nil, nil, nil, e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE BridgeState SET path=?,fingerprint='' WHERE trackId=?`, temporary, m.id); e != nil {
			return nil, nil, nil, e
		}
	}
	stateByID := make(map[int64]existingState, len(tracks))
	stateByPath := make(map[string]existingState, len(tracks))
	for _, t := range tracks {
		if removedSet[t.id] {
			continue
		}
		p, fp := t.oldPath, t.fp
		if tmp, moved := movedTemp[t.id]; moved {
			p, fp = tmp, ""
		}
		st := existingState{id: t.id, fp: fp, storedBPM: t.storedBPM}
		stateByID[t.id] = st
		stateByPath[p] = st
	}
	return owned, stateByID, stateByPath, nil
}

func foldPreparedPath(p string) string {
	return strings.ToLower(path.Clean(strings.ReplaceAll(p, "\\", "/")))
}
