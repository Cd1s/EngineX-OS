package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// Builds before the snapshot kept their bookkeeping in an SQLite file beside m.db and a set of
// marks. A library they imported is taken over once: its rows keep their Engine IDs, so what
// Engine made for them stays, and every track is staged again by the background job, which is
// what applies this build's rules; a row the deck changed keeps the deck's grid, cues and loops.
// The lists are written anew (they may now sit under the three folders).

// oldMarks are the files and directories of earlier builds in Database2.
var oldMarks = []string{oldStateName, oldStateName + "-journal", oldStateName + ".compact", ".rb-bridge-stamp", ".rb-bridge-pending",
	".rb-bridge-reread", ".rb-bridge-overview", ".rb-bridge-rgb-verified", ".rb-bridge.lock", ".rb-bridge-import", ".rb-bridge-publish"}

// adopt reads an earlier build's state into a snapshot of the library uuid.
func adopt(ctx context.Context, dir, uuid string) (*rbstate.Snapshot, error) {
	ro := func(p string) string { return (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String() }
	db, err := sql.Open("sqlite", ro(filepath.Join(dir, "m.db"))+"&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `ATTACH DATABASE ? AS bridge`, ro(filepath.Join(dir, oldStateName))); err != nil {
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
	rows, err := db.QueryContext(ctx, `SELECT s.path,s.trackId,s.fingerprint,`+written+` FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId WHERE t.path=s.path`)
	if err != nil {
		return nil, err
	}
	var unknown []*rbstate.Track // imported in full, with no record of what the import wrote
	for rows.Next() {
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
		if complete && digest == nil {
			unknown = append(unknown, t)
		}
		s.Tracks[t.Key] = t
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	// Without a record of what the import wrote, Engine's import mark says whether Engine has
	// loaded the row since: with the mark the row is still the import's, without it the deck's.
	for _, t := range unknown {
		var marked bool
		if err = db.QueryRowContext(ctx, `SELECT thirdPartySourceId IS NOT NULL FROM PerformanceData WHERE trackId=?`, t.EngineID).Scan(&marked); errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, err
		}
		if !marked {
			t.Perf = rbstate.PerfDeck
		}
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
	s.Pending.Verify = true
	return s, nil
}

// dropOldState removes what earlier builds left in Database2, each file pull-safe.
func dropOldState(dir string) {
	for _, name := range oldMarks {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			_ = rbstate.RemoveFile(p)
			continue
		}
		_ = filepath.WalkDir(p, func(file string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				_ = rbstate.RemoveFile(file)
			}
			return nil
		})
		_ = os.RemoveAll(p)
	}
}
