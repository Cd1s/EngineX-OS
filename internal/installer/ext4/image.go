package ext4

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Entry is one regular file to install.
type Entry struct {
	Path   string // canonical image-relative POSIX path
	Mode   uint32 // permission bits; type bits, if present, must say regular file
	Size   int64
	SHA256 [32]byte
	Open   func() (io.ReadCloser, error)
}

// ChangeSet is everything one injection does. Apply order: Remove,
// Directories, Files (missing parents are created 0755), Links.
type ChangeSet struct {
	Directories []string
	Files       []Entry
	Links       map[string]string // path -> target, stored verbatim
	Remove      []string
}

// CreateOptions configure a fresh image.
type CreateOptions struct {
	Size  int64  // partition size in bytes; rounded down to 4 KiB blocks
	Label string // volume label, at most 16 bytes ("az01-data" or "data")
	Now   func() time.Time
}

// Image is an open filesystem image file.
type Image struct {
	path string
	file *os.File
	fs   *fsys
	ro   bool
}

// Create makes a fresh filesystem in a new file at dest. dest must not
// exist; on error nothing is left behind.
func Create(ctx context.Context, dest string, opts CreateOptions) (img *Image, err error) {
	if len(opts.Label) > 16 {
		return nil, fmt.Errorf("ext4: label %q is longer than 16 bytes", opts.Label)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	p := mkfsParams{size: opts.Size, label: opts.Label, now: now()}
	if p.uuid, err = randomUUID(); err != nil {
		return nil, err
	}
	if p.hash, err = randomUUID(); err != nil {
		return nil, err
	}
	fh, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			fh.Close()
			os.Remove(dest)
		}
	}()
	if err = fh.Truncate(opts.Size); err != nil {
		return nil, err
	}
	f, err := mkfs(fh, p)
	if err != nil {
		return nil, err
	}
	f.now = now
	if err = fh.Sync(); err != nil {
		return nil, err
	}
	return &Image{path: dest, file: fh, fs: f}, nil
}

type fileSource struct {
	*os.File
	size int64
}

func (s fileSource) Size() int64 { return s.size }

// OpenFileSource opens a backup read-only as a Source.
func OpenFileSource(path string) (Source, io.Closer, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	st, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		fh.Close()
		return nil, nil, fmt.Errorf("ext4: %s is not a regular file", path)
	}
	return fileSource{fh, st.Size()}, fh, nil
}

// OpenCopy probes original and, only if it is supported and clean, copies
// it byte for byte to the new file dest and opens the copy for changes.
// original is only ever read. All-zero blocks become holes in the copy;
// they read back as the same zeros.
func OpenCopy(ctx context.Context, original Source, dest string) (img *Image, err error) {
	if _, err := Probe(original); err != nil {
		return nil, err
	}
	if of, ok := original.(fileSource); ok {
		if st, err := os.Stat(dest); err == nil {
			if ost, err2 := of.Stat(); err2 == nil && os.SameFile(st, ost) {
				return nil, errors.New("ext4: refusing to use the backup itself as the output")
			}
		}
	}
	fh, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			fh.Close()
			os.Remove(dest)
		}
	}()
	size := original.Size()
	if err = fh.Truncate(size); err != nil {
		return nil, err
	}
	buf := make([]byte, 1<<20)
	for off := int64(0); off < size; off += int64(len(buf)) {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		n := min(int64(len(buf)), size-off)
		chunk := buf[:n]
		if _, err = original.ReadAt(chunk, off); err != nil && !(errors.Is(err, io.EOF) && off+n == size) {
			return nil, fmt.Errorf("ext4: read backup: %w", err)
		}
		err = nil
		for b := int64(0); b < n; b += 4096 {
			blk := chunk[b:min(b+4096, n)]
			if !allZero(blk) {
				if _, err = fh.WriteAt(blk, off+b); err != nil {
					return nil, err
				}
			}
		}
	}
	if err = fh.Sync(); err != nil {
		return nil, err
	}
	f, err := openFS(fh, size, true)
	if err != nil {
		return nil, err
	}
	if err = f.checkClean(); err != nil {
		return nil, err
	}
	return &Image{path: dest, file: fh, fs: f}, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Open opens a finished image read-only for Verify and Check.
func Open(path string) (*Image, error) {
	src, c, err := OpenFileSource(path)
	if err != nil {
		return nil, err
	}
	fs := src.(fileSource)
	f, err := openFS(fs, fs.size, false)
	if err != nil {
		c.Close()
		return nil, err
	}
	return &Image{path: path, file: fs.File, fs: f, ro: true}, nil
}

// Profile returns the filesystem's current profile.
func (img *Image) Profile() *Profile { return img.fs.profile() }

// Close flushes metadata, syncs and closes the file.
func (img *Image) Close() error {
	if img.file == nil {
		return nil
	}
	var err error
	if !img.ro {
		err = img.fs.flush()
		if e := img.file.Sync(); err == nil {
			err = e
		}
	}
	if e := img.file.Close(); err == nil {
		err = e
	}
	img.file = nil
	return err
}

// Kind of a path inside the image.
type Kind int

const (
	Missing Kind = iota
	Directory
	Regular
	Symlink
	Other
)

func (k Kind) String() string {
	return [...]string{"missing", "directory", "regular", "symlink", "other"}[k]
}

// Lstat reports the type of path without following symlinks. An
// intermediate symlink is an ErrUnsafePath error.
func (img *Image) Lstat(path string) (Kind, error) {
	in, err := img.fs.lstat(path)
	if err != nil || in == nil {
		return Missing, err
	}
	switch {
	case in.isDir():
		return Directory, nil
	case in.isReg():
		return Regular, nil
	case in.isSymlink():
		return Symlink, nil
	}
	return Other, nil
}

// ReadFile returns a regular file's content (at most limit bytes).
func (img *Image) ReadFile(path string, limit int64) ([]byte, error) {
	in, err := img.fs.lstat(path)
	if err != nil {
		return nil, err
	}
	if in == nil || !in.isReg() {
		return nil, fmt.Errorf("ext4: %s is not a regular file", path)
	}
	if in.flags()&flagEncrypt != 0 {
		return nil, fmt.Errorf("%w: %s is encrypted", ErrUnsupported, path)
	}
	return img.fs.readAll(in, uint64(limit))
}

// Readlink returns a symlink's stored target.
func (img *Image) Readlink(path string) (string, error) {
	in, err := img.fs.lstat(path)
	if err != nil {
		return "", err
	}
	if in == nil || !in.isSymlink() {
		return "", fmt.Errorf("ext4: %s is not a symlink", path)
	}
	b, err := img.fs.readAll(in, uint64(img.fs.bs))
	return string(b), err
}

// Stat returns permission bits, size, uid and gid of a path (no follow).
func (img *Image) Stat(path string) (mode uint32, size int64, uid, gid uint32, err error) {
	in, err := img.fs.lstat(path)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if in == nil {
		return 0, 0, 0, 0, fmt.Errorf("%w: %s", errNotFound, path)
	}
	return uint32(in.mode()), int64(in.size()), in.uid(), in.gid(), nil
}

// ChooseRuntimeDir applies the #103 rule: a deck that still has a real
// legacy /sc5000-custom directory and no /enginex keeps being written
// there (first boot migrates it); otherwise files go to /enginex and, if
// /sc5000-custom is absent, the compatibility link sc5000-custom -> enginex
// is created.
func (img *Image) ChooseRuntimeDir() (root string, links map[string]string, err error) {
	legacy, err := img.Lstat("/sc5000-custom")
	if err != nil {
		return "", nil, err
	}
	cur, err := img.Lstat("/enginex")
	if err != nil {
		return "", nil, err
	}
	if legacy == Directory && cur == Missing {
		return "sc5000-custom", map[string]string{}, nil
	}
	if cur != Missing && cur != Directory {
		return "", nil, fmt.Errorf("%w: /enginex is a %s", ErrExists, cur)
	}
	if legacy == Missing {
		return "enginex", map[string]string{"sc5000-custom": "enginex"}, nil
	}
	return "enginex", map[string]string{}, nil
}

type plan struct {
	removes []([]string)
	dirs    [][]string
	files   []fileOp
	links   []linkOp
}

type fileOp struct {
	parts []string
	e     Entry
	perm  uint16
}

type linkOp struct {
	parts  []string
	target string
}

func (cs ChangeSet) plan() (*plan, error) {
	p := &plan{}
	seen := map[string]string{}
	claim := func(parts []string, what string) error {
		k := strings.Join(parts, "/")
		if k == "" {
			return fmt.Errorf("%w: the root directory cannot be a %s", ErrUnsafePath, what)
		}
		if prev, ok := seen[k]; ok && !(prev == "dir" && what == "dir") {
			return fmt.Errorf("%w: /%s is listed as both %s and %s", ErrUnsafePath, k, prev, what)
		}
		seen[k] = what
		return nil
	}
	for _, r := range cs.Remove {
		parts, err := splitPath(r)
		if err != nil {
			return nil, err
		}
		if err := claim(parts, "remove"); err != nil {
			return nil, err
		}
		p.removes = append(p.removes, parts)
	}
	for _, d := range cs.Directories {
		parts, err := splitPath(d)
		if err != nil {
			return nil, err
		}
		if err := claim(parts, "dir"); err != nil {
			return nil, err
		}
		p.dirs = append(p.dirs, parts)
	}
	sort.Slice(p.dirs, func(i, j int) bool { return len(p.dirs[i]) < len(p.dirs[j]) })
	for _, e := range cs.Files {
		parts, err := splitPath(e.Path)
		if err != nil {
			return nil, err
		}
		if err := claim(parts, "file"); err != nil {
			return nil, err
		}
		if e.Mode&sIFMT != 0 && e.Mode&sIFMT != sIFREG || e.Mode>>16 != 0 {
			return nil, fmt.Errorf("ext4: %s: mode %#o is not a regular file mode", e.Path, e.Mode)
		}
		if e.Open == nil || e.Size < 0 {
			return nil, fmt.Errorf("ext4: %s: no content", e.Path)
		}
		p.files = append(p.files, fileOp{parts, e, uint16(e.Mode & 0o7777)})
	}
	keys := make([]string, 0, len(cs.Links))
	for k := range cs.Links {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts, err := splitPath(k)
		if err != nil {
			return nil, err
		}
		if err := claim(parts, "link"); err != nil {
			return nil, err
		}
		p.links = append(p.links, linkOp{parts, cs.Links[k]})
	}
	return p, nil
}

// Apply performs a change set on a writable image. Every path is checked
// and the space needed is computed before the first write; a change that
// does not fit fails with ErrNoSpace and leaves the image unmodified.
func (img *Image) Apply(ctx context.Context, cs ChangeSet) error {
	if img.ro {
		return errors.New("ext4: image is open read-only")
	}
	p, err := cs.plan()
	if err != nil {
		return err
	}
	if err := img.preflight(p); err != nil {
		return err
	}
	f := img.fs
	for _, parts := range p.removes {
		if _, err := f.unlink(parts); err != nil {
			return err
		}
	}
	for _, parts := range p.dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := f.mkdirAll(parts, 0o755); err != nil {
			return err
		}
	}
	for _, op := range p.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := img.installFile(op); err != nil {
			return fmt.Errorf("install /%s: %w", strings.Join(op.parts, "/"), err)
		}
	}
	for _, l := range p.links {
		if err := img.installLink(l); err != nil {
			return fmt.Errorf("link /%s: %w", strings.Join(l.parts, "/"), err)
		}
	}
	return f.flush()
}

func (img *Image) installFile(op fileOp) error {
	f := img.fs
	name := op.parts[len(op.parts)-1]
	parent, err := f.mkdirAll(op.parts[:len(op.parts)-1], 0o755)
	if err != nil {
		return err
	}
	ino, _, err := f.lookup(parent, name)
	if err != nil {
		return err
	}
	if ino != 0 {
		old, err := f.readInode(ino)
		if err != nil {
			return err
		}
		if !old.isReg() {
			return fmt.Errorf("%w: it is not a regular file", ErrExists)
		}
		if _, err := f.unlink(op.parts); err != nil {
			return err
		}
		if parent, err = f.readInode(parent.num); err != nil {
			return err
		}
	}
	rc, err := op.e.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return f.createFile(parent, name, op.perm, op.e.Size, op.e.SHA256, rc)
}

func (img *Image) installLink(l linkOp) error {
	f := img.fs
	name := l.parts[len(l.parts)-1]
	parent, err := f.mkdirAll(l.parts[:len(l.parts)-1], 0o755)
	if err != nil {
		return err
	}
	ino, _, err := f.lookup(parent, name)
	if err != nil {
		return err
	}
	if ino != 0 {
		old, err := f.readInode(ino)
		if err != nil {
			return err
		}
		if !old.isSymlink() {
			return fmt.Errorf("%w: it exists and is not a symlink", ErrExists)
		}
		if t, err := f.readAll(old, uint64(f.bs)); err == nil && string(t) == l.target {
			return nil
		}
		if _, err := f.unlink(l.parts); err != nil {
			return err
		}
		if parent, err = f.readInode(parent.num); err != nil {
			return err
		}
	}
	return f.symlink(parent, name, l.target)
}

// Verify re-reads every path of the change set: each file's mode, size and
// SHA-256, each directory, each link target, and the absence of each
// removed path. Use it on an image reopened with Open after Close.
func (img *Image) Verify(ctx context.Context, cs ChangeSet) error {
	p, err := cs.plan()
	if err != nil {
		return err
	}
	f := img.fs
	var bad []string
	for _, parts := range p.removes {
		in, err := f.lstat(strings.Join(parts, "/"))
		if err != nil {
			bad = append(bad, fmt.Sprintf("/%s: %v", strings.Join(parts, "/"), err))
		} else if in != nil {
			bad = append(bad, fmt.Sprintf("/%s is still present", strings.Join(parts, "/")))
		}
	}
	for _, parts := range p.dirs {
		k, err := img.Lstat(strings.Join(parts, "/"))
		if err != nil || k != Directory {
			bad = append(bad, fmt.Sprintf("/%s is %v, not a directory (%v)", strings.Join(parts, "/"), k, err))
		}
	}
	for _, op := range p.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := "/" + strings.Join(op.parts, "/")
		in, err := f.lstat(path)
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: %v", path, err))
			continue
		case in == nil || !in.isReg():
			bad = append(bad, fmt.Sprintf("%s is missing", path))
			continue
		case in.mode()&0o7777 != op.perm:
			bad = append(bad, fmt.Sprintf("%s has mode %04o, not %04o", path, in.mode()&0o7777, op.perm))
		case int64(in.size()) != op.e.Size:
			bad = append(bad, fmt.Sprintf("%s has %d bytes, not %d", path, in.size(), op.e.Size))
			continue
		}
		sum, err := f.hashFile(in)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", path, err))
		} else if sum != op.e.SHA256 {
			bad = append(bad, fmt.Sprintf("%s content differs (SHA-256 %x)", path, sum))
		}
	}
	for _, l := range p.links {
		path := "/" + strings.Join(l.parts, "/")
		t, err := img.Readlink(path)
		if err != nil || t != l.target {
			bad = append(bad, fmt.Sprintf("%s is not a link to %q (%q, %v)", path, l.target, t, err))
		}
	}
	if len(bad) != 0 {
		return &VerifyError{Problems: bad}
	}
	return nil
}

// VerifyError lists every readback mismatch.
type VerifyError struct{ Problems []string }

func (e *VerifyError) Error() string {
	s := e.Problems
	more := ""
	if len(s) > 10 {
		more = fmt.Sprintf(" (+%d more)", len(s)-10)
		s = s[:10]
	}
	return "ext4: readback failed: " + strings.Join(s, "; ") + more
}

func (f *fsys) hashFile(in *inode) ([32]byte, error) {
	h := sha256.New()
	buf := make([]byte, 1<<20)
	size := in.size()
	for off := uint64(0); off < size; off += uint64(len(buf)) {
		n := min(uint64(len(buf)), size-off)
		if err := f.readRange(in, buf[:n], off); err != nil {
			return [32]byte{}, err
		}
		h.Write(buf[:n])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// VerifyFile is the publish gate: it reopens a closed image read-only, runs
// the structural check and verifies the change set.
func VerifyFile(ctx context.Context, path string, cs ChangeSet) error {
	img, err := Open(path)
	if err != nil {
		return err
	}
	defer img.Close()
	if err := img.Check(ctx); err != nil {
		return err
	}
	return img.Verify(ctx, cs)
}
