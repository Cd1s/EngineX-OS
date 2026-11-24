package bridge

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// The background job, `rb-bridge --import-path` (design §1.5). It builds each track's performance
// data from the stick's ANLZ files and audio headers into the stage on /data, writes the .rgb
// mini waveform of each finished track into the live OverviewData (never over a valid one), and
// says whether a remount should let a window apply what it staged. It never opens m.db, never
// writes the snapshot and writes nothing else on the stick.

// FillOptions are FillInBackground's options.
type FillOptions struct {
	// Progress is told how many of the view's tracks have been compared so far, out of how many;
	// the count never goes down (#328).
	Progress func(done, total int)
	// Stage is where the records go; nil opens the stage of the snapshot's library.
	Stage *rbstate.Stage
}

// FillResult is what one run of the job did.
type FillResult struct {
	Staged, Degraded, Missing int
	// Unread tracks could not be built now (a file that cannot be read, a path the volume
	// refuses): they are logged and looked at again by the next job.
	Unread int
	// Left is the tracks not compared yet when ctx ended.
	Left int
	// Publish: the stage holds something a window would apply. The caller asks for the remount.
	Publish bool
}

// putStaged is Stage.Put; a variable for tests of a full /data.
var putStaged = (*rbstate.Stage).Put

// fillLockWait is how long the job waits for a window to release the volume.
var fillLockWait = 2 * time.Minute

// fillTrack is a track of the view as the job needs it.
type fillTrack struct {
	key   string
	track rekordbox.Track
	hash  string // the view's hash of the export fields: Sig.Row
}

// fillView is the exports as the job needs them.
type fillView struct {
	stamp  string
	tracks []fillTrack
}

// FillInBackground is `rb-bridge --import-path`. Order of work: the tracks in the priority queue,
// then every track whose signature differs from Track.Built or whose Perf is PerfNone, sorted by
// path. For each it stats the audio file, the ANLZ files and the cover, skips the track when the
// snapshot or the stage has that signature already, and otherwise builds it and puts it in the
// stage. When every track of the export's stamp has been compared it calls MarkVerified. Once
// the stage verified a stamp, a later job does not stat the tracks the snapshot has built for
// it. It yields to a playing deck (pace.go).
func FillInBackground(ctx context.Context, mount string, o FillOptions) (FillResult, error) {
	var res FillResult
	start := time.Now()
	r, err := compat.NewResolver(mount)
	if err != nil {
		return res, err
	}
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	// A window of this mount may still run: its lock is the job's start signal (design §1.5).
	unlock, err := rbstate.LockVolume(mount, fillLockWait)
	if err != nil {
		return res, fmt.Errorf("volume lock: %w", err)
	}
	snap, err := rbstate.Load(dbDir)
	unlock()
	if err != nil {
		return res, fmt.Errorf("snapshot: %w", err)
	}
	loaded := time.Now()
	stage := o.Stage
	if stage == nil {
		if stage, err = rbstate.OpenStage(snap.LibraryUUID); err != nil {
			return res, err
		}
		defer stage.Close()
	}
	view, err := readFillView(ctx, r, mount, snap.Layout)
	if err != nil {
		return res, err
	}
	read := time.Now()
	entries, verified, err := stage.Entries()
	if err != nil {
		return res, fmt.Errorf("stage: %w", err)
	}
	f := &filler{
		ctx: ctx, mount: mount, r: r, snap: snap, stage: stage, view: view, entries: entries,
		quick: verified != "" && verified == view.stamp, pace: newPacer(ctx), retries: map[string]int{},
		byKey: make(map[string]int, len(view.tracks)), handled: make([]bool, len(view.tracks)), built: make([]bool, len(view.tracks)),
		rgbDir: dbDir, progress: o.Progress,
	}
	for i, t := range view.tracks {
		f.byKey[t.key] = i
		if s := snap.Tracks[t.key]; s != nil && s.Native {
			f.handled[i], f.built[i] = true, true // Engine's own row: the bridge never writes it (#252)
			continue
		}
		f.total++
	}
	order := make([]int, 0, len(view.tracks))
	for i := range view.tracks {
		if !f.handled[i] {
			order = append(order, i)
		}
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Or(strings.Compare(view.tracks[a].key, view.tracks[b].key), cmp.Compare(view.tracks[a].track.ID, view.tracks[b].track.ID))
	})
	f.report()
	err = f.run(order)
	if errors.Is(err, rbstate.ErrStageFull) {
		// /data is at its floor: what is staged is published, a window applies and drops it, and
		// the next job goes on.
		log.Printf("rb-bridge: fill of %s stopped: %v", mount, err)
		err = nil
	}
	res = f.res
	res.Left = f.total - f.done
	if syncErr := stage.Sync(); err == nil {
		err = syncErr
	}
	if err == nil && res.Left == 0 && view.stamp != "" && verified != view.stamp {
		err = stage.MarkVerified(view.stamp)
	}
	if err == nil {
		res.Publish = f.publish()
	}
	log.Printf("rb-bridge: fill of %s: %d staged, %d degraded, %d missing, %d unread, %d left of %d in %s (snapshot %s, exports %s; %s); publish=%v",
		mount, res.Staged, res.Degraded, res.Missing, res.Unread, res.Left, f.total,
		time.Since(start).Round(time.Millisecond), loaded.Sub(start).Round(time.Millisecond), read.Sub(loaded).Round(time.Millisecond), f.pace, res.Publish)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return res, nil // stopped: the next job goes on from the stage
	}
	return res, err
}

type filler struct {
	ctx      context.Context
	mount    string
	r        *compat.Resolver
	snap     *rbstate.Snapshot
	stage    *rbstate.Stage
	view     *fillView
	entries  map[string]*rbstate.Staged // the stage's index, with this run's records
	quick    bool                       // the stage verified this stamp: built tracks are not stat'ed
	pace     *pacer
	byKey    map[string]int
	handled  []bool // compared (counted in done)
	built    []bool // compared and staged or up to date
	total    int
	done     int
	shown    int
	puts     int
	res      FillResult
	progress func(done, total int)
	rgbDir   string
	// the priority queue
	lastAckSeq uint64
	retries    map[string]int
}

// stageSyncEvery is how many records go into the stage between syncs of its index.
const stageSyncEvery = 16

// maxPriorityRetries is how often a prioritized path that cannot be staged is tried.
const maxPriorityRetries = 3

func (f *filler) report() {
	if f.progress != nil && (f.done > f.shown || f.shown == 0) {
		f.shown = f.done
		f.progress(f.done, f.total)
	}
}

func (f *filler) run(order []int) error {
	if err := f.servePriority(); err != nil {
		return err
	}
	for _, i := range order {
		if err := f.ctx.Err(); err != nil {
			return err
		}
		if HasPendingPriority(f.mount, f.lastAckSeq) {
			if err := f.servePriority(); err != nil {
				return err
			}
		}
		if f.handled[i] {
			continue
		}
		if _, err := f.fill(i); err != nil {
			return err
		}
	}
	return nil
}

// fill compares one track with the snapshot and the stage and stages it when it must. ok is
// false when it could not be built now. The error is a stop (ctx) or a stage that cannot take it.
func (f *filler) fill(i int) (ok bool, err error) {
	vt := &f.view.tracks[i]
	defer func() {
		if err != nil {
			return
		}
		f.built[i] = ok
		if !f.handled[i] {
			f.handled[i] = true
			f.done++
			f.report()
		}
	}()
	snapTrack := f.snap.Tracks[vt.key]
	if f.quick && snapTrack != nil && (snapTrack.Perf == rbstate.PerfApplied || snapTrack.Perf == rbstate.PerfDeck) &&
		snapTrack.Built.Rules == rbstate.Rules && snapTrack.Built.Row == vt.hash {
		// Built for this export and verified: not looked at again. A missing or degraded track
		// is stat'ed by every job, so a file put back or fixed is found without a re-export.
		return true, nil
	}
	opts, playing := f.pace.readOptions()
	t0 := time.Now()
	src, err := enginedb.StatTrack(f.ctx, f.r, vt.track)
	if err == nil {
		err = f.pace.afterStat(playing, time.Since(t0))
	}
	if err != nil {
		return f.unread(vt, err)
	}
	src.Sig.Row = vt.hash
	if snapTrack != nil && snapTrack.Perf != rbstate.PerfNone && snapTrack.Built.Equal(src.Sig) {
		return true, nil
	}
	if e := f.entries[vt.key]; e != nil && e.Sig.Equal(src.Sig) {
		return true, nil
	}
	rec, err := enginedb.BuildTrackFrom(f.r, vt.track, src, opts)
	if err != nil {
		return f.unread(vt, err)
	}
	rec.Key, rec.Sig.Row = vt.key, vt.hash
	if err = putStaged(f.stage, rec); err != nil {
		return false, fmt.Errorf("stage %s: %w", vt.key, err)
	}
	if f.puts++; f.puts%stageSyncEvery == 0 {
		if err = f.stage.Sync(); err != nil {
			return false, err
		}
	}
	index := *rec
	index.Perf, index.RGB, index.Art = rbstate.Perf{}, nil, nil
	f.entries[vt.key] = &index
	switch {
	case rec.Missing:
		f.res.Missing++
	case rec.Degraded != "":
		f.res.Degraded++
	default:
		f.res.Staged++
	}
	if len(rec.RGB) > 0 && snapTrack != nil && snapTrack.EngineID > 0 {
		f.writeRGB(snapTrack.EngineID, rec.RGB)
	}
	return true, nil
}

// unread logs a track that could not be built now. A stop is returned.
func (f *filler) unread(vt *fillTrack, err error) (bool, error) {
	if f.ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return false, f.ctx.Err()
	}
	log.Printf("rb-bridge: track %d (%s) not staged: %v", vt.track.ID, vt.key, err)
	f.res.Unread++
	return false, nil
}

// writeRGB puts a finished track's mini waveform where Engine reads it, unless a valid one is
// there (Engine's own, or one written before).
func (f *filler) writeRGB(id int64, rgb []byte) {
	dst := filepath.Join(f.rgbDir, "OverviewData", f.snap.LibraryUUID, strconv.FormatInt(id, 10)+".rgb")
	if b, err := os.ReadFile(dst); err == nil {
		if raw, err := enginedb.QUncompress(b, 1<<20); err == nil && enginedb.ValidRGBOverview(raw) {
			return
		}
	}
	if err := enginedb.WriteRGBOverviewAtomic(f.rgbDir, f.snap.LibraryUUID, id, rgb); err != nil {
		log.Printf("rb-bridge: mini waveform of track %d not written: %v", id, err)
	}
}

// servePriority stages the tracks of the priority queue first and acknowledges them. A path
// the view does not have, or a track that cannot be built, is tried maxPriorityRetries times.
func (f *filler) servePriority() error {
	pending, maxSeq := PendingPriority(f.mount)
	var acked []PriorityItem
	left := false
	for _, item := range pending {
		key := foldPriorityPath(item.Path)
		retry := key + "#" + strconv.FormatUint(item.Seq, 10)
		if key == "" || f.retries[retry] >= maxPriorityRetries {
			continue
		}
		i, known := f.byKey[key]
		if !known {
			f.retries[retry]++
			left = left || f.retries[retry] < maxPriorityRetries
			continue
		}
		if f.handled[i] && f.built[i] {
			acked = append(acked, item) // staged or up to date already, or Engine's own row
			continue
		}
		started := time.Now()
		ok, err := f.fill(i)
		if err != nil {
			return err
		}
		if !ok {
			f.retries[retry]++
			left = left || f.retries[retry] < maxPriorityRetries
			continue
		}
		log.Printf("rb-bridge: prioritized track %s (%s, seq %d) in %s", key, item.Source, item.Seq, time.Since(started).Round(time.Millisecond))
		acked = append(acked, item)
	}
	if len(acked) > 0 {
		if err := f.stage.Sync(); err != nil {
			return err
		}
		_ = AckPriority(f.mount, acked)
		for _, it := range acked {
			f.lastAckSeq = max(f.lastAckSeq, it.Seq)
		}
	}
	if !left {
		f.lastAckSeq = max(f.lastAckSeq, maxSeq) // nothing more to do for these: do not spin on them
	}
	return nil
}

// publish reports whether the stage holds a record a window would apply: of the view's current
// row hash and rules, for a track the bridge writes, not applied yet. The same set as the
// remount asked for last time on this volume is not published again: the window did not take
// it, and a remount would only reload the library.
func (f *filler) publish() bool {
	var keys []string
	for key, e := range f.entries {
		i, ok := f.byKey[key]
		if !ok || e.Sig.Rules != rbstate.Rules || e.Sig.Row != f.view.tracks[i].hash {
			continue
		}
		if s := f.snap.Tracks[key]; s != nil && (s.Native || s.Perf != rbstate.PerfNone && s.Built.Equal(e.Sig)) {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return false
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, k := range keys {
		e := f.entries[k]
		fmt.Fprintf(h, "%s\x00%v\x00%v\x00", k, e.Sig.Audio, e.Sig.ANLZ)
	}
	sum := f.snap.LibraryUUID + " " + hex.EncodeToString(h.Sum(nil))
	vk, err := volumeIdentity(f.mount)
	if err != nil {
		return true
	}
	mark := publishMarkPath(vk)
	if b, err := os.ReadFile(mark); err == nil && string(b) == sum {
		log.Printf("rb-bridge: the stage holds %d tracks the last window did not apply; no remount for them", len(keys))
		return false
	}
	if err := writePrivateJSONRaw(mark, []byte(sum)); err != nil {
		log.Printf("rb-bridge: publish mark not written: %v", err)
	}
	return true
}

func publishMarkPath(v volumeKey) string {
	return filepath.Join(priorityRoot(), fmt.Sprintf("rb-publish-%x-%x", v.dev, v.ino))
}

// writePrivateJSONRaw writes data atomically with mode 0600, like writePrivateJSON.
func writePrivateJSONRaw(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".rb-pub-*")
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
