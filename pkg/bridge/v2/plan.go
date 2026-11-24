package bridge

import (
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// Move is a row that follows its file: the library that listed it at Old now lists the same
// record at the view's track New, and nothing is at Old any more.
type Move struct {
	Old string // the snapshot's key
	New int    // index into View.Tracks
}

// Plan is what a window has to do to make m.db the view. Its sizes are what the window's time
// is planned with: a re-export that adds one list has one entry in PutLists and nothing else.
type Plan struct {
	AddTracks    []int    // indexes into View.Tracks: no row yet
	UpdateTracks []int    // the row's export fields changed
	Moves        []Move   // the file moved
	RemoveTracks []string // snapshot keys the view no longer has
	PutLists     []int    // indexes into View.Lists: new, or its hash changed
	RemoveLists  []string // snapshot keys the view no longer has
}

// Empty reports that m.db already is the view.
func (p *Plan) Empty() bool {
	return len(p.AddTracks)+len(p.UpdateTracks)+len(p.Moves)+len(p.RemoveTracks)+len(p.PutLists)+len(p.RemoveLists) == 0
}

type refKey struct {
	kind   rbstate.LibraryKind
	source string
	id     uint32
}

// Diff compares the view with the snapshot of what m.db holds. Identity is the key, the path:
// a track is the same track when its file is. A source ID only serves to follow a file that
// moved: the view has a track at a new path whose record, by library and ID, the snapshot has
// at a path the view no longer has at all. An ID that a re-export handed to another file whose
// own path is still there names nothing (#253, the 11-23 cross-track damage).
func Diff(v *View, s *rbstate.Snapshot) *Plan {
	p := &Plan{}
	gone := map[string]bool{}
	for key, t := range s.Tracks {
		if _, ok := v.index[key]; !ok && !t.Native {
			gone[key] = true
		}
	}
	// The record of each track that is gone, for the moves; a record two gone tracks share names neither.
	byRef := map[refKey]string{}
	for key := range gone {
		for _, r := range s.Tracks[key].Refs {
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
		st, ok := s.Tracks[vt.Key]
		switch {
		case ok && st.Native:
		case ok && st.EngineID != 0:
			if st.Row != vt.Hash {
				p.UpdateTracks = append(p.UpdateTracks, i)
			}
		default:
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
	}
	for key := range s.Tracks {
		if gone[key] {
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
