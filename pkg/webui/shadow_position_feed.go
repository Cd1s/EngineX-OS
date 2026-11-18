package webui

import (
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The paused shadow deck's position feed, read by the Engine hook on its render thread:
// one 64-byte little-endian record per deck 1-4, guarded by a sequence that is odd while
// it is written: u32 seq, u32 active, u32 deck, 4 unused, u64 FNV-1a of the track key,
// f64 position in samples (before sample 0 for a cue in the lead-in), f64 samples per
// second (0: the source stands; negative: a scratch backwards), u64 CLOCK_MONOTONIC_RAW
// ns of that position, f64 loop in and f64 loop out in samples (both 0: no loop).
var shadowPositionFeedPath = "/run/enginex/shadow-position"

const shadowPositionFeedRecordSize = 64

// shadowPositionFeedRequest is what the shadow follower posts. At is stream-sync's own
// clock (ms) when it extrapolated the position, handed back unchanged: the record's time
// is then that instant, whatever the GUI thread and the request took since.
type shadowPositionFeedRequest struct {
	Deck              int     `json:"deck"`
	Key               string  `json:"key"`
	Active            bool    `json:"active"`
	PositionSamples   float64 `json:"positionSamples"`
	RateSamplesPerSec float64 `json:"rateSamplesPerSec"`
	LoopInSamples     float64 `json:"loopInSamples"`
	LoopOutSamples    float64 `json:"loopOutSamples"`
	At                int64   `json:"at"`
}

var shadowPositionFeed struct {
	sync.Mutex
	f    *os.File
	path string
	seq  [4]uint32
}

// shadowPositionFeedAge is how long ago at was, by this process's clock; a time that is
// missing, ahead or older than a feed stays useful for counts as now.
func shadowPositionFeedAge(at int64, now time.Time) time.Duration {
	age := time.Duration(now.UnixMilli()-at) * time.Millisecond
	if at <= 0 || age < 0 || age > 2*time.Second {
		return 0
	}
	return age
}

func shadowPositionFeedWrite(r shadowPositionFeedRequest) error {
	if r.Deck < 1 || r.Deck > 4 {
		return nil
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if !finite(r.PositionSamples) || !finite(r.RateSamplesPerSec) {
		r.Active, r.PositionSamples, r.RateSamplesPerSec = false, 0, 0
	}
	if !finite(r.LoopInSamples) || !finite(r.LoopOutSamples) || r.LoopInSamples < 0 || r.LoopOutSamples <= r.LoopInSamples {
		r.LoopInSamples, r.LoopOutSamples = 0, 0
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &ts); err != nil {
		return err
	}
	at := ts.Nano() - int64(shadowPositionFeedAge(r.At, time.Now()))
	feed := &shadowPositionFeed
	feed.Lock()
	defer feed.Unlock()
	if feed.f == nil || feed.path != shadowPositionFeedPath {
		if feed.f != nil {
			feed.f.Close()
			feed.f = nil
		}
		if err := os.MkdirAll(filepath.Dir(shadowPositionFeedPath), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(shadowPositionFeedPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return err
		}
		if err := f.Truncate(shadowPositionFeedRecordSize * 4); err != nil {
			f.Close()
			return err
		}
		feed.f, feed.path, feed.seq = f, shadowPositionFeedPath, [4]uint32{}
	}
	off := int64(r.Deck-1) * shadowPositionFeedRecordSize
	seq := &feed.seq[r.Deck-1]
	var b [shadowPositionFeedRecordSize]byte
	*seq |= 1
	binary.LittleEndian.PutUint32(b[:4], *seq)
	if _, err := feed.f.WriteAt(b[:4], off); err != nil {
		return err
	}
	if r.Active {
		binary.LittleEndian.PutUint32(b[4:], 1)
	}
	binary.LittleEndian.PutUint32(b[8:], uint32(r.Deck))
	key := fnv.New64a()
	key.Write([]byte(r.Key))
	binary.LittleEndian.PutUint64(b[16:], key.Sum64())
	binary.LittleEndian.PutUint64(b[24:], math.Float64bits(r.PositionSamples))
	binary.LittleEndian.PutUint64(b[32:], math.Float64bits(r.RateSamplesPerSec))
	binary.LittleEndian.PutUint64(b[40:], uint64(at))
	binary.LittleEndian.PutUint64(b[48:], math.Float64bits(r.LoopInSamples))
	binary.LittleEndian.PutUint64(b[56:], math.Float64bits(r.LoopOutSamples))
	if _, err := feed.f.WriteAt(b[4:], off+4); err != nil {
		return err
	}
	*seq++
	if *seq == 0 {
		*seq = 2 // 0 is a record never written
	}
	binary.LittleEndian.PutUint32(b[:4], *seq)
	_, err := feed.f.WriteAt(b[:4], off)
	return err
}

func (s *Server) shadowPositionFeedPost(w http.ResponseWriter, r *http.Request) {
	var req shadowPositionFeedRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := shadowPositionFeedWrite(req); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
