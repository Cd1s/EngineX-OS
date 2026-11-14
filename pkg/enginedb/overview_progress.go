package enginedb

import (
	"encoding/hex"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// overviewProgressFile is beside m.db: how far FillOverviewMaxima has come in this
// library, as "<generation> <last trackId>", or "<generation>" alone once it has been
// through every overview. A generation of "-" stands for none.
const overviewProgressFile = ".rb-bridge-overview"

// overviewScan begins every generation: what the backfill reads. 2 is every overview. The
// scan before it read only the rows still marked as imported and recorded the header
// alone, so a library it recorded as done is read again: the tracks Engine had loaded
// were not among them.
const overviewScan = "2:"

// OverviewProgress is how far the backfill of a library's overviews has come.
type OverviewProgress struct {
	after      int64 // every overview up to this track has been seen
	done       bool
	unverified bool // recorded without a generation: an import behind it would not have shown
}

// importGeneration changes with every import, an earlier build's on another deck too,
// and that build leaves overviews without band maxima: each import writes rb-bridge's own
// database, and with a rollback journal SQLite counts every committed write in the file's
// header. In WAL mode a commit need not touch the header, and beside a journal the header
// is that of a write cut short: then there is no generation.
func importGeneration(dir string) string {
	state := filepath.Join(dir, stateFile)
	f, err := os.Open(state)
	if err != nil {
		return ""
	}
	defer f.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(f, header); err != nil || header[18] != 1 || header[19] != 1 { // the file format versions: 2 is WAL
		return ""
	}
	if _, err := os.Lstat(state + "-journal"); err == nil {
		return ""
	}
	return overviewScan + hex.EncodeToString(header)
}

// ReadOverviewProgress is the progress recorded for the library on mount, if no import
// has run since it was recorded; from the start otherwise. Without a generation nothing
// shows an import, so the place is kept and "done" never is: the backfill then goes
// round the library, a mount's time at a go, and that place is not kept into a generation.
func ReadOverviewProgress(mount string) OverviewProgress {
	dir := filepath.Join(mount, "Engine Library", "Database2")
	start := OverviewProgress{after: math.MinInt64}
	b, err := os.ReadFile(filepath.Join(dir, overviewProgressFile))
	if err != nil {
		return start
	}
	generation := importGeneration(dir)
	if generation == "" {
		generation = "-"
	}
	f := strings.Fields(string(b))
	switch {
	case len(f) == 0 || f[0] != generation:
		return start
	case len(f) == 1 && generation != "-":
		return OverviewProgress{done: true}
	case len(f) == 2:
		if after, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			return OverviewProgress{after: after, unverified: generation == "-"}
		}
	}
	return start
}

// Keep records the progress for the library as it is now. rb-bridge calls it after an
// import of its own, which changes the generation and leaves no overview without band
// maxima: where the backfill stood still holds. A place recorded without a generation
// does not hold: an earlier build may have imported behind it, so with a generation now
// the record stays as it is, which no longer matches, and the next read starts over.
func (p OverviewProgress) Keep(mount string) error {
	dir := filepath.Join(mount, "Engine Library", "Database2")
	name := filepath.Join(dir, overviewProgressFile)
	generation := importGeneration(dir)
	if p.unverified && generation != "" {
		return nil
	}
	text := generation + " " + strconv.FormatInt(p.after, 10)
	switch {
	case p.done && generation == "": // nothing would show a later import
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case p.done:
		text = generation
	case p.after == math.MinInt64: // nothing to keep
		return nil
	case generation == "":
		text = "- " + strconv.FormatInt(p.after, 10)
	}
	// In one step: a kill leaves the progress as it was or as it is, never half a file.
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, []byte(text+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
