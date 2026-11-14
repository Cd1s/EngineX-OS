package prolink

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const maxAudio = 1 << 30

// StagedDir is where Stage puts players' libraries on a Denon drive; bridge
// imports what it finds there, under a "[Pro DJ Link: <name>]" folder (#59).
const StagedDir = ".prolink-cache"

// exportName is what a deck of ours calls itself on the link when it shares its library
// as a player (export.go).
const exportName = "EngineX Export Beta"

// OwnExport reports whether d is a deck of ours sharing its library as a player. What it
// shares is that deck's own drive, which Engine Link already reaches: it is no player's
// library to copy (two decks copied each other's whole stick onto an SD card, 50 GB).
func OwnExport(d Device) bool { return d.Name == exportName }

// StagedExport is where Stage leaves the export.pdb of the library it copied to dir. It is
// written last and at the end of every copy: a folder without it was never finished, and
// its time is when its player was last copied.
func StagedExport(dir string) string { return filepath.Join(dir, pdbPath) }

// SafeRel turns a path from a remote export into one that stays inside its root.
func SafeRel(p string) (string, bool) {
	rel := filepath.Clean(strings.TrimPrefix(p, "/"))
	return rel, p != "" && filepath.IsLocal(rel)
}

// Stage copies a player's Rekordbox library from one media slot into dst, in
// the stick's own layout: every track's audio, ANLZ and artwork, then
// export.pdb last, so a half-finished copy never looks newer than the last
// complete one. Files already in dst are kept (they are only ever written
// whole). It stops once the copied bytes would pass max; tracks left without
// audio are skipped by the import. Returns the number of files copied.
// stageRate caps the copy: the destination is usually the stick a deck is
// playing from, and a full-speed copy starved playback ("short read" stutter).
var stageRate int64 = 2 << 20

type throttled struct {
	ctx   context.Context
	w     io.Writer
	start time.Time
	n     int64
}

func (t *throttled) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.n += int64(n)
	if ahead := time.Duration(t.n*int64(time.Second)/stageRate) - time.Since(t.start); ahead > 0 && err == nil {
		select {
		case <-time.After(ahead):
		case <-t.ctx.Done():
			return n, t.ctx.Err()
		}
	}
	return n, err
}

func Stage(ctx context.Context, c *NFSClient, export, dst string, max int64) (int, error) {
	var pdb []byte
	var err error
	for _, cand := range remotePDBCandidates {
		pdb, _, err = c.Fetch(ctx, export, "/"+cand, maxPDB)
		if err == nil {
			break
		}
	}
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp("", "prolink-pdb-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(pdb)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	lib, err := rekordbox.ReadPDB(tmp.Name())
	if err != nil {
		return 0, err
	}
	copied, used := 0, int64(0)
	localPDB := filepath.Clean(filepath.Join(dst, pdbPath))
	_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if filepath.Clean(p) == localPDB || strings.HasPrefix(d.Name(), ".fetch-") {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			used += info.Size()
		}
		return nil
	})
	full := errors.New("full")
	tw := &throttled{ctx: ctx, start: time.Now()}
	fetch := func(remote string, limit uint32) error {
		rel, ok := SafeRel(remote)
		if !ok {
			return nil
		}
		local := filepath.Join(dst, rel)
		if _, err := os.Stat(local); err == nil {
			return nil
		}
		_, file, err := c.Lookup(ctx, export, "/"+filepath.ToSlash(rel))
		if err != nil {
			return nil // a file missing on the stick is skipped, as rekordbox does
		}
		if used+int64(file.Size) > max {
			return full
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		f, err := os.CreateTemp(filepath.Dir(local), ".fetch-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		tw.w = f
		_, err = c.FetchTo(ctx, export, "/"+filepath.ToSlash(rel), tw, limit)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if err := os.Rename(f.Name(), local); err != nil {
			return err
		}
		used += int64(file.Size)
		copied++
		return nil
	}
tracks:
	for _, t := range lib.Tracks {
		var files []string
		if base, ok := SafeRel(t.AnalysisPath); ok {
			base = strings.TrimSuffix(base, filepath.Ext(base))
			files = append(files, base+".DAT", base+".EXT", base+".2EX")
		}
		files = append(files, t.ArtworkPath, t.Path)
		for i, f := range files {
			limit := uint32(maxANLZ)
			if i == len(files)-1 {
				limit = maxAudio
			}
			if err := fetch(f, limit); err == full {
				break tracks
			} else if err != nil {
				return copied, err
			}
		}
		if ctx.Err() != nil {
			return copied, ctx.Err()
		}
	}
	return copied, writeFile(filepath.Join(dst, pdbPath), pdb)
}
