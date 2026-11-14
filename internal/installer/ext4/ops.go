package ext4

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
)

const linkMax = 65000

// splitPath turns a canonical image-relative POSIX path into components.
// A leading '/' is accepted. Empty, '.', '..', backslash and NUL components
// are refused: callers translate host paths before reaching this package.
func splitPath(p string) ([]string, error) {
	q := strings.TrimPrefix(p, "/")
	if q == "" {
		return nil, nil
	}
	parts := strings.Split(q, "/")
	for _, c := range parts {
		if c == "" || c == "." || c == ".." || strings.ContainsAny(c, "\\\x00") || len(c) > 255 {
			return nil, fmt.Errorf("%w: %q", ErrUnsafePath, p)
		}
	}
	return parts, nil
}

// walkDir resolves the directory holding the last component of parts. Every
// intermediate component must be a real directory: symlinks are never
// followed, so a link planted in a backup cannot redirect writes.
func (f *fsys) walkDir(parts []string) (*inode, error) {
	dir, err := f.readInode(rootIno)
	if err != nil {
		return nil, err
	}
	for i, c := range parts {
		ino, ft, err := f.lookup(dir, c)
		if err != nil {
			return nil, err
		}
		if ino == 0 {
			return nil, fmt.Errorf("%w: /%s", errNotFound, strings.Join(parts[:i+1], "/"))
		}
		in, err := f.readInode(ino)
		if err != nil {
			return nil, err
		}
		if !in.isDir() || ft != ftDir {
			if in.isSymlink() {
				return nil, fmt.Errorf("%w: /%s is a symlink", ErrUnsafePath, strings.Join(parts[:i+1], "/"))
			}
			return nil, fmt.Errorf("%w: /%s is not a directory", ErrExists, strings.Join(parts[:i+1], "/"))
		}
		dir = in
	}
	return dir, nil
}

var errNotFound = errors.New("ext4: not found")

// lstat returns the inode at path without following a final symlink, or nil.
func (f *fsys) lstat(p string) (*inode, error) {
	parts, err := splitPath(p)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return f.readInode(rootIno)
	}
	dir, err := f.walkDir(parts[:len(parts)-1])
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ino, _, err := f.lookup(dir, parts[len(parts)-1])
	if err != nil || ino == 0 {
		return nil, err
	}
	return f.readInode(ino)
}

// mkdirAll creates every missing directory of parts (mode perm).
func (f *fsys) mkdirAll(parts []string, perm uint16) (*inode, error) {
	dir, err := f.readInode(rootIno)
	if err != nil {
		return nil, err
	}
	for i, c := range parts {
		ino, ft, err := f.lookup(dir, c)
		if err != nil {
			return nil, err
		}
		if ino == 0 {
			if dir, err = f.mkdir(dir, c, perm); err != nil {
				return nil, err
			}
			continue
		}
		in, err := f.readInode(ino)
		if err != nil {
			return nil, err
		}
		if !in.isDir() || ft != ftDir {
			if in.isSymlink() {
				return nil, fmt.Errorf("%w: /%s is a symlink", ErrUnsafePath, strings.Join(parts[:i+1], "/"))
			}
			return nil, fmt.Errorf("%w: /%s exists and is not a directory", ErrExists, strings.Join(parts[:i+1], "/"))
		}
		dir = in
	}
	return dir, nil
}

func (f *fsys) mkdir(parent *inode, name string, perm uint16) (*inode, error) {
	if parent.flags()&(flagEncrypt|flagCasefold) != 0 {
		return nil, fmt.Errorf("%w: directory inode %d is encrypted or case-folded", ErrUnsupported, parent.num)
	}
	ino, err := f.allocInode(f.inodeGroup(parent.num), true)
	if err != nil {
		return nil, err
	}
	in := f.newInodeRaw(ino, sIFDIR|perm&0o7777, f.now())
	blk, err := f.allocData(f.goalFor(ino), 1, 0)
	if err != nil {
		return nil, err
	}
	if err := f.writeBlock(blk[0].phys, f.newDirBlock(in, parent.num)); err != nil {
		return nil, err
	}
	if _, err := f.writeExtentTree(in, blk, 0); err != nil {
		return nil, err
	}
	in.setSize(uint64(f.bs))
	in.setLinks(2)
	if err := in.setBlocks512(f, uint64(f.bs/512)); err != nil {
		return nil, err
	}
	if err := f.writeInode(in); err != nil {
		return nil, err
	}
	if err := f.addEntry(parent, name, ino, ftDir); err != nil {
		return nil, err
	}
	if l := parent.links(); l >= linkMax-1 || l == 1 {
		if f.roCompat()&roCompatDirNlink == 0 {
			return nil, fmt.Errorf("%w: directory inode %d has too many subdirectories", ErrUnsupported, parent.num)
		}
		parent.setLinks(1)
	} else {
		parent.setLinks(l + 1)
	}
	return in, f.writeInode(parent)
}

// createFile writes a new regular file of exactly size bytes from r. The
// content must hash to sum; a short, long or different stream fails.
func (f *fsys) createFile(parent *inode, name string, perm uint16, size int64, sum [32]byte, r io.Reader) error {
	if parent.flags()&(flagEncrypt|flagCasefold) != 0 {
		return fmt.Errorf("%w: directory inode %d is encrypted or case-folded", ErrUnsupported, parent.num)
	}
	if size < 0 || size >= 1<<31 && f.roCompat()&roCompatLargeFile == 0 {
		// The preflight refuses this first; this guards direct callers.
		return fmt.Errorf("%w: %s: %d bytes without large_file", ErrUnsupported, name, size)
	}
	ino, err := f.allocInode(f.inodeGroup(parent.num), false)
	if err != nil {
		return err
	}
	in := f.newInodeRaw(ino, sIFREG|perm&0o7777, f.now())
	nblocks := (uint64(size) + uint64(f.bs) - 1) / uint64(f.bs)
	var exts []extent
	if nblocks > 0 {
		if exts, err = f.allocData(f.goalFor(parent.num), nblocks, 0); err != nil {
			return err
		}
	}
	h := sha256.New()
	buf := make([]byte, 256*int(f.bs))
	left := uint64(size)
	for _, e := range exts {
		for done := uint64(0); done < uint64(e.length); {
			n := min(uint64(e.length)-done, uint64(len(buf))/uint64(f.bs))
			chunk := buf[:n*uint64(f.bs)]
			want := min(left, uint64(len(chunk)))
			if _, err := io.ReadFull(r, chunk[:want]); err != nil {
				return fmt.Errorf("ext4: %s: source shorter than %d bytes: %w", name, size, err)
			}
			h.Write(chunk[:want])
			clear(chunk[want:])
			left -= want
			if _, err := f.wdev.WriteAt(chunk, int64(e.phys+done)*f.bs); err != nil {
				return fmt.Errorf("ext4: write %s: %w", name, err)
			}
			done += n
		}
	}
	var extra [1]byte
	if n, _ := io.ReadFull(r, extra[:]); n != 0 {
		return fmt.Errorf("ext4: %s: source is longer than %d bytes", name, size)
	}
	if got := h.Sum(nil); string(got) != string(sum[:]) {
		return fmt.Errorf("ext4: %s: source SHA-256 %x does not match %x", name, got, sum)
	}
	meta, err := f.writeExtentTree(in, exts, f.goalFor(parent.num))
	if err != nil {
		return err
	}
	in.setSize(uint64(size))
	in.setLinks(1)
	if err := in.setBlocks512(f, (nblocks+uint64(len(meta)))*uint64(f.bs/512)); err != nil {
		return err
	}
	if err := f.writeInode(in); err != nil {
		return err
	}
	return f.addEntry(parent, name, ino, ftReg)
}

func (f *fsys) symlink(parent *inode, name, target string) error {
	if parent.flags()&(flagEncrypt|flagCasefold) != 0 {
		return fmt.Errorf("%w: directory inode %d is encrypted or case-folded", ErrUnsupported, parent.num)
	}
	if target == "" || len(target) >= int(f.bs) || strings.ContainsRune(target, 0) {
		return fmt.Errorf("ext4: invalid symlink target %q", target)
	}
	ino, err := f.allocInode(f.inodeGroup(parent.num), false)
	if err != nil {
		return err
	}
	in := f.newInodeRaw(ino, sIFLNK|0o777, f.now())
	in.setLinks(1)
	in.setSize(uint64(len(target)))
	if len(target) < inodeBlockLen {
		copy(in.iblock(), target)
	} else {
		ext, err := f.allocData(f.goalFor(parent.num), 1, 0)
		if err != nil {
			return err
		}
		blk := make([]byte, f.bs)
		copy(blk, target)
		if err := f.writeBlock(ext[0].phys, blk); err != nil {
			return err
		}
		if _, err := f.writeExtentTree(in, ext, 0); err != nil {
			return err
		}
		if err := in.setBlocks512(f, uint64(f.bs/512)); err != nil {
			return err
		}
	}
	if err := f.writeInode(in); err != nil {
		return err
	}
	return f.addEntry(parent, name, ino, ftSymlink)
}

// unlink removes parts' final entry. Directories must be empty. The inode
// and its blocks are released when its last link goes.
func (f *fsys) unlink(parts []string) (bool, error) {
	parent, err := f.walkDir(parts[:len(parts)-1])
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	name := parts[len(parts)-1]
	ino, _, err := f.lookup(parent, name)
	if err != nil || ino == 0 {
		return false, err
	}
	in, err := f.readInode(ino)
	if err != nil {
		return false, err
	}
	if in.flags()&(flagEncrypt) != 0 {
		return false, fmt.Errorf("%w: /%s is encrypted; refusing to remove it", ErrUnsupported, strings.Join(parts, "/"))
	}
	if in.isDir() {
		blocks, err := f.readDir(in)
		if err != nil {
			return false, err
		}
		ents, err := f.listEntries(blocks)
		if err != nil {
			return false, err
		}
		if len(ents) > 2 {
			return false, fmt.Errorf("%w: /%s is a non-empty directory", ErrExists, strings.Join(parts, "/"))
		}
	}
	if _, err := f.removeEntry(parent, name); err != nil {
		return false, err
	}
	if in.isDir() {
		if l := parent.links(); l > 2 && l < linkMax {
			parent.setLinks(l - 1)
		}
		if err := f.writeInode(parent); err != nil {
			return false, err
		}
		return true, f.release(in, true)
	}
	if l := in.links(); l > 1 {
		in.setLinks(l - 1)
		in.touch(f.now())
		return true, f.writeInode(in)
	}
	return true, f.release(in, false)
}

// release frees an inode, its data, mapping blocks and its share of an
// external xattr block.
func (f *fsys) release(in *inode, dir bool) error {
	exts, meta, err := f.fileMap(in)
	if err != nil {
		return err
	}
	for _, e := range exts {
		if err := f.freeBlocks(e.phys, uint64(e.length)); err != nil {
			return err
		}
	}
	for _, b := range meta {
		if err := f.freeBlocks(b, 1); err != nil {
			return err
		}
	}
	if acl := in.fileACL(f); acl != 0 {
		if err := f.dropXattrBlock(acl); err != nil {
			return err
		}
	}
	in.setLinks(0)
	s, _ := encodeTime(f.now())
	le.PutUint32(in.raw[inDtime:], s)
	if err := f.writeInode(in); err != nil {
		return err
	}
	return f.freeInode(in.num, dir)
}

const xattrMagic = 0xEA020000

func (f *fsys) xattrBlockCsum(blk uint64, data []byte) uint32 {
	var b8 [8]byte
	le.PutUint64(b8[:], blk)
	c := crc32c(f.csumSeed, b8[:])
	c = crc32c(c, data[:0x10])
	c = crc32c(c, []byte{0, 0, 0, 0})
	return crc32c(c, data[0x14:])
}

func (f *fsys) readXattrBlock(blk uint64) ([]byte, error) {
	data, err := f.readBlock(blk)
	if err != nil {
		return nil, err
	}
	if le.Uint32(data[0:]) != xattrMagic || le.Uint32(data[8:]) != 1 {
		return nil, corruptf("xattr block %d has a bad header", blk)
	}
	if f.hasMetadataCsum() {
		if got, want := le.Uint32(data[0x10:]), f.xattrBlockCsum(blk, data); got != want {
			return nil, corruptf("xattr block %d checksum %#x, computed %#x", blk, got, want)
		}
	}
	return data, nil
}

func (f *fsys) dropXattrBlock(blk uint64) error {
	data, err := f.readXattrBlock(blk)
	if err != nil {
		return err
	}
	ref := le.Uint32(data[4:])
	if ref > 1 {
		le.PutUint32(data[4:], ref-1)
		if f.hasMetadataCsum() {
			le.PutUint32(data[0x10:], f.xattrBlockCsum(blk, data))
		}
		return f.writeBlock(blk, data)
	}
	return f.freeBlocks(blk, 1)
}
