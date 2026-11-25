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

// The background job (design §1.5): performance data from the stick's ANLZ files and audio
// headers into the stage on /data, and the .rgb file of each finished track into the live
// OverviewData (never over a valid one). It never opens m.db and writes nothing else on the stick.

type FillOptions struct {
	Progress func(done, total int) // tracks compared so far; never goes down (#328)
	Stage    *rbstate.Stage        // nil: the stage of the snapshot's library
	Settings string                // the deck's settings file (layout, language), "" for the defaults
}

type FillResult struct {
	Staged, Degraded, Missing int
	Unread                    int  // could not be built now; logged, and the next job tries again
	Left                      int  // not compared when ctx ended
	Publish                   bool // the stage holds something a window would apply: ask for the remount
}

// putStaged is Stage.Put; a variable for tests of a full /data.
var putStaged = (*rbstate.Stage).Put

// fillLockWait is how long the job waits for a window to release the volume.
var fillLockWait = 2 * time.Minute

type fillTrack struct {
	key   string
	track rekordbox.Track
	hash  string // the view's hash of the export fields: Sig.Row
}

type fillView struct {
	stamp  string
	tracks []fillTrack
	view   *View // as a window builds it: what a next library is made from
}

// hookWindow is the time the hook gives a window (design section 3).
const hookWindow = 17 * time.Second

// readFillView reads the exports as a window would (FindSources, ReadSources, the staged CDJ
// libraries, BuildView) into the view's tracks and the stamp; a variable for tests.
var readFillView = func(ctx context.Context, r *compat.Resolver, mount, layout string) (*fillView, error) {
	return readJobView(ctx, r, mount, layout, "")
}

func readJobView(ctx context.Context, r *compat.Resolver, mount, layout, lang string) (*fillView, error) {
	files, err := FindSources(r, mount)
	if err != nil {
		return nil, err
	}
	libs, failed, err := ReadSources(ctx, r, mount, files, nil)
	if err != nil {
		return nil, err
	}
	v, err := BuildView(append(libs, readStaged(mount)...), layout, lang)
	if err != nil {
		return nil, err
	}
	v.Stamp, v.Sources = rbstate.Stamp(layout, files, stagedStamp(mount)), files
	out := &fillView{stamp: v.Stamp, tracks: make([]fillTrack, len(v.Tracks)), view: v}
	if len(failed) > 0 || len(files) == 0 {
		out.view = nil // a library could not be read: nothing is removed for it, by a window only
	}
	for i, t := range v.Tracks {
		out.tracks[i] = fillTrack{key: t.Key, track: t.Track, hash: t.Hash}
	}
	return out, nil
}

// FillInBackground is `rb-bridge --import-path`: the priority queue first, then the view's
// tracks by path. Each is stat'ed and built into the stage unless the snapshot or the stage has
// its signature; once the stage verified the stamp, built and staged tracks are not stat'ed
// again. All compared: MarkVerified. It yields to a playing deck (pace.go).
func FillInBackground(ctx context.Context, mount string, o FillOptions) (FillResult, error) {
	var res FillResult
	start := time.Now()
	r, err := compat.NewResolver(mount)
	if err != nil {
		return res, err
	}
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	// A window of this mount may still run (design §1.5).
	unlock, err := rbstate.LockVolume(mount, fillLockWait)
	if err != nil {
		return res, fmt.Errorf("volume lock: %w", err)
	}
	snap, err := rbstate.Load(dbDir)
	unlock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("snapshot: %w", err)
	}
	loaded := time.Now()
	layout := ReadLayout(o.Settings)
	if snap != nil {
		layout = snap.Layout
	}
	view, err := readJobView(ctx, r, mount, layout, readSetting(o.Settings, "lang"))
	if snap == nil && (err != nil || view.view == nil) {
		return res, fmt.Errorf("snapshot: %w", rbstate.ErrNoSnapshot) // no window converted this stick, and there is nothing to convert
	}
	if err != nil {
		return res, err
	}
	read := time.Now()
	pace := newPacer(ctx)
	next := &nextBuild{ctx: ctx, mount: mount, dir: dbDir, view: view.view, live: snap,
		pace: func(took time.Duration) error { return pace.afterStat(pace.active(), took) }}
	// Rows and lists a window left to the job come first, with what is staged already: they are
	// what the DJ browses. The performance data follows in the job the swap's window starts.
	if rows := snap == nil || view.view != nil && Diff(view.view, snap).structural(); rows && view.view != nil && o.Stage == nil {
		if err = next.build(false, true); err == nil {
			res.Publish = next.ask()
			log.Printf("rb-bridge: rows and lists of %s are in the next library (exports read in %s); publish=%v", mount, read.Sub(loaded).Round(time.Millisecond), res.Publish)
			return res, nil
		}
		if snap == nil || !errors.Is(err, errNoNext) {
			return res, err
		}
		log.Printf("rb-bridge: %v; rows and lists wait for the windows", err)
	}
	if snap == nil {
		return res, fmt.Errorf("snapshot: %w", rbstate.ErrNoSnapshot)
	}
	if view.view != nil && snap.Stamp != view.stamp && o.Stage == nil {
		// The window left the exports to the job and they change no row and no list: the snapshot
		// takes their stamp, as that window would have given it, and the job compares every file.
		if unlock, err = rbstate.LockVolume(mount, fillLockWait); err != nil {
			return res, fmt.Errorf("volume lock: %w", err)
		}
		if snap, err = rbstate.Load(dbDir); err == nil && snap.Stamp != view.stamp {
			snap.Stamp, snap.Sources, snap.Pending.Verify = view.stamp, view.view.Sources, true
			err = snap.Save(dbDir)
		}
		if unlock(); err != nil {
			return res, fmt.Errorf("snapshot: %w", err)
		}
		next.live = snap
	}
	stage := o.Stage
	if stage == nil {
		if stage, err = rbstate.OpenStage(snap.LibraryUUID); err != nil {
			return res, err
		}
		defer stage.Close()
	}
	entries, verified, err := stage.Entries()
	if err != nil {
		return res, fmt.Errorf("stage: %w", err)
	}
	// A record a next library applied stayed in the stage for the deck's loads until the swap:
	// its list waveform is written now that the row is the stick's, and it goes.
	for key, e := range entries {
		if t := snap.Tracks[key]; t != nil && t.EngineID != 0 && t.Perf != rbstate.PerfNone && !e.Missing && t.Built.Equal(e.Sig) {
			if full, e2 := stage.Read(e); e2 == nil && len(full.RGB) > 0 {
				writeOverview(dbDir, snap.LibraryUUID, t.EngineID, full.RGB)
			}
			if err = stage.Drop(key); err != nil {
				return res, err
			}
			delete(entries, key)
		}
	}
	// What a next library holds already is not staged again.
	inNext, _ := rbstate.Load(nextState(snap.LibraryUUID))
	f := &filler{
		ctx: ctx, mount: mount, r: r, snap: snap, stage: stage, view: view, entries: entries,
		quick: verified != "" && verified == view.stamp, pace: pace, retries: map[string]int{}, inNext: inNext,
		byKey: make(map[string]int, len(view.tracks)), handled: make([]bool, len(view.tracks)), built: make([]bool, len(view.tracks)),
		rgbDir: dbDir, progress: o.Progress,
	}
	order := make([]int, 0, len(view.tracks))
	for i, t := range view.tracks {
		f.byKey[t.key] = i
		if s := snap.Tracks[t.key]; s != nil && s.Native {
			f.handled[i], f.built[i] = true, true // Engine's own row: the bridge never writes it (#252)
		} else {
			order = append(order, i)
		}
	}
	f.total = len(order)
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Or(strings.Compare(view.tracks[a].key, view.tracks[b].key), cmp.Compare(view.tracks[a].track.ID, view.tracks[b].track.ID))
	})
	f.report()
	err = f.run(order)
	for full := 0; errors.Is(err, rbstate.ErrStageFull) && view.view != nil && o.Stage == nil && full < 64; full++ {
		// /data is at its floor: what is staged goes into the next library and leaves the stage, and
		// the job goes on. The remount is still asked for once, at the end.
		log.Printf("rb-bridge: the stage of %s is full after %d of %d tracks; they go into the next library", mount, f.done, f.total)
		if e := next.build(true, false); e != nil {
			log.Printf("rb-bridge: %v", e)
			break
		}
		if f.inNext, err = rbstate.Load(nextState(snap.LibraryUUID)); err == nil {
			if f.entries, _, err = stage.Entries(); err == nil {
				err = f.run(order)
			}
		}
	}
	if err == nil && f.done == f.total {
		err = f.refuseUnreadable()
	}
	if errors.Is(err, rbstate.ErrStageFull) { // what is staged is published; a window drops it
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
	if err == nil || errors.Is(err, ErrFilesUnreadable) {
		// What could be read is published all the same: in place when one window does it all,
		// else as a next library, which a window only swaps in.
		ahead := exportWork(view.view.sources(), len(snap.Tracks))
		ahead.warm, ahead.staged, ahead.updates = true, f.applicable(), snap.Pending.Rows
		ahead.commits += ahead.staged/applyBatch + ahead.updates/updateBatch
		if e := errNoNext; view.view != nil && o.Stage == nil && ahead.cost() > hookWindow-spare {
			if e = next.build(false, true); e == nil {
				res.Publish = next.ask()
			} else if !errors.Is(e, errNoNext) && err == nil {
				err = e
			}
			if e != nil {
				log.Printf("rb-bridge: %v; the staged data is published in place, a window at a time", e)
			}
		}
		if !res.Publish {
			res.Publish = f.publish()
		}
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
	inNext   *rbstate.Snapshot // of the next library, nil without one
	// the priority queue: the last sequence served, and tries of a path that cannot be staged
	lastAckSeq uint64
	retries    map[string]int
}

const (
	stageSyncEvery     = 16 // records between syncs of the stage's index
	maxPriorityRetries = 3
)

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

// fill stages one track unless the snapshot or the stage has it. ok is false when it could not
// be built now; the error is a stop or a stage that cannot take it.
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
		// Verified and built: not stat'ed. Missing and degraded tracks are, so a fixed file is found.
		return true, nil
	}
	if e := f.entries[vt.key]; f.quick && e != nil && !e.Missing && e.Degraded == "" && e.Sig.Rules == rbstate.Rules && e.Sig.Row == vt.hash {
		// Verified and staged, waiting for a window: not stat'ed either (KDATA: 155 s a round for 5,500).
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
	if f.inNext != nil {
		if t := f.inNext.Tracks[vt.key]; t != nil && t.Perf != rbstate.PerfNone && t.Built.Equal(src.Sig) {
			return true, nil
		}
	}
	rec, err := enginedb.BuildTrack(f.r, vt.track, src, opts)
	if err != nil {
		return f.unread(vt, err)
	}
	rec.Key, rec.Sig.Row = vt.key, vt.hash
	if snapTrack != nil {
		rec.EngineID = snapTrack.EngineID // for the deck's UI hook, which fills a row a deck loads before its window
	}
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

// ErrFilesUnreadable: many tracks have no file under their name and unclaimed files lie where
// those names should be: the mount does not give the names as the export wrote them (real stick
// 2026-11-19: a mount without UTF-8 names made 707 tracks look deleted). Nothing is removed.
var ErrFilesUnreadable = errors.New("files of the export are on the drive under names that cannot be read")

// unreadableLimit is how many such tracks pass as deleted: 2 %, at least 5, at most 50.
func unreadableLimit(tracks int) int { return min(50, max(5, tracks/50)) }

// refuseUnreadable drops the Missing records when too many are files the mount cannot name.
// Files deleted by hand, however many, stay Missing: the window removes their rows.
func (f *filler) refuseUnreadable() error {
	kept := map[string]bool{}
	var gone, keys []string
	for i := range f.view.tracks {
		vt := &f.view.tracks[i]
		if e := f.entries[vt.key]; e != nil && e.Sig.Row == vt.hash {
			if e.Missing {
				gone, keys = append(gone, vt.track.Path), append(keys, vt.key)
			} else {
				kept[e.Rel] = true
			}
		} else if s := f.snap.Tracks[vt.key]; s != nil && s.Perf != rbstate.PerfMissing {
			kept[s.Rel] = true
		}
	}
	hidden := hiddenTracks(f.r, kept, gone)
	if hidden <= unreadableLimit(len(f.view.tracks)) {
		return nil
	}
	log.Printf("rb-bridge: NOT REMOVED: the files of %d of the export's %d tracks are on the drive under names that cannot be read as the export wrote them. Nothing is removed; the next insert tries again", hidden, len(f.view.tracks))
	if err := f.stage.Drop(keys...); err != nil {
		return err
	}
	for _, k := range keys {
		delete(f.entries, k)
	}
	f.res.Missing = 0
	return fmt.Errorf("%w: %d of %d tracks", ErrFilesUnreadable, hidden, len(f.view.tracks))
}

// hiddenTracks counts the tracks of gone (paths without a file) whose file is probably there
// under another name: where the lookup stopped lies an entry no found track (kept) leads to, of
// the same kind: a folder holding a file (an album deleted by hand leaves empty folders), or a
// file with the same extension.
func hiddenTracks(r *compat.Resolver, kept map[string]bool, gone []string) int {
	claimed := map[string]map[string]bool{}
	for rel := range kept {
		dir := ""
		for _, name := range strings.Split(rel, "/") {
			if claimed[dir] == nil {
				claimed[dir] = map[string]bool{}
			}
			claimed[dir][name] = true
			dir = filepath.Join(dir, name)
		}
	}
	type place struct{ dir, kind string } // kind: "/" for a folder, else the file's extension
	lost := map[place]map[string]int{}    // the names not found there, and their tracks
	for _, p := range gone {
		stop, ok := r.Stopped(p)
		if !ok {
			continue
		}
		at := place{stop.Dir, "/"}
		if stop.File {
			at.kind = strings.ToLower(filepath.Ext(stop.Name))
		}
		if lost[at] == nil {
			lost[at] = map[string]int{}
		}
		lost[at][stop.Name]++
	}
	hidden := 0
	for at, names := range lost {
		free, tracks := 0, 0
		for _, name := range r.Entries(at.dir, at.kind == "/") {
			if free >= len(names) || claimed[at.dir][name] || strings.HasPrefix(name, ".") {
				continue
			}
			if at.kind == "/" {
				if !holdsFile(r.FullPath(filepath.Join(at.dir, name))) {
					continue
				}
			} else if strings.ToLower(filepath.Ext(name)) != at.kind {
				continue
			}
			free++
		}
		for _, n := range names {
			tracks += n
		}
		hidden += tracks * min(free, len(names)) / len(names)
	}
	return hidden
}

// holdsFile reports whether a file, hidden ones aside, lies anywhere under dir.
func holdsFile(dir string) bool {
	found := errors.New("found")
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err == nil && !d.IsDir() {
			return found
		}
		return nil
	}) == found
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

// writeRGB puts a finished track's mini waveform where Engine reads it, unless a valid one is there.
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

// servePriority stages the queue's tracks and acknowledges them. A path the view does not have,
// or a track that cannot be built, is tried maxPriorityRetries times.
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

// sources are the export files the view was read from; nil for no view.
func (v *View) sources() []rbstate.FileSig {
	if v == nil {
		return nil
	}
	return v.Sources
}

// staged are the stage's records a window would apply, sorted.
func (f *filler) staged() []string {
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
	slices.Sort(keys)
	return keys
}

func (f *filler) applicable() int { return len(f.staged()) }

// askOnce reports whether a remount for this publication has not been asked for yet, and notes
// it: a publication the last window did not take is not asked for again.
func askOnce(mount, sum string) bool {
	vk, err := volumeIdentity(mount)
	if err != nil {
		return true
	}
	mark := volumeFile("rb-publish", vk, "")
	if b, err := os.ReadFile(mark); err == nil && string(b) == sum {
		return false
	}
	if err := writePrivate(mark, []byte(sum)); err != nil {
		log.Printf("rb-bridge: publish mark not written: %v", err)
	}
	return true
}

// ask reports whether the remount that swaps this next library in is to be asked for.
func (n *nextBuild) ask() bool {
	h, err := rbstate.ReadHeaderFile(filepath.Join(n.dir, rbstate.SwapName))
	if err != nil {
		return false
	}
	if !askOnce(n.mount, fmt.Sprintf("next %s %d %s", h.LibraryUUID, h.Commit, h.Base)) {
		log.Printf("rb-bridge: the last window did not swap the next library in; no remount for it")
		return false
	}
	return true
}

// publish reports whether the stage holds a record a window would apply. The set the last
// remount was asked for is not asked for again (that window did nothing with it), unless rows
// were still waiting: they are part of the sum (KDATA: a take-over window updated 5,524 rows and
// applied nothing, and without another window cue write-back stayed locked out).
func (f *filler) publish() bool {
	keys := f.staged()
	if len(keys) == 0 {
		return false
	}
	h := sha256.New()
	for _, k := range keys {
		e := f.entries[k]
		fmt.Fprintf(h, "%s\x00%v\x00%v\x00", k, e.Sig.Audio, e.Sig.ANLZ)
	}
	if !askOnce(f.mount, fmt.Sprintf("%s %s %d", f.snap.LibraryUUID, hex.EncodeToString(h.Sum(nil)), f.snap.Pending.Rows)) {
		log.Printf("rb-bridge: the stage holds %d tracks the last window did not apply; no remount for them", len(keys))
		return false
	}
	return true
}
