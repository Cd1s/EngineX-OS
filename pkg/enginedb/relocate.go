package enginedb

// Relocate missing songs (#74): a Track whose file is gone from the drive (the
// red mark on the deck) is pointed at the same song's new place on that drive.
// Engine stores Track.path relative to the "Engine Library" folder, as
// "../Music/a.mp3", so a drive root R and a file R/Music/a.mp3 give that value.
// Three tiers, most certain first; a song matched by none stays as it is.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

var relocateExts = map[string]bool{".mp3": true, ".flac": true, ".wav": true, ".aiff": true, ".aif": true, ".m4a": true, ".aac": true, ".ogg": true, ".opus": true}

const (
	TierExact   = 1 // same file name and byte size (or unique name when the size is unknown)
	TierNoCase  = 2 // same file name ignoring case, the only one on the drive
	TierCleaned = 3 // same extension and the same name once punctuation, case and numbering are stripped
)

type RelocateOptions struct {
	// Root is the drive: the folder that holds "Engine Library".
	Root   string
	DryRun bool
}

type Move struct {
	ID      int64  `json:"id"`
	Title   string `json:"title,omitempty"`
	OldPath string `json:"oldPath"`
	NewPath string `json:"newPath"`
	Tier    int    `json:"tier"`
}

type RelocateResult struct {
	Missing    int    `json:"missing"`    // tracks whose file is not on the drive
	Moves      []Move `json:"moves"`      // tracks that found a new place
	Unresolved int    `json:"unresolved"` // missing tracks with no single match
	Applied    bool   `json:"applied"`
	Backup     string `json:"backup,omitempty"`
	// Queued: Engine has the library open, so the moves wait in the library
	// and are applied before Engine next opens it (eject and insert the drive).
	Queued bool `json:"queued,omitempty"`
}

// relocateScanned, when set (tests), runs after the drive is scanned and before anything is
// written: where Engine may open the library meanwhile.
var relocateScanned func()

type driveFile struct {
	rel  string // from the drive root, slash separated
	size int64
}

type missingTrack struct {
	id            int64
	path, name    string
	title, artist string
	bytes         int64
}

// Relocate finds the missing tracks of the drive's m.db and, unless DryRun,
// rewrites their path and filename in one transaction after backing m.db up.
func Relocate(ctx context.Context, opts RelocateOptions) (RelocateResult, error) {
	res := RelocateResult{Moves: []Move{}}
	dbPath := filepath.Join(opts.Root, "Engine Library", "Database2", "m.db")
	if err := safeFile(dbPath); err != nil {
		return res, err
	}
	if st, err := os.Stat(dbPath); err != nil || st.Size() == 0 {
		return res, errors.New("no Engine Library on this drive")
	}
	// The search only reads: m.db is opened read-only for it, and for writing only once the
	// moves are known and no other process has the library open at that moment.
	u := url.URL{Scheme: "file", Path: dbPath}
	ro, err := sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return res, err
	}
	defer ro.Close()
	ro.SetMaxOpenConns(1)
	db := ro

	missing, taken, err := findMissing(ctx, db, opts.Root)
	if err != nil {
		return res, err
	}
	res.Missing = len(missing)
	if len(missing) == 0 {
		return res, nil
	}
	files, err := scanDrive(ctx, opts.Root)
	if err != nil {
		return res, err
	}
	if relocateScanned != nil {
		relocateScanned()
	}
	res.Moves = match(missing, files, taken)
	res.Unresolved = len(missing) - len(res.Moves)
	if opts.DryRun || len(res.Moves) == 0 {
		return res, nil
	}
	// Engine may have opened the drive while it was scanned.
	if LibraryInUse(opts.Root) {
		// No write while Engine has m.db open (BH-GC1-3).
		if err = QueuePending(opts.Root, func(p *Pending) { p.Relocate = append(p.Relocate, res.Moves...) }); err != nil {
			return res, err
		}
		res.Queued = true
		return res, nil
	}
	ro.Close()
	if db, err = sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)"); err != nil {
		return res, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	backup, err := relocateBackup(ctx, db, dbPath)
	if err != nil {
		return res, fmt.Errorf("backup failed, nothing changed: %w", err)
	}
	res.Backup = backup
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, m := range res.Moves {
		// The old path in the WHERE keeps a track Engine changed meanwhile untouched.
		r, err := tx.ExecContext(ctx, `UPDATE Track SET path=?, filename=? WHERE id=? AND path=?`, m.NewPath, filepath.Base(m.NewPath), m.ID, m.OldPath)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return res, fmt.Errorf("track %d changed while relocating; nothing was written", m.ID)
		}
	}
	if err = tx.Commit(); err != nil {
		return res, err
	}
	res.Applied = true
	return res, nil
}

// relocateBackup keeps the first backup (m.db.bak.relocate) and the one made
// before the latest relocation (m.db.bak.relocate.last); every run used to add
// another full copy of m.db in Database2.
func relocateBackup(ctx context.Context, db *sql.DB, dbPath string) (string, error) {
	first := dbPath + ".bak.relocate"
	if _, err := os.Lstat(first); errors.Is(err, os.ErrNotExist) {
		_, err = db.ExecContext(ctx, `VACUUM INTO ?`, first)
		return first, err
	}
	last := first + ".last"
	tmp := fmt.Sprintf("%s.tmp-%d", last, time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, last); err != nil {
		os.Remove(tmp)
		return "", err
	}
	// Timestamped copies older builds left beside it.
	if old, _ := filepath.Glob(first + ".[0-9]*"); len(old) > 0 {
		for _, f := range old {
			os.Remove(f)
		}
	}
	return last, nil
}

// findMissing returns the local tracks without a file, and the lower-cased paths
// of every track row so that no file is given to a second track.
func findMissing(ctx context.Context, db *sql.DB, root string) ([]missingTrack, map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(path,''), COALESCE(filename,''), COALESCE(fileBytes,0), COALESCE(title,''), COALESCE(artist,'')
		FROM Track WHERE COALESCE(streamingSource,'')='' ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var missing []missingTrack
	taken := map[string]bool{}
	lib := filepath.Join(root, "Engine Library")
	for rows.Next() {
		var t missingTrack
		if err = rows.Scan(&t.id, &t.path, &t.name, &t.bytes, &t.title, &t.artist); err != nil {
			return nil, nil, err
		}
		// A missing track's own path stays free, or a file that differs only in case could never match it.
		file := filepath.Join(lib, filepath.FromSlash(t.path))
		r, e := filepath.Rel(root, file)
		if t.path != "" && !filepath.IsAbs(t.path) && e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
				missing = append(missing, t)
				continue
			}
		}
		taken[strings.ToLower(t.path)] = true
	}
	return missing, taken, rows.Err()
}

func scanDrive(ctx context.Context, root string) ([]driveFile, error) {
	var out []driveFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable folder must not stop the scan
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || name == "Engine Library" || name == "System Volume Information" || name == "$RECYCLE.BIN") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") || !relocateExts[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, driveFile{filepath.ToSlash(rel), info.Size()})
		return nil
	})
	return out, err
}

// clean keeps letters and digits only, so "01 - Artist_Song (1).mp3" and
// "artist song 1" meet.
func clean(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stem(name string) string { return strings.TrimSuffix(name, filepath.Ext(name)) }

func match(missing []missingTrack, files []driveFile, taken map[string]bool) []Move {
	byName, byLower, byClean := map[string][]driveFile{}, map[string][]driveFile{}, map[string][]driveFile{}
	for _, f := range files {
		if taken["../"+strings.ToLower(f.rel)] {
			continue
		}
		base := filepath.Base(f.rel)
		byName[base] = append(byName[base], f)
		byLower[strings.ToLower(base)] = append(byLower[strings.ToLower(base)], f)
		c, ext := clean(stem(base)), strings.ToLower(filepath.Ext(base))+"\x00"
		byClean[ext+c] = append(byClean[ext+c], f)
		// "01 - Song" is also filed as "song": folders get sorted with track numbers added.
		if n := strings.TrimLeftFunc(c, unicode.IsDigit); n != "" && n != c {
			byClean[ext+n] = append(byClean[ext+n], f)
		}
	}
	claimed := map[string]bool{}
	free := func(in []driveFile, size int64) (driveFile, bool) {
		var pick []driveFile
		for _, f := range in {
			if !claimed[f.rel] && (size <= 0 || f.size == size) {
				pick = append(pick, f)
			}
		}
		if len(pick) != 1 {
			return driveFile{}, false
		}
		return pick[0], true
	}
	var moves []Move
	for _, t := range missing {
		old := filepath.Base(filepath.FromSlash(t.path))
		if t.name != "" {
			old = t.name
		}
		var f driveFile
		var tier int
		var ok bool
		if f, ok = free(byName[old], t.bytes); ok {
			tier = TierExact
		} else if f, ok = free(byLower[strings.ToLower(old)], 0); ok {
			tier = TierNoCase
		} else {
			ext := strings.ToLower(filepath.Ext(old)) + "\x00"
			for _, key := range []string{clean(stem(old)), clean(t.title), clean(t.artist + t.title)} {
				if key == "" {
					continue
				}
				if f, ok = free(byClean[ext+key], 0); ok {
					tier = TierCleaned
					break
				}
			}
		}
		if ok {
			claimed[f.rel] = true
			moves = append(moves, Move{ID: t.id, Title: t.title, OldPath: t.path, NewPath: "../" + f.rel, Tier: tier})
		}
	}
	sort.SliceStable(moves, func(i, j int) bool { return moves[i].ID < moves[j].ID })
	return moves
}
