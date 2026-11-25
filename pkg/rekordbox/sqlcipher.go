package rekordbox

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Rekordbox OneLibrary SQLCipher 4 passphrase from pyrekordbox/onelibrary/database.py (deobfuscate(BLOB)).
const oneLibraryPassphrase = "r8gddnr4k847830ar6cqzbkk0el6qytmb3trbbx805jm74vez64i5o8fnrqryqls"

const (
	sqlcipherPageSize     = 4096
	sqlcipherSaltSize     = 16
	sqlcipherIVSize       = 16
	sqlcipherHMACSize     = 64
	sqlcipherReserveSize  = sqlcipherIVSize + sqlcipherHMACSize // 80 bytes
	sqlcipherKDFIter      = 256000
	sqlcipherFastKDFIter  = 2
	sqlcipherHMACSaltMask = 0x3a

	// MaxSQLCipherBytes bounds the export that is decrypted. A 20000-track
	// OneLibrary is far below it; the plaintext copy needs the same space on /data.
	MaxSQLCipherBytes = 256 << 20

	sqliteHeaderMagic = "SQLite format 3\x00"
)

// isEncryptedSQLite reports whether path is a non-plaintext file large enough
// to be a SQLCipher database, without opening the file for writing.
func isEncryptedSQLite(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() < int64(len(sqliteHeaderMagic)) {
		return false, nil
	}
	var hdr [16]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false, err
	}
	return string(hdr[:]) != sqliteHeaderMagic, nil
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// DecryptSQLCipher4 streams a SQLCipher 4 (4096-byte page, PBKDF2-HMAC-SHA512
// 256,000 iterations, AES-256-CBC, HMAC-SHA512, 80-byte page reserve) database
// from r to w one 4 KiB page at a time, reconstructing page 1's SQLite header
// with reserve byte 80 and zeroed reserve trailers so B-tree payload sizes
// stay identical to SQLCipher's in-memory pager representation.
func DecryptSQLCipher4(ctx context.Context, r io.Reader, size int64, w io.Writer, passphrase []byte) error {
	return decryptSQLCipher4(ctx, r, size, w, passphrase, nil)
}

func cleanupKey(kc *KeyCache, keys **cipherKeys) func() {
	return func() { kc.done(*keys) }
}

func decryptSQLCipher4(ctx context.Context, r io.Reader, size int64, w io.Writer, passphrase []byte, kc *KeyCache) error {
	if size < sqlcipherPageSize || size%sqlcipherPageSize != 0 {
		return fmt.Errorf("sqlcipher: invalid database size %d (must be a positive multiple of %d)", size, sqlcipherPageSize)
	}
	if size > MaxSQLCipherBytes {
		return fmt.Errorf("sqlcipher: database size %d exceeds %d-byte limit", size, MaxSQLCipherBytes)
	}
	if len(passphrase) == 0 {
		return errors.New("sqlcipher: empty passphrase")
	}

	var inPage, outPage [sqlcipherPageSize]byte
	if _, err := io.ReadFull(r, inPage[:]); err != nil {
		return fmt.Errorf("sqlcipher: read page 1: %w", err)
	}

	salt := append([]byte(nil), inPage[:sqlcipherSaltSize]...)
	keys, fromCache, lease, err := kc.getWithLease(ctx, salt, passphrase)
	if err != nil {
		return err
	}
	defer func() {
		cleanupKey(kc, &keys)()
		lease.release()
	}()

	block, err := aes.NewCipher(keys.enc)
	if err != nil {
		return fmt.Errorf("sqlcipher: init AES-256: %w", err)
	}

	mac := hmac.New(sha512.New, keys.mac)
	var sumBuf [sqlcipherHMACSize]byte
	var pgLE [4]byte
	autoVacuum := false
	retriedCachedKey := false

	totalPages := uint32(size / sqlcipherPageSize)
	const usableEnd = sqlcipherPageSize - sqlcipherReserveSize // 4016
	const ivEnd = usableEnd + sqlcipherIVSize                  // 4032

	for pgno := uint32(1); pgno <= totalPages; pgno++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pgno > 1 {
			if _, err := io.ReadFull(r, inPage[:]); err != nil {
				return fmt.Errorf("sqlcipher: read page %d: %w", pgno, err)
			}
			if autoVacuum && isAllZero(inPage[:]) {
				clear(outPage[:])
				if _, err := w.Write(outPage[:]); err != nil {
					return fmt.Errorf("sqlcipher: write page %d: %w", pgno, err)
				}
				continue
			}
		}

		off := 0
		if pgno == 1 {
			off = sqlcipherSaltSize
		}

		mac.Reset()
		_, _ = mac.Write(inPage[off:ivEnd])
		binary.LittleEndian.PutUint32(pgLE[:], pgno)
		_, _ = mac.Write(pgLE[:])
		gotMAC := mac.Sum(sumBuf[:0])
		if !hmac.Equal(gotMAC, inPage[ivEnd:sqlcipherPageSize]) {
			if pgno == 1 && kc != nil && fromCache && !retriedCachedKey {
				retriedCachedKey = true
				kc.done(keys)
				kc.invalidate(salt, passphrase)
				keys, fromCache, lease, err = kc.getWithLease(ctx, salt, passphrase)
				if err != nil {
					return err
				}
				block, err = aes.NewCipher(keys.enc)
				if err != nil {
					return fmt.Errorf("sqlcipher: init AES-256: %w", err)
				}
				mac = hmac.New(sha512.New, keys.mac)
				pgno = 0
				continue
			}
			return fmt.Errorf("sqlcipher: page %d HMAC verification failed", pgno)
		}
		clear(outPage[:])
		cipher.NewCBCDecrypter(block, inPage[usableEnd:ivEnd]).CryptBlocks(outPage[off:usableEnd], inPage[off:usableEnd])

		if pgno == 1 {
			copy(outPage[:sqlcipherSaltSize], sqliteHeaderMagic)
			rawPageSz := binary.BigEndian.Uint16(outPage[16:18])
			if rawPageSz != sqlcipherPageSize {
				return fmt.Errorf("sqlcipher: unsupported SQLite page size %d", rawPageSz)
			}
			outPage[20] = sqlcipherReserveSize
			autoVacuum = binary.BigEndian.Uint32(outPage[52:56]) != 0
		}

		if _, err := w.Write(outPage[:]); err != nil {
			return fmt.Errorf("sqlcipher: write page %d: %w", pgno, err)
		}
		if pgno == 1 && kc != nil && !fromCache {
			kc.writeShared(salt, passphrase, keys)
			lease.release()
			lease = nil
		}
	}
	return nil
}

// The plaintext copy goes to the EngineX state directory on /data (eMMC), not
// to /tmp, which is a RAM tmpfs on the decks (SC5000: 1 GB of 2 GB RAM).
// Tests and hosts without /data/enginex use a private directory under the
// system temp dir so running go test never writes to a build host's
// /data/enginex/tmp. Never the temp dir itself: prepareOneLibraryTempDir
// chmods its directory to 0700, which turned a root build host's /tmp to 0700.
func selectOneLibraryTempDir(dataRoot string, inTest bool) string {
	if !inTest {
		if st, err := os.Stat(dataRoot); err == nil && st.IsDir() {
			return filepath.Join(dataRoot, "tmp")
		}
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("enginex-onelibrary-%d", os.Geteuid()))
}

var oneLibraryTempDir = func() string {
	return selectOneLibraryTempDir("/data/enginex", testing.Testing())
}

// freeBytes reports the space available to us on the filesystem of dir.
var freeBytes = func(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

const (
	oneLibraryTempPrefix = "rb-onelibrary-"
	oneLibraryTempSlack  = 64 << 20
)

// prepareOneLibraryTempDir creates dir 0700 (never through a symlink) and
// removes plaintext copies left by an rb-bridge that is gone: the mount hook
// SIGKILLs it when the budget runs out, so its deferred remove never ran. The
// owner's PID is in the name; a file of a live process is left alone.
func prepareOneLibraryTempDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("sqlcipher: temporary directory %s is not a directory", dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("sqlcipher: temporary directory %s is not ours", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, oneLibraryTempPrefix+"*.db"))
	for _, m := range matches {
		rest := strings.TrimPrefix(filepath.Base(m), oneLibraryTempPrefix)
		pid, err := strconv.Atoi(rest[:max(strings.IndexByte(rest, '-'), 0)])
		if err == nil && pid > 0 && pid != os.Getpid() && syscall.Kill(pid, 0) == syscall.ESRCH {
			os.Remove(m)
		}
	}
	return nil
}

func decryptSQLCipher4ToTemp(ctx context.Context, srcPath string, passphrase []byte) (string, error) {
	return decryptSQLCipher4ToTempKeys(ctx, srcPath, passphrase, nil)
}

func decryptSQLCipher4ToTempKeys(ctx context.Context, srcPath string, passphrase []byte, kc *KeyCache) (string, error) {
	return decryptSQLCipher4ToTempKeysPaced(ctx, srcPath, passphrase, kc, nil)
}

func decryptSQLCipher4ToTempKeysPaced(ctx context.Context, srcPath string, passphrase []byte, kc *KeyCache, wrap func(io.Reader) io.Reader) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	st, err := src.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("sqlcipher: non-regular database file %s", srcPath)
	}

	if st.Size() > MaxSQLCipherBytes {
		return "", fmt.Errorf("sqlcipher: database size %d exceeds %d-byte limit", st.Size(), MaxSQLCipherBytes)
	}
	dir := oneLibraryTempDir()
	if err := prepareOneLibraryTempDir(dir); err != nil {
		return "", err
	}
	free, err := freeBytes(dir)
	if err != nil {
		return "", err
	}
	if need := uint64(2*st.Size() + oneLibraryTempSlack); free < need {
		return "", fmt.Errorf("sqlcipher: %d bytes free in %s, %d needed to decrypt the export", free, dir, need)
	}
	tmp, err := os.CreateTemp(dir, fmt.Sprintf("%s%d-*.db", oneLibraryTempPrefix, os.Getpid()))
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", err
	}

	var r io.Reader = io.LimitReader(src, st.Size())
	if wrap != nil {
		r = wrap(r)
	}
	bw := bufio.NewWriterSize(tmp, 64*1024)
	if err := decryptSQLCipher4(ctx, r, st.Size(), bw, passphrase, kc); err != nil {
		return "", err
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	ok = true
	return tmpName, nil
}

// ReadSQLCipherSalt reads the 16-byte page-1 salt from an existing SQLCipher database.
func ReadSQLCipherSalt(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	salt := make([]byte, sqlcipherSaltSize)
	if _, err := io.ReadFull(f, salt); err != nil {
		return nil, fmt.Errorf("sqlcipher: read salt: %w", err)
	}
	return salt, nil
}

// EncryptSQLCipher4 encrypts a plaintext SQLite database (with 4096-byte pages
// and 80-byte reserve space) using SQLCipher 4 defaults (PBKDF2-HMAC-SHA512,
// 256000 iterations, AES-256-CBC, per-page random IV + HMAC-SHA512 trailer).
func EncryptSQLCipher4(ctx context.Context, r io.Reader, size int64, w io.Writer, salt []byte, passphrase []byte) error {
	return encryptSQLCipher4(ctx, r, size, w, salt, passphrase, nil)
}

func encryptSQLCipher4(ctx context.Context, r io.Reader, size int64, w io.Writer, salt []byte, passphrase []byte, kc *KeyCache) error {
	if size < sqlcipherPageSize || size%sqlcipherPageSize != 0 {
		return fmt.Errorf("sqlcipher: invalid database size %d (must be a positive multiple of %d)", size, sqlcipherPageSize)
	}
	if size > MaxSQLCipherBytes {
		return fmt.Errorf("sqlcipher: database size %d exceeds %d-byte limit", size, MaxSQLCipherBytes)
	}
	if len(salt) != sqlcipherSaltSize {
		return fmt.Errorf("sqlcipher: invalid salt length %d", len(salt))
	}
	if len(passphrase) == 0 {
		return errors.New("sqlcipher: empty passphrase")
	}

	keys, fromCache, lease, err := kc.getWithLease(ctx, salt, passphrase)
	if err != nil {
		return err
	}
	defer func() {
		cleanupKey(kc, &keys)()
		lease.release()
	}()

	block, err := aes.NewCipher(keys.enc)
	if err != nil {
		return fmt.Errorf("sqlcipher: init AES-256: %w", err)
	}

	mac := hmac.New(sha512.New, keys.mac)
	var inPage, outPage [sqlcipherPageSize]byte
	var sumBuf [sqlcipherHMACSize]byte
	var pgLE [4]byte
	autoVacuum := false

	totalPages := uint32(size / sqlcipherPageSize)
	const usableEnd = sqlcipherPageSize - sqlcipherReserveSize // 4016
	const ivEnd = usableEnd + sqlcipherIVSize                  // 4032

	for pgno := uint32(1); pgno <= totalPages; pgno++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.ReadFull(r, inPage[:]); err != nil {
			return fmt.Errorf("sqlcipher: read plaintext page %d: %w", pgno, err)
		}

		clear(outPage[:])
		off := 0
		if pgno == 1 {
			if string(inPage[:sqlcipherSaltSize]) != sqliteHeaderMagic {
				return errors.New("sqlcipher: plaintext page 1 missing SQLite header")
			}
			if rawPageSz := binary.BigEndian.Uint16(inPage[16:18]); rawPageSz != sqlcipherPageSize {
				return fmt.Errorf("sqlcipher: unsupported SQLite page size %d", rawPageSz)
			}
			if inPage[20] != sqlcipherReserveSize {
				return fmt.Errorf("sqlcipher: unsupported SQLite reserve size %d (want %d)", inPage[20], sqlcipherReserveSize)
			}
			autoVacuum = binary.BigEndian.Uint32(inPage[52:56]) != 0
			copy(outPage[:sqlcipherSaltSize], salt)
			off = sqlcipherSaltSize
		} else if autoVacuum && isAllZero(inPage[:]) {
			if _, err := w.Write(outPage[:]); err != nil {
				return fmt.Errorf("sqlcipher: write page %d: %w", pgno, err)
			}
			continue
		}

		if _, err := rand.Read(outPage[usableEnd:ivEnd]); err != nil {
			return fmt.Errorf("sqlcipher: generate IV for page %d: %w", pgno, err)
		}
		cipher.NewCBCEncrypter(block, outPage[usableEnd:ivEnd]).CryptBlocks(outPage[off:usableEnd], inPage[off:usableEnd])

		mac.Reset()
		_, _ = mac.Write(outPage[off:ivEnd])
		binary.LittleEndian.PutUint32(pgLE[:], pgno)
		_, _ = mac.Write(pgLE[:])
		copy(outPage[ivEnd:sqlcipherPageSize], mac.Sum(sumBuf[:0]))

		if _, err := w.Write(outPage[:]); err != nil {
			return fmt.Errorf("sqlcipher: write page %d: %w", pgno, err)
		}
		if pgno == 1 && kc != nil && !fromCache {
			kc.writeShared(salt, passphrase, keys)
			lease.release()
			lease = nil
		}
	}
	return nil
}
