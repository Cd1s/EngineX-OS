package ext4

import "slices"

// dirModel replays addEntry and removeEntry on a directory's entry layout,
// so the preflight knows exactly when a directory must grow.
type dirModel struct {
	slots  []slot
	blocks int  // directory blocks, including appended ones
	extent bool // extent-mapped: appendBlock can grow it
	exts   int  // extents before the change
	meta   int  // extent tree blocks before the change
	added  int  // blocks appended by the change
}

type slot struct {
	block int
	name  string // "" for an unused entry
	rec   int
	used  int // direntLen of the name, 0 when unused
}

// loadDirModel reads dir's layout as addEntry will see it: an htree root
// or node becomes the plain block linearize makes of it.
func (f *fsys) loadDirModel(dir *inode) (*dirModel, error) {
	blocks, err := f.readDir(dir)
	if err != nil {
		return nil, err
	}
	exts, meta, err := f.fileMap(dir)
	if err != nil {
		return nil, err
	}
	m := &dirModel{blocks: len(blocks), extent: dir.flags()&flagExtents != 0, exts: len(exts), meta: len(meta)}
	end := f.dirEnd()
	for i, b := range blocks {
		switch {
		case b.dx && i == 0:
			m.slots = append(m.slots, slot{0, ".", 12, 12}, slot{0, "..", end - 12, 12})
		case b.dx:
			m.slots = append(m.slots, slot{i, "", end, 0})
		default:
			ents, err := f.parseEntries(b.data, end, i)
			if err != nil {
				return nil, err
			}
			for _, e := range ents {
				if e.ino == 0 {
					m.slots = append(m.slots, slot{i, "", e.recLen, 0})
				} else {
					m.slots = append(m.slots, slot{i, string(e.name), e.recLen, direntLen(len(e.name))})
				}
			}
		}
	}
	return m, nil
}

// add places name first-fit as addEntry does; it reports whether a block
// had to be appended.
func (m *dirModel) add(name string, end int) bool {
	need := direntLen(len(name))
	for k := range m.slots {
		s := &m.slots[k]
		if s.name == "" && s.rec >= need {
			s.name, s.used = name, need
			return false
		}
		if s.name != "" && s.rec-s.used >= need {
			n := slot{s.block, name, s.rec - s.used, need}
			s.rec = s.used
			m.slots = slices.Insert(m.slots, k+1, n)
			return false
		}
	}
	m.slots = append(m.slots, slot{m.blocks, name, end, need})
	m.blocks++
	m.added++
	return true
}

// remove drops name as removeEntry does: its space joins the previous entry
// of the same block, or the entry becomes unused.
func (m *dirModel) remove(name string) {
	for k, s := range m.slots {
		if s.name != name {
			continue
		}
		if k > 0 && m.slots[k-1].block == s.block {
			m.slots[k-1].rec += s.rec
			m.slots = slices.Delete(m.slots, k, k+1)
		} else {
			m.slots[k].name, m.slots[k].used = "", 0
		}
		return
	}
}

// cost is the worst-case net block use of the appended blocks: one data
// block each, plus the growth of the extent tree that appendBlock rebuilds
// (each block may add an extent). Freed old tree blocks are not credited,
// which also covers appendBlock allocating the data block first.
func (m *dirModel) cost(bs int64) uint64 {
	added := uint64(m.added)
	tree := treeBlocks(uint64(m.exts)+added, bs)
	if tree > uint64(m.meta) {
		added += tree - uint64(m.meta)
	}
	return added
}
