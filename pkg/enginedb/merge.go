package enginedb

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
)

// An update of a converted library that does not fit in the mount's time is built in the
// background on a snapshot of it and replaces it at a later mount (#301). Engine keeps writing
// the live library meanwhile: play marks, the prepare list, cues edited on the deck, the
// analysis of tracks loaded without one and the cover art it finds. MergeLive carries those
// into the build first, with the rules an import in place follows, so replacing the library
// loses nothing Engine wrote. A track, playlist or playlist entry Engine added or removed is a
// change of structure the build cannot take: ErrMergeConflict, and the live library stays.

// BaseCounts is the size of a snapshot's tables: rows and sqlite_sequence.
type BaseCounts map[string][2]int64

// BaseTimes is Track.lastEditTime of a snapshot's tracks, by id, and the clock's second at
// the snapshot. Engine's triggers set the date whenever it writes a track's analysis, cues or
// loops, so a track whose date is the snapshot's has the snapshot's PerformanceData.
type BaseTimes struct {
	At    int64           `json:"at"`
	Times map[int64]int64 `json:"times"`
}

// SnapshotTimes is the BaseTimes of the database in dir.
func SnapshotTimes(ctx context.Context, dir string) (BaseTimes, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, "m.db"), RawQuery: "mode=ro"}).String())
	if err != nil {
		return BaseTimes{}, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id,COALESCE(lastEditTime,0) FROM Track`)
	if err != nil {
		return BaseTimes{}, err
	}
	defer rows.Close()
	times := BaseTimes{At: time.Now().Unix(), Times: map[int64]int64{}}
	for rows.Next() {
		var id, t int64
		if err = rows.Scan(&id, &t); err != nil {
			return BaseTimes{}, err
		}
		times.Times[id] = t
	}
	return times, rows.Err()
}

// The tables whose rows the build may add or remove itself, so Engine must not have.
var baseTables = []string{"Track", "Playlist", "PlaylistEntity", "AlbumArt"}

// Tables an import never writes: the live library's are taken whole.
var engineTables = []string{"PreparelistEntity", "Pack", "Smartlist"}

// The Track columns an import writes. Every other column is Engine's.
var importTrackColumns = map[string]bool{"id": true, "length": true, "bpm": true, "year": true, "path": true, "filename": true, "bitrate": true,
	"bpmAnalyzed": true, "albumArtId": true, "fileBytes": true, "title": true, "artist": true, "album": true, "genre": true, "comment": true,
	"label": true, "composer": true, "remixer": true, "key": true, "rating": true, "fileType": true, "isAnalyzed": true, "isAvailable": true,
	"isMetadataImported": true, "pdbImportKey": true, "dateCreated": true, "dateAdded": true, "originDatabaseUuid": true, "originTrackId": true,
	"lastEditTime": true}

// ErrMergeConflict: Engine added or removed rows the build cannot take.
var ErrMergeConflict = errors.New("Engine added or removed tracks or playlists since the update started")

// CountTables is the BaseCounts of the database in dir.
func CountTables(ctx context.Context, dir string) (BaseCounts, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, "m.db"), RawQuery: "mode=ro"}).String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return countTables(ctx, db, "main")
}

func countTables(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, schema string) (BaseCounts, error) {
	c := BaseCounts{}
	for _, t := range baseTables {
		var rows, seq int64
		if err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT (SELECT count(*) FROM %[1]s.%[2]s),COALESCE((SELECT seq FROM %[1]s.sqlite_sequence WHERE name='%[2]s'),0)`, schema, t)).Scan(&rows, &seq); err != nil {
			return nil, err
		}
		c[t] = [2]int64{rows, seq}
	}
	return c, nil
}

// MergeLive brings what Engine wrote to the live library in liveDir since the snapshot counted
// as base into the build in buildDir. Engine must not have the live library open.
//
// times are the snapshot's: PerformanceData is compared only for the tracks Engine has
// written since, and their number is returned. Comparing every track's reads both libraries'
// analysis off the stick, 190 MB on a stick of 6013 tracks, and the mount this runs in has
// 20 s (real stick, 2026-11-19: the hook killed the publish in it at every mount once a deck
// had loaded a track during the build). Without times (a stage of a build before this), or
// with a live library without Engine's trigger that dates those writes: every track.
//
// The date has whole seconds: a write in the second a track already had as its date does
// not show. So a track dated between two seconds before the snapshot and now counts as
// written; the clock has been through no other second since. Dates outside that, also the
// future ones of a deck whose clock is behind the library's, cannot have been met.
func MergeLive(ctx context.Context, buildDir, liveDir string, base BaseCounts, times BaseTimes) (int, error) {
	touched := 0
	err := mergeLive(ctx, buildDir, liveDir, base, times, &touched)
	return touched, err
}

func mergeLive(ctx context.Context, buildDir, liveDir string, base BaseCounts, times BaseTimes, touched *int) error {
	ro := func(p string) string { return (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String() }
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(buildDir, "m.db")}).String()+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	liveState := ro(filepath.Join(liveDir, stateFile))
	if _, err := os.Stat(filepath.Join(liveDir, stateFile)); errors.Is(err, os.ErrNotExist) {
		liveState = ":memory:" // a library rb-bridge never imported: no track of it is the import's
	}
	for _, a := range [][2]string{{ro(filepath.Join(liveDir, "m.db")), "live"}, {filepath.Join(buildDir, stateFile), "bridge"}, {liveState, "livebridge"}} {
		if _, err = db.ExecContext(ctx, `ATTACH DATABASE ? AS `+a[1], a[0]); err != nil {
			return err
		}
	}
	if liveState == ":memory:" {
		if _, err = db.ExecContext(ctx, strings.ReplaceAll(bridgeSchema, "bridge.", "livebridge.")); err != nil {
			return err
		}
	}
	// What the fast import left in the rows the build reads in full for the first time (#328).
	fastBase := false
	if _, err := os.Stat(filepath.Join(buildDir, FastBaseFile)); err == nil {
		if _, err = db.ExecContext(ctx, `ATTACH DATABASE ? AS fastbase`, ro(filepath.Join(buildDir, FastBaseFile))); err != nil {
			return err
		}
		fastBase = true
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	live, err := countTables(ctx, tx, "live")
	if err != nil {
		return err
	}
	for _, t := range baseTables {
		b, l := base[t], live[t]
		if t == "AlbumArt" { // Engine adds the covers it finds; they are carried below
			var added int64 // the covers Engine added are at the table's end: only they are read
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM live.AlbumArt WHERE id>?`, b[1]).Scan(&added); err != nil {
				return err
			}
			if l[0]-added != b[0] {
				return fmt.Errorf("%w: AlbumArt", ErrMergeConflict)
			}
			continue
		}
		if l != b {
			return fmt.Errorf("%w: %s %v, was %v", ErrMergeConflict, t, l, b)
		}
	}
	// Engine's own clock on the rows taken from it: no timestamp trigger while they are copied.
	var triggers []string
	rows, err := tx.QueryContext(ctx, `SELECT sql FROM main.sqlite_master WHERE type='trigger' AND name IN ('trigger_after_update_only_Track_timestamp','trigger_PerformanceData_after_update_Track_timestamp') ORDER BY rowid`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var text string
		if err = rows.Scan(&text); err != nil {
			rows.Close()
			return err
		}
		triggers = append(triggers, text)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	cols := func(table string, keep func(string) bool) ([]string, error) {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err = rows.Scan(&n); err != nil {
				return nil, err
			}
			if keep(n) {
				out = append(out, n)
			}
		}
		return out, rows.Err()
	}
	all := func(n string) bool { return n != "id" && n != "trackId" }
	trackAll, err := cols("Track", all)
	if err != nil {
		return err
	}
	trackEngine, err := cols("Track", func(n string) bool { return !importTrackColumns[n] })
	if err != nil {
		return err
	}
	perfAll, err := cols("PerformanceData", all)
	if err != nil {
		return err
	}
	// copyRows sets columns of main.table from the live row with the same key where any differs.
	copyRows := func(table, key string, columns []string, where string, args ...any) error {
		if len(columns) == 0 {
			return nil
		}
		set := make([]string, len(columns))
		differ := make([]string, len(columns))
		for i, c := range columns {
			value := `l."` + c + `"`
			if table == "Track" && c == "albumArtId" { // a cover Engine added has another id in the build
				value = `CASE WHEN l.albumArtId>` + fmt.Sprint(base["AlbumArt"][1]) + ` THEN (SELECT build FROM temp.artmap WHERE live=l.albumArtId) ELSE l.albumArtId END`
			}
			set[i] = `"` + c + `"=` + value
			differ[i] = value + ` IS NOT m."` + c + `"`
		}
		// where first: a row it leaves out is never read for the comparison.
		q := fmt.Sprintf(`UPDATE main.%[1]s AS m SET %[2]s FROM live.%[1]s AS l WHERE l.%[3]s=m.%[3]s AND (%[5]s) AND (%[4]s)`,
			table, strings.Join(set, ","), key, strings.Join(differ, " OR "), where)
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}
	steps := []func() error{
		func() error {
			for _, t := range triggers {
				name := strings.Fields(t)[2]
				if _, err := tx.ExecContext(ctx, `DROP TRIGGER main.`+name); err != nil {
					return err
				}
			}
			return nil
		},
		// The tracks the build changed: what it imported differs from the live library's.
		func() error {
			_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE changed AS SELECT s.trackId AS id FROM bridge.BridgeState s
				LEFT JOIN livebridge.BridgeState l ON l.trackId=s.trackId WHERE l.fingerprint IS NOT s.fingerprint OR l.path IS NOT s.path`)
			return err
		},
		// The tracks Engine has written since the snapshot.
		func() error {
			// Without Engine's trigger in the live library nothing dates a write: every track.
			var dated int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM live.sqlite_master WHERE type='trigger' AND name='trigger_PerformanceData_after_update_Track_timestamp'`).Scan(&dated); err != nil {
				return err
			}
			q, args := `CREATE TEMP TABLE touched AS SELECT id FROM live.Track`, []any(nil)
			if times.Times != nil && dated > 0 {
				if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE basetimes(id INTEGER PRIMARY KEY, t INTEGER NOT NULL)`); err != nil {
					return err
				}
				insert, err := tx.PrepareContext(ctx, `INSERT INTO temp.basetimes(id,t) VALUES(?,?)`)
				if err != nil {
					return err
				}
				defer insert.Close()
				for id, t := range times.Times {
					if _, err = insert.ExecContext(ctx, id, t); err != nil {
						return err
					}
				}
				q = `CREATE TEMP TABLE touched AS SELECT l.id FROM live.Track l LEFT JOIN temp.basetimes b ON b.id=l.id
					WHERE b.id IS NULL OR COALESCE(l.lastEditTime,0)<>b.t OR b.t BETWEEN ? AND ?`
				args = []any{times.At - 2, time.Now().Unix() + 2}
			}
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM temp.touched`).Scan(touched); err != nil {
				return err
			}
			if int64(*touched) >= live["Track"][0] && *touched > 0 {
				log.Printf("rb-bridge: the publish compares every one of the library's %d tracks (no dates to go by, or all of them written)", *touched)
			}
			return nil
		},
		// Covers Engine added, by their hash.
		func() error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO main.AlbumArt(hash,albumArt) SELECT l.hash,l.albumArt FROM live.AlbumArt l
				WHERE l.id>? AND NOT EXISTS (SELECT 1 FROM main.AlbumArt a WHERE a.hash IS l.hash)`, base["AlbumArt"][1]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE artmap AS SELECT l.id AS live,(SELECT a.id FROM main.AlbumArt a WHERE a.hash IS l.hash ORDER BY a.id LIMIT 1) AS build
				FROM live.AlbumArt l WHERE l.id>?`, base["AlbumArt"][1])
			return err
		},
		// Tracks the build left alone, and Engine's own: the live row.
		func() error {
			return copyRows("Track", "id", trackAll, `m.id NOT IN (SELECT id FROM temp.changed)`)
		},
		func() error {
			return copyRows("PerformanceData", "trackId", perfAll, `m.trackId IN (SELECT id FROM temp.touched) AND m.trackId NOT IN (SELECT id FROM temp.changed)`)
		},
		// Tracks the build changed: Engine's columns, and deck edits an import keeps (sync.go).
		func() error {
			return copyRows("Track", "id", trackEngine, `m.id IN (SELECT id FROM temp.changed)`)
		},
		func() error { return carryLiveEdits(ctx, tx) },
		func() error {
			if !fastBase {
				return nil
			}
			return carryFastEdits(ctx, tx)
		},
		func() error {
			_, err := tx.ExecContext(ctx, `UPDATE main.Track AS m SET lastEditTime=l.lastEditTime FROM live.Track AS l
				WHERE l.id=m.id AND m.id IN (SELECT id FROM temp.changed) AND l.lastEditTime>COALESCE(m.lastEditTime,0)`)
			return err
		},
		// Playlists and entries that are not the import's.
		func() error {
			// Not the sibling link: the build put the import's playlists beside the stick's
			// own, and the live link would break that chain (UNIQUE parentListId, nextListId).
			c, err := cols("Playlist", func(n string) bool { return all(n) && n != "nextListId" })
			if err != nil {
				return err
			}
			return copyRows("Playlist", "id", c, `m.id NOT IN (SELECT listId FROM bridge.BridgePlaylist)`)
		},
		func() error {
			c, err := cols("PlaylistEntity", all)
			if err != nil {
				return err
			}
			return copyRows("PlaylistEntity", "id", c, `m.listId NOT IN (SELECT listId FROM bridge.BridgePlaylist)`)
		},
		func() error {
			for _, t := range engineTables {
				where := ""
				if t == "PreparelistEntity" {
					where = ` WHERE trackId IN (SELECT id FROM main.Track)`
				}
				for _, q := range []string{`DELETE FROM main.` + t, `INSERT INTO main.` + t + ` SELECT * FROM live.` + t + where,
					`DELETE FROM main.sqlite_sequence WHERE name='` + t + `'`,
					`INSERT INTO main.sqlite_sequence(name,seq) SELECT name,seq FROM live.sqlite_sequence WHERE name='` + t + `'`} {
					if _, err := tx.ExecContext(ctx, q); err != nil {
						return fmt.Errorf("%s: %w", t, err)
					}
				}
			}
			return nil
		},
		func() error {
			_, err := tx.ExecContext(ctx, `UPDATE main.Information SET currentPlayedIndiciator=(SELECT currentPlayedIndiciator FROM live.Information)`)
			return err
		},
		func() error {
			for _, t := range triggers {
				if _, err := tx.ExecContext(ctx, t); err != nil {
					return err
				}
			}
			return nil
		},
	}
	for _, step := range steps {
		if err = step(); err != nil {
			return err
		}
	}
	return tx.Commit()
}
