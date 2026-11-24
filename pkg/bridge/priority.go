package bridge

// The priority queue and the deck-activity file (design §1.5, formats kept): stream-sync writes
// them when a DJ loads, previews or focuses a track, and the background job reads them. Each is a
// 0600 file in priorityRoot(); a volume's are keyed by its device and inode.

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

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

const (
	defaultPriorityDir = "/run/enginex"
	maxPriorityItems   = 16
	deckActivityTTL    = 10 * time.Second
	idleSampleMinGap   = 400 * time.Millisecond
	idleSamplesNeeded  = 2
)

var (
	priorityDirMu sync.RWMutex
	priorityDir   = defaultPriorityDir
)

// SetPriorityDirForTest redirects the runtime files to dir and returns the restore function.
func SetPriorityDirForTest(dir string) func() {
	priorityDirMu.Lock()
	old := priorityDir
	priorityDir = dir
	priorityDirMu.Unlock()
	return func() { SetPriorityDirForTest(old) }
}

func priorityRoot() string {
	priorityDirMu.RLock()
	defer priorityDirMu.RUnlock()
	if priorityDir == defaultPriorityDir && testing.Testing() {
		return filepath.Join(os.TempDir(), fmt.Sprintf("enginex-rb-priority-test-%d", os.Getpid()))
	}
	return priorityDir
}

// PriorityItem is one prioritised Engine path on a volume ("../Contents/Artist/Track.mp3").
type PriorityItem struct {
	Path   string `json:"path"`
	Source string `json:"source"` // "deck", "cue", "preview", "focus"
	Deck   int    `json:"deck,omitempty"`
	Seq    uint64 `json:"seq"`
}

// PriorityRecord is a volume's queue, newest first; only stream-sync writes it.
type PriorityRecord struct {
	Dev         uint64         `json:"dev"`
	Ino         uint64         `json:"ino"`
	Seq         uint64         `json:"seq"`
	UpdatedAtMS int64          `json:"updatedAtMs"`
	Items       []PriorityItem `json:"items,omitempty"`
}

// PriorityAck is what rb-bridge has served of a volume's queue; only rb-bridge writes it.
type PriorityAck struct {
	AckSeq      uint64            `json:"ackSeq"`
	Paths       map[string]uint64 `json:"paths,omitempty"`
	UpdatedAtMS int64             `json:"updatedAtMs"`
}

// DeckActivity says whether a deck is playing, cue-previewing, scratching or moving its platter:
// the job then yields the stick (pace.go).
type DeckActivity struct {
	Active      bool  `json:"active"`
	IdleSamples int   `json:"idleSamples"`
	UpdatedAtMS int64 `json:"updatedAtMs"`
}

type volumeKey struct{ dev, ino uint64 }

func volumeIdentity(mount string) (volumeKey, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(mount, &st); err != nil {
		return volumeKey{}, err
	}
	return volumeKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

// volumeFile is a runtime file of a volume: rb-priority-<dev>-<ino>.json and .ack, rb-publish-<dev>-<ino>.
func volumeFile(prefix string, v volumeKey, ext string) string {
	return filepath.Join(priorityRoot(), fmt.Sprintf("%s-%x-%x%s", prefix, v.dev, v.ino, ext))
}

func deckActivityPath() string { return filepath.Join(priorityRoot(), "rb-deck-activity.json") }

func normalizePriorityEnginePath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if p == "" {
		return ""
	}
	p = strings.TrimLeft(path.Clean("/"+strings.TrimPrefix(p, "../")), "/")
	if p == "" || p == "." || strings.HasPrefix(p, "..") {
		return ""
	}
	return "../" + p
}

func foldPriorityPath(p string) string {
	return rbstate.FoldPath(strings.TrimPrefix(normalizePriorityEnginePath(p), "../"))
}

// writePrivate replaces dst with data through a rename; os.CreateTemp makes the file 0600.
func writePrivate(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".rb-pri-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

func writePrivateJSON(dst string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writePrivate(dst, data)
}

func readPrivateJSON(src string, dst any) error {
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > 64<<10 {
		return fmt.Errorf("priority: invalid record %s", src)
	}
	return json.NewDecoder(f).Decode(dst)
}

// PriorityQueue is stream-sync's side: the volumes' queues and the deck-activity state.
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

func NewPriorityQueue() *PriorityQueue {
	return &PriorityQueue{volumes: map[volumeKey]*PriorityRecord{}}
}

// Enqueue puts a track at the head of its volume's queue. The same head edge again changes nothing.
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
		// Sequence numbers go on from what an earlier stream-sync wrote.
		rec = &PriorityRecord{Dev: vk.dev, Ino: vk.ino}
		var old PriorityRecord
		if readPrivateJSON(volumeFile("rb-priority", vk, ".json"), &old) == nil {
			q.seq = max(q.seq, old.Seq)
		}
		q.seq = max(q.seq, readAck(vk).AckSeq)
		q.volumes[vk] = rec
	}
	if len(rec.Items) > 0 && foldPriorityPath(rec.Items[0].Path) == key && rec.Items[0].Source == source && rec.Items[0].Deck == deck {
		return rec.Items[0].Seq, nil
	}
	q.seq++
	next := []PriorityItem{{Path: norm, Source: source, Deck: deck, Seq: q.seq}}
	for _, it := range rec.Items {
		if len(next) == maxPriorityItems {
			break
		}
		if foldPriorityPath(it.Path) != key {
			next = append(next, it)
		}
	}
	rec.Items, rec.Seq, rec.UpdatedAtMS = next, q.seq, time.Now().UnixMilli()
	if err := writePrivateJSON(volumeFile("rb-priority", vk, ".json"), rec); err != nil {
		return 0, err
	}
	return q.seq, nil
}

// UpdateDeckActivity takes a sample of the decks (stream-sync's 500 ms state post). Active takes
// effect at once; idle after idleSamplesNeeded samples at least idleSampleMinGap apart. The file is
// written at each change and every 2 s while a deck is active.
func (q *PriorityQueue) UpdateDeckActivity(active bool, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	changed := !q.hasActivity
	if active {
		changed = changed || !q.deckActive || q.idleSamples != 0
		q.deckActive, q.idleSamples, q.lastIdleAt = true, 0, time.Time{}
	} else if q.lastIdleAt.IsZero() || now.Sub(q.lastIdleAt) >= idleSampleMinGap {
		q.idleSamples++
		q.lastIdleAt = now
		if q.deckActive && q.idleSamples >= idleSamplesNeeded {
			q.deckActive, changed = false, true
		}
	}
	q.hasActivity = true
	if changed || (q.deckActive && now.Sub(q.lastActWrite) >= 2*time.Second) {
		act := DeckActivity{Active: q.deckActive, IdleSamples: q.idleSamples, UpdatedAtMS: now.UnixMilli()}
		if writePrivateJSON(deckActivityPath(), act) == nil {
			q.lastActWrite = now
		}
	}
	return q.deckActive
}

// readQueue is the volume's record, nil when there is none or it names another volume.
func readQueue(mount string) (*PriorityRecord, volumeKey) {
	vk, err := volumeIdentity(mount)
	if err != nil {
		return nil, vk
	}
	var rec PriorityRecord
	if readPrivateJSON(volumeFile("rb-priority", vk, ".json"), &rec) != nil || rec.Dev != vk.dev || rec.Ino != vk.ino {
		return nil, vk
	}
	return &rec, vk
}

func readAck(vk volumeKey) PriorityAck {
	var ack PriorityAck
	if readPrivateJSON(volumeFile("rb-priority", vk, ".ack"), &ack) != nil {
		return PriorityAck{}
	}
	return ack
}

// PendingPriority is the volume's items not served yet, newest first, and the record's sequence.
func PendingPriority(mount string) ([]PriorityItem, uint64) {
	rec, vk := readQueue(mount)
	if rec == nil || len(rec.Items) == 0 {
		return nil, 0
	}
	ack := readAck(vk)
	var pending []PriorityItem
	for _, it := range rec.Items {
		if key := foldPriorityPath(it.Path); key != "" && ack.Paths[key] < it.Seq {
			pending = append(pending, it)
		}
	}
	return pending, rec.Seq
}

// AckPriority records items as served. The ack keeps the paths of its last 64 sequence numbers.
func AckPriority(mount string, items []PriorityItem) error {
	if len(items) == 0 {
		return nil
	}
	vk, err := volumeIdentity(mount)
	if err != nil {
		return err
	}
	ack := readAck(vk)
	if ack.Paths == nil {
		ack.Paths = map[string]uint64{}
	}
	for _, it := range items {
		if key := foldPriorityPath(it.Path); key != "" {
			ack.Paths[key] = max(ack.Paths[key], it.Seq)
			ack.AckSeq = max(ack.AckSeq, it.Seq)
		}
	}
	if len(ack.Paths) > 64 && ack.AckSeq > 64 {
		for k, s := range ack.Paths {
			if s < ack.AckSeq-64 {
				delete(ack.Paths, k)
			}
		}
	}
	ack.UpdatedAtMS = time.Now().UnixMilli()
	return writePrivateJSON(volumeFile("rb-priority", vk, ".ack"), ack)
}

// HasPendingPriority reports an item not served yet in a record newer than lastSeq.
func HasPendingPriority(mount string, lastSeq uint64) bool {
	if rec, _ := readQueue(mount); rec == nil || rec.Seq <= lastSeq || len(rec.Items) == 0 {
		return false
	}
	pending, _ := PendingPriority(mount)
	return len(pending) > 0
}

// IsDeckActive reports a deck stream-sync saw active within deckActivityTTL; no file or a stale
// one is idle, so the job runs at full speed.
func IsDeckActive() bool {
	var act DeckActivity
	if readPrivateJSON(deckActivityPath(), &act) != nil || !act.Active || act.UpdatedAtMS <= 0 {
		return false
	}
	age := time.Since(time.UnixMilli(act.UpdatedAtMS))
	return age >= -time.Second && age <= deckActivityTTL
}
