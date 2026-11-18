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

	"golang.org/x/sys/unix"
)

var shadowPositionFeedPath = "/run/enginex/shadow-position"

const shadowPositionFeedRecordSize int64 = 48

type shadowPositionFeedRequest struct {
	Deck              int     `json:"deck"`
	Key               string  `json:"key"`
	Active            bool    `json:"active"`
	PositionSamples   float64 `json:"positionSamples"`
	RateSamplesPerSec float64 `json:"rateSamplesPerSec"`
}

var shadowPositionFeedMu sync.Mutex

func shadowPositionFeedHash(key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

func shadowPositionFeedWrite(r shadowPositionFeedRequest) error {
	if r.Deck < 1 || r.Deck > 4 {
		return nil
	}
	if r.PositionSamples < 0 || r.RateSamplesPerSec < 0 {
		r.Active = false
	}
	if err := os.MkdirAll(filepath.Dir(shadowPositionFeedPath), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(shadowPositionFeedPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(shadowPositionFeedRecordSize * 4); err != nil {
		return err
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &ts); err != nil {
		return err
	}
	var b [shadowPositionFeedRecordSize]byte
	odd := uint32(1)
	if _, err := f.ReadAt(b[:4], (int64(r.Deck)-1)*shadowPositionFeedRecordSize); err == nil {
		odd = binary.LittleEndian.Uint32(b[:4]) | 1
		if odd == 0 || odd == ^uint32(0) {
			odd = 1
		}
	}
	off := (int64(r.Deck) - 1) * shadowPositionFeedRecordSize
	var seq [4]byte
	binary.LittleEndian.PutUint32(seq[:], odd)
	if _, err := f.WriteAt(seq[:], off); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(b[4:8], boolUint32(r.Active))
	binary.LittleEndian.PutUint32(b[8:12], uint32(r.Deck))
	binary.LittleEndian.PutUint64(b[16:24], shadowPositionFeedHash(r.Key))
	putFloat64(b[24:32], r.PositionSamples)
	putFloat64(b[32:40], r.RateSamplesPerSec)
	binary.LittleEndian.PutUint64(b[40:48], uint64(ts.Nano()))
	if _, err := f.WriteAt(b[4:], off+4); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(seq[:], odd+1)
	_, err = f.WriteAt(seq[:], off)
	return err
}

func putFloat64(dst []byte, value float64) { binary.LittleEndian.PutUint64(dst, mathFloat64bits(value)) }

func mathFloat64bits(value float64) uint64 { return math.Float64bits(value) }

func boolUint32(value bool) uint32 {
	if value {
		return 1
	}
	return 0
}

func (s *Server) shadowPositionFeedPost(w http.ResponseWriter, r *http.Request) {
	var req shadowPositionFeedRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	shadowPositionFeedMu.Lock()
	err := shadowPositionFeedWrite(req)
	shadowPositionFeedMu.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
