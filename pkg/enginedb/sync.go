package enginedb

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Cd1s/sc5000-custom-os/pkg/compat"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

const bridgeSchema = `
CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL REFERENCES Track(id) ON DELETE CASCADE, fingerprint TEXT NOT NULL);
CREATE TABLE BridgeAnalysis(trackId INTEGER PRIMARY KEY REFERENCES Track(id) ON DELETE CASCADE, sourceJSON BLOB NOT NULL, audioJSON BLOB NOT NULL);
CREATE TABLE BridgePlaylist(sourceId INTEGER PRIMARY KEY, listId INTEGER NOT NULL UNIQUE REFERENCES Playlist(id) ON DELETE CASCADE);
CREATE TABLE BridgeLibrary(key TEXT PRIMARY KEY, value BLOB);
`

type Options struct {
	SmartMatch   bool
	CurrentTrack uint32
}
type Result struct {
	Added, Updated, Unchanged int
	PlaylistsChanged          bool
	Bypassed                  bool
}

// safeFile refuses redirection into PIONEER through symlinks or hard links.
func safeFile(p string) error {
	st, e := os.Lstat(p)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("unsafe database file %s", p)
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && s.Nlink != 1 {
		return fmt.Errorf("hard-linked database file %s", p)
	}
	return nil
}

// open keeps schema initialization in the same transaction as the import.
func open(ctx context.Context, p string) (*sql.DB, *sql.Tx, error) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if e := safeFile(p + suffix); e != nil {
			return nil, nil, e
		}
	}
	u := url.URL{Scheme: "file", Path: p}
	db, e := sql.Open("sqlite", u.String()+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if e != nil {
		return nil, nil, e
	}
	db.SetMaxOpenConns(1)
	var tx *sql.Tx
	success := false
	defer func() {
		if !success {
			if tx != nil {
				tx.Rollback()
			}
			db.Close()
		}
	}()
	tx, e = db.BeginTx(ctx, nil)
	if e != nil {
		return nil, nil, e
	}
	var count int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&count); e != nil {
		return nil, nil, e
	}
	if count == 0 {
		if _, e = tx.ExecContext(ctx, schema); e != nil {
			return nil, nil, fmt.Errorf("engine schema: %w", e)
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch,currentPlayedIndiciator,lastRekordBoxLibraryImportReadCounter) VALUES(?,3,0,2,0,0)`, uuid.NewString())
	} else {
		var major, minor, patch, n int
		e = tx.QueryRowContext(ctx, `SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information`).Scan(&major, &minor, &patch)
		if e == nil {
			e = tx.QueryRowContext(ctx, `SELECT count(*) FROM Information`).Scan(&n)
		}
		if e == nil && (n != 1 || major != 3 || minor != 0 || patch != 2) {
			e = fmt.Errorf("unsupported Engine database schema %d.%d.%d", major, minor, patch)
		}
	}
	if e != nil {
		return nil, nil, e
	}
	var n int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='BridgeState'`).Scan(&n); e != nil {
		return nil, nil, e
	}
	if n == 0 {
		if _, e = tx.ExecContext(ctx, bridgeSchema); e != nil {
			return nil, nil, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO BridgeLibrary(key,value) VALUES('version','1')`); e != nil {
			return nil, nil, e
		}
	}
	var v string
	if e = tx.QueryRowContext(ctx, `SELECT value FROM BridgeLibrary WHERE key='version'`).Scan(&v); e != nil {
		return nil, nil, e
	}
	if v != "1" {
		return nil, nil, fmt.Errorf("unsupported bridge schema %s", v)
	}
	success = true
	return db, tx, nil
}

type analysisFile struct {
	rel         string
	size, mtime int64
}
type fingerprint struct {
	Record       string
	ResolvedPath string
	Size, MTime  int64
	Analysis     []analysisFileExport
	Artwork      *analysisFileExport
}
type analysisFileExport struct {
	Path        string
	Size, MTime int64
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func sourceFiles(res *compat.Resolver, t rekordbox.Track) ([]analysisFile, *analysisFileExport, error) {
	var files []analysisFile
	if t.AnalysisPath != "" {
		base := strings.TrimSuffix(t.AnalysisPath, path.Ext(t.AnalysisPath))
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			rel, e := res.Resolve(base + ext)
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				return nil, nil, e
			}
			s, e := os.Stat(res.FullPath(rel))
			if e != nil {
				return nil, nil, e
			}
			if !s.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("analysis not regular: %s", rel)
			}
			files = append(files, analysisFile{rel, s.Size(), s.ModTime().UnixNano()})
		}
		if len(files) == 0 {
			return nil, nil, fmt.Errorf("no analysis files for %s", t.AnalysisPath)
		}
	}
	var art *analysisFileExport
	if t.ArtworkPath != "" {
		rel, e := res.Resolve(t.ArtworkPath)
		if e != nil {
			return nil, nil, e
		}
		s, e := os.Stat(res.FullPath(rel))
		if e != nil {
			return nil, nil, e
		}
		art = &analysisFileExport{rel, s.Size(), s.ModTime().UnixNano()}
	}
	return files, art, nil
}

// Sync imports only bridge-owned rows. All source files are opened read-only;
// the only output is Engine Library. Unchanged tracks do not read audio headers
// or ANLZ bodies and do not rewrite Track/PerformanceData.
func Sync(ctx context.Context, mount string, l *rekordbox.Library, opts Options) (result Result, err error) {
	if err = l.Validate(); err != nil {
		return result, err
	}
	res, err := compat.NewResolver(mount)
	if err != nil {
		return result, err
	}
	if _, err = res.Resolve("PIONEER"); errors.Is(err, os.ErrNotExist) {
		result.Bypassed = true
		return result, nil
	} else if err != nil {
		return result, err
	}
	if opts.SmartMatch {
		found := false
		for _, t := range l.Tracks {
			found = found || t.ID == opts.CurrentTrack
		}
		if !found {
			return result, fmt.Errorf("Smart Match reference track %d not found", opts.CurrentTrack)
		}
	}
	prepared, err := prepareTracks(ctx, res, l)
	if err != nil {
		return result, err
	}
	dir, err := compat.SafeOutputDir(mount, "Engine Library/Database2")
	if err != nil {
		return result, err
	}
	lockPath := filepath.Join(dir, ".rb-bridge.lock")
	if err = safeFile(lockPath); err != nil {
		return result, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return result, fmt.Errorf("bridge already syncing: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	db, tx, err := open(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		return result, err
	}
	defer db.Close()
	defer tx.Rollback()
	var databaseUUID string
	if err = tx.QueryRowContext(ctx, `SELECT uuid FROM Information`).Scan(&databaseUUID); err != nil {
		return result, err
	}
	mapping := map[uint32]int64{}
	effectiveTracks := append([]rekordbox.Track(nil), l.Tracks...)
	owned, err := stageOwnedMoves(ctx, tx, prepared)
	if err != nil {
		return result, err
	}
	for i, preparedTrack := range prepared {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		original := preparedTrack.track
		t := original
		rel, st, files, art, hash := preparedTrack.rel, preparedTrack.info, preparedTrack.files, preparedTrack.art, preparedTrack.hash
		full := res.FullPath(rel)
		enginePath := "../" + rel
		var id int64
		var previous string
		var storedBPM float64
		if ownedID := owned[t.ID]; ownedID != 0 {
			e := tx.QueryRowContext(ctx, `SELECT s.trackId,s.fingerprint,COALESCE(t.bpmAnalyzed,0) FROM BridgeState s JOIN Track t ON t.id=s.trackId WHERE s.trackId=?`, ownedID).Scan(&id, &previous, &storedBPM)
			if e != nil {
				return result, e
			}
		} else {
			e := tx.QueryRowContext(ctx, `SELECT s.trackId,s.fingerprint,COALESCE(t.bpmAnalyzed,0) FROM BridgeState s JOIN Track t ON t.id=s.trackId WHERE s.path=?`, enginePath).Scan(&id, &previous, &storedBPM)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return result, e
			}
		}
		existing := id != 0
		mapping[t.ID] = id
		if previous == hash {
			if t.BPM == 0 {
				effectiveTracks[i].BPM = storedBPM
			}
			result.Unchanged++
			continue
		}
		var e error
		a := &rekordbox.Analysis{}
		for _, f := range files {
			aa, e := rekordbox.ReadAnalysis(res.FullPath(f.rel))
			if e != nil {
				return result, e
			}
			a.Merge(aa)
		}
		audio, e := compat.AudioHeader(full)
		if e != nil {
			return result, fmt.Errorf("track %d audio header: %w", t.ID, e)
		}
		if audio.SampleRate > 0 {
			t.SampleRate = audio.SampleRate
			t.SampleCount = audio.Samples
			if audio.Duration > 0 {
				t.Duration = audio.Duration
			}
			t.BitDepth = audio.BitDepth
			if audio.Bitrate > 0 {
				t.Bitrate = audio.Bitrate
			}
		}
		if t.BPM == 0 && len(a.Beats) > 0 {
			t.BPM = a.Beats[0].BPM
		}
		effectiveTracks[i] = t
		perf, e := EncodePerformance(t, a)
		if e != nil {
			return result, fmt.Errorf("track %d performance: %w", t.ID, e)
		}
		var artID any
		if art != nil {
			if art.Size > 32<<20 {
				return result, fmt.Errorf("artwork exceeds 32 MiB")
			}
			f, e := os.Open(res.FullPath(art.Path))
			if e != nil {
				return result, e
			}
			data, e := io.ReadAll(io.LimitReader(f, 32<<20+1))
			f.Close()
			if e != nil {
				return result, e
			}
			h := sha1.Sum(data)
			artHash := hex.EncodeToString(h[:])
			var n int64
			e = tx.QueryRowContext(ctx, `SELECT id FROM AlbumArt WHERE hash=? LIMIT 1`, artHash).Scan(&n)
			if errors.Is(e, sql.ErrNoRows) {
				r, e := tx.ExecContext(ctx, `INSERT INTO AlbumArt(hash,albumArt) VALUES(?,?)`, artHash, data)
				if e != nil {
					return result, e
				}
				n, e = r.LastInsertId()
				if e != nil {
					return result, e
				}
			} else if e != nil {
				return result, e
			}
			artID = n
		}
		args := []any{int(t.Duration + 0.5), int(t.BPM + 0.5), t.Year, enginePath, path.Base(rel), t.Bitrate, t.BPM, artID, st.Size(), t.Title, t.Artist, t.Album, t.Genre, t.Comment, t.Label, t.Composer, t.Remixer, EngineKey(t.Key), t.Rating, strings.TrimPrefix(strings.ToLower(path.Ext(rel)), "."), perf.Complete, true, true, int64(t.ID)}
		if id == 0 {
			// A pre-existing native track at the same path is never adopted/overwritten.
			r, e := tx.ExecContext(ctx, `INSERT INTO Track(length,bpm,year,path,filename,bitrate,bpmAnalyzed,albumArtId,fileBytes,title,artist,album,genre,comment,label,composer,remixer,key,rating,fileType,isAnalyzed,isAvailable,isMetadataImported,pdbImportKey,dateCreated,dateAdded) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,strftime('%s'),strftime('%s'))`, args...)
			if e != nil {
				return result, fmt.Errorf("insert track %d (native path conflicts are refused): %w", t.ID, e)
			}
			id, e = r.LastInsertId()
			if e != nil {
				return result, e
			}
			result.Added++
		} else {
			args = append(args, id)
			_, e = tx.ExecContext(ctx, `UPDATE Track SET length=?,bpm=?,year=?,path=?,filename=?,bitrate=?,bpmAnalyzed=?,albumArtId=?,fileBytes=?,title=?,artist=?,album=?,genre=?,comment=?,label=?,composer=?,remixer=?,key=?,rating=?,fileType=?,isAnalyzed=?,isAvailable=?,isMetadataImported=?,pdbImportKey=? WHERE id=?`, args...)
			if e != nil {
				return result, e
			}
			result.Updated++
		}
		mapping[t.ID] = id
		// Track's factory trigger already inserted PerformanceData(trackId).
		if _, e = tx.ExecContext(ctx, `UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?,thirdPartySourceId=1,activeOnLoadLoops=? WHERE trackId=?`, perf.TrackData, perf.Overview, perf.Beats, perf.QuickCues, perf.Loops, perf.ActiveLoops, id); e != nil {
			return result, e
		}
		sourceJSON, _ := json.Marshal(struct {
			Track    rekordbox.Track
			Analysis *rekordbox.Analysis
		}{original, a})
		audioJSON, _ := json.Marshal(audio)
		if _, e = tx.ExecContext(ctx, `INSERT INTO BridgeAnalysis(trackId,sourceJSON,audioJSON) VALUES(?,?,?) ON CONFLICT(trackId) DO UPDATE SET sourceJSON=excluded.sourceJSON,audioJSON=excluded.audioJSON`, id, sourceJSON, audioJSON); e != nil {
			return result, e
		}
		if existing {
			_, e = tx.ExecContext(ctx, `UPDATE BridgeState SET path=?,fingerprint=? WHERE trackId=?`, enginePath, hash, id)
		} else {
			_, e = tx.ExecContext(ctx, `INSERT INTO BridgeState(path,trackId,fingerprint) VALUES(?,?,?)`, enginePath, id, hash)
		}
		if e != nil {
			return result, e
		}
	}
	recommendations := SmartMatch(effectiveTracks, opts.CurrentTrack)
	playlistHash := digest(struct {
		Playlists       []rekordbox.Playlist
		Options         Options
		Mapping         map[uint32]int64
		Recommendations []uint32
	}{l.Playlists, opts, mapping, recommendations})
	var oldHash string
	err = tx.QueryRowContext(ctx, `SELECT value FROM BridgeLibrary WHERE key='playlists'`).Scan(&oldHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if oldHash != playlistHash {
		if err = writePlaylists(ctx, tx, l, opts, mapping, databaseUUID, recommendations); err != nil {
			return result, err
		}
		result.PlaylistsChanged = true
		if _, err = tx.ExecContext(ctx, `INSERT INTO BridgeLibrary(key,value) VALUES('playlists',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, playlistHash); err != nil {
			return result, err
		}
	}
	if result.Added+result.Updated > 0 || result.PlaylistsChanged {
		if _, err = tx.ExecContext(ctx, `UPDATE Information SET lastRekordBoxLibraryImportReadCounter=COALESCE(lastRekordBoxLibraryImportReadCounter,0)+1`); err != nil {
			return result, err
		}
		metadata, _ := json.Marshal(struct {
			Columns   [][]byte
			Playlists []rekordbox.Playlist
		}{l.Columns, l.Playlists})
		if _, err = tx.ExecContext(ctx, `INSERT INTO BridgeLibrary(key,value) VALUES('source',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, metadata); err != nil {
			return result, err
		}
	}
	err = tx.Commit()
	return result, err
}

func writePlaylists(ctx context.Context, tx *sql.Tx, l *rekordbox.Library, opts Options, mapping map[uint32]int64, databaseUUID string, recommendations []uint32) error {
	// Refuse to cascade-delete an Engine-created child of an imported folder.
	var n int
	if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM Playlist WHERE parentListId IN (SELECT listId FROM BridgePlaylist) AND id NOT IN (SELECT listId FROM BridgePlaylist)`).Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		return fmt.Errorf("imported folders contain native playlists; refusing destructive rebuild")
	}
	if _, e := tx.ExecContext(ctx, `DELETE FROM PlaylistEntity WHERE listId IN (SELECT listId FROM BridgePlaylist)`); e != nil {
		return e
	}
	// Delete leaves first: factory parent-delete triggers only cascade one level
	// when recursive_triggers is disabled, so explicitly remove the whole owned tree.
	rows, e := tx.QueryContext(ctx, `SELECT listId FROM BridgePlaylist ORDER BY listId DESC`)
	if e != nil {
		return e
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if _, e = tx.ExecContext(ctx, `DELETE FROM Playlist WHERE id=?`, id); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM BridgePlaylist`); e != nil {
		return e
	}
	byParent := map[uint32][]rekordbox.Playlist{}
	for _, p := range l.Playlists {
		byParent[p.ParentID] = append(byParent[p.ParentID], p)
	}
	var insert func(uint32, int64) error
	insert = func(sourceParent uint32, parent int64) error {
		for _, p := range byParent[sourceParent] {
			r, e := tx.ExecContext(ctx, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES(?,?,1,0,1)`, p.Name, parent)
			if e != nil {
				return e
			}
			id, e := r.LastInsertId()
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO BridgePlaylist(sourceId,listId) VALUES(?,?)`, p.ID, id); e != nil {
				return e
			}
			if e = writeEntities(ctx, tx, id, p.Tracks, mapping, databaseUUID); e != nil {
				return e
			}
			if e = insert(p.ID, id); e != nil {
				return e
			}
		}
		return nil
	}
	if e = insert(0, 0); e != nil {
		return e
	}
	if opts.SmartMatch {
		// Never collide with an original playlist of the same name.
		var exists int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM Playlist WHERE parentListId=0 AND title='[Smart Match]'`).Scan(&exists); e != nil {
			return e
		}
		if exists > 0 {
			return fmt.Errorf("[Smart Match] already exists; source playlist is protected")
		}
		r, e := tx.ExecContext(ctx, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES('[Smart Match]',0,1,0,1)`)
		if e != nil {
			return e
		}
		id, e := r.LastInsertId()
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO BridgePlaylist(sourceId,listId) VALUES(-1,?)`, id); e != nil {
			return e
		}
		return writeEntities(ctx, tx, id, recommendations, mapping, databaseUUID)
	}
	return nil
}

func writeEntities(ctx context.Context, tx *sql.Tx, list int64, tracks []uint32, mapping map[uint32]int64, databaseUUID string) error {
	// Engine membership is unique by track/list. Preserve first occurrence order.
	seen := map[uint32]bool{}
	var previous int64
	for _, sourceID := range tracks {
		if seen[sourceID] {
			continue
		}
		seen[sourceID] = true
		id, ok := mapping[sourceID]
		if !ok || id == 0 {
			return fmt.Errorf("missing Engine mapping for track %d", sourceID)
		}
		r, e := tx.ExecContext(ctx, `INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,?,?,0,0)`, list, id, databaseUUID)
		if e != nil {
			return e
		}
		entity, e := r.LastInsertId()
		if e != nil {
			return e
		}
		if previous != 0 {
			if _, e = tx.ExecContext(ctx, `UPDATE PlaylistEntity SET nextEntityId=? WHERE id=?`, entity, previous); e != nil {
				return e
			}
		}
		previous = entity
	}
	return nil
}
