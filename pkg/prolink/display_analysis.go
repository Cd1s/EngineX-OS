package prolink

// This cache is exclusively for display analysis. It never resolves or reads
// Track.Path (audio), and is independent of the legacy FetchTrack path.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const (
	DisplayPDBLimit   = 64 << 20
	DisplayANLZLimit  = 16 << 20
	DisplayCacheLimit = 128 << 20
)

// AnalysisReader allows wire-format fixtures to exercise the same bounded reads.
type AnalysisReader interface {
	Lookup(context.Context, string, string) ([]byte, NFSFile, error)
	Fetch(context.Context, string, string, uint32) ([]byte, NFSFile, error)
}

// DisplayAnalysisCache serializes reads and bounds disk use across host/slots.
// Only export.pdb is persisted; ANLZ is transient and display assets have their
// own four-entry cache. The caller supplies a dedicated directory under /data.
type DisplayAnalysisCache struct{ mu sync.Mutex }

func (c *DisplayAnalysisCache) Read(ctx context.Context, r AnalysisReader, host string, slot byte, id uint32, dir string) (rekordbox.Track, *rekordbox.Analysis, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t rekordbox.Track
	export, err := SlotExport(slot)
	if err != nil || id == 0 || host == "" {
		return t, nil, fmt.Errorf("display: no exported rekordbox track")
	}
	const pdb = "PIONEER/rekordbox/export.pdb"
	_, stamp, err := r.Lookup(ctx, export, pdb)
	if err != nil {
		return t, nil, err
	}
	if stamp.Size == 0 || stamp.Size > DisplayPDBLimit {
		return t, nil, fmt.Errorf("display: PDB size limit")
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		return t, nil, err
	}
	if err = cleanDisplayTemps(dir); err != nil {
		return t, nil, err
	}
	name := fmt.Sprintf("%x.pdb", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", host, slot))))
	local := filepath.Join(dir, name)
	var saved NFSFile
	sb, _ := os.ReadFile(local + ".stamp")
	_ = json.Unmarshal(sb, &saved)
	st, statErr := os.Stat(local)
	if saved != stamp || statErr != nil || st.Size() != int64(stamp.Size) {
		data, got, e := r.Fetch(ctx, export, pdb, DisplayPDBLimit)
		if e != nil {
			return t, nil, e
		}
		if got != stamp || len(data) != int(stamp.Size) {
			return t, nil, fmt.Errorf("display: PDB changed during read")
		}
		// Validate before replacing a previously usable cache file.
		if _, e = rekordbox.ParsePDB(bytes.NewReader(data), int64(len(data))); e != nil {
			return t, nil, e
		}
		if e = trimDisplayPDB(dir, name, int64(len(data))); e != nil {
			return t, nil, e
		}
		if e = writeFile(local, data); e != nil {
			return t, nil, e
		}
		sb, _ = json.Marshal(stamp)
		if e = writeFile(local+".stamp", sb); e != nil {
			return t, nil, e
		}
	}
	f, err := os.Open(local)
	if err != nil {
		return t, nil, err
	}
	lib, err := rekordbox.ParsePDB(mediaReaderAt{ctx, f}, int64(stamp.Size))
	f.Close()
	if err != nil {
		return t, nil, err
	}
	found := false
	for _, row := range lib.Tracks {
		if row.ID == id {
			t, found = row, true
			break
		}
	}
	if !found {
		return t, nil, fmt.Errorf("display: track %d absent", id)
	}
	rel, safe := SafeRel(t.AnalysisPath)
	if !safe || !strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
		return t, nil, fmt.Errorf("display: unsafe or absent ANLZ path")
	}
	base := strings.TrimSuffix(rel, filepath.Ext(rel))
	var merged *rekordbox.Analysis
	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		if err = ctx.Err(); err != nil {
			return t, nil, err
		}
		path, ok := SafeRel(base + ext)
		if !ok {
			return t, nil, fmt.Errorf("display: unsafe ANLZ path")
		}
		data, info, e := r.Fetch(ctx, export, path, DisplayANLZLimit)
		if e != nil {
			continue
		} // unavailable optional formats are normal
		if len(data) > DisplayANLZLimit || len(data) != int(info.Size) {
			return t, nil, fmt.Errorf("display: ANLZ size limit")
		}
		a, e := rekordbox.ParseAnalysis(data)
		if e != nil {
			return t, nil, e
		}
		if merged == nil {
			merged = a
		} else {
			merged.Merge(a)
		}
	}
	if merged == nil {
		return t, nil, fmt.Errorf("display: no analysis")
	}
	return t, merged, nil
}

func trimDisplayPDB(dir, replace string, incoming int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var files []os.FileInfo
	// Budget stamp files and atomic-write overhead conservatively, and cap the
	// entry count too (many tiny databases must not grow the directory forever).
	total := incoming + 4096
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pdb") {
			continue
		}
		if e.Name() == replace {
			if err = os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
			_ = os.Remove(filepath.Join(dir, e.Name()+".stamp"))
			continue
		}
		st, e := e.Info()
		if e != nil {
			return e
		}
		total += st.Size() + 4096
		files = append(files, st)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime().Before(files[j].ModTime()) })
	count := len(files) + 1
	for _, st := range files {
		if total <= DisplayCacheLimit && count <= 8 {
			break
		}
		p := filepath.Join(dir, st.Name())
		if err = os.Remove(p); err != nil {
			return err
		}
		_ = os.Remove(p + ".stamp")
		total -= st.Size() + 4096
		count--
	}
	return nil
}

// writeFile's deferred removal cannot run after a power loss. This dedicated
// directory has one serialized writer, so any leftover fetch file is abandoned.
func cleanDisplayTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		remove := strings.HasPrefix(e.Name(), ".fetch-")
		if strings.HasSuffix(e.Name(), ".pdb.stamp") {
			_, err := os.Stat(strings.TrimSuffix(p, ".stamp"))
			remove = os.IsNotExist(err)
		}
		if remove {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	return nil
}
