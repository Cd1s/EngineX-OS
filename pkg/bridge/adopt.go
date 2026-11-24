package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// adopt reads an earlier build's state into a snapshot of the library uuid.
func adopt(ctx context.Context, dir, uuid string) (*rbstate.Snapshot, error) {
	db, err := openRO(filepath.Join(dir, "m.db"), 5000)
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
	synced := `0,NULL`
	if has("BridgeSynced") {
		synced = `(SELECT CAST(y.lastEditTime AS INTEGER) FROM bridge.BridgeSynced y WHERE y.trackId=s.trackId),(SELECT y.digest FROM bridge.BridgeSynced y WHERE y.trackId=s.trackId)`
	}
	// A row imported in full with no record of what the import wrote is still the import's while
	// it has Engine's import mark, and the deck's once Engine loaded it.
	rows, err := db.QueryContext(ctx, `SELECT s.path,s.trackId,s.fingerprint,`+written+`,`+synced+`,
		(SELECT thirdPartySourceId IS NOT NULL FROM PerformanceData p WHERE p.trackId=s.trackId)
		FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId WHERE t.path=s.path`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p, fingerprint string
		var id int64
		var digest, syncedDigest []byte
		var edit sql.NullInt64
		var marked sql.NullBool
		if err = rows.Scan(&p, &id, &fingerprint, &digest, &edit, &syncedDigest, &marked); err != nil {
			rows.Close()
			return nil, err
		}
		rel := strings.TrimPrefix(p, "../")
		complete := fingerprint != "" && !strings.HasPrefix(fingerprint, "fast:")
		t := &rbstate.Track{Key: rbstate.FoldPath(rel), EngineID: id, Rel: rel, Resolved: complete, Rating: -1, Written: digest, Edit: edit.Int64, Synced: syncedDigest}
		if complete && digest == nil && marked.Valid && !marked.Bool {
			t.Perf = rbstate.PerfDeck
		}
		s.Tracks[t.Key] = t
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if has("BridgePlaylist") {
		lists, err := db.QueryContext(ctx, `SELECT b.listId FROM bridge.BridgePlaylist b JOIN Playlist p ON p.id=b.listId`)
		if err != nil {
			return nil, err
		}
		for lists.Next() {
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
