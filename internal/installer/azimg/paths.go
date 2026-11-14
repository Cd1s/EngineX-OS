package azimg

import (
	"fmt"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	// MaxArchivePathLen is the maximum allowed byte length of an archive path.
	MaxArchivePathLen = 4096
	// MaxArchiveSegmentLen is the maximum allowed byte length of a single path component.
	MaxArchiveSegmentLen = 255
)

// ValidateHostPathOS validates a host OS filesystem path using the current runtime's OS rules.
func ValidateHostPathOS(path string) error {
	return ValidateHostPath(path, runtime.GOOS == "windows")
}

// ValidateHostPath validates a host filesystem path while distinguishing Windows host paths
// (where '\' is a valid separator) from POSIX host paths (where '\' is rejected).
// Matches az01_packer.py's safe_host_path / BH2-A7-1.
func ValidateHostPath(path string, isWindows bool) error {
	if path == "" {
		return fmt.Errorf("%w: empty host path", ErrUnsafePath)
	}
	if !utf8.ValidString(path) {
		return fmt.Errorf("%w: invalid UTF-8 in host path", ErrUnsafePath)
	}
	for _, r := range path {
		if isUnsafeControlOrBidi(r) || r == '"' {
			return fmt.Errorf("%w: unsafe character in host path", ErrUnsafePath)
		}
		if !isWindows && r == '\\' {
			return fmt.Errorf("%w: backslash in POSIX host path", ErrUnsafePath)
		}
	}
	return nil
}

func isUnsafeControlOrBidi(r rune) bool {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
		return true
	}
	switch {
	case r >= 0x202a && r <= 0x202e: // LRE, RLE, PDF, LRO, RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0x200e || r == 0x200f || r == 0x061c: // LRM, RLM, ALM
		return true
	}
	return false
}

func isWindowsReservedName(seg string) bool {
	stem := seg
	if dot := strings.IndexByte(seg, '.'); dot >= 0 {
		stem = seg[:dot]
	}
	if len(stem) < 3 || len(stem) > 4 {
		return false
	}
	var buf [4]byte
	for i := 0; i < len(stem); i++ {
		c := stem[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		buf[i] = c
	}
	switch string(buf[:len(stem)]) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	case "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9":
		return true
	case "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

// CleanArchivePath validates and returns a canonical relative POSIX archive path without
// allocating heap buffers on rejected paths.
// It rejects empty paths, paths >4096 bytes, segments >255 bytes, invalid UTF-8,
// C0/DEL/C1/bidi controls, backslashes, colons (Windows drives and NTFS ADS),
// leading slashes (absolute/UNC paths), traversal segments ("." / ".."), empty segments ("//"),
// trailing dots/spaces on segments, Windows reserved device names (CON, PRN, AUX, NUL, COM1-9, LPT1-9),
// and trailing slashes (unless isDir is true, in which case one trailing slash is stripped).
func CleanArchivePath(raw string, isDir bool) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: empty archive path", ErrUnsafePath)
	}
	if len(raw) > MaxArchivePathLen {
		return "", fmt.Errorf("%w: archive path length %d exceeds %d", ErrUnsafePath, len(raw), MaxArchivePathLen)
	}
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("%w: invalid UTF-8 in archive path", ErrUnsafePath)
	}
	for _, r := range raw {
		if isUnsafeControlOrBidi(r) {
			return "", fmt.Errorf("%w: unsafe control or bidi character in archive path", ErrUnsafePath)
		}
		if r == '\\' || r == ':' {
			return "", fmt.Errorf("%w: forbidden character %q in archive path", ErrUnsafePath, r)
		}
	}
	if raw[0] == '/' {
		return "", fmt.Errorf("%w: absolute or UNC archive path", ErrUnsafePath)
	}
	if isDir && (raw == "." || raw == "./") {
		return ".", nil
	}
	s := strings.TrimPrefix(raw, "./")
	if isDir {
		s = strings.TrimSuffix(s, "/")
	}
	if s == "" || s[len(s)-1] == '/' {
		return "", fmt.Errorf("%w: invalid or trailing-slash archive path", ErrUnsafePath)
	}

	// Scan segments in place without allocating a slice.
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '/' {
			continue
		}
		seg := s[start:i]
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: non-canonical or traversal segment in archive path", ErrUnsafePath)
		}
		if len(seg) > MaxArchiveSegmentLen {
			return "", fmt.Errorf("%w: archive path segment length %d exceeds %d", ErrUnsafePath, len(seg), MaxArchiveSegmentLen)
		}
		last := seg[len(seg)-1]
		if last == '.' || last == ' ' {
			return "", fmt.Errorf("%w: trailing dot or space in archive path segment", ErrUnsafePath)
		}
		if isWindowsReservedName(seg) {
			return "", fmt.Errorf("%w: Windows reserved device name in archive path", ErrUnsafePath)
		}
		start = i + 1
	}
	return s, nil
}

// foldArchivePath returns the NFC-normalized, case-folded collision key for a cleaned
// archive path to detect case-insensitive and NFC/NFD duplicates on macOS/Windows hosts.
func foldArchivePath(cleaned string) string {
	return strings.ToLower(norm.NFC.String(cleaned))
}
