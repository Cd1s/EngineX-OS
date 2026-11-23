package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	defaultPriorityDir = "/run/enginex"
	maxPriorityItems   = 16
	deckActivityFile   = "rb-deck-activity.json"
	deckActivityTTL    = 10 * time.Second
	idleSampleMinGap   = 400 * time.Millisecond
	idleSamplesNeeded  = 2
)

var (
	priorityDirMu sync.RWMutex
	priorityDir   = defaultPriorityDir
)

// SetPriorityDirForTest redirects priority and deck-activity runtime files
// to dir for the duration of a test and returns a restore function.
func SetPriorityDirForTest(dir string) func() {
	priorityDirMu.Lock()
	old := priorityDir
	priorityDir = dir
	priorityDirMu.Unlock()
	return func() {
		priorityDirMu.Lock()
		priorityDir = old
		priorityDirMu.Unlock()
	}
}

func priorityRoot() string {
	priorityDirMu.RLock()
	dir := priorityDir
	priorityDirMu.RUnlock()
	if dir == defaultPriorityDir && testing.Testing() {
		return filepath.Join(os.TempDir(), fmt.Sprintf("enginex-rb-priority-test-%d", os.Getpid()))
	}
	return dir
}

// PriorityItem is a single prioritized Engine-relative track path on a mounted
// rekordbox volume (e.g. "../Contents/Artist/Track.mp3").
type PriorityItem struct {
	Path   string `json:"path"`
	Source string `json:"source"` // "deck", "cue", "preview", "focus"
	Deck   int    `json:"deck,omitempty"`
	Seq    uint64 `json:"seq"`
}

// PriorityRecord is the mode-0600 file written atomically by stream-sync for
// each mounted rekordbox volume when a DJ loads, previews, or focuses a track.
type PriorityRecord struct {
	Dev         uint64         `json:"dev"`
	Ino         uint64         `json:"ino"`
	Seq         uint64         `json:"seq"`
	UpdatedAtMS int64          `json:"updatedAtMs"`
	Items       []PriorityItem `json:"items,omitempty"`
}

// PriorityAck is written atomically by rb-bridge after a prioritized track is
// imported or validated, keeping writer ownership strictly separate from
// PriorityRecord.
type PriorityAck struct {
	AckSeq      uint64            `json:"ackSeq"`
	Paths       map[string]uint64 `json:"paths,omitempty"`
	UpdatedAtMS int64             `json:"updatedAtMs"`
}

// DeckActivity records whether any deck is actively playing, CUE previewing,
// scratching, or moving the platter, so rb-bridge runs at full speed when idle
// and yields immediately when a deck becomes active.
type DeckActivity struct {
	Active      bool  `json:"active"`
	IdleSamples int   `json:"idleSamples"`
	UpdatedAtMS int64 `json:"updatedAtMs"`
}

type volumeKey struct {
	dev uint64
	ino uint64
}

func volumeIdentity(mount string) (volumeKey, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(mount, &st); err != nil {
		return volumeKey{}, err
	}
	return volumeKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

func priorityRecordPath(v volumeKey) string {
	return filepath.Join(priorityRoot(), fmt.Sprintf("rb-priority-%x-%x.json", v.dev, v.ino))
}

func priorityAckPath(v volumeKey) string {
	return filepath.Join(priorityRoot(), fmt.Sprintf("rb-priority-%x-%x.ack", v.dev, v.ino))
}

func deckActivityPath() string {
	return filepath.Join(priorityRoot(), deckActivityFile)
}

func normalizePriorityEnginePath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if p == "" {
		return ""
	}
	p = strings.TrimPrefix(p, "../")
	p = strings.TrimLeft(path.Clean("/"+p), "/")
	if p == "" || p == "." || strings.HasPrefix(p, "..") {
		return ""
	}
	return "../" + p
}

func foldPriorityPath(p string) string {
	return foldPath(strings.TrimPrefix(normalizePriorityEnginePath(p), "../"))
}

func writePrivateJSON(dst string, v any) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if st, err := os.Lstat(dst); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("priority: refusing symlink target %s", dst)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".rb-pri-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

func readPrivateJSON(src string, dst any) error {
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > 64<<10 {
		return fmt.Errorf("priority: invalid record %s", src)
	}
	return json.NewDecoder(f).Decode(dst)
}

// PriorityQueue maintains coalesced per-volume priority queues and global deck
// activity state for stream-sync, persisting each update atomically under
// priorityRoot().
type PriorityQueue struct {
	mu           sync.Mutex
	seq          uint64
	volumes      map[volumeKey]*PriorityRecord
	deckActive   bool
	idleSamples  int
	lastIdleAt   time.Time
	lastActWrite time.Time
	hasActivity  bool
}

// NewPriorityQueue creates a new in-memory priority queue backed by mode-0600
// files under priorityRoot().
func NewPriorityQueue() *PriorityQueue {
	return &PriorityQueue{
		volumes: map[volumeKey]*PriorityRecord{},
	}
}

// Enqueue records a high-priority track on mount, coalescing repeated edges
// for the same folded path and moving the newest request to the head.
func (q *PriorityQueue) Enqueue(mount, enginePath, source string, deck int) (uint64, error) {
	vk, err := volumeIdentity(mount)
	if err != nil {
		return 0, err
	}
	norm := normalizePriorityEnginePath(enginePath)
	if norm == "" {
		return 0, errors.New("priority: empty track path")
	}
	if source == "" {
		source = "deck"
	}
	key := foldPriorityPath(norm)

	q.mu.Lock()
	defer q.mu.Unlock()
	rec := q.volumes[vk]
	if rec == nil {
		rec = &PriorityRecord{Dev: vk.dev, Ino: vk.ino}
		q.volumes[vk] = rec
	}
	// Coalesce repeated identical head edges without churning seq or disk.
	if len(rec.Items) > 0 && foldPriorityPath(rec.Items[0].Path) == key &&
		rec.Items[0].Source == source && rec.Items[0].Deck == deck {
		return rec.Items[0].Seq, nil
	}
	q.seq++
	seq := q.seq
	item := PriorityItem{Path: norm, Source: source, Deck: deck, Seq: seq}
	next := make([]PriorityItem, 0, min(len(rec.Items)+1, maxPriorityItems))
	next = append(next, item)
	for _, existing := range rec.Items {
		if foldPriorityPath(existing.Path) == key {
			continue
		}
		if len(next) >= maxPriorityItems {
			break
		}
		next = append(next, existing)
	}
	rec.Items = next
	rec.Seq = seq
	rec.UpdatedAtMS = time.Now().UnixMilli()
	if err := writePrivateJSON(priorityRecordPath(vk), rec); err != nil {
		return 0, err
	}
	return seq, nil
}

// RemoveVolume deletes the persisted priority record and ack for mount (e.g.
// on eject or failed resolution).
func (q *PriorityQueue) RemoveVolume(mount string) {
	vk, err := volumeIdentity(mount)
	if err != nil {
		return
	}
	q.RemoveVolumeID(vk.dev, vk.ino)
}

// RemoveVolumeID deletes the priority record and ack for a volume identity.
func (q *PriorityQueue) RemoveVolumeID(dev, ino uint64) {
	vk := volumeKey{dev: dev, ino: ino}
	q.mu.Lock()
	delete(q.volumes, vk)
	q.mu.Unlock()
	_ = os.Remove(priorityRecordPath(vk))
	_ = os.Remove(priorityAckPath(vk))
}

// UpdateDeckActivity updates the global deck-activity state file. Active edges
// (PlayState, CUE preview, scratch, or platter movement) take effect
// immediately; returning to idle requires idleSamplesNeeded consecutive idle
// samples separated by at least idleSampleMinGap (from the 500 ms state post).
func (q *PriorityQueue) UpdateDeckActivity(active bool, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	changed := !q.hasActivity
	if active {
		if !q.deckActive || q.idleSamples != 0 {
			changed = true
		}
		q.deckActive = true
		q.idleSamples = 0
		q.lastIdleAt = time.Time{}
	} else {
		if q.lastIdleAt.IsZero() || now.Sub(q.lastIdleAt) >= idleSampleMinGap {
			q.idleSamples++
			q.lastIdleAt = now
			if q.deckActive && q.idleSamples >= idleSamplesNeeded {
				q.deckActive = false
				changed = true
			}
		}
	}
	q.hasActivity = true
	// Persist immediately on transitions and refresh heartbeat every 2 s while active.
	if changed || (q.deckActive && now.Sub(q.lastActWrite) >= 2*time.Second) {
		act := DeckActivity{
			Active:      q.deckActive,
			IdleSamples: q.idleSamples,
			UpdatedAtMS: now.UnixMilli(),
		}
		if writePrivateJSON(deckActivityPath(), act) == nil {
			q.lastActWrite = now
		}
	}
	return q.deckActive
}

// ReadPriority reads the current priority record for mount if present and its
// device/inode match mount.
func ReadPriority(mount string) (*PriorityRecord, error) {
	vk, err := volumeIdentity(mount)
	if err != nil {
		return nil, err
	}
	var rec PriorityRecord
	if err := readPrivateJSON(priorityRecordPath(vk), &rec); err != nil {
		return nil, err
	}
	if rec.Dev != vk.dev || rec.Ino != vk.ino {
		return nil, os.ErrNotExist
	}
	return &rec, nil
}

// ReadPriorityAck reads the acknowledged sequence state for mount.
func ReadPriorityAck(mount string) PriorityAck {
	vk, err := volumeIdentity(mount)
	if err != nil {
		return PriorityAck{}
	}
	var ack PriorityAck
	if err := readPrivateJSON(priorityAckPath(vk), &ack); err != nil {
		return PriorityAck{}
	}
	return ack
}

// PendingPriority returns unacknowledged priority items for mount in
// newest-first order along with the highest sequence seen.
func PendingPriority(mount string) ([]PriorityItem, uint64) {
	rec, err := ReadPriority(mount)
	if err != nil || len(rec.Items) == 0 {
		return nil, 0
	}
	ack := ReadPriorityAck(mount)
	var pending []PriorityItem
	for _, item := range rec.Items {
		key := foldPriorityPath(item.Path)
		if key == "" {
			continue
		}
		if ack.Paths[key] >= item.Seq {
			continue
		}
		pending = append(pending, item)
	}
	return pending, rec.Seq
}

// AckPriority records acknowledged sequence numbers for the given Engine paths
// on mount.
func AckPriority(mount string, items []PriorityItem) error {
	if len(items) == 0 {
		return nil
	}
	vk, err := volumeIdentity(mount)
	if err != nil {
		return err
	}
	ack := ReadPriorityAck(mount)
	if ack.Paths == nil {
		ack.Paths = map[string]uint64{}
	}
	for _, it := range items {
		key := foldPriorityPath(it.Path)
		if key == "" {
			continue
		}
		if it.Seq > ack.Paths[key] {
			ack.Paths[key] = it.Seq
		}
		if it.Seq > ack.AckSeq {
			ack.AckSeq = it.Seq
		}
	}
	// Keep the ack map bounded to active/recent paths.
	if len(ack.Paths) > 64 {
		cutoff := uint64(0)
		if ack.AckSeq > 64 {
			cutoff = ack.AckSeq - 64
		}
		for k, s := range ack.Paths {
			if s < cutoff {
				delete(ack.Paths, k)
			}
		}
	}
	ack.UpdatedAtMS = time.Now().UnixMilli()
	return writePrivateJSON(priorityAckPath(vk), ack)
}

// ClearPriority removes priority files for mount when an import finishes or a
// volume is ejected.
func ClearPriority(mount string) {
	vk, err := volumeIdentity(mount)
	if err != nil {
		return
	}
	_ = os.Remove(priorityRecordPath(vk))
	_ = os.Remove(priorityAckPath(vk))
}

// HasPendingPriority reports whether mount currently has any unacknowledged
// priority item with a sequence newer than lastSeq.
func HasPendingPriority(mount string, lastSeq uint64) bool {
	rec, err := ReadPriority(mount)
	if err != nil || rec.Seq <= lastSeq || len(rec.Items) == 0 {
		return false
	}
	pending, _ := PendingPriority(mount)
	return len(pending) > 0
}

// IsDeckActive reports whether stream-sync recently observed an active deck
// (PlayState, CUE preview, scratch, or platter movement). Missing or stale
// state defaults to false so idle imports run at full speed.
func IsDeckActive() bool {
	return isDeckActiveAt(time.Now())
}

func isDeckActiveAt(now time.Time) bool {
	var act DeckActivity
	if err := readPrivateJSON(deckActivityPath(), &act); err != nil {
		return false
	}
	if !act.Active || act.UpdatedAtMS <= 0 {
		return false
	}
	age := now.Sub(time.UnixMilli(act.UpdatedAtMS))
	return age >= -time.Second && age <= deckActivityTTL
}
