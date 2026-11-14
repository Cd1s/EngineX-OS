// Package ext4 creates, copies, mutates, reads back and checks the ext4
// /data images the installer flashes to second-generation decks, without
// mke2fs, debugfs, e2fsck or a host mount.
//
// Scope and safety rules (#255 work package C):
//
//   - Fresh images use the stock deck profile minus resize_inode,
//     orphan_file and metadata_csum_seed (see create.go). The encrypt
//     feature bit is carried so the deck's fscrypt keeps working; no file
//     written here is encrypted.
//   - Backups are probed before any output exists. Unknown features, a
//     journal that needs recovery, pending orphans, an unclean or errored
//     filesystem and any checksum or structure error are refused.
//   - A backup is never modified in place: OpenCopy writes a private copy to
//     a new path (O_EXCL) and only that copy is changed.
//   - Paths never follow symlinks. Encrypted and case-folded directories are
//     never modified; encrypted files are never read or removed.
//   - Holes in an image produced here are filesystem zeros (journal, unused
//     inode table space). The whole image, zeros included, must reach the
//     partition; a protocol may encode zero runs as FILL chunks but not as
//     DONT_CARE.
package ext4

import "errors"

// Errors returned before any output is published.
var (
	ErrNotExt4     = errors.New("ext4: not an ext4 filesystem")
	ErrUnsupported = errors.New("ext4: unsupported filesystem profile")
	ErrDirty       = errors.New("ext4: filesystem is not clean")
	ErrCorrupt     = errCorrupt
	ErrNoSpace     = errors.New("ext4: not enough space in the filesystem")
	ErrUnsafePath  = errors.New("ext4: unsafe path")
	ErrExists      = errors.New("ext4: path exists with a different type")
)
