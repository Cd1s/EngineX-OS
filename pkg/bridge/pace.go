package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// How the job shares the stick with a playing or loading deck (design §1.5, handbook §3.11).
// The decks' I/O schedulers do not honour the job's idle class:
//   - While any deck is loading a track (/Track/SongLoading or Engine copying into
//     /data/inMusicTempFiles/), the job blocks and performs zero stick I/O, even on a paused deck.
//   - While a deck plays, cue-previews, scratches or moves its platter (the deck-activity file),
//     reads take at most playingReadChunk, and each that went to the stick (slower than fromCache)
//     is followed by a pause as long as it took, at least playingReadGap: the stick is free half
//     the time. A stat or write that missed the cache pauses alike.
//   - No deck loading or playing: full speed, still checking before every chunk so a mid-track
//     load or play edge takes effect immediately.
var (
	playingReadChunk = 64 << 10
	playingReadGap   = 20 * time.Millisecond
	fromCache        = 2 * time.Millisecond // a USB 2.0 read takes 10-15 ms; a page-cache copy well under 1 ms
	loadingPollGap   = 50 * time.Millisecond

	engineProcDir   = "/proc"
	engineCopiesDir = "/data/inMusicTempFiles/"
	enginePIDMu     sync.Mutex
	enginePIDProc   string
	enginePIDAt     time.Time
	enginePIDs      []string
)

func engineProcessIDs() []string {
	enginePIDMu.Lock()
	defer enginePIDMu.Unlock()
	if enginePIDProc == engineProcDir && engineProcDir == "/proc" && time.Since(enginePIDAt) < 2*time.Second {
		return append([]string(nil), enginePIDs...)
	}
	entries, _ := os.ReadDir(engineProcDir)
	var pids []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if comm, err := os.ReadFile(filepath.Join(engineProcDir, e.Name(), "comm")); err == nil && strings.TrimSpace(string(comm)) == "Engine" {
			pids = append(pids, e.Name())
		}
	}
	enginePIDProc, enginePIDAt, enginePIDs = engineProcDir, time.Now(), pids
	return append([]string(nil), pids...)
}

// IsEngineCopying reports Engine writing a copy of a track it loads into /data/inMusicTempFiles/.
func IsEngineCopying() bool {
	if _, err := os.Stat(engineCopiesDir); err != nil {
		return false
	}
	for _, pid := range engineProcessIDs() {
		fds, err := os.ReadDir(filepath.Join(engineProcDir, pid, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(engineProcDir, pid, "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(target, engineCopiesDir) {
				continue
			}
			info, err := os.ReadFile(filepath.Join(engineProcDir, pid, "fdinfo", fd.Name()))
			if err != nil {
				return true
			}
			for _, line := range strings.Split(string(info), "\n") {
				if v, ok := strings.CutPrefix(line, "flags:"); ok {
					if flags, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32); err != nil || flags&3 != 0 {
						return true // O_WRONLY or O_RDWR
					}
				}
			}
		}
	}
	return false
}

// pacer decides, before each track or stick operation, how the job yields, and counts what the
// pacing did.
type pacer struct {
	ctx     context.Context
	active  func() bool // IsDeckActive
	loading func() bool // IsDeckLoading || IsEngineCopying
	// while a deck played: paced tracks, pauses, the reads' time and the time the stick was left alone
	tracks, pauses int
	busy, idle     time.Duration
}

func newPacer(ctx context.Context) *pacer {
	return &pacer{
		ctx:     ctx,
		active:  IsDeckActive,
		loading: func() bool { return IsDeckLoading() || IsEngineCopying() },
	}
}

func (p *pacer) isBusy() bool {
	if p == nil {
		return false
	}
	return p.active() || (p.loading != nil && p.loading())
}

// waitIdle blocks while any deck is playing, cueing, scratching, or loading a track.
// Used to defer whole-DB writes (.rb-next.db copy and fsync) until the deck is idle.
func (p *pacer) waitIdle() error {
	if p == nil {
		return nil
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if !p.isBusy() {
		return nil
	}
	t := time.NewTicker(loadingPollGap)
	defer t.Stop()
	for p.isBusy() {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-t.C:
		}
	}
	return p.ctx.Err()
}

// waitNotLoading blocks while any deck is loading a track, performing zero stick I/O until loading
// clears or ctx ends.
func (p *pacer) waitNotLoading() error {
	if p == nil {
		return nil
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if p.loading == nil || !p.loading() {
		return nil
	}
	t := time.NewTicker(loadingPollGap)
	defer t.Stop()
	for p.loading() {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-t.C:
		}
	}
	return p.ctx.Err()
}

// readOptions are the read options of the next track and whether a deck is playing.
// Chunking stays enabled with chunkPause even on a paused deck so a mid-track SongLoading or Play
// edge halts or paces the very next 64 KiB read instead of running at full speed.
func (p *pacer) readOptions() (enginedb.ReadOptions, bool) {
	playing := p.active()
	if playing {
		p.tracks++
	}
	return enginedb.ReadOptions{Chunk: playingReadChunk, Pause: p.chunkPause}, playing
}

// chunkPause runs after each 64 KiB read chunk: halts while any deck is loading, and paces when a
// deck is playing; when idle it returns immediately without sleeping.
func (p *pacer) chunkPause(n int, took time.Duration) error {
	if err := p.waitNotLoading(); err != nil {
		return err
	}
	if !p.active() {
		return p.ctx.Err()
	}
	return p.pause(n, took)
}

// pause waits as long as a read took and at least playingReadGap, or until the job is stopped.
func (p *pacer) pause(_ int, took time.Duration) error {
	if took < fromCache {
		return p.waitNotLoading()
	}
	wait := max(took, playingReadGap)
	p.pauses++
	p.busy += took
	p.idle += wait
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	case <-t.C:
		return p.waitNotLoading()
	}
}

// String is the pacing of a job for its log line.
func (p *pacer) String() string {
	if p.tracks == 0 {
		return "no deck playing"
	}
	return fmt.Sprintf("%d tracks while a deck played: %d reads of the stick in %s, then left alone %s",
		p.tracks, p.pauses, p.busy.Round(time.Millisecond), p.idle.Round(time.Millisecond))
}

// afterStat pauses after a track's stat or write that read/wrote the disk while a deck plays, and
// always blocks while a deck is loading.
func (p *pacer) afterStat(playing bool, took time.Duration) error {
	if err := p.waitNotLoading(); err != nil {
		return err
	}
	if !playing && !p.active() {
		return p.ctx.Err()
	}
	return p.pause(0, took)
}
