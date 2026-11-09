package prolink

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const (
	DefaultMediaCacheDir = "/tmp/sc5000-prolink-media"
	pdbPath              = "PIONEER/rekordbox/export.pdb"
	maxPDB               = 64 << 20
	maxANLZ              = 16 << 20
	mediaFetch           = 4 * time.Second // bounds how long Observe holds the discovery lock
)

// MediaCache reads a track's Rekordbox metadata and ANLZ waveform straight
// from the Pioneer player whose USB/SD holds it (#59), so the waveform shows
// on the Denon without that drive being plugged into the Denon. It keeps
// export.pdb and the ANLZ files of loaded tracks under Dir/<player>-<slot>/
// in the drive's own layout, and fetches export.pdb again only when its size
// or mtime changes (a different stick was inserted).
type MediaCache struct {
	Context context.Context // optional owner cancellation; nil uses Background
	Dir     string
	Enabled func() bool // checked on every new track; nil means on
	Portmap int         // tests only; 0 is UDP 111
}

func (m *MediaCache) resolve(dev Device, host net.IP) (*rekordbox.Analysis, string, string) {
	if m == nil || host == nil || dev.TrackID == 0 || (m.Enabled != nil && !m.Enabled()) {
		return nil, "", ""
	}
	export, err := SlotExport(dev.SourceSlot)
	if err != nil {
		return nil, "", ""
	}
	parent := m.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, mediaFetch)
	defer cancel()
	c := &NFSClient{IP: host, Portmap: m.Portmap}
	root := filepath.Join(m.Dir, fmt.Sprintf("%d-%d", dev.SourcePlayer, dev.SourceSlot))
	local := filepath.Join(root, pdbPath)
	_, file, err := c.Lookup(ctx, export, "/"+pdbPath)
	if err != nil {
		return nil, "", ""
	}
	stamp := fmt.Sprintf("%d %d", file.Size, file.MTime)
	owner, _ := os.ReadFile(filepath.Join(root, ".host"))
	if old, _ := os.ReadFile(local + ".stamp"); string(old) != stamp || string(owner) != host.String() {
		data, _, err := c.Fetch(ctx, export, "/"+pdbPath, maxPDB)
		if err != nil {
			return nil, "", ""
		}
		_ = os.RemoveAll(root) // ANLZ files of the previous stick
		if writeFile(local, data) != nil || writeFile(local+".stamp", []byte(stamp)) != nil || writeFile(filepath.Join(root, ".host"), []byte(host.String())) != nil {
			return nil, "", ""
		}
	}
	pdbFile, err := os.Open(local)
	if err != nil {
		return nil, "", ""
	}
	defer pdbFile.Close()
	st, err := pdbFile.Stat()
	if err != nil {
		return nil, "", ""
	}
	lib, err := rekordbox.ParsePDB(mediaReaderAt{ctx, pdbFile}, st.Size())
	if err != nil || lib == nil {
		return nil, "", ""
	}
	for _, trk := range lib.Tracks {
		if trk.ID != dev.TrackID {
			continue
		}
		// The path comes from the remote PDB: only files under PIONEER/USBANLZ.
		rel := filepath.Clean(strings.TrimPrefix(trk.AnalysisPath, "/"))
		if !filepath.IsLocal(rel) || !strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
			return nil, trk.Title, trk.Artist
		}
		base := strings.TrimSuffix(rel, filepath.Ext(rel))
		var merged *rekordbox.Analysis
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			if ctx.Err() != nil {
				return nil, "", ""
			}
			path := filepath.Join(root, base+ext)
			if _, err := os.Stat(path); err != nil {
				data, _, err := c.Fetch(ctx, export, "/"+base+ext, maxANLZ)
				if err != nil || writeFile(path, data) != nil {
					continue // .EXT and .2EX are optional
				}
			}
			if a, err := rekordbox.ReadAnalysis(path); err == nil && a != nil {
				if merged == nil {
					merged = a
				} else {
					merged.Merge(a)
				}
			}
		}
		return merged, trk.Title, trk.Artist
	}
	return nil, "", ""
}

// CachedAnalysis reads the exact track from an existing media cache. This also
// lets WebUI consume rb-bridge's cache across the process boundary. It never
// opens a network client, writes files, scans unrelated ANLZ, or reads audio.
func (m *MediaCache) CachedAnalysis(dev Device, host net.IP) (*rekordbox.Analysis, string, string) {
	if m == nil || m.Dir == "" || host == nil || dev.TrackID == 0 {
		return nil, "", ""
	}
	root := filepath.Join(m.Dir, fmt.Sprintf("%d-%d", dev.SourcePlayer, dev.SourceSlot))
	owner, err := os.ReadFile(filepath.Join(root, ".host"))
	if err != nil || string(owner) != host.String() {
		return nil, "", ""
	}
	lib, err := rekordbox.ReadPDB(filepath.Join(root, pdbPath))
	if err != nil || lib == nil {
		return nil, "", ""
	}
	for _, trk := range lib.Tracks {
		if trk.ID != dev.TrackID {
			continue
		}
		rel := filepath.Clean(strings.TrimPrefix(trk.AnalysisPath, "/"))
		if !filepath.IsLocal(rel) || !strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
			return nil, trk.Title, trk.Artist
		}
		base := strings.TrimSuffix(rel, filepath.Ext(rel))
		var merged *rekordbox.Analysis
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			if a, err := rekordbox.ReadAnalysis(filepath.Join(root, base+ext)); err == nil {
				if merged == nil {
					merged = a
				} else {
					merged.Merge(a)
				}
			}
		}
		return merged, trk.Title, trk.Artist
	}
	return nil, "", ""
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Check cancellation between PDB page reads as well as during network I/O.
type mediaReaderAt struct {
	context.Context
	io.ReaderAt
}

func (r mediaReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.ReaderAt.ReadAt(p, off)
}

// FetchTrack copies the audio of track id from a player's media slot into dir
// and returns the local file (#49: a shadow deck plays what a CDJ plays). A
// copy already in dir is reused.
// shortcut: the copy is keyed by host, slot and track id, so a different stick
// in the same slot with the same id reuses a stale copy until dir is cleared.
func FetchTrack(ctx context.Context, c *NFSClient, slot byte, id uint32, dir string) (string, error) {
	export, t, err := fetchTrackRow(ctx, c, slot, id)
	if err != nil {
		return "", err
	}
	rel, _ := SafeRel(t.Path)
	if rel == "" {
		return "", fmt.Errorf("prolink: track %d has no usable path", id)
	}
	// The copy keeps the file's name: Engine shows it as the title when the file has no tags (WAV),
	// and an export's Contents folder: the UI stretches that low overview by it (scOverviewScale).
	sub := ""
	if first, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); first == "Contents" {
		sub = "Contents"
	}
	local := filepath.Join(dir, fmt.Sprintf("%s-%d-%d", c.IP, slot, id), sub, filepath.Base(rel))
	if _, err := os.Stat(local); err == nil {
		return local, nil
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(local), ".fetch-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, err = c.FetchTo(ctx, export, "/"+filepath.ToSlash(rel), f, maxAudio)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return local, os.Rename(f.Name(), local)
}

// fetchTrackRow reads track id's row from the export.pdb on a player's slot.
func fetchTrackRow(ctx context.Context, c *NFSClient, slot byte, id uint32) (string, rekordbox.Track, error) {
	export, err := SlotExport(slot)
	if err != nil {
		return "", rekordbox.Track{}, err
	}
	pdb, _, err := c.Fetch(ctx, export, "/"+pdbPath, maxPDB)
	if err != nil {
		return "", rekordbox.Track{}, err
	}
	lib, err := rekordbox.ParsePDB(bytes.NewReader(pdb), int64(len(pdb)))
	if err != nil {
		return "", rekordbox.Track{}, err
	}
	for _, t := range lib.Tracks {
		if t.ID == id {
			return export, t, nil
		}
	}
	return "", rekordbox.Track{}, fmt.Errorf("prolink: track %d is not in the player's export", id)
}

// FetchAnalysis reads track id's row and its ANLZ files (.DAT, plus .EXT and
// .2EX when present) from a player's slot, so a shadow deck gets the CDJ's
// grid and waveform instead of analysing its copy (#49).
func FetchAnalysis(ctx context.Context, c *NFSClient, slot byte, id uint32) (rekordbox.Track, *rekordbox.Analysis, error) {
	export, t, err := fetchTrackRow(ctx, c, slot, id)
	if err != nil {
		return t, nil, err
	}
	// The path comes from the remote PDB: only files under PIONEER/USBANLZ.
	rel := filepath.Clean(strings.TrimPrefix(t.AnalysisPath, "/"))
	if !filepath.IsLocal(rel) || !strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
		return t, nil, fmt.Errorf("prolink: track %d has no ANLZ path", id)
	}
	base := strings.TrimSuffix(rel, filepath.Ext(rel))
	var merged *rekordbox.Analysis
	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		data, _, err := c.Fetch(ctx, export, "/"+base+ext, maxANLZ)
		if err != nil {
			continue // .EXT and .2EX are optional
		}
		if a, err := rekordbox.ParseAnalysis(data); err == nil {
			if merged == nil {
				merged = a
			} else {
				merged.Merge(a)
			}
		}
	}
	if merged == nil {
		return t, nil, fmt.Errorf("prolink: no ANLZ for track %d", id)
	}
	return t, merged, nil
}
