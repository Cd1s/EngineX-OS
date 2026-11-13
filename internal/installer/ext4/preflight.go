package ext4

import (
	"fmt"
	"strings"
)

// preflight walks every path read-only, refusing unsafe or conflicting ones,
// then replays Apply's exact order (removes, directories, files with their
// replaced predecessor unlinked just before, links) and tracks the running
// use of blocks and inodes. A change whose peak would exceed what is free
// fails here with ErrNoSpace, before the first write. Space estimates are
// upper bounds; space freed by an unlink is only credited from that step on.
func (img *Image) preflight(p *plan) error {
	f := img.fs
	sim := &simulation{f: f, freeBlocks: int64(f.sbFreeBlocks()), freeInodes: int64(f.sbFreeInodes()),
		created: map[string]bool{}, entryBytes: map[string]uint64{}}
	root, err := f.readInode(rootIno)
	if err != nil {
		return err
	}
	if root.flags()&(flagEncrypt|flagCasefold) != 0 {
		return fmt.Errorf("%w: the root directory is encrypted or case-folded", ErrUnsupported)
	}
	for _, op := range p.files {
		if op.e.Size >= 1<<31 && f.roCompat()&roCompatLargeFile == 0 {
			return fmt.Errorf("%w: /%s is %d bytes and the filesystem lacks large_file",
				ErrUnsupported, strings.Join(op.parts, "/"), op.e.Size)
		}
	}

	for _, parts := range p.removes {
		k := strings.Join(parts, "/")
		if parent, err := f.lstat(strings.Join(parts[:len(parts)-1], "/")); err != nil {
			return err
		} else if parent != nil && parent.flags()&(flagEncrypt|flagCasefold) != 0 {
			return fmt.Errorf("%w: /%s is in an encrypted directory", ErrUnsupported, k)
		}
		in, err := f.lstat(k)
		if err != nil {
			return err
		}
		if in == nil {
			continue
		}
		if in.flags()&flagEncrypt != 0 {
			return fmt.Errorf("%w: /%s is encrypted; refusing to remove it", ErrUnsupported, k)
		}
		if in.isDir() {
			blocks, err := f.readDir(in)
			if err != nil {
				return err
			}
			ents, err := f.listEntries(blocks)
			if err != nil {
				return err
			}
			if len(ents) > 2 {
				return fmt.Errorf("%w: /%s is a non-empty directory", ErrExists, k)
			}
		}
		sim.release(in)
	}
	for _, parts := range p.dirs {
		if err := sim.ensureDir(parts); err != nil {
			return err
		}
	}
	for _, op := range p.files {
		if err := sim.ensureDir(op.parts[:len(op.parts)-1]); err != nil {
			return err
		}
		k := strings.Join(op.parts, "/")
		in, err := f.lstat(k)
		if err != nil {
			return err
		}
		if in != nil {
			if !in.isReg() {
				return fmt.Errorf("%w: /%s exists and is not a regular file", ErrExists, k)
			}
			if in.flags()&flagEncrypt != 0 {
				return fmt.Errorf("%w: /%s is encrypted", ErrUnsupported, k)
			}
			sim.release(in) // installFile unlinks it first
		}
		n := (uint64(op.e.Size) + uint64(f.bs) - 1) / uint64(f.bs)
		if err := sim.use(n+treeBlocks(n, f.bs), 1); err != nil {
			return err
		}
		if err := sim.entry(op.parts); err != nil {
			return err
		}
	}
	for _, l := range p.links {
		if err := sim.ensureDir(l.parts[:len(l.parts)-1]); err != nil {
			return err
		}
		k := strings.Join(l.parts, "/")
		if l.target == "" || len(l.target) >= int(f.bs) || strings.ContainsRune(l.target, 0) {
			return fmt.Errorf("ext4: /%s: invalid link target %q", k, l.target)
		}
		in, err := f.lstat(k)
		if err != nil {
			return err
		}
		if in != nil {
			if !in.isSymlink() {
				return fmt.Errorf("%w: /%s exists and is not a symlink", ErrExists, k)
			}
			if t, err := f.readAll(in, uint64(f.bs)); err == nil && string(t) == l.target {
				continue // installLink keeps it
			}
			sim.release(in)
		}
		var blocks uint64
		if len(l.target) >= inodeBlockLen {
			blocks = 1
		}
		if err := sim.use(blocks, 1); err != nil {
			return err
		}
		if err := sim.entry(l.parts); err != nil {
			return err
		}
	}
	return nil
}

// simulation tracks the running use of blocks and inodes during preflight.
type simulation struct {
	f                      *fsys
	freeBlocks, freeInodes int64
	usedBlocks, usedInodes int64 // net, from the start of the change
	peakBlocks, peakInodes int64
	created                map[string]bool   // directories this change creates
	entryBytes             map[string]uint64 // per directory: bytes of new entries
}

func (s *simulation) use(blocks, inodes uint64) error {
	s.usedBlocks += int64(blocks)
	s.usedInodes += int64(inodes)
	s.peakBlocks = max(s.peakBlocks, s.usedBlocks)
	s.peakInodes = max(s.peakInodes, s.usedInodes)
	if s.peakBlocks > s.freeBlocks || s.peakInodes > s.freeInodes {
		return fmt.Errorf("%w: at its peak the change needs up to %d blocks and %d inodes, %d blocks and %d inodes are free (%d-byte blocks)",
			ErrNoSpace, s.peakBlocks, s.peakInodes, s.freeBlocks, s.freeInodes, s.f.bs)
	}
	return nil
}

// release credits what unlinking in frees: its blocks and inode when this
// is its last link. Blocks shared through an xattr block are not credited.
func (s *simulation) release(in *inode) {
	if in.links() > 1 && !in.isDir() {
		return
	}
	if in.fileACL(s.f) == 0 {
		s.usedBlocks -= int64(in.blocks512(s.f) * 512 / uint64(s.f.bs))
	}
	s.usedInodes--
}

// ensureDir walks parts, charging every directory the change must create.
func (s *simulation) ensureDir(parts []string) error {
	f := s.f
	for i := range parts {
		k := strings.Join(parts[:i+1], "/")
		if s.created[k] {
			continue
		}
		in, err := f.lstat(k)
		if err != nil {
			return err
		}
		if in == nil {
			s.created[k] = true
			if err := s.use(1, 1); err != nil {
				return err
			}
			if err := s.entry(parts[:i+1]); err != nil {
				return err
			}
			continue
		}
		if !in.isDir() {
			if in.isSymlink() {
				return fmt.Errorf("%w: /%s is a symlink", ErrUnsafePath, k)
			}
			return fmt.Errorf("%w: /%s exists and is not a directory", ErrExists, k)
		}
		if in.flags()&(flagEncrypt|flagCasefold) != 0 {
			return fmt.Errorf("%w: /%s is an encrypted or case-folded directory", ErrUnsupported, k)
		}
	}
	return nil
}

// entry charges the growth of the parent directory for one new name.
func (s *simulation) entry(parts []string) error {
	dir := strings.Join(parts[:len(parts)-1], "/")
	before := s.dirBlocks(dir, s.entryBytes[dir])
	s.entryBytes[dir] += uint64(direntLen(len(parts[len(parts)-1])))
	return s.use(s.dirBlocks(dir, s.entryBytes[dir])-before, 0)
}

// dirBlocks bounds the blocks a directory gains from b bytes of new entries.
func (s *simulation) dirBlocks(dir string, b uint64) uint64 {
	if b == 0 {
		return 0
	}
	end := uint64(s.f.dirEnd())
	if s.created[dir] {
		// A new directory's first block (charged at creation) has room
		// after '.' and '..'.
		if b <= end-24 {
			return 0
		}
		return (b-(end-24)+end/2-1)/(end/2) + 1
	}
	// Existing directory: assume no reusable slack; every block added is
	// at least half full, and one more block may hold its extent tree.
	return (b+end/2-1)/(end/2) + 1
}

// treeBlocks bounds the extent tree blocks a file of n blocks can need if
// free space is fully fragmented.
func treeBlocks(n uint64, bs int64) uint64 {
	if n <= 4 {
		return 0
	}
	per := uint64((bs - 12) / 12)
	var total uint64
	for level := n; level > 4; {
		level = (level + per - 1) / per
		total += level
	}
	return total
}
