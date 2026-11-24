package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const maxPriorityRetries = 3

type priorityTracker struct {
	lastAckSeq uint64
	retries    map[string]int
}

func newPriorityTracker() *priorityTracker {
	return &priorityTracker{retries: map[string]int{}}
}

func priorityRetryKey(key string, seq uint64) string {
	return fmt.Sprintf("%s#%d", key, seq)
}

func (pt *priorityTracker) hasPending(mount string) bool {
	return HasPendingPriority(mount, pt.lastAckSeq)
}

type stageTrackStatus struct {
	trackID     int64
	fingerprint string
	sourceJSON  []byte
}

func readStageTrackStatuses(ctx context.Context, stage string) map[string]stageTrackStatus {
	out := map[string]stageTrackStatus{}
	mDB := filepath.Join(stage, "m.db")
	stateDB := filepath.Join(stage, stateName)
	if _, err := os.Stat(mDB); err != nil {
		return out
	}
	if _, err := os.Stat(stateDB); err != nil {
		return out
	}
	uri := url.URL{Scheme: "file", Path: mDB, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return out
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	stateURI := url.URL{Scheme: "file", Path: stateDB, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}
	if _, err := db.ExecContext(ctx, `ATTACH DATABASE ? AS bridge`, stateURI.String()); err != nil {
		return out
	}
	rows, err := db.QueryContext(ctx, `SELECT s.path, s.trackId, s.fingerprint, a.sourceJSON
		FROM bridge.BridgeState s
		LEFT JOIN bridge.BridgeAnalysis a ON a.trackId = s.trackId`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p, fp string
		var id int64
		var src []byte
		if rows.Scan(&p, &id, &fp, &src) == nil {
			out[foldPriorityPath(p)] = stageTrackStatus{
				trackID:     id,
				fingerprint: fp,
				sourceJSON:  append([]byte(nil), src...),
			}
		}
	}
	return out
}

// servicePriority consumes any pending priority items for mount before the
// normal background work list. Fast or unread tracks are imported first in a
// dedicated minimal transaction; already-imported RBA2 tracks validate their
// referenced .2EX/.EXT files before acknowledgement. Missing paths, unmounts,
// and library-in-use results leave the item unacknowledged for bounded retry.
func (pt *priorityTracker) service(
	ctx context.Context,
	mount, stage, stamp string,
	r *compat.Resolver,
	l *rekordbox.Library,
	stagedOnly, legacy, legacyAAC, resume bool,
	onFinished func(int),
) (changed bool, err error) {
	pending, maxSeq := PendingPriority(mount)
	if len(pending) == 0 {
		if maxSeq > pt.lastAckSeq {
			pt.lastAckSeq = maxSeq
		}
		return false, nil
	}
	byFold := make(map[string]rekordbox.Track, len(l.Tracks))
	for _, t := range l.Tracks {
		if rel, e := compat.CleanRelative(t.Path); e == nil {
			byFold[foldPriorityPath("../"+rel)] = t
		}
	}
	statuses := readStageTrackStatuses(ctx, stage)
	refsByID := enginedb.ExportRefs(l)

	var toImport []string
	var importItems []PriorityItem
	var validateItems []PriorityItem
	actionable := 0
	for _, item := range pending {
		key := foldPriorityPath(item.Path)
		if key == "" {
			continue
		}
		retryKey := priorityRetryKey(key, item.Seq)
		if pt.retries[retryKey] >= maxPriorityRetries {
			continue
		}
		actionable++
		tr, ok := byFold[key]
		if !ok {
			pt.retries[retryKey]++
			continue
		}
		st, hasStatus := statuses[key]
		if !hasStatus || st.fingerprint == "" || strings.HasPrefix(st.fingerprint, "fast:") || legacy || (legacyAAC && compat.IsMP4AudioPath(tr.Path)) {
			toImport = append(toImport, item.Path)
			importItems = append(importItems, item)
		} else {
			validateItems = append(validateItems, item)
		}
	}
	if actionable == 0 {
		if maxSeq > pt.lastAckSeq {
			pt.lastAckSeq = maxSeq
		}
		return false, nil
	}

	var acked []PriorityItem
	if len(toImport) > 0 {
		start := time.Now()
		o := enginedb.Options{
			DatabaseDir:      stage,
			StagedOnly:       stagedOnly,
			LegacyOffsets:    legacy,
			LegacyAACOffsets: legacyAAC,
			Resume:           resume,
			PriorityPaths:    toImport,
			MaxTracks:        len(toImport),
			OnFinished:       onFinished,
		}
		res, syncErr := enginedb.SyncResolved(ctx, r, l, o)
		if syncErr != nil {
			if errors.Is(syncErr, enginedb.ErrLibraryInUse) || ctx.Err() != nil {
				for _, it := range importItems {
					pt.retries[priorityRetryKey(foldPriorityPath(it.Path), it.Seq)]++
				}
				return false, syncErr
			}
			return false, syncErr
		}
		changed = changed || res.Added+res.Updated > 0 || res.PlaylistsChanged
		after := readStageTrackStatuses(ctx, stage)
		for _, it := range importItems {
			key := foldPriorityPath(it.Path)
			if st, ok := after[key]; ok && st.fingerprint != "" && !strings.HasPrefix(st.fingerprint, "fast:") {
				acked = append(acked, it)
			} else {
				pt.retries[priorityRetryKey(key, it.Seq)]++
			}
		}
		if len(acked) > 0 {
			mirrorPrioritizedRGB(ctx, stage, acked, after)
			log.Printf("rb-bridge: prioritized %d track(s) (seq=%d) in %s", len(acked), maxSeq, time.Since(start).Round(time.Millisecond))
		}
	}

	// Already-imported RBA2 rows validate their referenced .2EX/.EXT before ack.
	for _, it := range validateItems {
		if ctx.Err() != nil {
			return changed, ctx.Err()
		}
		key := foldPriorityPath(it.Path)
		tr := byFold[key]
		files := refsByID[tr.ID]
		if len(files) == 0 {
			acked = append(acked, it)
			continue
		}
		ok, _, derr := enginedb.DetailServes(r, files, nil)
		if derr != nil {
			return changed, derr
		}
		if ok {
			acked = append(acked, it)
		} else {
			pt.retries[priorityRetryKey(key, it.Seq)]++
		}
	}

	if len(acked) > 0 {
		_ = AckPriority(mount, acked)
		for _, it := range acked {
			if it.Seq > pt.lastAckSeq {
				pt.lastAckSeq = it.Seq
			}
		}
	}
	// If every remaining pending item has exhausted retries, advance lastAckSeq
	// so ShouldYield does not spin on unresolvable paths.
	stillPending := false
	for _, item := range pending {
		key := foldPriorityPath(item.Path)
		if key == "" || pt.retries[priorityRetryKey(key, item.Seq)] >= maxPriorityRetries {
			continue
		}
		wasAcked := false
		for _, a := range acked {
			if a.Seq == item.Seq && foldPriorityPath(a.Path) == key {
				wasAcked = true
				break
			}
		}
		if !wasAcked {
			stillPending = true
			break
		}
	}
	if !stillPending && maxSeq > pt.lastAckSeq {
		pt.lastAckSeq = maxSeq
	}
	return changed, nil
}

// mirrorPrioritizedRGB atomically mirrors newly generated stage .rgb overview
// sidecars into the live Database2/OverviewData/<uuid>/ directory when live m.db
// shares the stage's Information.uuid and track mapping (read-only immutable
// check; zero SQLite locks or writes on live m.db).
func mirrorPrioritizedRGB(ctx context.Context, stage string, acked []PriorityItem, after map[string]stageTrackStatus) {
	liveDir := filepath.Dir(stage)
	if liveDir == stage {
		return
	}
	stageMDB := filepath.Join(stage, "m.db")
	liveMDB := filepath.Join(liveDir, "m.db")
	if _, err := os.Stat(liveMDB); err != nil {
		return
	}
	sDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: stageMDB, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}).String())
	if err != nil {
		return
	}
	defer sDB.Close()
	var stageUUID string
	if err := sDB.QueryRowContext(ctx, `SELECT uuid FROM Information WHERE id = 1`).Scan(&stageUUID); err != nil || stageUUID == "" {
		return
	}
	lDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: liveMDB, RawQuery: "mode=ro&immutable=1"}).String())
	if err != nil {
		return
	}
	defer lDB.Close()
	var liveUUID string
	if err := lDB.QueryRowContext(ctx, `SELECT uuid FROM Information WHERE id = 1`).Scan(&liveUUID); err != nil || liveUUID != stageUUID {
		return
	}
	rows, err := lDB.QueryContext(ctx, `SELECT id, path FROM Track WHERE path IS NOT NULL`)
	if err != nil {
		return
	}
	liveIDByFold := map[string]int64{}
	for rows.Next() {
		var id int64
		var p string
		if rows.Scan(&id, &p) == nil && p != "" {
			liveIDByFold[foldPriorityPath(p)] = id
		}
	}
	_ = rows.Close()
	for _, it := range acked {
		key := foldPriorityPath(it.Path)
		st, ok := after[key]
		if !ok || st.trackID <= 0 {
			continue
		}
		liveID, ok := liveIDByFold[key]
		if !ok || liveID <= 0 {
			continue
		}
		srcRGB := filepath.Join(stage, "OverviewData", stageUUID, fmt.Sprintf("%d.rgb", st.trackID))
		data, err := os.ReadFile(srcRGB)
		if err != nil || len(data) <= 12 {
			continue
		}
		dstRGB := filepath.Join(liveDir, "OverviewData", liveUUID, fmt.Sprintf("%d.rgb", liveID))
		if fi, err := os.Stat(dstRGB); err == nil && fi.Size() > 12 {
			continue
		}
		_ = enginedb.WriteRGBOverviewAtomic(liveDir, liveUUID, liveID, data)
	}
}
