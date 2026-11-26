package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// window is one mount's work on the library.
type window struct {
	ctx     context.Context
	mount   string
	dir     string // Engine Library/Database2
	view    *View
	st      *steps
	o       MountOptions
	res     *MountResult
	load    func() (*rbstate.Snapshot, error) // the stick's snapshot (its read may have started already)
	partial bool                              // a library was unreadable: nothing is removed and the stamp is not kept

	// next: the work is done on the next library (next.go), by the job and without a deadline. Its
	// snapshot is kept on the deck until it is ready; the stick's own library is only read.
	next bool
	keep bool                      // staged records stay in the stage once applied (until the swap)
	rest func(time.Duration) error // after each commit, with the time it took: the job's pacing

	lib   *enginedb.Library // nil: the stick has no library to keep, one is built
	snap  *rbstate.Snapshot
	index map[string]enginedb.Row // every row of the library by key
	head  rbstate.Header          // the snapshot's header as the stick has it
	dirty bool                    // tracks or lists changed outside a commit
	left  bool                    // work that would not end before the kill was left to the job
	built bool                    // library was built fresh from exports; holds no legacy artwork
	took  laps                    // where the time of the commits and of the staged step went
}

// leave notes that the window leaves this work to the job (see work).
func (w *window) leave(what work, name string) {
	w.left = true
	log.Printf("rb-bridge: mount leaves %s to the job: about %d ms of work, %d ms left", name, what.cost().Milliseconds(), time.Until(w.st.kill).Milliseconds())
}

// laps are the parts of a window's commits and of its staged step, for the journal.
type laps struct{ read, rows, save, commit, promote, rgb, drop time.Duration }

// lap adds the time since *t to d and moves *t on.
func lap(d *time.Duration, t *time.Time) {
	now := time.Now()
	*d += now.Sub(*t)
	*t = now
}

// cut, in tests, is called between two writes that a power cut or the hook's kill can separate.
var cut = func(step string) {}

var errWindowDeadline = errors.New("mount window deadline reached")

// file is the library the window writes.
func (w *window) file() string {
	if w.next {
		return filepath.Join(w.dir, nextDB)
	}
	return filepath.Join(w.dir, "m.db")
}

// sdir is where the snapshot of that library is.
func (w *window) sdir() string {
	if w.next {
		dir := nextState(w.snap.LibraryUUID)
		_ = os.MkdirAll(dir, 0o700)
		return dir
	}
	return w.dir
}

func (w *window) close() {
	if w.lib != nil {
		w.lib.Close()
		w.lib = nil
	}
}

// run does the window's work in the order of design 1.1 rule 4: rows and lists, then staged data.
func (w *window) run() (err error) {
	defer w.close()
	if err = w.open(); err != nil {
		return err
	}
	if !w.o.OnDemand && w.snap.Stamp != w.view.Stamp { // the job compares every track's files with what was built
		w.snap.Pending.Verify, w.dirty = true, true
	}
	n := len(w.view.Tracks)
	if first := (work{build: n, commits: 1, tracks: n}); w.lib != nil {
		err = w.apply(Diff(w.view, w.snap))
	} else if w.st.fits(first) {
		err = w.build()
	} else {
		w.leave(first, "the first library")
		return nil
	}
	// Staged data is applied in place by the window the job asked for, and by a run without a
	// kill: an insert gives the DJ the stick. In OnDemand mode, tracks are read directly at load.
	if err == nil && !w.o.OnDemand && !w.left && (w.next || w.o.Publish || w.st.kill.IsZero()) {
		err = w.applyStaged()
	}
	if err == nil && (!w.left || w.st.fits(work{commits: 1, tracks: n})) {
		err = w.saveIfChanged()
	}
	if err == nil {
		err = w.migrateLegacyArtwork()
	}
	return err
}

// open opens the stick's library and its snapshot. w.lib stays nil when there is no library to
// keep: no m.db, or Engine's empty skeleton (one with rows Engine added is a library).
func (w *window) open() error {
	db := w.file()
	if !w.next && enginedb.OpenElsewhere(db) != 0 {
		return enginedb.ErrLibraryInUse // Engine would go on writing a file that is no longer the stick's
	}
	if w.snap == nil {
		w.snap = rbstate.New("")
	}
	if st, err := os.Stat(db); err != nil || st.Size() == 0 {
		return nil
	}
	lib, err := enginedb.OpenLibraryFile(w.ctx, db, filepath.Dir(filepath.Dir(w.dir)))
	if err != nil {
		return err
	}
	w.lib, w.snap = lib, rbstate.New(lib.UUID())
	if err = w.settle(); err != nil {
		return err
	}
	if w.load == nil {
		w.load = func() (*rbstate.Snapshot, error) { return rbstate.Load(w.sdir()) }
	}
	snap, err := w.load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, oldErr := os.Stat(filepath.Join(w.dir, oldStateName))
	empty, err := lib.Empty(w.ctx)
	if err != nil {
		return err
	}
	if empty {
		hasSequence, seqErr := lib.HasTrackSequence(w.ctx)
		if seqErr != nil {
			return seqErr
		}
		if enginedb.HasRGBOverviews(w.dir, lib.UUID()) || hasSequence {
			// A previously populated library emptied on the deck must not reuse its UUID
			// or restart Track IDs onto stale OverviewData/<uuid>/<id>.rgb files.
			w.snap = rbstate.New("")
			w.close()
			return nil
		}
		if snap == nil && oldErr != nil {
			w.close() // Engine's pristine skeleton: a library is built under its UUID
			return nil
		}
	}
	switch {
	case snap != nil && snap.LibraryUUID == lib.UUID() && snap.Rules == rbstate.Rules:
		w.snap, w.head = snap, snap.Header
	case snap != nil && snap.LibraryUUID == lib.UUID():
		// Rules revision bumped (#178): keep bridge ownership, Engine IDs, and deck edits,
		// but clear Built and reset non-deck perf state so the background job re-stages once.
		for _, t := range snap.Tracks {
			t.Built = rbstate.TrackSig{}
			if t.Perf != rbstate.PerfDeck && t.Perf != rbstate.PerfMissing {
				t.Perf = rbstate.PerfNone
			}
		}
		snap.Rules, snap.Stamp = rbstate.Rules, ""
		w.snap, w.dirty = snap, true
	case oldErr == nil && !w.next:
		if w.snap, err = adopt(w.ctx, db, w.dir, lib.UUID()); err != nil {
			return err
		}
		log.Printf("rb-bridge: library of an earlier build taken over: %d tracks, %d lists", len(w.snap.Tracks), len(w.snap.Lists))
	}
	if err = w.reconcile(); err != nil {
		return err
	}
	w.st.done("snapshot", "%d tracks, %d lists", len(w.snap.Tracks), len(w.snap.Lists))
	return nil
}

// settle finishes a window cut around its commit: its next snapshot is promoted when the library
// has that commit, and dropped otherwise (design section 5).
func (w *window) settle() error {
	claims, err := rbstate.LoadNext(w.sdir())
	if err != nil {
		return nil
	}
	id, err := w.lib.CommitID(w.ctx)
	if err != nil {
		return err
	}
	if claims.LibraryUUID != w.lib.UUID() || claims.Commit != id || claims.Rules != rbstate.Rules {
		return rbstate.DropNext(w.sdir())
	}
	if err = rbstate.Promote(w.sdir()); err != nil {
		return err
	}
	log.Printf("rb-bridge: the last window was cut short after its commit; its %d tracks and %d lists are taken over", len(claims.Tracks), len(claims.Lists))
	return nil
}

// reconcile drops from the snapshot what Engine removed and takes the spelling another part of
// EngineX gave a row's path (#322). It fills w.index.
func (w *window) reconcile() (err error) {
	if w.index, err = w.lib.TrackIndex(w.ctx); err != nil {
		return err
	}
	lists, err := w.lib.ListIDs(w.ctx)
	if err != nil {
		return err
	}
	gone := 0
	n := 0
	for key, t := range w.snap.Tracks {
		if n&511 == 0 {
			if err = w.ctx.Err(); err != nil {
				return err
			}
		}
		n++
		row, ok := w.index[key]
		switch rel := strings.TrimPrefix(row.Path, "../"); {
		case t.EngineID == 0:
		case !ok || row.ID != t.EngineID:
			delete(w.snap.Tracks, key)
			gone++
		case rel != t.Rel:
			t.Rel, t.Resolved, w.dirty = rel, true, true
		}
	}
	n = 0
	for _, l := range w.snap.Lists {
		if n&511 == 0 {
			if err = w.ctx.Err(); err != nil {
				return err
			}
		}
		n++
		if l.EngineID != 0 && !lists[l.EngineID] {
			l.EngineID, l.Hash = 0, "" // still the bridge's: putLists takes back a list Engine made again
			gone++
		}
	}
	if gone > 0 {
		w.dirty = true
		log.Printf("rb-bridge: %d rows of the last import are no longer in the library", gone)
	}
	return nil
}

func (w *window) rows(idx []int) []enginedb.TrackRow {
	rows := make([]enginedb.TrackRow, len(idx))
	for i, at := range idx {
		vt := &w.view.Tracks[at]
		var artHash []byte
		if w.o.OnDemand && vt.Track.ArtworkPath != "" {
			artHash = rbstate.DirectArtworkHash(vt.Track.ArtworkPath)
		}
		rows[i] = enginedb.TrackRow{Rel: vt.Rel, Track: vt.Track, DirectArtHash: artHash}
	}
	return rows
}

func (w *window) note(vt *ViewTrack, id int64, native bool) *rbstate.Track {
	t := &rbstate.Track{
		Key:          vt.Key,
		EngineID:     id,
		Rel:          vt.Rel,
		AnalysisPath: vt.Track.AnalysisPath,
		ArtworkPath:  vt.Track.ArtworkPath,
		Duration:     vt.Track.Duration,
		SampleRate:   vt.Track.SampleRate,
		Refs:         vt.Refs,
		Row:          vt.Hash,
		Rating:       vt.Track.Rating,
		Native:       native,
	}
	if !native {
		base := enginedb.FastBaseForTrack(vt.Track)
		t.FastCues, t.FastLoops, t.FastActive, t.FastKnown = base.Cues, base.Loops, base.Active, true
		if w.o.OnDemand {
			// Record initial write-back digest so #321 skips all untouched rows in 0 ms without a perf pass.
			t.Edit = 0
			t.Synced = enginedb.SyncedDigest(nil, base.Cues, base.Loops, int64(base.Active), vt.Track.Genre, vt.Track.Comment, vt.Track.Label)
		}
	}
	w.snap.Tracks[vt.Key] = t
	return t
}

// noteSync keeps write-back's note of a row (Edit, Synced), so that write-back does not read a
// row only the bridge wrote. A written row is read back; its digest stands only when the row was
// in step before. An unwritten row keeps its note in step, and is dated without a read otherwise.
func (w *window) noteSync(b *enginedb.Batch, t *rbstate.Track, written bool) error {
	if t.Native {
		return nil
	}
	row := w.index[t.Key]
	insync := row.Edit == t.Edit && t.Perf != rbstate.PerfDeck
	if !written {
		if !insync {
			t.Edit, t.Synced = row.Edit, nil
		}
		return nil
	}
	edit, digest, err := b.Synced(w.ctx, t.EngineID)
	if err != nil {
		return err
	}
	if !insync {
		digest = nil
	}
	t.Edit, t.Synced, row.Edit = edit, digest, edit
	w.index[t.Key] = row
	return nil
}

// commit saves the next snapshot, runs the commit and promotes the snapshot (design section 5).
func (w *window) commit(id int64, do func() error) error {
	if w.rest != nil {
		if err := w.rest(0); err != nil {
			return err
		}
	}
	t := time.Now()
	w.count()
	w.snap.Applied, w.snap.Commit = t.Unix(), id
	if err := w.snap.SaveNext(w.sdir()); err != nil {
		return err
	}
	lap(&w.took.save, &t)
	cut("next saved")
	if err := do(); err != nil {
		_ = rbstate.DropNext(w.sdir())
		return err
	}
	lap(&w.took.commit, &t)
	cut("committed")
	if err := rbstate.Promote(w.sdir()); err != nil {
		return err
	}
	lap(&w.took.promote, &t)
	cut("promoted")
	w.saved()
	return nil
}

func (w *window) commitBatch(b *enginedb.Batch) error {
	return w.commit(b.CommitID(), func() error { return b.Commit(w.ctx) })
}

// saved notes that the stick has the snapshot in memory; an older build's state then goes.
func (w *window) saved() {
	if w.head, w.dirty = w.snap.Header, false; !w.next {
		dropOldState(w.dir) // of a next library only once it is the stick's (swapNext)
		if w.o.OnDemand {
			if err := rbstate.SaveBrowseIndex(w.dir, w.snap); err != nil {
				log.Printf("rb-bridge: browse index not saved: %v", err)
			}
		}
	}
}

// saveIfChanged saves the snapshot when it changed without a commit.
func (w *window) saveIfChanged() error {
	w.count()
	if !w.dirty && reflect.DeepEqual(w.snap.Header, w.head) {
		return nil
	}
	if err := w.snap.Save(w.sdir()); err != nil {
		return err
	}
	w.saved()
	return nil
}

// count fills the snapshot's header (what it was made from, what is pending) and its tracks' refs.
func (w *window) count() {
	s := w.snap
	s.Header.Tracks, s.Header.Lists = len(s.Tracks), len(s.Lists)
	s.Layout, s.Sources, s.Stamp = w.view.Layout, w.view.Sources, w.view.Stamp
	if w.partial {
		s.Stamp = "" // a library was unreadable: the next insert reads again, and write-back waits
	}
	p := rbstate.Pending{Verify: s.Pending.Verify && !w.o.OnDemand}
	for i := range w.view.Tracks {
		vt := &w.view.Tracks[i]
		t := s.Tracks[vt.Key]
		if t != nil {
			t.Refs = vt.Refs // this export's IDs: a re-export renumbers them, and write-back finds rows by them
			if t.AnalysisPath != vt.Track.AnalysisPath || t.ArtworkPath != vt.Track.ArtworkPath ||
				t.Duration != vt.Track.Duration || t.SampleRate != vt.Track.SampleRate {
				t.AnalysisPath, t.ArtworkPath = vt.Track.AnalysisPath, vt.Track.ArtworkPath
				t.Duration, t.SampleRate = vt.Track.Duration, vt.Track.SampleRate
				w.dirty = true
			}
		}
		switch {
		case t != nil && (t.Native || t.Perf == rbstate.PerfMissing):
		case t == nil || t.EngineID == 0 || t.Row != vt.Hash:
			p.Rows++
		case !w.o.OnDemand && t.Perf == rbstate.PerfNone:
			p.Perf++
		}
	}
	for i := range w.view.Lists {
		if l := s.Lists[w.view.Lists[i].Key]; l == nil || l.Hash != w.view.Lists[i].Hash {
			p.Lists = true
		}
	}
	s.Pending = p
}

// build makes a stick's first library on the deck's disk from the export alone, and puts it on
// the stick with one rename (design 1.1 rule 1).
func (w *window) build() error {
	if w.rest != nil {
		if err := w.rest(0); err != nil {
			return err
		}
	}
	stage, err := rbstate.StageRoot()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(stage, "build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	all := make([]int, max(len(w.view.Tracks), len(w.view.Lists)))
	for i := range all {
		all[i] = i
	}
	ids, uuid, commitID, err := enginedb.BuildLibrary(w.ctx, tmp, w.snap.LibraryUUID, w.rows(all[:len(w.view.Tracks)]), func(b *enginedb.Batch, ids []int64) error {
		for i, id := range ids {
			w.note(&w.view.Tracks[i], id, false)
		}
		return w.putLists(b, all[:len(w.view.Lists)])
	})
	if err != nil {
		return err
	}
	w.snap.LibraryUUID = uuid
	w.st.done("build", "%d rows, %d lists, on the deck's disk", len(ids), len(w.snap.Lists))
	if w.ctx.Err() != nil || (!w.next && !w.st.must(0)) {
		return errWindowDeadline
	}
	if !w.next && enginedb.OpenElsewhere(w.file()) != 0 {
		return enginedb.ErrLibraryInUse
	}
	if err = cleanSQLiteSidecars(w.file()); err != nil {
		return err
	}
	const installFlushBytes = 16 << 20
	install := func() error {
		src := filepath.Join(tmp, "m.db")
		if w.rest != nil {
			return rbstate.InstallFilePaced(src, w.file(), playingReadChunk, installFlushBytes, func(_ int, took time.Duration) error {
				return w.rest(took)
			})
		}
		return rbstate.InstallFile(src, w.file())
	}
	if err = w.commit(commitID, install); err != nil {
		return err
	}
	w.st.done("install", "library copied to the stick")
	w.res.Added, w.res.PlaylistsChanged = len(ids), len(w.view.Lists) > 0
	if w.o.Progress != nil {
		w.o.Progress(len(ids), len(ids))
	}
	if w.lib, err = enginedb.OpenLibraryFile(w.ctx, w.file(), filepath.Dir(filepath.Dir(w.dir))); err != nil {
		return err
	}
	w.built = true
	if w.index, err = w.lib.TrackIndex(w.ctx); err != nil {
		return err
	}
	log.Printf("rb-bridge: first import of %d tracks published from the export; grids and waveforms follow", len(ids))
	return nil
}

// putLists writes the view's lists at idx and notes them. Entries are the tracks that have a row;
// a list that lacks one is noted "partial" and written again later.
func (w *window) putLists(b *enginedb.Batch, idx []int) error {
	id := func(key string) int64 {
		if l := w.snap.Lists[key]; l != nil {
			return l.EngineID
		}
		return 0
	}
	reorder := map[string]bool{}
	commitCost := work{commits: 1, tracks: len(w.snap.Tracks)}.cost()
	for _, at := range idx {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if !w.next && !w.st.must(commitCost) {
			return errWindowDeadline
		}
		l := &w.view.Lists[at]
		parent := id(l.Parent)
		if l.Parent != "" && parent == 0 {
			return fmt.Errorf("list %d has no folder in the library", at)
		}
		var oldHead, oldEntries string
		old, listID := w.snap.Lists[l.Key], id(l.Key)
		if old != nil && listID != 0 {
			oldHead, oldEntries, _ = strings.Cut(old.Hash, ":")
		}
		var err error
		if listID == 0 && old != nil { // Engine removed the bridge's list: the one it made in its place, if any
			listID, err = b.FindList(w.ctx, l.Name, parent)
		}
		if listID == 0 && err == nil {
			listID, err = b.InsertList(w.ctx, enginedb.ListRow{Title: l.Name, Parent: parent})
		}
		if err != nil {
			return err
		}
		head, entries, _ := strings.Cut(l.Hash, ":")
		if oldHead != head {
			reorder[l.Parent] = true
		}
		hash, members := l.Hash, make([]int64, 0, len(l.Tracks))
		for _, key := range l.Tracks {
			if t := w.snap.Tracks[key]; t != nil && t.EngineID != 0 {
				members = append(members, t.EngineID)
			} else if t == nil || t.Perf != rbstate.PerfMissing { // a missing file has no row to wait for
				hash = "partial"
			}
		}
		if !l.Folder && oldEntries != entries {
			if err = b.SetEntries(w.ctx, listID, members); err != nil {
				return err
			}
		}
		w.snap.Lists[l.Key] = &rbstate.List{Key: l.Key, EngineID: listID, Hash: hash}
	}
	for parent := range reorder {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if !w.next && !w.st.must(commitCost) {
			return errWindowDeadline
		}
		if w.rest != nil {
			if err := w.rest(0); err != nil {
				return err
			}
		}
		var order []int64
		for i := range w.view.Lists {
			if w.view.Lists[i].Parent == parent {
				order = append(order, id(w.view.Lists[i].Key))
			}
		}
		if err := b.OrderLists(w.ctx, id(parent), order); err != nil {
			return err
		}
	}
	return nil
}

// apply writes a plan: removed, moved and new rows (while the window has time) and the lists in
// one transaction, then the rows whose export fields changed (updateRows).
func (w *window) apply(p *Plan) error {
	s := w.snap
	if w.partial { // nothing is removed of a library that was not read
		p.RemoveTracks = nil
		p.RemoveLists = nil
	}
	w.st.done("diff", "+%d ~%d >%d -%d tracks, %d lists to write, %d to remove", len(p.AddTracks), len(p.UpdateTracks), len(p.Moves), len(p.RemoveTracks), len(p.PutLists), len(p.RemoveLists))
	// A file that has a row the bridge did not make, under any spelling, keeps it: lists refer to
	// it, the bridge never writes it (#252). A file that moved onto one leaves its old row.
	p.Moves = slices.DeleteFunc(p.Moves, func(m Move) bool {
		_, native := w.index[w.view.Tracks[m.New].Key]
		if native {
			p.RemoveTracks, p.AddTracks = append(p.RemoveTracks, m.Old), append(p.AddTracks, m.New)
		}
		return native
	})
	rows := work{adds: len(p.AddTracks), removes: len(p.RemoveTracks), updates: len(p.Moves), lists: len(p.PutLists) + len(p.RemoveLists), commits: 1, tracks: len(s.Tracks)}
	for _, at := range p.PutLists {
		rows.entries += len(w.view.Lists[at].Tracks)
	}
	if !w.st.fits(rows) {
		w.leave(rows, "the rows and lists")
		return nil
	}
	if !p.structural() {
		return w.updateRows(p.UpdateTracks)
	}
	b, err := w.lib.Begin(w.ctx)
	if err != nil {
		return err
	}
	defer b.Rollback()
	listAndCommit := work{lists: len(p.PutLists) + len(p.RemoveLists), entries: rows.entries, commits: 1, tracks: len(s.Tracks)}.cost()
	commitCost := work{commits: 1, tracks: len(s.Tracks)}.cost()
	cutRows := false
	for idx, key := range p.RemoveTracks {
		if w.rest != nil && idx&15 == 0 {
			if err = w.rest(0); err != nil {
				return err
			}
		}
		if w.ctx.Err() != nil || (!w.next && !w.st.must(listAndCommit)) {
			cutRows = true
			break
		}
		if t := s.Tracks[key]; t.EngineID != 0 {
			if err = b.DeleteTrack(w.ctx, t.EngineID); err != nil {
				return err
			}
			w.res.Removed++
		}
		delete(s.Tracks, key)
	}
	const moveChunk = 16
	moved := 0
	for at := 0; !cutRows && at < len(p.Moves); at += moveChunk {
		if w.rest != nil {
			if err = w.rest(0); err != nil {
				return err
			}
		}
		if w.ctx.Err() != nil || (!w.next && !w.st.must(listAndCommit)) {
			cutRows = true
			break
		}
		part := p.Moves[at:min(at+moveChunk, len(p.Moves))]
		moves := make(map[int64]string, len(part))
		for _, m := range part {
			moves[s.Tracks[m.Old].EngineID] = w.view.Tracks[m.New].Rel
		}
		if err = b.MoveTracks(w.ctx, moves); err != nil {
			return err
		}
		for _, m := range part {
			vt, t := &w.view.Tracks[m.New], s.Tracks[m.Old]
			delete(s.Tracks, m.Old)
			t.Key, t.Rel, t.AnalysisPath, t.ArtworkPath, t.Resolved, t.Refs, t.Row = vt.Key, vt.Rel, vt.Track.AnalysisPath, vt.Track.ArtworkPath, false, vt.Refs, "" // its fields follow in updateRows
			s.Tracks[vt.Key] = t
			p.UpdateTracks = append(p.UpdateTracks, m.New)
		}
		moved += len(part)
	}
	p.AddTracks = slices.DeleteFunc(p.AddTracks, func(at int) bool {
		row, ok := w.index[w.view.Tracks[at].Key]
		if ok {
			w.note(&w.view.Tracks[at], row.ID, true)
		}
		return ok
	})
	const chunk = 16
	addMargin := max(listAndCommit, 2*time.Second)
	for at := 0; !cutRows && at < len(p.AddTracks); at += chunk {
		if w.rest != nil {
			if err = w.rest(0); err != nil {
				return err
			}
		}
		if w.ctx.Err() != nil || (!w.next && !w.st.must(addMargin)) {
			log.Printf("rb-bridge: %d new tracks wait for the next window", len(p.AddTracks)-at)
			break
		}
		part := p.AddTracks[at:min(at+chunk, len(p.AddTracks))]
		ids, native, err := b.InsertTracks(w.ctx, w.rows(part))
		if err != nil {
			return err
		}
		for i, vi := range part {
			t := w.note(&w.view.Tracks[vi], ids[i], native[i])
			w.index[t.Key] = enginedb.Row{ID: ids[i], Path: "../" + t.Rel}
			if err = w.noteSync(b, t, true); err != nil {
				return err
			}
			if !native[i] {
				w.res.Added++
			}
		}
		if w.o.Progress != nil {
			w.o.Progress(at+len(part), len(p.AddTracks))
		}
	}
	if cutRows && w.res.Removed == 0 && w.res.Added == 0 && moved == 0 && !w.st.must(commitCost) {
		w.leave(rows, "the rows and lists")
		return nil
	}
	w.st.done("rows", "%d added, %d moved, %d removed", w.res.Added, moved, w.res.Removed)

	owned := make(map[int64]bool, len(s.Lists))
	for _, l := range s.Lists {
		owned[l.EngineID] = true
	}
	for _, key := range p.RemoveLists {
		if w.ctx.Err() != nil || (!w.next && !w.st.must(commitCost)) {
			return errWindowDeadline
		}
		if l := s.Lists[key]; l.EngineID != 0 {
			if err = b.DeleteList(w.ctx, l.EngineID, func(id int64) bool { return owned[id] }); err != nil {
				return err
			}
		}
		delete(s.Lists, key)
	}
	if err = w.putLists(b, p.PutLists); err != nil {
		return err
	}
	w.res.PlaylistsChanged = len(p.PutLists)+len(p.RemoveLists) > 0
	if w.ctx.Err() != nil || (!w.next && !w.st.must(0)) {
		return errWindowDeadline
	}
	listBegan := time.Now()
	if err = w.commitBatch(b); err != nil {
		return err
	}
	if w.rest != nil {
		if err = w.rest(time.Since(listBegan)); err != nil {
			return err
		}
	}
	w.st.done("lists", "%d written, %d removed; committed", len(p.PutLists), len(p.RemoveLists))
	return w.updateRows(p.UpdateTracks)
}

// inBatches calls each for items 0..n-1 while ok(i), at most size a transaction, and committed
// after each commit. It returns how many items it went through.
func (w *window) inBatches(n, size int, ok func(i int) bool, each func(b *enginedb.Batch, i int) error, committed func(done int) error) (int, error) {
	i := 0
	for i < n && ok(i) {
		if w.rest != nil {
			if err := w.rest(0); err != nil {
				return i, err
			}
		}
		b, err := w.lib.Begin(w.ctx)
		if err != nil {
			return i, err
		}
		for first := i; err == nil && i < n && i-first < size && ok(i); i++ {
			if w.rest != nil && i > first && (i-first)%32 == 0 {
				if err = w.rest(0); err != nil {
					break
				}
			}
			err = each(b, i)
		}
		began := time.Now()
		if err == nil {
			err = w.commitBatch(b)
		}
		if err != nil {
			b.Rollback()
			return i, err
		}
		if err = committed(i); err == nil && w.rest != nil {
			err = w.rest(time.Since(began))
		}
		if err != nil {
			return i, err
		}
	}
	return i, nil
}

// updateBatch is how many rows one transaction gives the export's fields again.
var updateBatch = 1000

func fitUpdateCount(all int, fits func(int) bool) int {
	n := all
	for n > 0 && !fits(n) {
		if n <= updateBatch {
			n /= 2
			continue
		}
		n -= updateBatch
	}
	return n
}

// updateRows gives rows their changed export fields after the rows and lists are committed, a
// batch a transaction (a take-over changes thousands): the first batch is owed, the rest optional.
func (w *window) updateRows(idx []int) error {
	cost := func(n int) work { return work{updates: n, commits: 1 + n/updateBatch, tracks: len(w.snap.Tracks)} }
	if all := len(idx); all > 0 && !w.st.fits(cost(all)) {
		room := nextRoom(w.dir, w.snap.LibraryUUID)
		if room == nil {
			w.leave(cost(all), "the rows' export fields")
			return nil
		}
		// The job cannot make a next library on this stick, so nothing would ever update these rows:
		// the batches that fit are done here, and the next windows go on.
		n := fitUpdateCount(all, func(n int) bool { return w.st.fits(cost(n)) })
		w.left, idx = true, idx[:n]
		log.Printf("rb-bridge: %v; %d of %d rows get the export's fields in this window", room, n, all)
	}
	differed := 0
	done, err := w.inBatches(len(idx), updateBatch, func(i int) bool {
		if i < updateBatch {
			return w.st.must(applyReserve)
		}
		return w.st.more(applyReserve)
	}, func(b *enginedb.Batch, i int) error {
		vt := &w.view.Tracks[idx[i]]
		t := w.snap.Tracks[vt.Key]
		changed, err := b.UpdateTrack(w.ctx, t.EngineID, enginedb.TrackRow{Rel: t.Rel, Track: vt.Track}, t.Rating)
		if err != nil {
			return err
		}
		if w.o.OnDemand && vt.Track.ArtworkPath != "" {
			if err = b.SetDirectCover(w.ctx, t.EngineID, rbstate.DirectArtworkHash(vt.Track.ArtworkPath)); err != nil {
				return err
			}
		}
		t.Row, t.Rating, t.Refs, t.AnalysisPath, t.ArtworkPath = vt.Hash, vt.Track.Rating, vt.Refs, vt.Track.AnalysisPath, vt.Track.ArtworkPath
		t.Duration, t.SampleRate = vt.Track.Duration, vt.Track.SampleRate
		if changed {
			differed++
		}
		return w.noteSync(b, t, changed)
	}, func(int) error { return nil })
	if w.res.Updated += done; len(idx) > 0 {
		w.st.done("update", "%d of %d rows got the export's fields (%d held others)", done, len(idx), differed)
	}
	return err
}

// applyStaged writes what the job staged, the tracks the DJ asked for first, a batch a
// transaction, while the window has optional time. A track without a row yet stays staged.
func (w *window) applyStaged() error {
	stage, err := rbstate.OpenStage(w.lib.UUID())
	if err != nil {
		log.Printf("rb-bridge: no stage on this deck: %v", err)
		return nil
	}
	defer stage.Close()
	entries, verified, err := stage.Entries()
	if err != nil {
		return err
	}
	if verified == w.view.Stamp { // the job compared every track of this export (of a partial one too)
		w.snap.Pending.Verify = false
	}
	var keys, stale, done []string
	for key, e := range entries {
		if vt, ok := w.view.Track(key); !ok || e.Sig.Rules != rbstate.Rules || e.Sig.Row != vt.Hash {
			stale = append(stale, key) // of another export or other rules
		} else if t := w.snap.Tracks[key]; t != nil && t.EngineID != 0 && t.Perf != rbstate.PerfNone && !e.Missing && t.Built.Equal(e.Sig) {
			// Applied: a next library kept the record for the deck's loads; the job drops it.
		} else {
			keys = append(keys, key)
		}
	}
	keys = w.inApplyOrder(keys)
	filled := stage.Filled()
	overviews := map[int64][]byte{} // written once their rows are committed
	back := map[string]bool{}       // tracks whose file is on the stick again
	applied, kept, degraded, gone, dropped := 0, 0, 0, 0, 0
	_, err = w.inBatches(len(keys), applyBatch, func(int) bool { return w.st.more(applyReserve) }, func(b *enginedb.Batch, i int) error {
		key := keys[i]
		e, t := entries[key], w.snap.Tracks[key]
		vt, _ := w.view.Track(key)
		row := w.index[key]
		at := time.Now()
		defer lap(&w.took.rows, &at)
		if t != nil && t.EngineID == 0 && t.Perf == rbstate.PerfMissing && !e.Missing {
			// The file is back: the row the diff no longer asks for comes with its data.
			ids, native, err := b.InsertTracks(w.ctx, []enginedb.TrackRow{{Rel: vt.Rel, Track: vt.Track}})
			if err != nil {
				return err
			}
			t = w.note(vt, ids[0], native[0])
			w.index[key] = enginedb.Row{ID: ids[0], Path: "../" + vt.Rel}
			w.res.Added++
			back[key] = true
		}
		switch {
		case t != nil && t.Native:
			done = append(done, key)
			return nil
		case e.Missing: // no row until the file is back
			if t != nil && t.EngineID != 0 {
				if err := b.DeleteTrack(w.ctx, t.EngineID); err != nil {
					return err
				}
				gone++
			}
			w.snap.Tracks[key] = &rbstate.Track{Key: key, Rel: vt.Rel, AnalysisPath: vt.Track.AnalysisPath, ArtworkPath: vt.Track.ArtworkPath, Refs: vt.Refs, Row: vt.Hash, Perf: rbstate.PerfMissing, Built: e.Sig}
			done = append(done, key)
			return nil
		case t == nil || t.EngineID == 0:
			return nil
		}
		full, err := stage.Read(e)
		lap(&w.took.read, &at)
		if errors.Is(err, os.ErrNotExist) {
			stale = append(stale, key) // its files are not whole: the job stages it again
			return nil
		} else if err != nil {
			return err
		}
		if full.Rel != "" && full.Rel != t.Rel && rbstate.FoldPath(full.Rel) == key {
			if err = b.MoveTracks(w.ctx, map[int64]string{t.EngineID: full.Rel}); err != nil {
				return err
			}
			t.Rel = full.Rel
		}
		t.Resolved, t.Built = true, e.Sig
		if e.Degraded != "" {
			if t.Perf != rbstate.PerfDeck {
				t.Perf = rbstate.PerfDegraded
			}
			if t.Perf == rbstate.PerfDeck || row.Edit != t.Edit {
				t.Synced = nil
			}
			degraded++
		} else {
			p := &full.Perf
			perf := enginedb.Performance{TrackData: p.TrackData, Overview: p.Overview, Beats: p.Beats, QuickCues: p.QuickCues, Loops: p.Loops, ActiveLoops: p.ActiveLoops, Complete: p.Complete, Digest: e.Digest}
			prev := enginedb.PerfPrev{State: t.Perf, Written: t.Written, FastKnown: t.FastKnown, FastCues: t.FastCues, FastLoops: t.FastLoops, FastActive: t.FastActive}
			if f, ok := filled[key]; ok && f.EngineID == t.EngineID && prev.Written == nil && prev.State != rbstate.PerfDeck {
				prev.Written = f.Written // the UI hook filled the row from the stage: the bridge's data, as if a window had written it
			}
			digest, outcome, err := b.ApplyPerformance(w.ctx, t.EngineID, perf, full.Facts, prev, vt.Track)
			if err != nil {
				return err
			}
			if t.Written, t.Perf = digest, rbstate.PerfApplied; outcome == enginedb.PerfKept || outcome == enginedb.PerfMerged {
				t.Perf = rbstate.PerfDeck
				kept++
			}
			t.FastCues, t.FastLoops, t.FastActive, t.FastKnown = nil, nil, 0, false
			// A list waveform only for a row the stick's library has, and not when the job staged the
			// track with this row (the line names it): it wrote the file then. The look for it alone
			// reads the folder, which holds a file a track (20,000: 4.3 s of a window for 30 tracks).
			if len(full.RGB) > 0 && !w.next && e.EngineID != t.EngineID {
				overviews[t.EngineID] = full.RGB
			}
		}
		if len(full.Art) > 0 {
			if err = b.PutArtwork(w.ctx, t.EngineID, full.Art); err != nil {
				log.Printf("rb-bridge: cover of track %d not written: %v", t.EngineID, err)
			}
		}
		applied++
		done = append(done, key)
		if e.Degraded != "" {
			return w.noteSync(b, t, false)
		}
		return w.noteSync(b, t, true)
	}, func(at int) error {
		t := time.Now()
		for id, body := range overviews {
			if w.ctx.Err() != nil || (!w.next && !w.st.more(0)) {
				break
			}
			writeOverview(w.dir, w.lib.UUID(), id, body)
		}
		clear(overviews)
		lap(&w.took.rgb, &t)
		var err error
		if !w.keep {
			err = stage.Drop(done...)
			dropped += len(done)
		}
		lap(&w.took.drop, &t)
		done = done[:0]
		if w.o.Progress != nil {
			w.o.Progress(at, len(keys))
		}
		return err
	})
	if err == nil && len(back) > 0 {
		err = w.relist(back)
	}
	if err != nil {
		return err
	}
	_ = stage.Drop(stale...)
	if len(entries) > 0 && dropped+len(stale) == len(entries) {
		_ = stage.Reset(verified) // everything staged is applied or of another export: the index starts again
	}
	w.res.Applied = applied
	w.res.Removed += gone
	w.st.done("staged", "%d applied (%d kept the deck's edits, %d without analysis), %d rows of missing files removed, %d of %d staged left", applied, kept, degraded, gone, max(len(keys)-applied-gone, 0), len(keys))
	if k := &w.took; applied > 0 {
		ms := func(d time.Duration) int64 { return d.Milliseconds() }
		log.Printf("rb-bridge: mount staged parts: stage read %d ms, rows %d ms, list waveforms %d ms, stage drop %d ms; all commits of the window: snapshot %d ms, commit %d ms, promote %d ms",
			ms(k.read), ms(k.rows), ms(k.rgb), ms(k.drop), ms(k.save), ms(k.commit), ms(k.promote))
	}
	return nil
}

// relist writes again the lists that hold a track whose row has just come back.
func (w *window) relist(back map[string]bool) error {
	if w.ctx.Err() != nil || (!w.next && !w.st.more(applyReserve)) {
		w.snap.Pending.Lists = true
		return nil
	}
	var idx []int
	for i := range w.view.Lists {
		l := &w.view.Lists[i]
		if sl := w.snap.Lists[l.Key]; sl != nil && slices.ContainsFunc(l.Tracks, func(k string) bool { return back[k] }) {
			sl.Hash, idx = "partial", append(idx, i)
		}
	}
	b, err := w.lib.Begin(w.ctx)
	if err != nil {
		return err
	}
	began := time.Now()
	if err = w.putLists(b, idx); err == nil {
		err = w.commitBatch(b)
	}
	if err != nil {
		b.Rollback()
	} else if w.rest != nil {
		err = w.rest(time.Since(began))
	}
	return err
}

// writeOverview puts a track's list waveform where Engine reads it unless one is there; it is not
// read (on a USB 2.0 stick that was half a window's time).
func writeOverview(dir, uuid string, id int64, body []byte) {
	if st, err := os.Stat(filepath.Join(dir, "OverviewData", uuid, fmt.Sprintf("%d.rgb", id))); err == nil && st.Size() > 4 {
		if body, err := os.ReadFile(filepath.Join(dir, "OverviewData", uuid, fmt.Sprintf("%d.rgb", id))); err == nil {
			if raw, err := enginedb.QUncompress(body, 1<<20); err == nil && enginedb.ValidRGBOverview(raw) {
				return
			}
		}
	}
	if err := enginedb.WriteRGBOverviewAtomic(dir, uuid, id, body); err != nil {
		log.Printf("rb-bridge: list waveform of track %d not written: %v", id, err)
	}
}

var (
	legacyArtworkWindowMin = 500 * time.Millisecond
	legacyArtworkWindowMax = time.Second
	artworkProcDir         = "/proc"
	artworkCopiesDir       = "/data/inMusicTempFiles/"
	deckBusyForArtwork     = isDeckPlayingOrLoading
)

// isDeckPlayingOrLoading checks deck playback/cue/scratch/loading flags in the runtime
// activity file and active Engine track copies under /data/inMusicTempFiles/.
func isDeckPlayingOrLoading() bool {
	if IsDeckActive() {
		return true
	}
	var act struct {
		Active      bool  `json:"active"`
		Loading     bool  `json:"loading"`
		UpdatedAtMS int64 `json:"updatedAtMs"`
	}
	if readPrivateJSON(deckActivityPath(), &act) == nil && (act.Active || act.Loading) && act.UpdatedAtMS > 0 {
		age := time.Since(time.UnixMilli(act.UpdatedAtMS))
		if age >= -time.Second && age <= deckActivityTTL {
			return true
		}
	}
	return isEngineCopyingForArtwork()
}

func isEngineCopyingForArtwork() bool {
	if _, err := os.Stat(artworkCopiesDir); err != nil {
		return false
	}
	entries, err := os.ReadDir(artworkProcDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(artworkProcDir, e.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "Engine" {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(artworkProcDir, e.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(artworkProcDir, e.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(target, artworkCopiesDir) {
				continue
			}
			info, err := os.ReadFile(filepath.Join(artworkProcDir, e.Name(), "fdinfo", fd.Name()))
			if err != nil {
				return true
			}
			for _, line := range strings.Split(string(info), "\n") {
				if v, ok := strings.CutPrefix(line, "flags:"); ok {
					if flags, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32); err != nil || flags&3 != 0 {
						return true
					}
				}
			}
		}
	}
	return false
}

// migrateLegacyArtwork converts legacy TEXT AlbumArt hashes under window pacing and deck-activity
// guards when an adopted older library still holds legacy TEXT covers. In a hook mount window it
// runs only after required work when well inside the 3 s spare margin; idle mounts or background
// next-library passes finish any deferred rows with job pacing and clear LegacyArtwork once done.
func (w *window) migrateLegacyArtwork() error {
	if w.lib == nil || w.built || !w.snap.LegacyArtwork || deckBusyForArtwork() {
		return nil
	}
	opts := enginedb.LegacyArtworkOptions{
		Busy: deckBusyForArtwork,
		Pace: w.rest,
	}
	if !w.next && !w.st.kill.IsZero() {
		if w.left || !w.st.more(legacyArtworkWindowMin) {
			return nil
		}
		budget := legacyArtworkWindowMax
		if !w.st.optional.IsZero() {
			budget = min(budget, time.Until(w.st.optional))
		}
		budget = min(budget, time.Until(w.st.kill.Add(-spare)))
		if budget <= 0 {
			return nil
		}
		opts.Budget = budget
	}
	if err := w.lib.MigrateLegacyArtworkWithOptions(w.ctx, opts); err != nil {
		return err
	}
	if deckBusyForArtwork() {
		return nil
	}
	more, err := w.lib.HasLegacyArtwork(w.ctx)
	if err != nil {
		return err
	}
	if !more {
		w.snap.LegacyArtwork, w.dirty = false, true
		if w.next || w.st.kill.IsZero() || w.st.fits(work{commits: 1, tracks: len(w.snap.Tracks)}) {
			return w.saveIfChanged()
		}
	}
	return nil
}
