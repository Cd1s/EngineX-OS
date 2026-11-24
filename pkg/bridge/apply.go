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
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// window is one mount's work on the library.
type window struct {
	ctx      context.Context
	mount    string
	dir      string // Engine Library/Database2
	view     *View
	st       *steps
	o        MountOptions
	res      *MountResult
	partial  bool // a library was unreadable: nothing is removed and the stamp is not kept
	linkOnly bool // only staged CDJ libraries were read: the stick's own import stays

	lib   *enginedb.Library // nil: the stick has no library to keep, one is built
	snap  *rbstate.Snapshot
	index map[string]enginedb.Row // every row of the library by key
	head  rbstate.Header          // the snapshot's header as the stick has it
	dirty bool                    // tracks or lists changed outside a commit
}

// cut, in tests, is called between two writes that a power cut or the hook's kill can separate.
var cut = func(step string) {}

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
	if w.snap.Stamp != w.view.Stamp { // the job compares every track's files with what was built
		w.snap.Pending.Verify, w.dirty = true, true
	}
	if w.lib == nil {
		err = w.build()
	} else {
		err = w.apply(Diff(w.view, w.snap))
	}
	if err == nil {
		err = w.applyStaged()
	}
	if err == nil {
		err = w.saveIfChanged()
	}
	return err
}

// open opens the stick's library and its snapshot. w.lib stays nil when there is no library to
// keep: no m.db, or Engine's empty skeleton (one with rows Engine added is a library).
func (w *window) open() error {
	db := filepath.Join(w.dir, "m.db")
	if enginedb.OpenElsewhere(db) != 0 {
		return enginedb.ErrLibraryInUse // Engine would go on writing a file that is no longer the stick's
	}
	w.snap = rbstate.New("")
	if st, err := os.Stat(db); err != nil || st.Size() == 0 {
		return nil
	}
	lib, err := enginedb.OpenLibrary(w.ctx, w.dir)
	if err != nil {
		return err
	}
	w.lib = lib
	if err = w.settle(); err != nil {
		return err
	}
	snap, err := rbstate.Load(w.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, oldErr := os.Stat(filepath.Join(w.dir, oldStateName))
	switch {
	case snap != nil && snap.LibraryUUID == lib.UUID() && snap.Rules == rbstate.Rules:
		w.snap, w.head = snap, snap.Header
	case oldErr == nil:
		if w.snap, err = adopt(w.ctx, w.dir, lib.UUID()); err != nil {
			return err
		}
		log.Printf("rb-bridge: library of an earlier build taken over: %d tracks, %d lists", len(w.snap.Tracks), len(w.snap.Lists))
	default:
		tracks, lists, err := lib.Counts(w.ctx)
		if err != nil {
			return err
		}
		if w.snap = rbstate.New(lib.UUID()); tracks == 0 && lists == 0 {
			w.close() // Engine's skeleton: a library is built under its UUID
			return nil
		}
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
	claims, err := rbstate.LoadNext(w.dir)
	if err != nil {
		return nil
	}
	id, err := w.lib.CommitID(w.ctx)
	if err != nil {
		return err
	}
	if claims.LibraryUUID != w.lib.UUID() || claims.Commit != id || claims.Rules != rbstate.Rules {
		return rbstate.DropNext(w.dir)
	}
	if err = rbstate.Promote(w.dir); err != nil {
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
	for key, t := range w.snap.Tracks {
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
	for _, l := range w.snap.Lists {
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
		rows[i] = enginedb.TrackRow{Rel: w.view.Tracks[at].Rel, Track: w.view.Tracks[at].Track}
	}
	return rows
}

func (w *window) note(vt *ViewTrack, id int64, native bool) *rbstate.Track {
	t := &rbstate.Track{Key: vt.Key, EngineID: id, Rel: vt.Rel, Refs: vt.Refs, Row: vt.Hash, Rating: vt.Track.Rating, Native: native}
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
	w.count()
	w.snap.Applied, w.snap.Commit = time.Now().Unix(), id
	if err := w.snap.SaveNext(w.dir); err != nil {
		return err
	}
	cut("next saved")
	if err := do(); err != nil {
		_ = rbstate.DropNext(w.dir)
		return err
	}
	cut("committed")
	if err := rbstate.Promote(w.dir); err != nil {
		return err
	}
	cut("promoted")
	w.saved()
	return nil
}

func (w *window) commitBatch(b *enginedb.Batch) error {
	return w.commit(b.CommitID(), func() error { return b.Commit(w.ctx) })
}

// saved notes that the stick has the snapshot in memory; an older build's state then goes.
func (w *window) saved() {
	w.head, w.dirty = w.snap.Header, false
	dropOldState(w.dir)
}

// saveIfChanged saves the snapshot when it changed without a commit.
func (w *window) saveIfChanged() error {
	w.count()
	if !w.dirty && reflect.DeepEqual(w.snap.Header, w.head) {
		return nil
	}
	if err := w.snap.Save(w.dir); err != nil {
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
	p := rbstate.Pending{Verify: s.Pending.Verify}
	for i := range w.view.Tracks {
		vt := &w.view.Tracks[i]
		t := s.Tracks[vt.Key]
		if t != nil {
			t.Refs = vt.Refs // this export's IDs: a re-export renumbers them, and write-back finds rows by them
		}
		switch {
		case t != nil && (t.Native || t.Perf == rbstate.PerfMissing):
		case t == nil || t.EngineID == 0 || t.Row != vt.Hash:
			p.Rows++
		case t.Perf == rbstate.PerfNone:
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
	if err = w.commit(commitID, func() error { return rbstate.InstallFile(filepath.Join(tmp, "m.db"), filepath.Join(w.dir, "m.db")) }); err != nil {
		return err
	}
	w.st.done("install", "library copied to the stick")
	w.res.Added, w.res.PlaylistsChanged = len(ids), len(w.view.Lists) > 0
	if w.o.Progress != nil {
		w.o.Progress(len(ids), len(ids))
	}
	if w.lib, err = enginedb.OpenLibrary(w.ctx, w.dir); err != nil {
		return err
	}
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
	for _, at := range idx {
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
			} else {
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
	if w.partial || w.linkOnly { // nothing is removed of a library that was not read
		link := func(r rbstate.SourceRef) bool { return r.Kind == rbstate.LinkLibrary }
		p.RemoveTracks = slices.DeleteFunc(p.RemoveTracks, func(k string) bool { return w.partial || !slices.ContainsFunc(s.Tracks[k].Refs, link) })
		p.RemoveLists = slices.DeleteFunc(p.RemoveLists, func(k string) bool { return w.partial || !strings.HasPrefix(k, viewLink) })
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
	if len(p.AddTracks)+len(p.Moves)+len(p.RemoveTracks)+len(p.PutLists)+len(p.RemoveLists) == 0 {
		return w.updateRows(p.UpdateTracks)
	}
	b, err := w.lib.Begin(w.ctx)
	if err != nil {
		return err
	}
	defer b.Rollback()
	for _, key := range p.RemoveTracks {
		if t := s.Tracks[key]; t.EngineID != 0 {
			if err = b.DeleteTrack(w.ctx, t.EngineID); err != nil {
				return err
			}
			w.res.Removed++
		}
		delete(s.Tracks, key)
	}
	moves := make(map[int64]string, len(p.Moves))
	for _, m := range p.Moves {
		moves[s.Tracks[m.Old].EngineID] = w.view.Tracks[m.New].Rel
	}
	if err = b.MoveTracks(w.ctx, moves); err != nil {
		return err
	}
	for _, m := range p.Moves {
		vt, t := &w.view.Tracks[m.New], s.Tracks[m.Old]
		delete(s.Tracks, m.Old)
		t.Key, t.Rel, t.Resolved, t.Refs, t.Row = vt.Key, vt.Rel, false, vt.Refs, "" // its fields follow in updateRows
		s.Tracks[vt.Key] = t
		p.UpdateTracks = append(p.UpdateTracks, m.New)
	}
	p.AddTracks = slices.DeleteFunc(p.AddTracks, func(at int) bool {
		row, ok := w.index[w.view.Tracks[at].Key]
		if ok {
			w.note(&w.view.Tracks[at], row.ID, true)
		}
		return ok
	})
	const chunk = 100
	for at := 0; at < len(p.AddTracks); at += chunk {
		if !w.st.must(2 * time.Second) {
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
	w.st.done("rows", "%d added, %d moved, %d removed", w.res.Added, len(p.Moves), w.res.Removed)

	owned := make(map[int64]bool, len(s.Lists))
	for _, l := range s.Lists {
		owned[l.EngineID] = true
	}
	for _, key := range p.RemoveLists {
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
	if err = w.commitBatch(b); err != nil {
		return err
	}
	w.st.done("lists", "%d written, %d removed; committed", len(p.PutLists), len(p.RemoveLists))
	return w.updateRows(p.UpdateTracks)
}

// inBatches calls each for items 0..n-1 while ok(i), at most size a transaction, and committed
// after each commit. It returns how many items it went through.
func (w *window) inBatches(n, size int, ok func(i int) bool, each func(b *enginedb.Batch, i int) error, committed func(done int) error) (int, error) {
	i := 0
	for i < n && ok(i) {
		b, err := w.lib.Begin(w.ctx)
		if err != nil {
			return i, err
		}
		for first := i; err == nil && i < n && i-first < size && ok(i); i++ {
			err = each(b, i)
		}
		if err == nil {
			err = w.commitBatch(b)
		}
		if err != nil {
			b.Rollback()
			return i, err
		}
		if err = committed(i); err != nil {
			return i, err
		}
	}
	return i, nil
}

// updateBatch is how many rows one transaction gives the export's fields again.
var updateBatch = 1000

// updateRows gives rows their changed export fields after the rows and lists are committed, a
// batch a transaction (a take-over changes thousands): the first batch is owed, the rest optional.
func (w *window) updateRows(idx []int) error {
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
		t.Row, t.Rating, t.Refs = vt.Hash, vt.Track.Rating, vt.Refs
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
		} else {
			keys = append(keys, key)
		}
	}
	keys = w.inApplyOrder(keys)
	overviews := map[int64][]byte{} // written once their rows are committed
	applied, kept, degraded, gone, dropped := 0, 0, 0, 0, 0
	_, err = w.inBatches(len(keys), applyBatch, func(int) bool { return w.st.more(applyReserve) }, func(b *enginedb.Batch, i int) error {
		key := keys[i]
		e, t := entries[key], w.snap.Tracks[key]
		vt, _ := w.view.Track(key)
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
			w.snap.Tracks[key] = &rbstate.Track{Key: key, Rel: vt.Rel, Refs: vt.Refs, Row: vt.Hash, Perf: rbstate.PerfMissing, Built: e.Sig}
			done = append(done, key)
			return nil
		case t == nil || t.EngineID == 0:
			return nil
		}
		full, err := stage.Read(e)
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
			t.Perf = rbstate.PerfDegraded
			degraded++
		} else {
			p := &full.Perf
			perf := enginedb.Performance{TrackData: p.TrackData, Overview: p.Overview, Beats: p.Beats, QuickCues: p.QuickCues, Loops: p.Loops, ActiveLoops: p.ActiveLoops, Complete: p.Complete, Digest: e.Digest}
			digest, outcome, err := b.ApplyPerformance(w.ctx, t.EngineID, perf, full.Facts, enginedb.PerfPrev{State: t.Perf, Written: t.Written}, vt.Track)
			if err != nil {
				return err
			}
			if t.Written, t.Perf = digest, rbstate.PerfApplied; outcome == enginedb.PerfKept || outcome == enginedb.PerfMerged {
				t.Perf = rbstate.PerfDeck
				kept++
			}
			if len(full.RGB) > 0 {
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
		return w.noteSync(b, t, true)
	}, func(at int) error {
		for id, body := range overviews {
			writeOverview(w.dir, w.lib.UUID(), id, body)
		}
		clear(overviews)
		err := stage.Drop(done...)
		dropped, done = dropped+len(done), done[:0]
		if w.o.Progress != nil {
			w.o.Progress(at, len(keys))
		}
		return err
	})
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
	return nil
}

// writeOverview puts a track's list waveform where Engine reads it unless one is there; it is not
// read (on a USB 2.0 stick that was half a window's time).
func writeOverview(dir, uuid string, id int64, body []byte) {
	if st, err := os.Stat(filepath.Join(dir, "OverviewData", uuid, fmt.Sprintf("%d.rgb", id))); err == nil && st.Size() > 4 {
		return
	}
	if err := enginedb.WriteRGBOverviewAtomic(dir, uuid, id, body); err != nil {
		log.Printf("rb-bridge: list waveform of track %d not written: %v", id, err)
	}
}
