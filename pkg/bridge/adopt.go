package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// A library an earlier build imported (its state in an SQLite file beside m.db, no snapshot) is
// taken over once (design 1.9): rows keep their Engine IDs, every track is staged again under
// this build's rules, a row the deck changed keeps its grid, cues and loops, and the lists are
// written anew.
const oldStateName = ".rb-bridge-state.db"

// oldMarks are the files and directories of earlier builds in Database2.
var oldMarks = []string{oldStateName, oldStateName + "-journal", oldStateName + ".compact", ".rb-bridge-stamp", ".rb-bridge-pending",
	".rb-bridge-reread", ".rb-bridge-overview", ".rb-bridge-rgb-verified", ".rb-bridge.lock", ".rb-bridge-import", ".rb-bridge-publish"}

// adopt reads an earlier build's state in dir into a snapshot of the library uuid, whose file is
// library: the stick's m.db, or the next library made from it.
func adopt(ctx context.Context, library, dir, uuid string) (*rbstate.Snapshot, error) {
	db, err := openRO(library, 5000)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `ATTACH DATABASE ? AS bridge`, roURI(filepath.Join(dir, oldStateName))); err != nil {
		return nil, fmt.Errorf("state of an earlier build: %w", err)
	}
	has := func(table string) bool {
		var n int
		return db.QueryRowContext(ctx, `SELECT count(*) FROM bridge.sqlite_master WHERE type='table' AND name=?`, table).Scan(&n) == nil && n == 1
	}
	s := rbstate.New(uuid)
	if !has("BridgeState") {
		return s, nil
	}
	written := `NULL`
	if has("BridgeWritten") {
		written = `(SELECT digest FROM bridge.BridgeWritten w WHERE w.trackId=s.trackId)`
	}
	rows, err := db.QueryContext(ctx, `SELECT s.path,s.trackId,s.fingerprint,`+written+`
		FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId WHERE t.path=s.path`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		var p, fingerprint string
		var id int64
		var digest []byte
		if err = rows.Scan(&p, &id, &fingerprint, &digest); err != nil {
			rows.Close()
			return nil, err
		}
		rel := strings.TrimPrefix(p, "../")
		complete := fingerprint != "" && !strings.HasPrefix(fingerprint, "fast:")
		t := &rbstate.Track{Key: rbstate.FoldPath(rel), EngineID: id, Rel: rel, Resolved: complete, Rating: -1, Written: digest}
		s.Tracks[t.Key] = t
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	byID := make(map[int64]*rbstate.Track, len(s.Tracks))
	for _, t := range s.Tracks {
		byID[t.EngineID] = t
	}
	if has("BridgeFastBase") {
		fastRows, e := db.QueryContext(ctx, `SELECT f.trackId,f.record,f.quickCues,f.loops,f.active,s.fingerprint
			FROM bridge.BridgeFastBase f JOIN bridge.BridgeState s ON s.trackId=f.trackId WHERE s.fingerprint LIKE 'fast:%'`)
		if e != nil {
			return nil, e
		}
		for fastRows.Next() {
			if e = ctx.Err(); e != nil {
				fastRows.Close()
				return nil, e
			}
			var id, record int64
			var cues, loops []byte
			var active int64
			var fingerprint string
			if e = fastRows.Scan(&id, &record, &cues, &loops, &active, &fingerprint); e != nil {
				fastRows.Close()
				return nil, e
			}
			if t := byID[id]; t != nil && record == enginedb.FastBaseRecord(fingerprint) {
				t.FastCues, t.FastLoops, t.FastActive, t.FastKnown = cues, loops, uint32(active), true
			}
		}
		if e = fastRows.Close(); e != nil {
			return nil, e
		}
	}
	if has("BridgePlaylist") {
		lists, err := db.QueryContext(ctx, `SELECT b.listId FROM bridge.BridgePlaylist b JOIN Playlist p ON p.id=b.listId`)
		if err != nil {
			return nil, err
		}
		for lists.Next() {
			if err = ctx.Err(); err != nil {
				lists.Close()
				return nil, err
			}
			var id int64
			if err = lists.Scan(&id); err != nil {
				lists.Close()
				return nil, err
			}
			key := fmt.Sprintf("old:%d", id) // no list of the view has this key: removed, then written anew
			s.Lists[key] = &rbstate.List{Key: key, EngineID: id}
		}
		if err = lists.Close(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// dropOldState removes what earlier builds left in Database2, each file pull-safe.
func dropOldState(dir string) {
	for _, name := range oldMarks {
		p := filepath.Join(dir, name)
		_ = filepath.WalkDir(p, func(file string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				_ = rbstate.RemoveFile(file)
			}
			return nil
		})
		if st, err := os.Lstat(p); err == nil && st.IsDir() {
			_ = os.RemoveAll(p)
		}
	}
}
