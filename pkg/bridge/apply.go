package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// window is one mount's work on the library.
type window struct {
	ctx      context.Context
	mount    string
	dir      string // Engine Library/Database2
	r        *compat.Resolver
	view     *View
	st       *steps
	o        MountOptions
	res      *MountResult
	partial  bool // a library could not be read: nothing is removed and the stamp is not kept
	linkOnly bool // the stick has only staged CDJ libraries now: its own earlier import stays

	lib     *enginedb.Library // nil: the stick has no library to keep, one is built
	uuid    string            // of Engine's empty skeleton, which the built library takes over
	snap    *rbstate.Snapshot
	index   map[string]enginedb.Row // every row of the library by key
	head    rbstate.Header          // the snapshot's header as the stick has it
	dirty   bool                    // the snapshot's tracks or lists changed outside a commit
	adopted bool                    // snap was read from an earlier build's state, which goes once it is saved
}

func (w *window) close() {
	if w.lib != nil {
		w.lib.Close()
	}
}

const oldStateName = ".rb-bridge-state.db" // the state file of builds before the snapshot

// open opens the stick's library and the snapshot of what the bridge has in it. w.lib stays nil
// when there is no library to keep: no m.db, or the empty skeleton Engine makes when it sees a
// stick. A skeleton with rows (Engine adds one for each track the DJ loads while it shows the
// stick's rekordbox library itself) is a library: its rows stay Engine's, the export is written
// into it in place.
func (w *window) open() error {
	// Never under a process that has the file open, whatever is in it: Engine would go on
	// writing a file that is no longer the stick's.
	if enginedb.OpenElsewhere(filepath.Join(w.dir, "m.db")) != 0 {
		return enginedb.ErrLibraryInUse
	}
	snap, err := rbstate.Load(w.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	st, statErr := os.Stat(filepath.Join(w.dir, "m.db"))
	if statErr != nil || st.Size() == 0 {
		w.snap = nil
		return nil
	}
	lib, err := enginedb.OpenLibrary(w.ctx, w.dir)
	if err != nil {
		return err
	}
	if snap != nil && (snap.LibraryUUID != lib.UUID() || snap.Rules != rbstate.Rules) {
		snap = nil // another library took the file's place: what the snapshot says is not about it
	}
	claims, claimErr := rbstate.LoadNext(w.dir)
	if snap == nil && claimErr != nil {
		tracks, lists, err := lib.Counts(w.ctx)
		if err != nil {
			lib.Close()
			return err
		}
		_, oldErr := os.Stat(filepath.Join(w.dir, oldStateName))
		if tracks == 0 && lists == 0 && errors.Is(oldErr, os.ErrNotExist) {
			w.uuid = lib.UUID()
			return lib.Close()
		}
		if oldErr == nil {
			if snap, err = adopt(w.ctx, w.dir, lib.UUID()); err != nil {
				lib.Close()
				return err
			}
			w.adopted, w.dirty = true, true
			log.Printf("rb-bridge: library of an earlier build taken over: %d tracks, %d lists", len(snap.Tracks), len(snap.Lists))
		}
	}
	if snap == nil {
		snap = rbstate.New(lib.UUID())
	} else if !w.adopted {
		w.head = snap.Header
		dropOldState(w.dir) // an adoption whose clean-up was cut short
	}
	w.lib, w.snap = lib, snap
	if claimErr == nil {
		if err = w.settle(claims); err != nil {
			return err
		}
	}
	if err = w.reconcile(); err != nil {
		return err
	}
	w.st.done("snapshot", "%d tracks, %d lists", len(w.snap.Tracks), len(w.snap.Lists))
	return nil
}

// settle finishes a window that was cut around its commit: the snapshot it had written for
// after the commit is the snapshot if the library has that commit, and is dropped if not.
func (w *window) settle(claims *rbstate.Snapshot) error {
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
	w.snap, w.head, w.dirty = claims, claims.Header, false
	log.Printf("rb-bridge: the last window was cut short after its commit; its %d tracks and %d lists are taken over", len(claims.Tracks), len(claims.Lists))
	return nil
}

// reconcile drops from the snapshot what Engine has removed from the library since, and takes
// the spelling of a path another part of EngineX gave a row (#322 writes a path as the volume
// spells it). It leaves w.index: every row of the library by key.
func (w *window) reconcile() error {
	index, err := w.lib.TrackIndex(w.ctx)
	if err != nil {
		return err
	}
	lists, err := w.lib.ListIDs(w.ctx)
	if err != nil {
		return err
	}
	w.index = index
	gone := 0
	for key, t := range w.snap.Tracks {
		if t.EngineID == 0 {
			continue
		}
		row, ok := index[key]
		if !ok || row.ID != t.EngineID {
			delete(w.snap.Tracks, key)
			gone++
			continue
		}
		if rel := strings.TrimPrefix(row.Path, "../"); rel != t.Rel {
			t.Rel, t.Resolved, w.dirty = rel, true, true
		}
	}
	for _, l := range w.snap.Lists {
		if l.EngineID != 0 && !lists[l.EngineID] {
			// Still known as the bridge's: when Engine has made the list again under a new ID,
			// putLists takes that one back instead of adding a second.
			l.EngineID, l.Hash = 0, ""
			gone++
		}
	}
	if gone > 0 {
		w.dirty = true
		log.Printf("rb-bridge: %d rows of the last import are no longer in the library", gone)
	}
	return nil
}

// rows are the view's tracks at idx as the library takes them.
func (w *window) rows(idx []int) []enginedb.TrackRow {
	rows := make([]enginedb.TrackRow, len(idx))
	for i, at := range idx {
		rows[i] = enginedb.TrackRow{Rel: w.view.Tracks[at].Rel, Track: w.view.Tracks[at].Track}
	}
	return rows
}

func (w *window) note(vt *ViewTrack, id int64, native bool) {
	w.snap.Tracks[vt.Key] = &rbstate.Track{Key: vt.Key, EngineID: id, Rel: vt.Rel, Refs: vt.Refs, Row: vt.Hash, Rating: vt.Track.Rating, Native: native}
}

// noteSync records a row's lastEditTime after the window wrote it, and with it the digest
// cue write-back compares by when the stick's export holds what the row holds now: nobody but
// the bridge has written the row since it was last noted, and nothing of the deck's was kept in
// it. Write-back then never reads the row. Without the digest it reads the row and compares it
// with the export.
func (w *window) noteSync(b *enginedb.Batch, t *rbstate.Track) error {
	if t.Native {
		return nil
	}
	row := w.index[t.Key]
	insync := row.Edit == t.Edit && t.Perf != rbstate.PerfDeck
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

// cut, in tests, is called between two writes that a power cut or the hook's kill can separate.
var cut = func(step string) {}

// commit writes the snapshot a transaction leads to beside the present one, commits, and makes
// it the snapshot.
func (w *window) commit(b *enginedb.Batch) error {
	w.count()
	w.snap.Applied, w.snap.Commit = time.Now().Unix(), b.CommitID()
	if err := w.snap.SaveNext(w.dir); err != nil {
		b.Rollback()
		return err
	}
	cut("next saved")
	if err := b.Commit(w.ctx); err != nil {
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

// saved notes that the stick has the snapshot as it is in memory.
func (w *window) saved() {
	w.head, w.dirty = w.snap.Header, false
	if w.adopted {
		dropOldState(w.dir)
		w.adopted = false
	}
}

// count fills the snapshot's header from its rows.
func (w *window) count() {
	s := w.snap
	s.Format, s.Rules, s.Header.Tracks, s.Header.Lists = rbstate.Format, rbstate.Rules, len(s.Tracks), len(s.Lists)
	s.Layout, s.Sources = w.view.Layout, w.view.Sources
	if w.partial {
		s.Stamp = "" // a library was unreadable: the next insert reads again, and write-back waits
	} else {
		s.Stamp = w.view.Stamp
	}
	p := rbstate.Pending{Verify: s.Pending.Verify}
	for i := range w.view.Tracks {
		vt := &w.view.Tracks[i]
		t := s.Tracks[vt.Key]
		switch {
		case t == nil:
			p.Rows++
		case t.Native || t.Perf == rbstate.PerfMissing:
		case t.EngineID == 0 || t.Row != vt.Hash:
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

// build makes the library of a stick that has none: built on the deck's own disk from the
// export alone, then copied to the stick and put in place with one rename.
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
	idx := make([]int, len(w.view.Tracks))
	for i := range idx {
		idx[i] = i
	}
	var listIDs map[string]int64
	ids, uuid, commitID, err := enginedb.BuildLibrary(w.ctx, tmp, w.uuid, w.rows(idx), func(b *enginedb.Batch, ids []int64) error {
		byKey := make(map[string]int64, len(ids))
		for i, id := range ids {
			byKey[w.view.Tracks[i].Key] = id
		}
		var e error
		idx := make([]int, len(w.view.Lists))
		for i := range idx {
			idx[i] = i
		}
		listIDs, e = putLists(w.ctx, b, w.view, idx, nil, byKey)
		return e
	})
	if err != nil {
		return err
	}
	w.st.done("build", "%d rows, %d lists, on the deck's disk", len(ids), len(listIDs))
	w.snap = rbstate.New(uuid)
	for i := range w.view.Tracks {
		w.note(&w.view.Tracks[i], ids[i], false)
	}
	for i := range w.view.Lists {
		l := &w.view.Lists[i]
		w.snap.Lists[l.Key] = &rbstate.List{Key: l.Key, EngineID: listIDs[l.Key], Hash: l.Hash}
	}
	w.snap.Pending.Verify = true
	w.count()
	w.snap.Applied, w.snap.Commit = time.Now().Unix(), commitID
	if err = w.snap.SaveNext(w.dir); err != nil {
		return err
	}
	cut("next saved")
	if err = rbstate.InstallFile(filepath.Join(tmp, "m.db"), filepath.Join(w.dir, "m.db")); err != nil {
		return err
	}
	cut("committed")
	if err = rbstate.Promote(w.dir); err != nil {
		return err
	}
	cut("promoted")
	w.saved()
	w.st.done("install", "library copied to the stick")
	w.res.Added, w.res.PlaylistsChanged = len(ids), len(listIDs) > 0
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

// putLists writes the view's lists at idx: new ones are added, changed ones get their entries
// and their place again. known are the lists' Playlist.id so far; the result adds the new ones.
// A list's entries are the tracks that have a row; one that lacks a row is left out for now.
func putLists(ctx context.Context, b *enginedb.Batch, v *View, idx []int, known map[string]*rbstate.List, tracks map[string]int64) (map[string]int64, error) {
	ids := make(map[string]int64, len(v.Lists))
	for key, l := range known {
		ids[key] = l.EngineID
	}
	reorder := map[string]bool{}
	for _, at := range idx {
		l := &v.Lists[at]
		parent := ids[l.Parent]
		if l.Parent != "" && parent == 0 {
			return nil, fmt.Errorf("list %d has no folder in the library", at)
		}
		head, entries, _ := strings.Cut(l.Hash, ":")
		old := known[l.Key]
		id := int64(0)
		if old != nil {
			id = old.EngineID
		}
		if id == 0 {
			var err error
			if old != nil { // Engine removed the bridge's list: the one it made in its place, if any
				if id, err = b.FindList(ctx, l.Name, parent); err != nil {
					return nil, err
				}
			}
			if id == 0 {
				if id, err = b.InsertList(ctx, enginedb.ListRow{Title: l.Name, Parent: parent}); err != nil {
					return nil, err
				}
			}
			ids[l.Key] = id
			old = nil
		}
		oldHead, oldEntries := "", ""
		if old != nil {
			oldHead, oldEntries, _ = strings.Cut(old.Hash, ":")
		}
		if old == nil || oldHead != head {
			reorder[l.Parent] = true
		}
		if !l.Folder && (old == nil || oldEntries != entries) {
			members := make([]int64, 0, len(l.Tracks))
			for _, key := range l.Tracks {
				if id := tracks[key]; id != 0 {
					members = append(members, id)
				}
			}
			if err := b.SetEntries(ctx, id, members); err != nil {
				return nil, err
			}
		}
	}
	for parent := range reorder {
		var order []int64
		for i := range v.Lists {
			if v.Lists[i].Parent == parent {
				order = append(order, ids[v.Lists[i].Key])
			}
		}
		if err := b.OrderLists(ctx, ids[parent], order); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// apply writes a plan to the library in one transaction: rows the export no longer has go, rows
// whose file moved follow it, changed rows get their fields, new tracks get rows, and the lists
// that changed are written. Rows are added only while the window has time; the rest is pending.
func (w *window) apply(p *Plan) error {
	if w.partial || w.linkOnly {
		p.RemoveTracks = keepOwn(p.RemoveTracks, w.snap, w.partial)
		p.RemoveLists = keepOwnLists(p.RemoveLists, w.partial)
	}
	w.st.done("diff", "+%d ~%d >%d -%d tracks, %d lists to write, %d to remove", len(p.AddTracks), len(p.UpdateTracks), len(p.Moves), len(p.RemoveTracks), len(p.PutLists), len(p.RemoveLists))
	if w.snap.Stamp != w.view.Stamp {
		w.snap.Pending.Verify, w.dirty = true, true // the job compares every track's files with what was built
		// The export's own IDs of every track, as this export has them: a re-export renumbers them,
		// and write-back finds a track's rows in the libraries through them.
		for i := range w.view.Tracks {
			if t := w.snap.Tracks[w.view.Tracks[i].Key]; t != nil {
				t.Refs = w.view.Tracks[i].Refs
			}
		}
	}
	if len(p.AddTracks)+len(p.Moves)+len(p.RemoveTracks)+len(p.PutLists)+len(p.RemoveLists) == 0 {
		return w.updateRows(p.UpdateTracks) // no row or list comes or goes
	}
	b, err := w.lib.Begin(w.ctx)
	if err != nil {
		return err
	}
	defer b.Rollback()
	s := w.snap
	for _, key := range p.RemoveTracks {
		if t := s.Tracks[key]; t.EngineID != 0 && !t.Native {
			if err = b.DeleteTrack(w.ctx, t.EngineID); err != nil {
				return err
			}
			w.res.Removed++
		}
		delete(s.Tracks, key)
	}
	// A file that already has a row the bridge did not make (Engine's own, under any spelling of
	// its path) keeps that row: lists refer to it, the bridge never writes it (#252).
	adds := p.AddTracks[:0:0]
	for _, at := range p.AddTracks {
		vt := &w.view.Tracks[at]
		if row, ok := w.index[vt.Key]; ok {
			w.note(vt, row.ID, true)
			continue
		}
		adds = append(adds, at)
	}
	p.AddTracks = adds
	moved := p.Moves[:0:0]
	for _, m := range p.Moves {
		vt := &w.view.Tracks[m.New]
		if row, ok := w.index[vt.Key]; ok { // the file's new place has a row of Engine's own
			if t := s.Tracks[m.Old]; t.EngineID != 0 {
				if err = b.DeleteTrack(w.ctx, t.EngineID); err != nil {
					return err
				}
				w.res.Removed++
			}
			delete(s.Tracks, m.Old)
			w.note(vt, row.ID, true)
			continue
		}
		moved = append(moved, m)
	}
	p.Moves = moved
	if len(p.Moves) > 0 {
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
	}
	// New rows, a hundred at a time while the window has time for them and the lists after them.
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
			vt := &w.view.Tracks[vi]
			w.note(vt, ids[i], native[i])
			w.index[vt.Key] = enginedb.Row{ID: ids[i], Path: "../" + vt.Rel}
			if err = w.noteSync(b, s.Tracks[vt.Key]); err != nil {
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
	trackIDs := make(map[string]int64, len(s.Tracks))
	for key, t := range s.Tracks {
		trackIDs[key] = t.EngineID
	}
	ids, err := putLists(w.ctx, b, w.view, p.PutLists, s.Lists, trackIDs)
	if err != nil {
		return err
	}
	for _, at := range p.PutLists {
		l := &w.view.Lists[at]
		hash := l.Hash
		for _, key := range l.Tracks {
			if trackIDs[key] == 0 {
				hash = "partial" // written again once its tracks have rows
				break
			}
		}
		s.Lists[l.Key] = &rbstate.List{Key: l.Key, EngineID: ids[l.Key], Hash: hash}
	}
	w.res.PlaylistsChanged = len(p.PutLists)+len(p.RemoveLists) > 0
	if err = w.commit(b); err != nil {
		return err
	}
	w.st.done("lists", "%d written, %d removed; committed", len(p.PutLists), len(p.RemoveLists))
	return w.updateRows(p.UpdateTracks)
}

// updateBatch is how many rows one transaction gives the export's fields again.
var updateBatch = 1000

// updateRows gives the rows whose export fields changed those fields. A re-export changes a
// few; the take-over of an older library, or a library retagged in rekordbox, thousands, and
// 2,416 of them in the transaction of the lists would have kept an insert past the hook's
// kill. So they come after the rows and lists are committed, a batch a transaction: the first
// batch is owed like a new row, the rest is done while the window has time, and what is left
// the next window does (the job stages those tracks again, so a remount follows).
func (w *window) updateRows(idx []int) error {
	done, differed := 0, 0
	ok := func() bool {
		if done < updateBatch {
			return w.st.must(applyReserve)
		}
		return w.st.more(applyReserve)
	}
	for done < len(idx) && ok() {
		b, err := w.lib.Begin(w.ctx)
		if err != nil {
			return err
		}
		for n := 0; n < updateBatch && done < len(idx) && ok(); n, done = n+1, done+1 {
			vt := &w.view.Tracks[idx[done]]
			t := w.snap.Tracks[vt.Key]
			var changed bool
			if changed, err = b.UpdateTrack(w.ctx, t.EngineID, enginedb.TrackRow{Rel: t.Rel, Track: vt.Track}, t.Rating); err == nil {
				t.Row, t.Rating, t.Refs = vt.Hash, vt.Track.Rating, vt.Refs
				// A row that held the fields already was not written, and its cues are not read to note
				// it again (that read is most of what a row costs on a cold stick). It is noted as
				// noteSync would have left it: a row in step with the stick keeps its note, any other
				// is dated as it is now and vouched for by nobody.
				if changed {
					differed++
					err = w.noteSync(b, t)
				} else if row := w.index[t.Key]; !t.Native && (row.Edit != t.Edit || t.Perf == rbstate.PerfDeck) {
					t.Edit, t.Synced = row.Edit, nil
				}
			}
			if err != nil {
				b.Rollback()
				return err
			}
		}
		if err = w.commit(b); err != nil {
			return err
		}
	}
	if w.res.Updated += done; len(idx) > 0 {
		w.st.done("update", "%d of %d rows got the export's fields (%d held others)", done, len(idx), differed)
	}
	return w.saveIfChanged()
}

// keepOwn drops from a removal list what a window must not remove: everything when a library
// was unreadable, the stick's own tracks when only staged CDJ libraries were read.
func keepOwn(keys []string, s *rbstate.Snapshot, partial bool) []string {
	if partial {
		return nil
	}
	var out []string
	for _, key := range keys {
		for _, r := range s.Tracks[key].Refs {
			if r.Kind == rbstate.LinkLibrary {
				out = append(out, key)
				break
			}
		}
	}
	return out
}

func keepOwnLists(keys []string, partial bool) []string {
	if partial {
		return nil
	}
	var out []string
	for _, key := range keys {
		if strings.HasPrefix(key, viewLink) {
			out = append(out, key)
		}
	}
	return out
}

// applyStaged writes the tracks the background job has staged, the ones the DJ asked for
// first, a batch a transaction, until the stage is empty or the window's time for it is up.
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
	// The stage covers the readable portion of a partial source set. Its stamp still
	// names every export file, so write-back can use the sources that were read.
	if w.snap.Pending.Verify && verified == w.view.Stamp {
		w.snap.Pending.Verify = false
	}
	var keys, stale, missing []string
	for key, e := range entries {
		vt, ok := w.view.Track(key)
		switch {
		case !ok || e.Sig.Rules != rbstate.Rules || e.Sig.Row != vt.Hash:
			stale = append(stale, key) // of another export or other rules
		case e.Missing:
			missing = append(missing, key)
		default:
			keys = append(keys, key)
		}
	}
	// missing: files gone from the stick. The job has told them from names its mount cannot read
	// (ErrFilesUnreadable) before it staged them.
	keys = w.inApplyOrder(append(keys, missing...))
	applied, kept, degraded, gone, left := 0, 0, 0, 0, len(entries)-len(stale)
	for at := 0; at < len(keys) && w.st.more(applyReserve); {
		b, err := w.lib.Begin(w.ctx)
		if err != nil {
			return err
		}
		type rgb struct {
			id   int64
			body []byte
		}
		var overviews []rgb
		var done []string
		for first := at; at < len(keys) && at-first < applyBatch && w.st.more(applyReserve); at++ {
			key := keys[at]
			e := entries[key]
			vt, _ := w.view.Track(key)
			t := w.snap.Tracks[key]
			if t != nil && t.Native {
				done = append(done, key)
				continue
			}
			if e.Missing {
				if t != nil && t.EngineID != 0 {
					if err = b.DeleteTrack(w.ctx, t.EngineID); err != nil {
						b.Rollback()
						return err
					}
					gone++
				}
				w.snap.Tracks[key] = &rbstate.Track{Key: key, Rel: vt.Rel, Refs: vt.Refs, Row: vt.Hash, Perf: rbstate.PerfMissing, Built: e.Sig}
				done = append(done, key)
				continue
			}
			if t == nil || t.EngineID == 0 { // no row yet: one the window had no time for, or a file that is back
				ids, native, err := b.InsertTracks(w.ctx, []enginedb.TrackRow{{Rel: vt.Rel, Track: vt.Track}})
				if err != nil {
					b.Rollback()
					return err
				}
				w.note(vt, ids[0], native[0])
				w.index[key] = enginedb.Row{ID: ids[0], Path: "../" + vt.Rel}
				if t = w.snap.Tracks[key]; t.Native {
					done = append(done, key)
					continue
				}
			}
			full, err := stage.Read(e)
			if errors.Is(err, os.ErrNotExist) {
				stale = append(stale, key) // its files are not whole: dropped, so the job stages it again
				continue
			}
			if err != nil {
				b.Rollback()
				return err
			}
			if full.Rel != "" && full.Rel != t.Rel && rbstate.FoldPath(full.Rel) == key {
				// The file's own spelling on the volume, which Engine needs to open it.
				if err = b.MoveTracks(w.ctx, map[int64]string{t.EngineID: full.Rel}); err != nil {
					b.Rollback()
					return err
				}
				t.Rel = full.Rel
			}
			t.Resolved, t.Built = true, e.Sig
			if e.Degraded != "" {
				t.Perf = rbstate.PerfDegraded
				degraded++
			} else {
				perf := enginedb.Performance{TrackData: full.Perf.TrackData, Overview: full.Perf.Overview, Beats: full.Perf.Beats,
					QuickCues: full.Perf.QuickCues, Loops: full.Perf.Loops, ActiveLoops: full.Perf.ActiveLoops, Complete: full.Perf.Complete, Digest: e.Digest}
				digest, outcome, err := b.ApplyPerformance(w.ctx, t.EngineID, perf, full.Facts, enginedb.PerfPrev{State: t.Perf, Written: t.Written}, vt.Track)
				if err != nil {
					b.Rollback()
					return err
				}
				t.Written, t.Perf = digest, rbstate.PerfApplied
				if outcome == enginedb.PerfKept || outcome == enginedb.PerfMerged {
					t.Perf = rbstate.PerfDeck
					kept++
				}
				if len(full.RGB) > 0 {
					overviews = append(overviews, rgb{t.EngineID, full.RGB})
				}
			}
			if len(full.Art) > 0 {
				if err = b.PutArtwork(w.ctx, t.EngineID, full.Art); err != nil {
					log.Printf("rb-bridge: cover of track %d not written: %v", t.EngineID, err)
				}
			}
			if err = w.noteSync(b, t); err != nil {
				b.Rollback()
				return err
			}
			applied++
			done = append(done, key)
		}
		if err = w.commit(b); err != nil {
			return err
		}
		for _, o := range overviews {
			writeOverview(w.dir, w.lib.UUID(), o.id, o.body)
		}
		if err = stage.Drop(done...); err != nil {
			return err
		}
		if w.o.Progress != nil {
			w.o.Progress(at, len(keys))
		}
		left -= len(done)
	}
	_ = stage.Drop(stale...)
	if left == 0 && len(entries) > 0 {
		_ = stage.Reset(verified) // everything staged is applied or of another export: the index starts again
	}
	w.res.Applied = applied
	w.res.Removed += gone
	w.st.done("staged", "%d applied (%d kept the deck's edits, %d without analysis), %d rows of missing files removed, %d of %d staged left", applied, kept, degraded, gone, max(len(keys)-applied-gone, 0), len(keys))
	if (applied > 0 || gone > 0) && w.snap.Pending.Lists {
		return w.finishLists()
	}
	return w.saveIfChanged()
}

// finishLists writes again the lists that waited for their tracks' rows.
func (w *window) finishLists() error {
	p := Diff(w.view, w.snap)
	if len(p.PutLists) == 0 {
		return w.saveIfChanged()
	}
	return w.apply(&Plan{PutLists: p.PutLists})
}

// saveIfChanged saves the snapshot when it changed without a transaction on m.db (the stamp, the
// export's IDs, pending counts, the verify mark, rows Engine removed).
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

// writeOverview puts a track's list waveform where Engine reads it, unless one is there: Engine's
// own, or the one the job wrote when it staged the track (a row made in this window has none).
// The file is not read: on a USB 2.0 disk a read a track was half of a window's time.
func writeOverview(dir, uuid string, id int64, body []byte) {
	if st, err := os.Stat(filepath.Join(dir, "OverviewData", uuid, fmt.Sprintf("%d.rgb", id))); err == nil && st.Size() > 4 {
		return
	}
	if err := enginedb.WriteRGBOverviewAtomic(dir, uuid, id, body); err != nil {
		log.Printf("rb-bridge: list waveform of track %d not written: %v", id, err)
	}
}
