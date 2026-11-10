package prolink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
// export.pdb and the ANLZ files of loaded tracks under Dir/<host>/<player>-<slot>/
// in the drive's own layout, and fetches export.pdb again only when its size
// or mtime changes (a different stick was inserted).
type MediaCache struct {
	Context   context.Context // optional owner cancellation; nil uses Background
	Dir       string
	Enabled   func() bool // checked on every new track; nil means on
	Portmap   int         // tests only; 0 is UDP 111
	mu        sync.Mutex
	libraries map[string]mediaLibrary
	resolved  map[string]mediaResolved
	probes    map[string]mediaProbe
	parses    atomic.Uint64
}

func (m *MediaCache) resolve(dev Device, host net.IP) (*rekordbox.Analysis, string, string) {
	if !m.allowed() || host == nil || dev.TrackID == 0 {
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
	m.mu.Lock()
	defer m.mu.Unlock()
	root := m.root(dev, host)
	local := filepath.Join(root, pdbPath)
	probe, ok := m.probes[root]
	if !ok || time.Since(probe.at) >= 2*time.Second {
		_, file, err := c.Lookup(ctx, export, "/"+pdbPath)
		probe = mediaProbe{at: time.Now(), ok: err == nil}
		if err == nil {
			probe.stamp = fmt.Sprintf("%d %d", file.Size, file.MTime)
		}
		if m.probes == nil {
			m.probes = map[string]mediaProbe{}
		}
		if len(m.probes) >= 32 {
			clear(m.probes)
		}
		m.probes[root] = probe
	}
	if !probe.ok {
		return nil, "", ""
	}
	stamp := probe.stamp
	resolvedKey := fmt.Sprintf("%s:%d", root, dev.TrackID)
	owner, _ := os.ReadFile(filepath.Join(root, ".host"))
	old, _ := os.ReadFile(local + ".stamp")
	_, pendingErr := os.Stat(root + ".pending")
	changed := string(old) != stamp || string(owner) != host.String() || pendingErr == nil
	if changed {
		if err := writeFile(root+".pending", []byte(stamp)); err != nil {
			return nil, "", ""
		}
	}
	version := m.MediaIdentity(dev, host)
	if cached, ok := m.resolved[resolvedKey]; !changed && ok && cached.version == version && cached.trackID == dev.TrackID && time.Since(cached.at) < 3*time.Second {
		return cached.analysis, cached.title, cached.artist
	}
	var data []byte
	var lib *rekordbox.Library
	if changed {
		data, _, err = c.Fetch(ctx, export, "/"+pdbPath, maxPDB)
		if err != nil {
			return nil, "", ""
		}
		m.parses.Add(1)
		lib, err = rekordbox.ParsePDB(mediaReaderAt{ctx, bytes.NewReader(data)}, int64(len(data)))
		if err != nil {
			return nil, "", ""
		}
	} else {
		lib = m.library(ctx, root, version)
	}
	if lib == nil {
		return nil, "", ""
	}
	var track *rekordbox.Track
	additions := map[string][]byte{}
	for i := range lib.Tracks {
		trk := &lib.Tracks[i]
		if trk.ID != dev.TrackID {
			continue
		}
		track = trk
		rel := filepath.Clean(strings.TrimPrefix(trk.AnalysisPath, "/"))
		if !filepath.IsLocal(rel) || !strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
			break
		}
		base := strings.TrimSuffix(rel, filepath.Ext(rel))
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			if ctx.Err() != nil {
				return nil, "", ""
			}
			if !changed {
				if _, err := os.Stat(filepath.Join(root, base+ext)); err == nil {
					continue
				}
			}
			if b, _, err := c.Fetch(ctx, export, "/"+base+ext, maxANLZ); err == nil {
				additions[base+ext] = b
			}
		}
		break
	}
	// Publish PDB and all available analysis together. Readers can never cache a
	// half-fetched disc as a complete overview-only display asset.
	if changed || len(additions) > 0 {
		if !changed {
			data, err = os.ReadFile(local)
			if err != nil || version != m.MediaIdentity(dev, host) {
				return nil, "", ""
			}
		}
		// A disc swapped during the download must never publish the old PDB with
		// the new disc's analysis. Keep the previous display invalid on failure.
		if _, current, err := c.Lookup(ctx, export, "/"+pdbPath); err != nil || fmt.Sprintf("%d %d", current.Size, current.MTime) != stamp {
			return nil, "", ""
		}
		if err := m.publish(root, host, stamp, data, mediaPublication{files: additions, preserve: !changed}); err != nil {
			return nil, "", ""
		}
		version = m.MediaIdentity(dev, host)
		if m.libraries == nil {
			m.libraries = map[string]mediaLibrary{}
		}
		if len(m.libraries) >= 32 {
			clear(m.libraries)
		}
		m.libraries[root] = mediaLibrary{version, lib}
	}
	var analysis *rekordbox.Analysis
	var title, artist string
	if track != nil {
		title, artist = track.Title, track.Artist
		rel := filepath.Clean(strings.TrimPrefix(track.AnalysisPath, "/"))
		if filepath.IsLocal(rel) && strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
			base := strings.TrimSuffix(rel, filepath.Ext(rel))
			for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
				if a, err := rekordbox.ReadAnalysis(filepath.Join(root, base+ext)); err == nil {
					if analysis == nil {
						analysis = a
					} else {
						analysis.Merge(a)
					}
				}
			}
			if analysis != nil {
				analysis.DurationMS = uint32(track.Duration * 1000)
			}
		}
	}
	if version == "" || version != m.MediaIdentity(dev, host) {
		return nil, "", ""
	}
	if m.resolved == nil {
		m.resolved = map[string]mediaResolved{}
	}
	if len(m.resolved) >= 32 {
		clear(m.resolved)
	}
	m.resolved[resolvedKey] = mediaResolved{version: version, trackID: dev.TrackID, analysis: analysis, title: title, artist: artist, at: time.Now()}
	return analysis, title, artist
}

// SharedMediaCacheDir is also rb-bridge's CLI default. A custom --media-cache
// must use the same SC5000_PROLINK_MEDIA_CACHE environment in the WebUI process.
func SharedMediaCacheDir() string {
	if dir := os.Getenv("SC5000_PROLINK_MEDIA_CACHE"); dir != "" {
		return dir
	}
	return DefaultMediaCacheDir
}

type mediaProbe struct {
	at    time.Time
	stamp string
	ok    bool
}

type mediaResolved struct {
	version       string
	trackID       uint32
	analysis      *rekordbox.Analysis
	title, artist string
	at            time.Time
}

type mediaLibrary struct {
	version string
	lib     *rekordbox.Library
}

func (m *MediaCache) root(dev Device, host net.IP) string {
	return filepath.Join(m.Dir, host.String(), fmt.Sprintf("%d-%d", dev.SourcePlayer, dev.SourceSlot))
}

func (m *MediaCache) allowed() bool {
	return m != nil && m.Dir != "" && (m.Enabled == nil || m.Enabled())
}

// MediaIdentity includes the remote PDB stamp and local publication/stat identity.
// The host is parsed as an IP by callers and cannot supply path components.
func (m *MediaCache) MediaIdentity(dev Device, host net.IP) string {
	if !m.allowed() || host == nil || dev.TrackID == 0 {
		return ""
	}
	root := m.root(dev, host)
	if _, err := os.Stat(root + ".pending"); err == nil || !os.IsNotExist(err) {
		return ""
	}
	owner, err := os.ReadFile(filepath.Join(root, ".host"))
	if err != nil || string(owner) != host.String() {
		return ""
	}
	stamp, err := os.ReadFile(filepath.Join(root, pdbPath+".stamp"))
	if err != nil || len(stamp) == 0 {
		return ""
	}
	st, err := os.Stat(filepath.Join(root, pdbPath))
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxPDB {
		return ""
	}
	target, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%d", target, stamp, st.Size(), st.ModTime().UnixNano()))))
}

// Publish a complete new generation, then replace the slot symlink atomically.
// Readers validate the generation before and after PDB/ANLZ reads.
type mediaPublication struct {
	files    map[string][]byte
	preserve bool
}

func (m *MediaCache) publish(root string, host net.IP, stamp string, data []byte, additions ...mediaPublication) error {
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".media-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for path, value := range map[string][]byte{pdbPath: data, pdbPath + ".stamp": []byte(stamp), ".host": []byte(host.String())} {
		if err := writeFile(filepath.Join(tmp, path), value); err != nil {
			return err
		}
	}
	for _, addition := range additions {
		if addition.preserve {
			// Existing immutable ANLZ files can be linked into the next generation.
			// Replacing a staged link uses writeFile's rename, preserving old readers.
			if oldRoot, err := filepath.EvalSymlinks(root); err == nil {
				source := filepath.Join(oldRoot, "PIONEER/USBANLZ")
				err = filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					rel, err := filepath.Rel(oldRoot, path)
					if err != nil || !filepath.IsLocal(rel) {
						return fmt.Errorf("media: invalid cached path")
					}
					target := filepath.Join(tmp, rel)
					if d.IsDir() {
						return os.MkdirAll(target, 0700)
					}
					if !d.Type().IsRegular() {
						return fmt.Errorf("media: non-regular cached analysis")
					}
					return os.Link(path, target)
				})
				if err != nil {
					return err
				}
			}
		}
		for rel, data := range addition.files {
			if !filepath.IsLocal(rel) || !strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
				return fmt.Errorf("media: invalid analysis path")
			}
			if err := writeFile(filepath.Join(tmp, rel), data); err != nil {
				return err
			}
		}
	}
	generation := tmp + "-ready"
	if err := os.Rename(tmp, generation); err != nil {
		return err
	}
	link := tmp + "-link"
	defer os.Remove(link)
	if err := os.Symlink(filepath.Base(generation), link); err != nil {
		os.RemoveAll(generation)
		return err
	}
	old, _ := os.Readlink(root)
	if err := os.Rename(link, root); err != nil {
		os.RemoveAll(generation)
		return err
	}
	_ = os.Remove(root + ".pending")
	if filepath.IsLocal(old) && filepath.Base(old) == old && strings.HasPrefix(old, ".media-stage-") {
		_ = os.RemoveAll(filepath.Join(parent, old))
	}
	return nil
}

// Caller holds m.mu. Failed parses are cached too, until the PDB changes.
func (m *MediaCache) library(ctx context.Context, root, version string) *rekordbox.Library {
	if version == "" {
		return nil
	}
	if c, ok := m.libraries[root]; ok && c.version == version {
		return c.lib
	}
	m.parses.Add(1)
	var lib *rekordbox.Library
	if f, err := os.Open(filepath.Join(root, pdbPath)); err == nil {
		if st, err := f.Stat(); err == nil {
			lib, _ = rekordbox.ParsePDB(mediaReaderAt{ctx, f}, st.Size())
		}
		f.Close()
	}
	if m.libraries == nil {
		m.libraries = map[string]mediaLibrary{}
	}
	if len(m.libraries) >= 32 {
		clear(m.libraries)
	}
	if ctx.Err() != nil {
		return nil
	}
	m.libraries[root] = mediaLibrary{version, lib}
	return lib
}

// PDBParseCount is monotonic, including unsuccessful parses.
func (m *MediaCache) PDBParseCount() uint64 { return m.parses.Load() }

// CachedAnalysis reads the exact track across the rb-bridge process boundary.
// It never opens a network client, writes files, scans unrelated ANLZ, or reads audio.
func (m *MediaCache) CachedAnalysis(dev Device, host net.IP) (*rekordbox.Analysis, string, string) {
	return m.CachedAnalysisVersion(dev, host, m.MediaIdentity(dev, host))
}

// CachedAnalysisVersion refuses a changed owner or medium, including changes
// while parsing. No network, audio, or cache writes are performed.
func (m *MediaCache) CachedAnalysisVersion(dev Device, host net.IP, version string) (*rekordbox.Analysis, string, string) {
	if version == "" || version != m.MediaIdentity(dev, host) {
		return nil, "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	root := m.root(dev, host)
	// Pin the generation so an atomic publication cannot mix two discs' files.
	snapshot, err := filepath.EvalSymlinks(root)
	if err != nil || version != m.MediaIdentity(dev, host) {
		return nil, "", ""
	}
	lib := m.library(context.Background(), root, version)
	if lib == nil {
		return nil, "", ""
	}
	for _, trk := range lib.Tracks {
		if trk.ID != dev.TrackID {
			continue
		}
		rel := filepath.Clean(strings.TrimPrefix(trk.AnalysisPath, "/"))
		if !filepath.IsLocal(rel) || !strings.HasPrefix(rel, "PIONEER/USBANLZ/") {
			return nil, "", ""
		}
		base := strings.TrimSuffix(rel, filepath.Ext(rel))
		var merged *rekordbox.Analysis
		for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
			path, err := filepath.EvalSymlinks(filepath.Join(snapshot, base+ext))
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(snapshot, path)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			if a, err := rekordbox.ReadAnalysis(path); err == nil {
				if merged == nil {
					merged = a
				} else {
					merged.Merge(a)
				}
			}
		}
		if version != m.MediaIdentity(dev, host) {
			return nil, "", ""
		}
		if merged != nil {
			merged.DurationMS = uint32(trk.Duration * 1000)
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
// progress, when given, hears the bytes copied so far and the file's size.
func FetchTrack(ctx context.Context, c *NFSClient, slot byte, id uint32, dir string, progress ...func(done, total int64)) (string, error) {
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
	var w io.Writer = f
	if len(progress) > 0 && progress[0] != nil {
		w = &fetchProgress{w: f, report: progress[0]}
	}
	_, err = c.FetchTo(ctx, export, "/"+filepath.ToSlash(rel), w, maxAudio)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return local, os.Rename(f.Name(), local)
}

// fetchProgress is FetchTrack's writer: FetchTo tells it the file's size.
type fetchProgress struct {
	w           io.Writer
	report      func(done, total int64)
	done, total int64
}

func (p *fetchProgress) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	p.report(p.done, p.total)
	return n, err
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
