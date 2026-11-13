package azimg

import (
	"fmt"
	"runtime"
	"strings"
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
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c < 32 || c == 127 || c == '"' {
			return fmt.Errorf("%w: unsafe character in host path", ErrUnsafePath)
		}
		if !isWindows && c == '\\' {
			return fmt.Errorf("%w: backslash in POSIX host path", ErrUnsafePath)
		}
	}
	return nil
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// CleanArchivePath validates and returns a canonical relative POSIX archive path.
// It rejects empty paths, absolute paths, UNC paths, Windows drive-letter paths,
// backslashes, control characters, traversal segments ("." / ".."), empty segments ("//"),
// and trailing slashes (unless isDir is true, in which case one trailing slash is stripped).
// A single leading "./" prefix is stripped only when followed by a valid relative path.
func CleanArchivePath(raw string, isDir bool) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: empty archive path", ErrUnsafePath)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < 32 || c == 127 {
			return "", fmt.Errorf("%w: control character in archive path %q", ErrUnsafePath, raw)
		}
		if c == '\\' {
			return "", fmt.Errorf("%w: backslash in archive path %q", ErrUnsafePath, raw)
		}
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("%w: absolute or UNC path %q", ErrUnsafePath, raw)
	}
	if len(raw) >= 2 && isASCIIAlpha(raw[0]) && raw[1] == ':' {
		return "", fmt.Errorf("%w: Windows drive path %q", ErrUnsafePath, raw)
	}
	if isDir && (raw == "." || raw == "./") {
		return ".", nil
	}
	s := strings.TrimPrefix(raw, "./")
	if isDir {
		s = strings.TrimSuffix(s, "/")
	}
	if s == "" || strings.HasSuffix(s, "/") {
		return "", fmt.Errorf("%w: invalid or trailing-slash archive path %q", ErrUnsafePath, raw)
	}
	parts := strings.Split(s, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: non-canonical or traversal segment in %q", ErrUnsafePath, raw)
		}
		if len(seg) >= 2 && isASCIIAlpha(seg[0]) && seg[1] == ':' {
			return "", fmt.Errorf("%w: drive-like segment in %q", ErrUnsafePath, raw)
		}
	}
	return s, nil
}
