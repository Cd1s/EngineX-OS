package enginedb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	sqlite "modernc.org/sqlite"
)

// The rule every writer of a volume's library follows (#336 Go review R13):
//
//   - it holds the volume's lock (rbstate.HoldVolume) for as long as it changes the library;
//   - a replacement of m.db is renamed in under an exclusive hold on the file it replaces
//     (HoldLibrary), with every check repeated under that hold.
//
// The lock orders our own programs, the files no database lock protects included (the queue of
// pending edits, the snapshots). The hold covers every program that speaks SQLite and does not
// know the lock, Engine among them.

func init() {
	// A hold must outlive any close of another descriptor of the file in this process, which
	// drops POSIX record locks (the reserve rebuild turns the same switch on).
	_, _ = sqlite.OFDLocking(true)
}

// EditLockWait is how long an edit in place waits for the volume's lock before it is refused.
var EditLockWait = 2 * time.Second

// ErrLibraryBusy: another EngineX program holds the volume's lock; nothing was written. The Web
// Console shows this text as it is.
var ErrLibraryBusy = errors.New("the library is busy, try again in a moment")

// ErrLibraryChanged: the library is not the one its replacement was made for; nothing was replaced.
var ErrLibraryChanged = errors.New("enginedb: the library changed before it could be replaced")

type busyError struct{ cause error }

func (e busyError) Error() string        { return ErrLibraryBusy.Error() }
func (e busyError) Is(target error) bool { return target == ErrLibraryBusy }
func (e busyError) Unwrap() error        { return e.cause }

// HoldForEdit returns ctx holding the lock of the volume mounted at root for an edit in place: a
// ctx that holds it already is reused, otherwise the lock is waited for up to EditLockWait.
// ErrLibraryBusy when another program keeps it that long.
func HoldForEdit(ctx context.Context, root string) (held context.Context, release func(), err error) {
	return holdFor(ctx, root, EditLockWait)
}

func holdFor(ctx context.Context, root string, wait time.Duration) (context.Context, func(), error) {
	held, release, err := rbstate.HoldVolume(ctx, root, wait)
	if errors.Is(err, rbstate.ErrLocked) {
		return held, nil, busyError{err}
	}
	return held, release, err
}

// OpenLibraryRW opens the library of the volume mounted at root for an edit in place. It is the
// one way in for such an edit, and it refuses a ctx that does not hold the volume's lock: an
// edit made without it can be lost under a replacement of the file.
func OpenLibraryRW(ctx context.Context, root, pragmas string) (*sql.DB, error) {
	if !rbstate.VolumeHeld(ctx, root) {
		return nil, fmt.Errorf("enginedb: %s opened for an edit without its volume's lock", root)
	}
	p := filepath.Join(root, "Engine Library", "Database2", "m.db")
	if err := safeFile(p); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: p}).String()
	if pragmas != "" {
		dsn += "?" + pragmas
	}
	return sql.Open("sqlite", dsn)
}

// LibraryHold is an exclusive hold on a volume's live library across its replacement. While it
// lasts no other SQLite connection, of any process, reads or writes the file or makes a sidecar
// of it (measured for rollback-journal and WAL libraries, #336 Go review R13).
type LibraryHold struct {
	path string
	pin  *os.File // one read descriptor, open from before the lock until after it: it is never closed under the hold
	info os.FileInfo
	db   *sql.DB
	conn *sql.Conn
}

// HoldLibrary takes the hold on path. ErrLibraryInUse when another connection has the library
// locked, or has a WAL library open at all; os.ErrNotExist when there is no file to hold.
// The caller holds the volume's lock, and has refused a sidecar with work in it.
func HoldLibrary(ctx context.Context, path string) (h *LibraryHold, err error) {
	pin, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	h = &LibraryHold{path: path, pin: pin}
	defer func() {
		if err != nil {
			h.Release()
			h = nil
		}
	}()
	if h.info, err = pin.Stat(); err != nil {
		return h, err
	}
	if !h.info.Mode().IsRegular() {
		return h, fmt.Errorf("unsafe database file %s", path)
	}
	if h.db, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=rw"); err != nil {
		return h, err
	}
	h.db.SetMaxOpenConns(1)
	if h.conn, err = h.db.Conn(ctx); err != nil {
		return h, err
	}
	busy := func(step string, e error) error { return fmt.Errorf("%w: %s: %v", ErrLibraryInUse, step, e) }
	for _, q := range []string{"PRAGMA main.busy_timeout=1", "PRAGMA main.locking_mode=EXCLUSIVE"} {
		if _, err = h.conn.ExecContext(ctx, q); err != nil {
			return h, busy(q, err)
		}
	}
	var mode string
	if err = h.conn.QueryRowContext(ctx, "PRAGMA main.journal_mode").Scan(&mode); err != nil {
		return h, busy("journal mode", err) // a WAL library another process has open answers SQLITE_BUSY here
	}
	wal := strings.EqualFold(mode, "wal")
	if wal {
		var blocked, frames, moved int
		if err = h.conn.QueryRowContext(ctx, "PRAGMA main.wal_checkpoint(TRUNCATE)").Scan(&blocked, &frames, &moved); err != nil {
			return h, busy("checkpoint", err)
		}
		if blocked != 0 {
			return h, busy("checkpoint", errors.New("a reader or writer is active"))
		}
	}
	if _, err = h.conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return h, busy("exclusive lock", err)
	}
	if wal {
		// The connection outlives the rename: its close must not unlink the -wal and -shm
		// names, which then belong to the new file.
		if err = h.conn.Raw(func(dc any) error {
			fc, ok := dc.(sqlite.FileControl)
			if !ok {
				return errors.New("sqlite driver connection does not support FileControl")
			}
			_, e := fc.FileControlPersistWAL("main", 1)
			return e
		}); err != nil {
			return h, err
		}
	}
	return h, h.Same()
}

// Same reports ErrLibraryChanged unless the path still names the held file.
func (h *LibraryHold) Same() error {
	cur, err := os.Lstat(h.path)
	if err != nil {
		return err
	}
	if !os.SameFile(h.info, cur) {
		return fmt.Errorf("%w: %s is another file", ErrLibraryChanged, h.path)
	}
	return nil
}

// Is reports whether the held library is the one LibraryState named state: no write was
// committed to it since, by any process. An empty WAL holds nothing and counts as none.
func (h *LibraryHold) Is(state string) bool {
	var head [28]byte
	st, err := h.pin.Stat()
	if _, e := h.pin.ReadAt(head[:], 0); err != nil || e != nil || h.Same() != nil {
		return false
	}
	if wal, e := os.Lstat(h.path + "-wal"); e == nil && wal.Size() != 0 {
		return false
	}
	part := strings.Split(state, "/")
	if len(part) != 3 && len(part) != 5 {
		return false
	}
	if len(part) == 5 && part[3] != "0" {
		return false
	}
	return part[0] == strconv.FormatUint(uint64(binary.BigEndian.Uint32(head[24:])), 10) &&
		part[1] == strconv.FormatInt(st.Size(), 10) && part[2] == strconv.FormatInt(st.ModTime().UnixNano(), 10)
}

// Empty reports whether the held library has no track and no list: Engine's skeleton, or a file
// without Engine's tables.
func (h *LibraryHold) Empty(ctx context.Context) (bool, error) {
	var tables int
	if err := h.conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('Track','Playlist')`).Scan(&tables); err != nil {
		return false, err
	}
	if tables != 2 {
		return true, nil
	}
	var has bool
	err := h.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM Track) OR EXISTS(SELECT 1 FROM Playlist)`).Scan(&has)
	return !has, err
}

// Release ends the hold. After a rename over the path the held file has no name any more.
func (h *LibraryHold) Release() {
	if h == nil {
		return
	}
	if h.conn != nil {
		_, _ = h.conn.ExecContext(context.Background(), "ROLLBACK")
		_ = h.conn.Close()
		h.conn = nil
	}
	if h.db != nil {
		_ = h.db.Close()
		h.db = nil
	}
	if h.pin != nil {
		_ = h.pin.Close()
		h.pin = nil
	}
}
