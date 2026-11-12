package rekordbox

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	// MaxSQLCipherBytes bounds decrypted export size to 256 MiB.
	MaxSQLCipherBytes = 256 << 20

	sqlcipherFreeHeadroomBytes = 64 << 20 // 64 MiB

	sqliteHeaderMagic   = "SQLite format 3\x00"
	sqlcipherTempPrefix = "rb-onelibrary-"
)

var (
	// deckDataRoot is "/data" on Engine OS decks (ext4 eMMC, avoiding RAM tmpfs /tmp);
	// tests override this or sqlcipherTempDirOverride to use an isolated directory.
	deckDataRoot             = "/data"
	sqlcipherTempDirOverride string
	availableBytesFn         = platformAvailableBytes
)

func resolveSQLCipherTempDirPath() string {
	if sqlcipherTempDirOverride != "" {
		return sqlcipherTempDirOverride
	}
	if deckDataRoot != "" {
		if st, err := os.Stat(filepath.Join(deckDataRoot, "enginex")); err == nil && st.IsDir() {
			return filepath.Join(deckDataRoot, "enginex", "tmp")
		}
		if st, err := os.Stat(deckDataRoot); err == nil && st.IsDir() {
			return filepath.Join(deckDataRoot, "enginex", "tmp")
		}
	}
	return filepath.Join(os.TempDir(), "enginex-rb-tmp")
}

func ensureSQLCipherTempDir() (string, error) {
	dir := resolveSQLCipherTempDirPath()
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("sqlcipher: unsafe temporary directory %s", dir)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("sqlcipher: unsafe temporary directory %s", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func parseOneLibraryTempPID(name string) (int, bool) {
	if !strings.HasPrefix(name, sqlcipherTempPrefix) || !strings.HasSuffix(name, ".db") {
		return 0, false
	}
	rest := strings.TrimPrefix(name, sqlcipherTempPrefix)
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(rest[:dash])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func sweepExistingSQLCipherTempDir() {
	dir := resolveSQLCipherTempDirPath()
	if st, err := os.Lstat(dir); err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
		sweepStaleSQLCipherTemps(dir)
	}
}

func sweepStaleSQLCipherTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	selfPID := os.Getpid()
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasPrefix(name, sqlcipherTempPrefix) {
			continue
		}
		if pid, ok := parseOneLibraryTempPID(name); ok {
			if pid == selfPID || platformProcessAlive(pid) {
				continue
			}
		}
		platformRemoveIfUnlocked(filepath.Join(dir, name))
	}
}

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

	salt := inPage[:sqlcipherSaltSize]
	encKey, err := pbkdf2.Key(sha512.New, string(passphrase), salt, sqlcipherKDFIter, 32)
	if err != nil {
		return fmt.Errorf("sqlcipher: derive encryption key: %w", err)
	}
	defer clear(encKey)

	var macSalt [sqlcipherSaltSize]byte
	for i := 0; i < sqlcipherSaltSize; i++ {
		macSalt[i] = salt[i] ^ sqlcipherHMACSaltMask
	}
	macKey, err := pbkdf2.Key(sha512.New, string(encKey), macSalt[:], sqlcipherFastKDFIter, 32)
	if err != nil {
		return fmt.Errorf("sqlcipher: derive HMAC key: %w", err)
	}
	defer clear(macKey)

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return fmt.Errorf("sqlcipher: init AES-256: %w", err)
	}

	mac := hmac.New(sha512.New, macKey)
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
	}
	return nil
}

func decryptSQLCipher4ToTemp(ctx context.Context, srcPath string, passphrase []byte) (string, func(), error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", nil, err
	}
	defer src.Close()

	st, err := src.Stat()
	if err != nil {
		return "", nil, err
	}
	if !st.Mode().IsRegular() {
		return "", nil, fmt.Errorf("sqlcipher: non-regular database file %s", srcPath)
	}
	size := st.Size()
	if size < sqlcipherPageSize || size%sqlcipherPageSize != 0 {
		return "", nil, fmt.Errorf("sqlcipher: invalid database size %d (must be a positive multiple of %d)", size, sqlcipherPageSize)
	}
	if size > MaxSQLCipherBytes {
		return "", nil, fmt.Errorf("sqlcipher: database size %d exceeds %d-byte limit", size, MaxSQLCipherBytes)
	}

	tmpDir, err := ensureSQLCipherTempDir()
	if err != nil {
		return "", nil, err
	}
	sweepStaleSQLCipherTemps(tmpDir)

	required := uint64(size)*2 + sqlcipherFreeHeadroomBytes
	avail, err := availableBytesFn(tmpDir)
	if err != nil {
		return "", nil, fmt.Errorf("sqlcipher: check free space in %s: %w", tmpDir, err)
	}
	if avail < required {
		return "", nil, fmt.Errorf("sqlcipher: insufficient free space in %s: have %d bytes, need %d bytes", tmpDir, avail, required)
	}

	tmp, err := os.CreateTemp(tmpDir, fmt.Sprintf("%s%d-*.db", sqlcipherTempPrefix, os.Getpid()))
	if err != nil {
		return "", nil, err
	}
	tmpName := tmp.Name()
	locked := false
	ok := false
	cleanup := func() {
		_ = os.Remove(tmpName)
		if locked {
			platformUnlockTempFile(tmp)
		}
		_ = tmp.Close()
	}
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	if err := platformLockTempFile(tmp); err != nil {
		return "", nil, err
	}
	locked = true
	if err := tmp.Chmod(0o600); err != nil {
		return "", nil, err
	}

	bw := bufio.NewWriterSize(tmp, 64*1024)
	if err := DecryptSQLCipher4(ctx, io.LimitReader(src, size), size, bw, passphrase); err != nil {
		return "", nil, err
	}
	if err := bw.Flush(); err != nil {
		return "", nil, err
	}
	ok = true
	return tmpName, cleanup, nil
}
