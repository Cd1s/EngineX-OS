package enginedb

// Library edits asked for while Engine has the drive open (inuse.go) wait in
// Database2/enginex-pending.json. engine-guardian applies them from the mount
// hook's private pre-mount, before Engine opens the library again.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const PendingFile = "enginex-pending.json"

// PendingCue is one Hot Cue edit: label and colour always, the position only
// when given (milliseconds).
type PendingCue struct {
	TrackID    int64    `json:"trackId"`
	Index      int      `json:"index"`
	Label      string   `json:"label"`
	Color      string   `json:"color"`
	PositionMS *float64 `json:"positionMs,omitempty"`
}

type Pending struct {
	Relocate []Move       `json:"relocate,omitempty"`
	HotCues  []PendingCue `json:"hotCues,omitempty"`
}

var pendingMu sync.Mutex

func pendingPath(root string) string {
	return filepath.Join(root, "Engine Library", "Database2", PendingFile)
}

func readPending(path string) (Pending, error) {
	var p Pending
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

// QueuePending adds edits to the drive's queue. root holds "Engine Library".
func QueuePending(root string, add func(*Pending)) error {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	path := pendingPath(root)
	if err := safeFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	p, err := readPending(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pending edits unreadable: %w", err)
	}
	add(&p)
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".enginex-pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		_ = os.Chmod(tmp.Name(), 0o644)
		err = os.Rename(tmp.Name(), path)
	}
	return err
}

// ApplyPending writes the queued edits into m.db and removes the queue. A
// relocation whose track Engine changed meanwhile, or whose new file is gone,
// is skipped; so is a Hot Cue edit for a track without cue data. It refuses
// while another process has the library open.
func ApplyPending(ctx context.Context, root string) (int, error) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	path := pendingPath(root)
	if err := safeFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	p, err := readPending(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("pending edits unreadable: %w", err)
	}
	if LibraryInUse(root) {
		return 0, ErrLibraryInUse
	}
	dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	if err := safeFile(dbPath); err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath}).String()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if len(p.Relocate) > 0 {
		if _, err := relocateBackup(ctx, db, dbPath); err != nil {
			return 0, fmt.Errorf("backup failed, nothing changed: %w", err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, m := range p.Relocate {
		if _, err := os.Stat(filepath.Join(root, "Engine Library", filepath.FromSlash(m.NewPath))); err != nil {
			continue
		}
		r, err := tx.ExecContext(ctx, `UPDATE Track SET path=?, filename=? WHERE id=? AND path=?`, m.NewPath, filepath.Base(m.NewPath), m.ID, m.OldPath)
		if err != nil {
			return 0, err
		}
		if k, _ := r.RowsAffected(); k == 1 {
			n++
		}
	}
	for _, c := range p.HotCues {
		if err := SetQuickCue(ctx, tx, c.TrackID, c.Index, c.Label, c.Color, c.PositionMS); err == nil {
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, os.Remove(path)
}

type sqlExecQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// QuickCueRate is the track's sample rate from PerformanceData.trackData, so
// cue positions (in samples) convert to and from milliseconds correctly for 48,
// 88.2 and 96 kHz files too. 44100 when it is unknown.
func QuickCueRate(ctx context.Context, q sqlExecQuerier, trackID int64) float64 {
	var blob []byte
	if q.QueryRowContext(ctx, `SELECT trackData FROM PerformanceData WHERE trackId=?`, trackID).Scan(&blob) != nil || len(blob) == 0 {
		return 44100
	}
	raw, err := QUncompress(blob, 4096)
	if err != nil || len(raw) < 8 {
		return 44100
	}
	rate := math.Float64frombits(binary.BigEndian.Uint64(raw))
	if math.IsNaN(rate) || rate < 8000 || rate > 384000 {
		return 44100
	}
	return rate
}

// SetQuickCue sets one of the 8 Hot Cues of a track in PerformanceData.quickCues:
// label and colour (#RRGGBB), and the position when posMS is not nil.
func SetQuickCue(ctx context.Context, q sqlExecQuerier, trackID int64, index int, label, color string, posMS *float64) error {
	if index < 0 || index > 7 {
		return errors.New("hot cue index out of range")
	}
	if posMS != nil && (math.IsNaN(*posMS) || math.IsInf(*posMS, 0) || *posMS < 0) {
		return errors.New("hot cue position must be a finite, non-negative time")
	}
	if len(color) != 7 || color[0] != '#' {
		return errors.New("invalid colour")
	}
	rgb, err := strconv.ParseUint(color[1:], 16, 32)
	if err != nil {
		return errors.New("invalid colour")
	}
	var blob []byte
	if err := q.QueryRowContext(ctx, `SELECT quickCues FROM PerformanceData WHERE trackId=?`, trackID).Scan(&blob); err != nil {
		return err
	}
	raw, err := QUncompress(blob, 64<<10)
	if err != nil || len(raw) < 8 || binary.BigEndian.Uint64(raw) != 8 {
		return errors.New("no hot cue data")
	}
	out := append([]byte(nil), raw[:8]...)
	off := 8
	for i := 0; i < 8; i++ {
		if off >= len(raw) || off+1+int(raw[off])+12 > len(raw) {
			return errors.New("truncated hot cue data")
		}
		end := off + 1 + int(raw[off]) + 12
		if i != index {
			out = append(out, raw[off:end]...)
			off = end
			continue
		}
		pos := raw[end-12 : end-4]
		lbl := []byte(label)
		if len(lbl) > 255 {
			lbl = lbl[:255]
		}
		out = append(out, byte(len(lbl)))
		out = append(out, lbl...)
		if posMS != nil {
			var b [8]byte
			binary.BigEndian.PutUint64(b[:], math.Float64bits(*posMS*QuickCueRate(ctx, q, trackID)/1000))
			pos = b[:]
		}
		out = append(out, pos...)
		out = append(out, 255, byte(rgb>>16), byte(rgb>>8), byte(rgb))
		off = end
	}
	out = append(out, raw[off:]...)
	enc, err := QCompress(out)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, enc, trackID)
	return err
}
