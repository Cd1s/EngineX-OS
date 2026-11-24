package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// How the background job shares the stick with a playing deck (design §1.5, handbook §3.11).
// The job runs in its own process at idle CPU and I/O class and never touches the audio thread,
// but the decks' I/O schedulers do not honour the idle class: a deck that loads or plays a track
// from the same USB stick waits behind whatever the job has asked the disk for. So:
//   - no deck playing (the deck-activity file of stream-sync): full speed, whole sections;
//   - a deck playing, cue-previewing, scratching or moving its platter: reads of at most
//     playingReadChunk, each that went to the stick followed by a pause as long as the read took
//     and at least playingReadGap, so the stick is free at least half the time and a deck's read
//     waits behind one 64 KiB read at most. A read served from the page cache (a section header
//     in a page the last read brought in) did not use the stick and is not followed by a pause.
//     A burst of directory reads (a stat that missed the cache) gets the same pause. The choice is
//     made again before every track, so a deck that starts playing slows the job within one
//     track, and the priority queue is looked at before every track.
var (
	playingReadChunk = 64 << 10
	playingReadGap   = 20 * time.Millisecond
	// fromCache is how long a read or a stat may take and still have been served from the page
	// cache: one that took longer used the stick and is followed by a pause while a deck plays.
	// A USB 2.0 disk takes 10-15 ms a read; a copy of 64 KiB from memory well under 1 ms.
	fromCache = 2 * time.Millisecond
)

// pacer decides, before each track, how the job reads, and counts what the pacing did.
type pacer struct {
	ctx    context.Context
	active func() bool // a deck is playing; IsDeckActive
	// While a deck played: the paced tracks, the reads and stats paused after, the time they
	// took and the time the job then left the stick alone.
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
