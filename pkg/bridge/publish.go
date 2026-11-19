package bridge

// A first import is built in a stage and published by renaming its files in (#5, #301).
//   - snapshot copies through SQLite (VACUUM INTO), so committed WAL frames are
//     included and a half-written file is never read;
//   - publishImport marks the stage complete before renaming the files in, and
//     finishPublish completes an interrupted publish at the next mount, so the
//     database and its bridge state never come from different imports;
//   - lockVolume is the per-volume lock cue write-back takes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

const (
	publishDir   = ".rb-bridge-publish"
	publishReady = "READY"
)

func lockVolume(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".rb-bridge.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("bridge already syncing: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// snapshot writes a consistent single-file copy of the SQLite database src to
// dst. A missing src is not an error.
func snapshot(ctx context.Context, src, dst string) error {
	if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: src, RawQuery: "_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("snapshot %s: %w", filepath.Base(src), err)
	}
	return nil
}

// finishPublish completes a staged publish, or drops one that never became
// ready. Old WAL/journal sidecars go first: they belong to the replaced file
// and would otherwise be replayed into the new one.
func finishPublish(dir string, names ...string) error {
	stage := filepath.Join(dir, publishDir)
	if _, err := os.Stat(filepath.Join(stage, publishReady)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.RemoveAll(stage)
		}
		return err
	}
	for _, name := range names {
		staged := filepath.Join(stage, name)
		if _, err := os.Stat(staged); errors.Is(err, os.ErrNotExist) {
			continue // already moved in before an interruption
		}
		for _, side := range []string{"-wal", "-shm", "-journal"} {
			if err := os.Remove(filepath.Join(dir, name+side)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.Rename(staged, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	if err := syncFile(dir); err != nil {
		return err
	}
	return os.RemoveAll(stage)
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// pruneRGBOverviews removes Engine's RGB overview files that no longer belong to their
// track (enginedb.PruneRGBOverviews). It never stops a mount: at worst a file stays.
func pruneRGBOverviews(ctx context.Context, dir, library, previous string) {
	if _, err := os.Stat(library); err != nil {
		return
	}
	n, err := enginedb.PruneRGBOverviews(ctx, dir, library, previous)
	if err != nil {
		log.Printf("rb-bridge: RGB overviews not checked: %v", err)
	}
	if n > 0 {
		log.Printf("rb-bridge: %d stale RGB overviews removed; Engine builds them again at load", n)
	}
}
