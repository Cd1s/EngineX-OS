package bridge

import (
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// Move is a row that follows its file: the record the snapshot has at Old is now at the view's
// track New, and nothing is at Old any more.
type Move struct {
	Old string // the snapshot's key
	New int    // index into View.Tracks
}

// Plan is what a window has to do to make m.db the view; its sizes are what the window's time is
// planned with.
type Plan struct {
	AddTracks    []int    // indexes into View.Tracks: no row yet
	UpdateTracks []int    // the row's export fields changed
	Moves        []Move   // the file moved
	RemoveTracks []string // snapshot keys the view no longer has
	PutLists     []int    // indexes into View.Lists: new, or its hash changed
	RemoveLists  []string // snapshot keys the view no longer has
}

type refKey struct {
	kind   rbstate.LibraryKind
	source string
	id     uint32
}

// Diff compares the view with the snapshot. Identity is the key, the path. A source ID only
// follows a file that moved: the view's track at a new path has the record, by library and ID,
// of a track at a path the view no longer has. An ID a re-export gave another file whose own
// path is still there names nothing (#253, the 11-23 cross-track damage).
func Diff(v *View, s *rbstate.Snapshot) *Plan {
	p := &Plan{}
	gone := map[string]bool{}
	byRef := map[refKey]string{} // the record of each track that is gone; "" for one two of them share
	for key, t := range s.Tracks {
		if _, ok := v.index[key]; ok || t.Native {
			continue
		}
		gone[key] = true
		if t.EngineID == 0 {
			continue // no row that could follow a file
		}
		for _, r := range t.Refs {
			k := refKey{r.Kind, r.Source, r.ID}
			if _, dup := byRef[k]; dup {
				byRef[k] = ""
			} else {
				byRef[k] = key
			}
		}
	}
	for i := range v.Tracks {
		vt := &v.Tracks[i]
		if t, ok := s.Tracks[vt.Key]; ok && (t.Native || t.EngineID != 0) {
			if !t.Native && t.Row != vt.Hash {
				p.UpdateTracks = append(p.UpdateTracks, i)
			}
			continue
		} else if ok && t.Perf == rbstate.PerfMissing {
			// No row until the job finds the file again (applyStaged gives it one then). Adding it
			// here put back, at every window and every re-export, the row the last window removed.
			continue
		}
		moved := ""
		for _, r := range vt.Refs {
			if old := byRef[refKey{r.Kind, r.Source, r.ID}]; old != "" && gone[old] {
				moved = old
				break
			}
		}
		if moved != "" {
			gone[moved] = false
			p.Moves = append(p.Moves, Move{Old: moved, New: i})
		} else {
			p.AddTracks = append(p.AddTracks, i)
		}
	}
	for key, g := range gone {
		if g {
			p.RemoveTracks = append(p.RemoveTracks, key)
		}
	}
	inView := make(map[string]bool, len(v.Lists))
	for i := range v.Lists {
		l := &v.Lists[i]
		inView[l.Key] = true
		if sl, ok := s.Lists[l.Key]; !ok || sl.EngineID == 0 || sl.Hash != l.Hash {
			p.PutLists = append(p.PutLists, i)
		}
	}
	for key := range s.Lists {
		if !inView[key] {
			p.RemoveLists = append(p.RemoveLists, key)
		}
	}
	return p
}
