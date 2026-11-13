package ext4

import (
	"fmt"
	"strings"
)

// preflight walks every path read-only, refusing unsafe or conflicting ones,
// and checks that the change fits. The space estimate is an upper bound:
// it never lets a change start that could run out of blocks or inodes.
func (img *Image) preflight(p *plan) error {
	f := img.fs
	bs := uint64(f.bs)
	var needBlocks, needInodes, freedBlocks, freedInodes uint64
	created := map[string]bool{}        // directories this change creates
	entryBytes := map[string]uint64{}   // per parent: bytes of new entries
	existingParent := map[string]bool{} // parent already exists in the image
	replaced := map[string]bool{}

	// ensureDir walks parts, recording which directories must be created.
	ensureDir := func(parts []string) error {
		for i := range parts {
			k := strings.Join(parts[:i+1], "/")
			if created[k] {
				continue
			}
			in, err := f.lstat(k)
			if err != nil {
				return err
			}
			if in == nil {
				created[k] = true
				needBlocks++
				needInodes++
				parent := strings.Join(parts[:i], "/")
				entryBytes[parent] += uint64(direntLen(len(parts[i])))
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
			existingParent[k] = true
		}
		return nil
	}
	root, err := f.readInode(rootIno)
	if err != nil {
		return err
	}
	if root.flags()&(flagEncrypt|flagCasefold) != 0 {
		return fmt.Errorf("%w: the root directory is encrypted or case-folded", ErrUnsupported)
	}
	existingParent[""] = true

	credit := func(in *inode) {
		if in.links() == 1 && in.fileACL(f) == 0 {
			freedBlocks += in.blocks512(f) * 512 / bs
		}
		if in.links() == 1 {
			freedInodes++
		}
	}
	for _, parts := range p.removes {
		k := strings.Join(parts, "/")
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
		if parent, _ := f.lstat(strings.Join(parts[:len(parts)-1], "/")); parent != nil &&
			parent.flags()&(flagEncrypt|flagCasefold) != 0 {
			return fmt.Errorf("%w: /%s is in an encrypted directory", ErrUnsupported, k)
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
		credit(in)
	}
	for _, parts := range p.dirs {
		if err := ensureDir(parts); err != nil {
			return err
		}
	}
	for _, op := range p.files {
		if err := ensureDir(op.parts[:len(op.parts)-1]); err != nil {
			return err
		}
		k := strings.Join(op.parts, "/")
		n := (uint64(op.e.Size) + bs - 1) / bs
		needBlocks += n + treeBlocks(n, f.bs)
		needInodes++
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
			if !replaced[k] {
				credit(in)
				replaced[k] = true
			}
		}
		entryBytes[strings.Join(op.parts[:len(op.parts)-1], "/")] += uint64(direntLen(len(op.parts[len(op.parts)-1])))
	}
	for _, l := range p.links {
		if err := ensureDir(l.parts[:len(l.parts)-1]); err != nil {
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
		if in != nil && !in.isSymlink() {
			return fmt.Errorf("%w: /%s exists and is not a symlink", ErrExists, k)
		}
		needInodes++
		if len(l.target) >= inodeBlockLen {
			needBlocks++
		}
		entryBytes[strings.Join(l.parts[:len(l.parts)-1], "/")] += uint64(direntLen(len(l.parts[len(l.parts)-1])))
	}
	end := uint64(f.dirEnd())
	for dir, b := range entryBytes {
		if created[dir] {
			// A new directory's first block has room after '.' and '..'.
			if b > end-24 {
				needBlocks += (b-(end-24))/(end/2) + 1
			}
			continue
		}
		// Existing directory: assume no reusable slack; every block we
		// add is at least half full; one more block may hold its tree.
		needBlocks += (b+end/2-1)/(end/2) + 1
	}
	freeBlocks := f.sbFreeBlocks() + freedBlocks
	freeInodes := uint64(f.sbFreeInodes()) + freedInodes
	if needBlocks > freeBlocks || needInodes > freeInodes {
		return fmt.Errorf("%w: needs up to %d blocks and %d inodes, %d blocks and %d inodes are free (%d-byte blocks)",
			ErrNoSpace, needBlocks, needInodes, freeBlocks, freeInodes, bs)
	}
	return nil
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
