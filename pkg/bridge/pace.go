package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// How the job shares the stick with a playing deck (design §1.5, handbook §3.11). The decks' I/O
// schedulers do not honour the job's idle class, so while a deck plays, cue-previews, scratches or
// moves its platter (the deck-activity file) reads take at most playingReadChunk, and each that
// went to the stick (slower than fromCache) is followed by a pause as long as it took, at least
// playingReadGap: the stick is free half the time. A stat that missed the cache pauses alike.
// No deck playing: full speed. The choice is made again before every track.
var (
	playingReadChunk = 64 << 10
	playingReadGap   = 20 * time.Millisecond
	fromCache        = 2 * time.Millisecond // a USB 2.0 read takes 10-15 ms; a page-cache copy well under 1 ms
)

// pacer decides, before each track, how the job reads, and counts what the pacing did.
type pacer struct {
	ctx    context.Context
	active func() bool // IsDeckActive
	// while a deck played: paced tracks, pauses, the reads' time and the time the stick was left alone
	tracks, pauses int
	busy, idle     time.Duration
}

func newPacer(ctx context.Context) *pacer { return &pacer{ctx: ctx, active: IsDeckActive} }

// readOptions are the read options of the next track and whether a deck is playing.
func (p *pacer) readOptions() (enginedb.ReadOptions, bool) {
	if !p.active() {
		return enginedb.ReadOptions{}, false
	}
	p.tracks++
	return enginedb.ReadOptions{Chunk: playingReadChunk, Pause: p.pause}, true
}

// pause waits as long as a read took and at least playingReadGap, or until the job is stopped.
func (p *pacer) pause(_ int, took time.Duration) error {
	if took < fromCache {
		return p.ctx.Err()
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
		return nil
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

// afterStat pauses after a track's stat that read the disk, while a deck plays.
func (p *pacer) afterStat(playing bool, took time.Duration) error {
	if !playing {
		return p.ctx.Err()
	}
	return p.pause(0, took)
}
