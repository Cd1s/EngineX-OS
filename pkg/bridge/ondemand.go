package bridge

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"golang.org/x/sys/unix"
)

const (
	onDemandMaxSnapshotBytes  = 32 << 20
	onDemandMaxANLZBytes      = 16 << 20
	onDemandMaxExportDBBytes  = 64 << 20
	onDemandMaxRBXA1Bytes     = 4 << 20
	onDemandGlobalConcurrency = 4
	onDemandVolumeConcurrency = 2
	onDemandBuildTimeout      = 3 * time.Second
)

var (
	// ErrOnDemandNotFound indicates the track or snapshot is absent or belongs to Engine's own library.
	ErrOnDemandNotFound = errors.New("bridge: on-demand track not found in snapshot")
	// ErrOnDemandUUIDMismatch indicates the request's library UUID differs from the stick's snapshot.
	ErrOnDemandUUIDMismatch = errors.New("bridge: on-demand snapshot uuid mismatch")
	// ErrOnDemandDegraded indicates the track's audio or ANLZ files are missing, corrupt, or incomplete.
	ErrOnDemandDegraded = errors.New("bridge: on-demand track degraded or incomplete")
	// ErrOnDemandUnavailable indicates the volume was ejected, replaced, or unmounted mid-flight.
	ErrOnDemandUnavailable = errors.New("bridge: on-demand volume unavailable")
	// ErrOnDemandLimit indicates a stick snapshot, ANLZ, or payload exceeded the deck budget.
	ErrOnDemandLimit = errors.New("bridge: on-demand budget limit exceeded")
)

var onDemandBuildHook func(ctx context.Context, stage, mount string) error

// engineKeyToCamelot is the inverse of enginedb.EngineKey (0..23 -> Camelot).
var engineKeyToCamelot = [24]string{
	"8B", "8A", "9B", "9A", "10B", "10A", "11B", "11A",
	"12B", "12A", "1B", "1A", "2B", "2A", "3B", "3A",
	"4B", "4A", "5B", "5A", "6B", "6A", "7B", "7A",
}

// OnDemandRequest identifies a single stick track that Engine is about to load onto a deck.
type OnDemandRequest struct {
	Mount      string  // mounted volume root (e.g. "/media/NEXTLIB")
	UUID       string  // Information.uuid from m.db (checked against snapshot when non-empty)
	EnginePath string  // Track.path as stored in m.db (e.g. "../Contents/Artist/Track.mp3")
	EngineID   int64   // Track.id in m.db (verified against snapshot when > 0)
	EngineKey  int     // Track.key (0..23) in m.db, or -1 if NULL/unknown
	EngineBPM  float64 // Track.bpm (or bpmAnalyzed) in m.db, 0 if unknown
	EngineLen  int     // Track.length (seconds) in m.db, 0 if unknown
}

// OnDemandTiming breaks down single-track direct-read latency in milliseconds.
type OnDemandTiming struct {
	SnapshotMS    float64 `json:"snapshotMs"`
	StatMS        float64 `json:"statMs"`
	ANLZReadMS    float64 `json:"anlzReadMs"`
	AudioHeaderMS float64 `json:"audioHeaderMs"`
	ExportMS      float64 `json:"exportMs"`
	EncodeMS      float64 `json:"encodeMs"`
	TotalMS       float64 `json:"totalMs"`
	ANLZBytes     int64   `json:"anlzBytes"`
	UsedExport    bool    `json:"usedExport"`
}

// OnDemandResult is the in-memory RBXA1 payload and digest metadata returned to the UI hook.
type OnDemandResult struct {
	RBXA1         []byte         // rbstate.EncodePerf(perf)
	Key           string         // folded track key (rbstate.Track.Key)
	Written       []byte         // 16-byte enginedb.WrittenDigest(beats, cues, loops)
	WrittenNoMain []byte         // 16-byte writtenDigestIgnoringMainCueTail(beats, cues, loops)
	RowHash       string         // rbstate.Track.Row
	ActiveLoops   uint32         // activeOnLoadLoops bitmask
	Rules         int            // rbstate.Rules (5)
	Timing        OnDemandTiming // per-stage latency breakdown
}

// OnDemandFileID pins a regular file inside a mounted volume by device, inode, size, and mtime.
type OnDemandFileID struct {
	Present bool
	Dev     uint64
	Ino     uint64
	Size    int64
	ModNS   int64
}

// OnDemandStickIdentity pins the mounted volume, bridge snapshot generation, live m.db file,
// Device Library exports, audio file, ANLZ sidecars, and resolved track entry.
type OnDemandStickIdentity struct {
	Mount        string
	MountDev     uint64
	MountMntID   uint64
	MountIno     uint64
	SnapshotUUID string
	SnapFile     OnDemandFileID
	DBFile       OnDemandFileID
	ExportDB     OnDemandFileID
	ExportPDB    OnDemandFileID
	AudioFile    OnDemandFileID
	ANLZFiles    [3]OnDemandFileID
	TrackKey     string
	TrackRel     string
	EngineID     int64
	RowHash      string
	FastKnown    bool
	FastCues     []byte
	FastLoops    []byte
	FastActive   uint32
}

type pinnedMountKey struct {
	mount string
	dev   uint64
	mntID uint64
	ino   uint64
}

type cachedOnDemandSnapshot struct {
	ino   uint64
	size  int64
	mtime time.Time
	snap  *rbstate.Snapshot
	res   *compat.Resolver
}

type onDemandCall struct {
	waiters int
	done    chan struct{}
	cancel  context.CancelFunc
	res     *OnDemandResult
	err     error
}

type metaCall[V any] struct {
	waiters int
	done    chan struct{}
	cancel  context.CancelFunc
	val     V
	err     error
}

// OnDemandBuilderConfig configures mount confinement and mountpoint enforcement for OnDemandBuilder.
type OnDemandBuilderConfig struct {
	MediaRoot         string
	RequireMountpoint bool
}

// OnDemandBuilder reads a single track's rekordbox ANLZ files and audio header directly from the
// stick and translates them in memory into Engine's RBXA1 format with zero staging writes.
type OnDemandBuilder struct {
	cfg        OnDemandBuilderConfig
	globalSem  chan struct{}
	volSemMu   sync.Mutex
	volSems    map[string]chan struct{}
	snapMu     sync.Mutex
	snaps      map[pinnedMountKey]*cachedOnDemandSnapshot
	flightMu   sync.Mutex
	inflight   map[string]*onDemandCall
	metaMu     sync.Mutex
	snapFlight map[pinnedMountKey]*metaCall[*cachedOnDemandSnapshot]
}

// NewOnDemandBuilder returns an OnDemandBuilder with bounded per-volume snapshot and flight caches.
func NewOnDemandBuilder() *OnDemandBuilder {
	return NewOnDemandBuilderWithConfig(OnDemandBuilderConfig{})
}

// NewOnDemandBuilderWithConfig returns an OnDemandBuilder enforcing cfg confinement and limits.
func NewOnDemandBuilderWithConfig(cfg OnDemandBuilderConfig) *OnDemandBuilder {
	if cfg.MediaRoot != "" {
		cfg.MediaRoot = filepath.Clean(strings.TrimSpace(cfg.MediaRoot))
	}
	return &OnDemandBuilder{
		cfg:        cfg,
		globalSem:  make(chan struct{}, onDemandGlobalConcurrency),
		volSems:    map[string]chan struct{}{},
		snaps:      map[pinnedMountKey]*cachedOnDemandSnapshot{},
		inflight:   map[string]*onDemandCall{},
		snapFlight: map[pinnedMountKey]*metaCall[*cachedOnDemandSnapshot]{},
	}
}

// NoteLoading signals any active background rb-bridge pass to yield the stick during an on-demand
// track load, without clearing an existing playing state and without writing when no pass is active.
func (q *PriorityQueue) NoteLoading(now time.Time) {
	if q == nil {
		return
	}
	if _, err := os.Lstat(usbPreparingFlagPath); err != nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	prevLoading := q.deckLoading
	q.deckLoading = true
	q.hasActivity = true
	if !prevLoading || now.Sub(q.lastActWrite) >= time.Second {
		act := DeckActivity{Active: q.deckActive, Loading: true, IdleSamples: q.idleSamples, UpdatedAtMS: now.UnixMilli()}
		if writePrivateJSON(deckActivityPath(), act) == nil {
			q.lastActWrite = now
		}
	}
}

func cleanEnginePath(p string) (string, error) {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "../")
	return compat.CleanRelative(p)
}

func isVolumeEjecting(mount string) bool {
	_, err := os.Lstat(prolink.EjectingMark(mount))
	return err == nil
}

func fdMntID(fd int) (uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			id, perr := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if perr == nil {
				return id, true
			}
			return 0, false
		}
	}
	return 0, false
}

func statFDIdentity(fd int) (uint64, uint64, uint64, error) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stx); err != nil {
		return 0, 0, 0, err
	}
	dev := unix.Mkdev(stx.Dev_major, stx.Dev_minor)
	if stx.Mask&unix.STATX_MNT_ID != 0 {
		return dev, stx.Mnt_id, stx.Ino, nil
	}
	if mid, ok := fdMntID(fd); ok {
		return dev, mid, stx.Ino, nil
	}
	return 0, 0, 0, errors.New("mount identity unavailable")
}

func statDirIdentity(dir string) (pinnedMountKey, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return pinnedMountKey{}, err
	}
	defer unix.Close(fd)
	dev, mntID, ino, err := statFDIdentity(fd)
	if err != nil {
		return pinnedMountKey{}, err
	}
	return pinnedMountKey{mount: dir, dev: dev, mntID: mntID, ino: ino}, nil
}

func statFileInfoID(st os.FileInfo) OnDemandFileID {
	id := OnDemandFileID{
		Present: true,
		Size:    st.Size(),
		ModNS:   st.ModTime().UnixNano(),
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys != nil {
		id.Dev = uint64(sys.Dev)
		id.Ino = uint64(sys.Ino)
	}
	return id
}

func statRootFileID(root *os.Root, rel string) (OnDemandFileID, error) {
	f, err := openRootFileNonblock(root, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OnDemandFileID{}, nil
		}
		return OnDemandFileID{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return OnDemandFileID{}, err
	}
	return statFileInfoID(st), nil
}

func statRootRegularFileID(ctx context.Context, root *os.Root, res *compat.Resolver, rel string) (OnDemandFileID, error) {
	if rel == "" {
		return OnDemandFileID{}, nil
	}
	f, err := openRootRegular(ctx, root, res, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OnDemandFileID{}, nil
		}
		return OnDemandFileID{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return OnDemandFileID{}, err
	}
	return statFileInfoID(st), nil
}

func openPinnedRoot(mediaRoot, mount string, requireMountpoint bool) (*os.Root, pinnedMountKey, error) {
	if mediaRoot != "" {
		rel, err := filepath.Rel(mediaRoot, mount)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "..") || strings.ContainsRune(filepath.ToSlash(rel), '/') {
			return nil, pinnedMountKey{}, ErrOnDemandNotFound
		}
	}
	dirID, err := statDirIdentity(mount)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
			return nil, pinnedMountKey{}, ErrOnDemandNotFound
		}
		return nil, pinnedMountKey{}, err
	}
	if requireMountpoint {
		parentDir := mediaRoot
		if parentDir == "" {
			parentDir = filepath.Dir(mount)
		}
		parentID, perr := statDirIdentity(parentDir)
		if perr != nil || (parentID.mntID == dirID.mntID && parentID.dev == dirID.dev) {
			return nil, pinnedMountKey{}, ErrOnDemandNotFound
		}
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, pinnedMountKey{}, ErrOnDemandNotFound
		}
		return nil, pinnedMountKey{}, err
	}
	dot, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, pinnedMountKey{}, err
	}
	dev, mntID, ino, idErr := statFDIdentity(int(dot.Fd()))
	dot.Close()
	if idErr != nil || dev != dirID.dev || mntID != dirID.mntID || ino != dirID.ino {
		root.Close()
		return nil, pinnedMountKey{}, ErrOnDemandUnavailable
	}
	return root, dirID, nil
}

func verifyMountPinned(want pinnedMountKey) error {
	if isVolumeEjecting(want.mount) {
		return ErrOnDemandUnavailable
	}
	got, err := statDirIdentity(want.mount)
	if err != nil || got != want {
		return ErrOnDemandUnavailable
	}
	return nil
}

type ctxReader struct {
	ctx   context.Context
	r     io.Reader
	mount string
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if isVolumeEjecting(c.mount) {
		return 0, ErrOnDemandUnavailable
	}
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type ctxReaderAt struct {
	ctx   context.Context
	r     io.ReaderAt
	mount string
}

func (c *ctxReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if isVolumeEjecting(c.mount) {
		return 0, ErrOnDemandUnavailable
	}
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.ReadAt(p, off)
}

func runSharedMeta[V any](ctx context.Context, mu *sync.Mutex, flights map[pinnedMountKey]*metaCall[V], key pinnedMountKey, fn func(context.Context) (V, error)) (V, error) {
	var zero V
	mu.Lock()
	if c, ok := flights[key]; ok {
		c.waiters++
		mu.Unlock()
		select {
		case <-c.done:
			return c.val, c.err
		case <-ctx.Done():
			mu.Lock()
			c.waiters--
			if c.waiters == 0 && c.cancel != nil {
				c.cancel()
			}
			mu.Unlock()
			return zero, ctx.Err()
		}
	}
	buildCtx, cancel := context.WithTimeout(context.Background(), onDemandBuildTimeout)
	c := &metaCall[V]{waiters: 1, done: make(chan struct{}), cancel: cancel}
	flights[key] = c
	mu.Unlock()

	go func() {
		defer cancel()
		c.val, c.err = fn(buildCtx)
		mu.Lock()
		if flights[key] == c {
			delete(flights, key)
		}
		mu.Unlock()
		close(c.done)
	}()

	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		mu.Lock()
		c.waiters--
		if c.waiters == 0 && c.cancel != nil {
			c.cancel()
		}
		mu.Unlock()
		return zero, ctx.Err()
	}
}

func (b *OnDemandBuilder) volSemFor(mount string) chan struct{} {
	b.volSemMu.Lock()
	defer b.volSemMu.Unlock()
	if s := b.volSems[mount]; s != nil {
		return s
	}
	if len(b.volSems) >= 16 {
		clear(b.volSems)
	}
	s := make(chan struct{}, onDemandVolumeConcurrency)
	b.volSems[mount] = s
	return s
}

func (b *OnDemandBuilder) acquireBuildSlots(ctx context.Context, mount string) (func(), error) {
	vsem := b.volSemFor(mount)
	select {
	case vsem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case b.globalSem <- struct{}{}:
	case <-ctx.Done():
		<-vsem
		return nil, ctx.Err()
	}
	return func() {
		<-b.globalSem
		<-vsem
	}, nil
}

// Build reads one track's rekordbox ANLZ + audio header straight from req.Mount and returns its
// in-memory RBXA1 performance payload. Concurrent requests for the same track are coalesced
// and cancelled immediately when the last waiter leaves.
func (b *OnDemandBuilder) Build(ctx context.Context, req OnDemandRequest) (*OnDemandResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	mount := filepath.Clean(strings.TrimSpace(req.Mount))
	if mount == "" || mount == "." || !filepath.IsAbs(mount) {
		return nil, ErrOnDemandNotFound
	}
	if isVolumeEjecting(mount) {
		return nil, ErrOnDemandUnavailable
	}
	cleanRel, err := cleanEnginePath(req.EnginePath)
	if err != nil {
		return nil, ErrOnDemandNotFound
	}
	req.Mount = mount
	req.EnginePath = cleanRel
	dirID, dirErr := statDirIdentity(mount)
	if dirErr != nil {
		if errors.Is(dirErr, os.ErrNotExist) || errors.Is(dirErr, syscall.ELOOP) || errors.Is(dirErr, syscall.ENOTDIR) {
			return nil, ErrOnDemandNotFound
		}
		return nil, dirErr
	}
	sfKey := fmt.Sprintf("%s\x00%d:%d:%d\x00%s\x00%s\x00%d\x00%d\x00%x\x00%d",
		mount, dirID.dev, dirID.mntID, dirID.ino, req.UUID, rbstate.FoldPath(cleanRel),
		req.EngineID, req.EngineKey, math.Float64bits(req.EngineBPM), req.EngineLen)

	b.flightMu.Lock()
	if call, ok := b.inflight[sfKey]; ok {
		call.waiters++
		b.flightMu.Unlock()
		select {
		case <-call.done:
			return call.res, call.err
		case <-ctx.Done():
			b.flightMu.Lock()
			call.waiters--
			if call.waiters == 0 && call.cancel != nil {
				call.cancel()
			}
			b.flightMu.Unlock()
			return nil, ctx.Err()
		}
	}
	buildCtx, cancel := context.WithTimeout(context.Background(), onDemandBuildTimeout)
	call := &onDemandCall{waiters: 1, done: make(chan struct{}), cancel: cancel}
	b.inflight[sfKey] = call
	b.flightMu.Unlock()

	go func() {
		defer cancel()
		call.res, call.err = b.buildBounded(buildCtx, req)
		b.flightMu.Lock()
		if b.inflight[sfKey] == call {
			delete(b.inflight, sfKey)
		}
		b.flightMu.Unlock()
		close(call.done)
	}()

	select {
	case <-call.done:
		return call.res, call.err
	case <-ctx.Done():
		b.flightMu.Lock()
		call.waiters--
		if call.waiters == 0 && call.cancel != nil {
			call.cancel()
		}
		b.flightMu.Unlock()
		return nil, ctx.Err()
	}
}

func (b *OnDemandBuilder) buildBounded(ctx context.Context, req OnDemandRequest) (*OnDemandResult, error) {
	release, err := b.acquireBuildSlots(ctx, req.Mount)
	if err != nil {
		return nil, err
	}
	defer release()
	return b.buildOnce(ctx, req)
}

func generatedExportRooted(root *os.Root) bool {
	pdbFile, err := openRootFileNonblock(root, devicePDB)
	if err != nil {
		return false
	}
	st, serr := pdbFile.Stat()
	pdbFile.Close()
	if serr != nil {
		return false
	}
	mf, err := openRootFileNonblock(root, "PIONEER/rekordbox/.enginex-generated")
	if err != nil {
		return false
	}
	defer mf.Close()
	b, err := io.ReadAll(io.LimitReader(mf, 256))
	return err == nil && string(b) == fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

func (b *OnDemandBuilder) loadSnapshot(ctx context.Context, key pinnedMountKey, root *os.Root) (*rbstate.Snapshot, *compat.Resolver, error) {
	relSnap := filepath.Join(databaseSubdir, rbstate.SnapshotName)
	f, err := openRootFileNonblock(root, relSnap)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrOnDemandNotFound
		}
		return nil, nil, err
	}
	st, serr := f.Stat()
	if serr != nil {
		f.Close()
		return nil, nil, serr
	}
	if st.Size() > onDemandMaxSnapshotBytes {
		f.Close()
		return nil, nil, fmt.Errorf("%w: snapshot exceeds %d bytes", ErrOnDemandLimit, onDemandMaxSnapshotBytes)
	}
	sid := statFileInfoID(st)
	b.snapMu.Lock()
	if c := b.snaps[key]; c != nil && c.ino == sid.Ino && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		b.snapMu.Unlock()
		f.Close()
		return c.snap, c.res, nil
	}
	b.snapMu.Unlock()
	f.Close()

	cached, err := runSharedMeta(ctx, &b.metaMu, b.snapFlight, key, func(mctx context.Context) (*cachedOnDemandSnapshot, error) {
		if onDemandBuildHook != nil {
			if herr := onDemandBuildHook(mctx, "snapshot", key.mount); herr != nil {
				return nil, herr
			}
		}
		mroot, mkey, err := openPinnedRoot(b.cfg.MediaRoot, key.mount, b.cfg.RequireMountpoint)
		if err != nil {
			return nil, err
		}
		defer mroot.Close()
		if mkey != key || isVolumeEjecting(key.mount) {
			return nil, ErrOnDemandUnavailable
		}
		sf, err := openRootFileNonblock(mroot, relSnap)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrOnDemandNotFound
			}
			return nil, err
		}
		defer sf.Close()
		sst, serr := sf.Stat()
		if serr != nil {
			return nil, serr
		}
		if sst.Size() > onDemandMaxSnapshotBytes {
			return nil, fmt.Errorf("%w: snapshot exceeds %d bytes", ErrOnDemandLimit, onDemandMaxSnapshotBytes)
		}
		ssid := statFileInfoID(sst)
		b.snapMu.Lock()
		if c := b.snaps[key]; c != nil && c.ino == ssid.Ino && c.size == sst.Size() && c.mtime.Equal(sst.ModTime()) {
			b.snapMu.Unlock()
			return c, nil
		}
		b.snapMu.Unlock()

		lr := &io.LimitedReader{R: &ctxReader{ctx: mctx, r: sf, mount: key.mount}, N: onDemandMaxSnapshotBytes + 1}
		snap, err := rbstate.LoadReader(bufio.NewReaderSize(lr, 64<<10))
		if err != nil {
			if lr.N <= 0 {
				return nil, fmt.Errorf("%w: snapshot exceeds %d bytes", ErrOnDemandLimit, onDemandMaxSnapshotBytes)
			}
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, rbstate.ErrNoSnapshot) {
				return nil, ErrOnDemandNotFound
			}
			return nil, err
		}
		res, err := compat.NewResolver(key.mount)
		if err != nil {
			return nil, err
		}
		entry := &cachedOnDemandSnapshot{ino: ssid.Ino, size: sst.Size(), mtime: sst.ModTime(), snap: snap, res: res}
		b.snapMu.Lock()
		if len(b.snaps) >= 4 {
			clear(b.snaps)
		}
		b.snaps[key] = entry
		b.snapMu.Unlock()
		return entry, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return cached.snap, cached.res, nil
}

// InspectIdentity pins the mounted volume, bridge snapshot generation, Engine DB file,
// Device Library exports, audio file, ANLZ sidecars, and resolved track entry without
// reading audio or decoding ANLZ data. Callers use it to key and revalidate cached builds.
func (b *OnDemandBuilder) InspectIdentity(ctx context.Context, req OnDemandRequest) (OnDemandStickIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	mount := filepath.Clean(strings.TrimSpace(req.Mount))
	if mount == "" || mount == "." || !filepath.IsAbs(mount) {
		return OnDemandStickIdentity{}, ErrOnDemandNotFound
	}
	if isVolumeEjecting(mount) {
		return OnDemandStickIdentity{}, ErrOnDemandUnavailable
	}
	cleanRel, err := cleanEnginePath(req.EnginePath)
	if err != nil {
		return OnDemandStickIdentity{}, ErrOnDemandNotFound
	}
	root, pinnedKey, err := openPinnedRoot(b.cfg.MediaRoot, mount, b.cfg.RequireMountpoint)
	if err != nil {
		return OnDemandStickIdentity{}, err
	}
	defer root.Close()
	if isVolumeEjecting(mount) {
		return OnDemandStickIdentity{}, ErrOnDemandUnavailable
	}
	snap, res, err := b.loadSnapshot(ctx, pinnedKey, root)
	if err != nil {
		return OnDemandStickIdentity{}, err
	}
	if req.UUID != "" && snap.LibraryUUID != req.UUID {
		return OnDemandStickIdentity{}, ErrOnDemandUUIDMismatch
	}
	tr, ok := snap.TrackAt(cleanRel)
	if !ok || tr == nil || tr.Native {
		return OnDemandStickIdentity{}, ErrOnDemandNotFound
	}
	if req.EngineID > 0 && tr.EngineID > 0 && tr.EngineID != req.EngineID {
		return OnDemandStickIdentity{}, ErrOnDemandNotFound
	}
	snapID, err := statRootFileID(root, filepath.Join(databaseSubdir, rbstate.SnapshotName))
	if err != nil {
		return OnDemandStickIdentity{}, err
	}
	if !snapID.Present {
		return OnDemandStickIdentity{}, ErrOnDemandNotFound
	}
	dbID, err := statRootFileID(root, filepath.Join(databaseSubdir, "m.db"))
	if err != nil {
		return OnDemandStickIdentity{}, err
	}
	expDBID, err := statRootFileID(root, "PIONEER/rekordbox/exportLibrary.db")
	if err != nil {
		return OnDemandStickIdentity{}, err
	}
	expPDBID, err := statRootFileID(root, devicePDB)
	if err != nil {
		return OnDemandStickIdentity{}, err
	}
	audioRel := tr.Rel
	if audioRel == "" {
		audioRel = cleanRel
	}
	audioID, err := statRootRegularFileID(ctx, root, res, audioRel)
	if err != nil {
		return OnDemandStickIdentity{}, err
	}
	if !audioID.Present {
		return OnDemandStickIdentity{}, ErrOnDemandDegraded
	}
	datRel, extRel, ex2Rel := anlzCandidates(tr, "")
	var anlzIDs [3]OnDemandFileID
	for i, rel := range []string{datRel, extRel, ex2Rel} {
		if rel == "" {
			continue
		}
		aid, aerr := statRootRegularFileID(ctx, root, res, rel)
		if aerr != nil {
			return OnDemandStickIdentity{}, aerr
		}
		anlzIDs[i] = aid
	}
	if err := verifyMountPinned(pinnedKey); err != nil {
		return OnDemandStickIdentity{}, err
	}
	return OnDemandStickIdentity{
		Mount:        mount,
		MountDev:     pinnedKey.dev,
		MountMntID:   pinnedKey.mntID,
		MountIno:     pinnedKey.ino,
		SnapshotUUID: snap.LibraryUUID,
		SnapFile:     snapID,
		DBFile:       dbID,
		ExportDB:     expDBID,
		ExportPDB:    expPDBID,
		AudioFile:    audioID,
		ANLZFiles:    anlzIDs,
		TrackKey:     tr.Key,
		TrackRel:     audioRel,
		EngineID:     tr.EngineID,
		RowHash:      tr.Row,
		FastKnown:    tr.FastKnown,
		FastCues:     tr.FastCues,
		FastLoops:    tr.FastLoops,
		FastActive:   tr.FastActive,
	}, nil
}

// findPDBTrackRooted scans ONLY the Track table of PIONEER/rekordbox/export.pdb strictly through
// the pinned root fd for legacy snapshots that lack AnalysisPath or AAC SBR SampleRate. It never
// opens or decrypts exportLibrary.db and never parses playlists or lookup tables on the load path.
func (b *OnDemandBuilder) findPDBTrackRooted(ctx context.Context, key pinnedMountKey, root *os.Root, res *compat.Resolver, tr *rbstate.Track) (rekordbox.Track, bool, error) {
	if err := verifyMountPinned(key); err != nil {
		return rekordbox.Track{}, false, err
	}
	if generatedExportRooted(root) {
		return rekordbox.Track{}, false, nil
	}
	f, _, err := openRootRegularWithRel(ctx, root, res, devicePDB)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rekordbox.Track{}, false, nil
		}
		return rekordbox.Track{}, false, err
	}
	defer f.Close()
	st, serr := f.Stat()
	if serr != nil {
		return rekordbox.Track{}, false, serr
	}
	if st.Size() > onDemandMaxExportDBBytes {
		return rekordbox.Track{}, false, ErrOnDemandLimit
	}
	var wantDeviceID uint32
	for _, ref := range tr.Refs {
		if ref.Kind == rbstate.DeviceLibrary && ref.ID != 0 {
			wantDeviceID = ref.ID
			break
		}
	}
	foundTrack, ok, err := rekordbox.FindPDBTrack(&ctxReaderAt{ctx: ctx, r: f, mount: key.mount}, st.Size(), func(id uint32, trackPath string) bool {
		if wantDeviceID != 0 && id == wantDeviceID && rbstate.FoldPath(trackPath) == tr.Key {
			return true
		}
		return rbstate.FoldPath(trackPath) == tr.Key
	})
	if err != nil || !ok {
		return rekordbox.Track{}, false, err
	}
	if err := verifyMountPinned(key); err != nil {
		return rekordbox.Track{}, false, err
	}
	return foundTrack, true, nil
}

func openRootFileNonblock(root *os.Root, sub string) (*os.File, error) {
	f, err := root.OpenFile(filepath.FromSlash(sub), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, serr := f.Stat()
	if serr != nil || !st.Mode().IsRegular() {
		f.Close()
		if serr != nil {
			return nil, serr
		}
		return nil, fmt.Errorf("not a regular file: %s", sub)
	}
	return f, nil
}

// openRootRegularWithRel resolves and opens rel strictly confined within root with O_NONBLOCK + IsRegular.
func openRootRegularWithRel(ctx context.Context, root *os.Root, res *compat.Resolver, rel string) (*os.File, string, error) {
	clean, err := compat.CleanRelative(rel)
	if err != nil {
		return nil, "", err
	}
	f, err := openRootFileNonblock(root, clean)
	if err == nil {
		return f, clean, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	resolved, err := res.ResolveContext(ctx, clean)
	if err != nil {
		return nil, "", err
	}
	f, err = openRootFileNonblock(root, resolved)
	if err != nil {
		return nil, "", err
	}
	return f, resolved, nil
}

// openRootRegular opens rel strictly confined within root (refusing symlinks, FIFOs, and path escapes).
func openRootRegular(ctx context.Context, root *os.Root, res *compat.Resolver, rel string) (*os.File, error) {
	f, _, err := openRootRegularWithRel(ctx, root, res, rel)
	return f, err
}

func anlzCandidates(tr *rbstate.Track, fallbackAnalysisPath string) (datRel, extRel, ex2Rel string) {
	if len(tr.Built.ANLZ) > 0 {
		for _, f := range tr.Built.ANLZ {
			switch strings.ToUpper(path.Ext(f.Rel)) {
			case ".DAT":
				datRel = f.Rel
			case ".EXT":
				extRel = f.Rel
			case ".2EX":
				ex2Rel = f.Rel
			}
		}
		if datRel != "" || extRel != "" || ex2Rel != "" {
			return datRel, extRel, ex2Rel
		}
	}
	ap := tr.AnalysisPath
	if ap == "" {
		ap = fallbackAnalysisPath
	}
	if ap == "" {
		return "", "", ""
	}
	base := strings.TrimSuffix(ap, path.Ext(ap))
	return base + ".DAT", base + ".EXT", base + ".2EX"
}

func hasPWV7Waveform(a *rekordbox.Analysis) bool {
	if a == nil {
		return false
	}
	for i := range a.Waveforms {
		if a.Waveforms[i].Tag == "PWV7" && len(a.Waveforms[i].Data) > 0 {
			return true
		}
	}
	return false
}

func (b *OnDemandBuilder) buildOnce(ctx context.Context, req OnDemandRequest) (*OnDemandResult, error) {
	tStart := time.Now()
	var timing OnDemandTiming

	root, pinnedKey, err := openPinnedRoot(b.cfg.MediaRoot, req.Mount, b.cfg.RequireMountpoint)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if isVolumeEjecting(req.Mount) {
		return nil, ErrOnDemandUnavailable
	}

	t0 := time.Now()
	snap, res, err := b.loadSnapshot(ctx, pinnedKey, root)
	timing.SnapshotMS = float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		return nil, err
	}
	if req.UUID != "" && snap.LibraryUUID != req.UUID {
		return nil, ErrOnDemandUUIDMismatch
	}
	tr, ok := snap.TrackAt(req.EnginePath)
	if !ok || tr == nil || tr.Native {
		return nil, ErrOnDemandNotFound
	}
	if req.EngineID > 0 && tr.EngineID > 0 && tr.EngineID != req.EngineID {
		return nil, ErrOnDemandNotFound
	}

	var expTrack rekordbox.Track
	var hasExpTrack bool
	ensureExport := func() error {
		if hasExpTrack {
			return nil
		}
		te := time.Now()
		et, found, lerr := b.findPDBTrackRooted(ctx, pinnedKey, root, res, tr)
		timing.ExportMS += float64(time.Since(te).Microseconds()) / 1000.0
		timing.UsedExport = true
		if lerr != nil {
			return lerr
		}
		if found {
			expTrack, hasExpTrack = et, true
		}
		return nil
	}

	// Open audio file confined to root (direct nonblocking open first, zero ReadDir on common case).
	tStat := time.Now()
	audioRel := tr.Rel
	if audioRel == "" {
		audioRel = req.EnginePath
	}
	audioFile, err := openRootRegular(ctx, root, res, audioRel)
	timing.StatMS = float64(time.Since(tStat).Microseconds()) / 1000.0
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: audio missing: %v", ErrOnDemandDegraded, err)
		}
		return nil, err
	}
	defer audioFile.Close()

	// Determine ANLZ sibling paths; fall back to rooted export DB only when snapshot lacks AnalysisPath.
	datRel, extRel, ex2Rel := anlzCandidates(tr, "")
	if datRel == "" && extRel == "" && ex2Rel == "" {
		if eerr := ensureExport(); eerr != nil {
			if errors.Is(eerr, ErrOnDemandUnavailable) || errors.Is(eerr, context.Canceled) || errors.Is(eerr, context.DeadlineExceeded) {
				return nil, eerr
			}
		}
		if hasExpTrack && expTrack.AnalysisPath != "" {
			datRel, extRel, ex2Rel = anlzCandidates(tr, expTrack.AnalysisPath)
		}
	}
	if datRel == "" && extRel == "" && ex2Rel == "" {
		return nil, fmt.Errorf("%w: no analysis path for %s", ErrOnDemandDegraded, tr.Key)
	}

	if onDemandBuildHook != nil {
		if herr := onDemandBuildHook(ctx, "anlz", req.Mount); herr != nil {
			return nil, herr
		}
	}

	// Read ANLZ files with chunk-level cancellation: .2EX first (PWV7 only); if PWV7 is present,
	// skip all waveform bodies in .DAT and .EXT.
	tANLZ := time.Now()
	var a2ex, aext, adat *rekordbox.Analysis
	readCount := 0
	readOne := func(rel string, skip func(string) bool) (*rekordbox.Analysis, error) {
		if rel == "" {
			return nil, nil
		}
		f, err := openRootRegular(ctx, root, res, rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		defer f.Close()
		st, serr := f.Stat()
		if serr != nil {
			return nil, serr
		}
		if st.Size() > onDemandMaxANLZBytes {
			return nil, fmt.Errorf("%w: anlz %s exceeds %d bytes", ErrOnDemandLimit, rel, onDemandMaxANLZBytes)
		}
		a, n, err := rekordbox.ReadAnalysisFilePaced(f, skip, 64<<10, func(int, time.Duration) error {
			if isVolumeEjecting(req.Mount) {
				return ErrOnDemandUnavailable
			}
			return ctx.Err()
		})
		timing.ANLZBytes += n
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOnDemandUnavailable) {
				return nil, err
			}
			var pe *os.PathError
			if errors.As(err, &pe) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: anlz %s: %v", ErrOnDemandDegraded, rel, err)
		}
		readCount++
		return a, nil
	}

	if a2ex, err = readOne(ex2Rel, func(tag string) bool { return tag != "PWV7" }); err != nil {
		return nil, err
	}
	var otherSkip func(string) bool
	if hasPWV7Waveform(a2ex) {
		otherSkip = func(string) bool { return true }
	}
	if adat, err = readOne(datRel, otherSkip); err != nil {
		return nil, err
	}
	if aext, err = readOne(extRel, otherSkip); err != nil {
		return nil, err
	}
	timing.ANLZReadMS = float64(time.Since(tANLZ).Microseconds()) / 1000.0
	if readCount == 0 {
		return nil, fmt.Errorf("%w: no ANLZ files found for %s", ErrOnDemandDegraded, tr.Key)
	}

	// Merge in the exact same order as enginedb.BuildTrack: .DAT -> .EXT -> .2EX.
	merged := &rekordbox.Analysis{}
	for _, part := range []*rekordbox.Analysis{adat, aext, a2ex} {
		if part != nil {
			merged.Merge(part)
		}
	}

	// Read audio header from the confined file handle (no TOCTOU re-open by path).
	tAud := time.Now()
	audio, err := compat.AudioHeaderFile(audioFile, audioRel)
	timing.AudioHeaderMS = float64(time.Since(tAud).Microseconds()) / 1000.0
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: audio header: %v", ErrOnDemandDegraded, err)
	}

	rt := rekordbox.Track{
		ID:       uint32(tr.EngineID),
		Path:     "/" + audioRel,
		Duration: float64(req.EngineLen),
		BPM:      req.EngineBPM,
	}
	if req.EngineKey >= 0 && req.EngineKey < len(engineKeyToCamelot) {
		rt.Key = engineKeyToCamelot[req.EngineKey]
	}
	if hasExpTrack {
		if rt.Key == "" {
			rt.Key = expTrack.Key
		}
		if rt.BPM == 0 {
			rt.BPM = expTrack.BPM
		}
		if expTrack.SampleRate > 0 {
			rt.SampleRate = expTrack.SampleRate
		}
		if expTrack.Duration > 0 {
			rt.Duration = expTrack.Duration
		}
		if len(expTrack.Cues) > 0 {
			merged.Merge(&rekordbox.Analysis{Cues: expTrack.Cues})
		}
	}

	// Implicit AAC SBR: core rate <= 24 kHz in header may double in export metadata (trackbuild.go:239).
	// Use SampleRate/Duration already recorded on current snapshots without reparsing whole exports.
	if audio.Format == "aac" && audio.SampleRate > 0 && audio.SampleRate <= 24000 && !hasExpTrack {
		if tr.SampleRate > audio.SampleRate {
			rt.SampleRate = tr.SampleRate
			if tr.Duration > 0 {
				rt.Duration = tr.Duration
			}
		} else if tr.SampleRate == 0 {
			if eerr := ensureExport(); eerr != nil {
				if errors.Is(eerr, ErrOnDemandUnavailable) || errors.Is(eerr, context.Canceled) || errors.Is(eerr, context.DeadlineExceeded) {
					return nil, eerr
				}
			}
			if hasExpTrack {
				if expTrack.SampleRate > 0 {
					rt.SampleRate = expTrack.SampleRate
				}
				if expTrack.Duration > 0 {
					rt.Duration = expTrack.Duration
				}
				if rt.Key == "" {
					rt.Key = expTrack.Key
				}
				if len(expTrack.Cues) > 0 {
					merged.Merge(&rekordbox.Analysis{Cues: expTrack.Cues})
				}
			}
		}
	}

	if audio.Format == "aac" && rt.SampleRate > 0 && rt.SampleRate != audio.SampleRate {
		// Implicit SBR: keep export sample rate/duration; priming tag is in decoded samples.
		rt.StartOffsetSamples = audio.RekordboxOffset()
	} else if audio.SampleRate > 0 {
		rt.SampleRate = audio.SampleRate
		if audio.Samples > 0 {
			rt.SampleCount = audio.Samples
		}
		if audio.Duration > 0 {
			rt.Duration = audio.Duration
		}
		rt.StartOffsetSamples = audio.RekordboxOffset()
	}

	// Preserve OneLibrary-only cues recorded in snapshot FastCues/FastLoops without touching export DB.
	if !hasExpTrack && (len(tr.FastCues) > 0 || len(tr.FastLoops) > 0) {
		mergeFastCues(merged, tr, rt.SampleRate, rt.SampleCount, rt.Duration)
	}

	// CF049: clamp misparsed 500 BPM tracks and grids identically to enginedb.BuildTrack.
	if rt.BPM >= 499.5 {
		rt.BPM = 0
	}
	if len(merged.Beats) > 0 && merged.Beats[0].BPM >= 499.5 {
		merged.Beats = nil
	}
	if rt.BPM == 0 && len(merged.Beats) > 0 {
		rt.BPM = merged.Beats[0].BPM
	}

	tEnc := time.Now()
	perf, err := enginedb.EncodePerformance(rt, merged)
	if err != nil {
		return nil, fmt.Errorf("%w: encode performance: %v", ErrOnDemandDegraded, err)
	}
	if !perf.Complete || len(perf.TrackData) == 0 || len(perf.Beats) == 0 {
		return nil, fmt.Errorf("%w: incomplete performance data for %s", ErrOnDemandDegraded, tr.Key)
	}
	written := enginedb.WrittenDigest(perf.Beats, perf.QuickCues, perf.Loops)
	writtenNoMain := writtenDigestIgnoringMainCueTail(perf.Beats, perf.QuickCues, perf.Loops)
	rbxa1 := rbstate.EncodePerf(rbstate.Perf{
		TrackData:   perf.TrackData,
		Overview:    perf.Overview,
		Beats:       perf.Beats,
		QuickCues:   perf.QuickCues,
		Loops:       perf.Loops,
		ActiveLoops: perf.ActiveLoops,
		Complete:    perf.Complete,
	})
	if len(rbxa1) > onDemandMaxRBXA1Bytes {
		return nil, fmt.Errorf("%w: rbxa1 exceeds %d bytes", ErrOnDemandLimit, onDemandMaxRBXA1Bytes)
	}
	if err := verifyMountPinned(pinnedKey); err != nil {
		return nil, err
	}
	timing.EncodeMS = float64(time.Since(tEnc).Microseconds()) / 1000.0
	timing.TotalMS = float64(time.Since(tStart).Microseconds()) / 1000.0

	return &OnDemandResult{
		RBXA1:         rbxa1,
		Key:           tr.Key,
		Written:       written,
		WrittenNoMain: writtenNoMain,
		RowHash:       tr.Row,
		ActiveLoops:   perf.ActiveLoops,
		Rules:         rbstate.Rules,
		Timing:        timing,
	}, nil
}

// mergeFastCues recovers OneLibrary-only cues from the snapshot's FastCues/FastLoops blobs when
// exportLibrary.db held cues not present in ANLZ files (zero stick I/O).
func mergeFastCues(a *rekordbox.Analysis, tr *rbstate.Track, sr int, sampleCount uint64, duration float64) {
	if sr <= 0 {
		sr = 44100
	}
	if sampleCount == 0 {
		if duration > 0 {
			sampleCount = uint64(math.Round(duration * float64(sr)))
		} else {
			sampleCount = uint64(sr) * 3600
		}
	}
	// Build a minimal valid trackData header for ParseDeckCuesAndLoops.
	raw := make([]byte, 68)
	binary.BigEndian.PutUint64(raw[0:], math.Float64bits(float64(sr)))
	binary.BigEndian.PutUint64(raw[8:], sampleCount)
	td, err := enginedb.QCompress(raw)
	if err != nil {
		return
	}
	deck, err := enginedb.ParseDeckCuesAndLoops(td, tr.FastCues, tr.FastLoops, int64(tr.FastActive))
	if err != nil {
		return
	}
	hasHot := [9]bool{}
	hasMem := false
	hasMemLoop := make(map[[2]uint32]bool)
	hasMemLoopStart := make(map[uint32]bool)
	for _, c := range a.Cues {
		if c.HotCue >= 1 && c.HotCue <= 8 {
			hasHot[c.HotCue] = true
		} else if c.HotCue == 0 {
			if c.Loop {
				hasMemLoop[[2]uint32{c.TimeMS, c.EndMS}] = true
				hasMemLoopStart[c.TimeMS] = true
			}
			hasMem = true
		}
	}
	var extra []rekordbox.Cue
	for slot := 0; slot < 8; slot++ {
		hc := deck.HotCues[slot]
		lp := deck.Loops[slot]
		pairedLoop := hc != nil && lp != nil && math.Abs(lp.StartSamples-hc.Samples) <= float64(sr)/200
		if !hasHot[slot+1] && hc != nil {
			cue := rekordbox.Cue{
				HotCue:   uint32(slot + 1),
				TimeMS:   uint32(math.Round(hc.Samples * 1000 / float64(sr))),
				Name:     hc.Name,
				Color:    [4]byte{hc.Color[1], hc.Color[2], hc.Color[3], hc.Color[0]},
				Extended: true,
			}
			if pairedLoop {
				cue.Loop = true
				cue.EndMS = uint32(math.Round(lp.EndSamples * 1000 / float64(sr)))
				cue.Active = lp.Active
			}
			extra = append(extra, cue)
		}
		if lp != nil && !pairedLoop {
			startMS := uint32(math.Round(lp.StartSamples * 1000 / float64(sr)))
			endMS := uint32(math.Round(lp.EndSamples * 1000 / float64(sr)))
			if !hasMemLoop[[2]uint32{startMS, endMS}] {
				hasMemLoop[[2]uint32{startMS, endMS}] = true
				hasMemLoopStart[startMS] = true
				extra = append(extra, rekordbox.Cue{
					HotCue:   0,
					Loop:     true,
					TimeMS:   startMS,
					EndMS:    endMS,
					Name:     lp.Name,
					Color:    [4]byte{lp.Color[1], lp.Color[2], lp.Color[3], lp.Color[0]},
					Active:   lp.Active,
					Extended: true,
				})
			}
		}
	}
	if !hasMem && deck.MainCue >= 0 {
		mainMS := uint32(math.Round(deck.MainCue * 1000 / float64(sr)))
		if !hasMemLoopStart[mainMS] {
			extra = append(extra, rekordbox.Cue{
				HotCue:   0,
				TimeMS:   mainMS,
				Extended: true,
			})
		}
	}
	if len(extra) > 0 {
		a.Merge(&rekordbox.Analysis{Cues: extra})
	}
}
