package rekordbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReviewSQLCipherCorruptPageOneStopsAfterOneRetry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENGINEX_RB_SHARED_KEY_CACHE", dir)
	path := filepath.Join(t.TempDir(), "corrupt_onelibrary.db")
	raw := make([]byte, sqlcipherPageSize)
	copy(raw[:sqlcipherSaltSize], []byte("HOSTILESALT12345"))
	raw[100] = 0xaa
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	kc := NewKeyCache()
	before := KeyDerivations()
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	_, err := ReadSQLiteKeys(ctx, path, nil, kc)
	after := KeyDerivations()
	if errors.Is(err, context.DeadlineExceeded) || after-before > 2 {
		t.Fatalf("corrupt page one retried derivation indefinitely: derivations=%d err=%v", after-before, err)
	}
	if err == nil || !strings.Contains(err.Error(), "page 1 HMAC") {
		t.Fatalf("corrupt page one error = %v, want an HMAC error", err)
	}
}

func TestReviewKeyCacheInvalidateAndClearAreRaceFree(t *testing.T) {
	t.Setenv("ENGINEX_RB_SHARED_KEY_CACHE", t.TempDir())
	kc := NewKeyCache()
	salt, pass := []byte("RACESALT12345678"), []byte("shortpass")
	if _, err := kc.get(salt, pass); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			kc.invalidate(salt, pass)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			kc.Clear()
		}
	}()
	wg.Wait()
}

func TestReviewKeyCacheReturnsIndependentCopies(t *testing.T) {
	kc := NewKeyCache()
	salt, pass := []byte("CLONESALT1234567"), []byte("shortpass")
	first, err := kc.get(salt, pass)
	if err != nil {
		t.Fatal(err)
	}
	second, err := kc.get(salt, pass)
	if err != nil {
		t.Fatal(err)
	}
	kc.Clear()
	if isAllZero(first.enc) || isAllZero(first.mac) || isAllZero(second.enc) || isAllZero(second.mac) {
		t.Fatal("clearing the cache zeroed keys still held by callers")
	}
	kc.done(first)
	kc.done(second)
}

func TestReviewSharedKeyDirectoryIsBoundedAndLeavesNoPerKeyLocks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENGINEX_RB_SHARED_KEY_CACHE", dir)
	kc := NewKeyCache()
	for i := 0; i < 12; i++ {
		var salt [sqlcipherSaltSize]byte
		binary.LittleEndian.PutUint64(salt[:8], uint64(i+1))
		copy(salt[8:], []byte("saltseed"))
		key := &cipherKeys{enc: bytes.Repeat([]byte{byte(i + 1)}, 32), mac: bytes.Repeat([]byte{byte(i + 17)}, 32)}
		kc.writeShared(salt[:], []byte("passphrase"), key)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".lock") {
			t.Fatalf("orphan lock file %q", entry.Name())
		}
		if st, err := entry.Info(); err == nil && st.Mode().IsRegular() && st.Size() == 64 && filepath.Ext(entry.Name()) == "" {
			keys++
		}
	}
	if keys > 8 {
		t.Fatalf("shared key cache has %d entries, want at most 8", keys)
	}
}

type sparseZeroPDBReaderAt struct {
	header []byte
	size   int64
}

func (s sparseZeroPDBReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	clear(p)
	n := min(len(p), int(s.size-off))
	if off < int64(len(s.header)) {
		copy(p, s.header[off:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestReviewPDBSparseFileDoesNotPreallocateByFileSize(t *testing.T) {
	header := make([]byte, 28)
	binary.LittleEndian.PutUint32(header[4:], 4096)
	const size = 512 << 20
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := ParsePDB(sparseZeroPDBReaderAt{header: header, size: size}, size); err != nil {
		t.Fatalf("ParsePDB returned an error for an empty sparse export: %v", err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("empty 512 MiB PDB allocated %d MiB", allocated>>20)
	}
}

func TestReviewRecordHashMatchesEncodingJSONAtEdgeValues(t *testing.T) {
	for _, track := range []Track{
		{ID: 2, Path: "/b.mp3", Title: string([]byte{'x', 0xff, 0xfe, 'y'})},
		{ID: 3, Path: "/c.mp3", Duration: 1e6},
		{ID: 4, Path: "/d.mp3", BPM: 1e-5},
	} {
		wantJSON, err := json.Marshal(track)
		if err != nil {
			t.Fatal(err)
		}
		gotJSON := appendTrackJSON(nil, track)
		wantSum := sha256.Sum256(wantJSON)
		if string(gotJSON) != string(wantJSON) || RecordHash(track) != hex.EncodeToString(wantSum[:]) {
			t.Errorf("id=%d mismatch: got %s want %s", track.ID, gotJSON, wantJSON)
		}
	}
}

func TestReviewReaderErrorsNameTheirFile(t *testing.T) {
	dir := t.TempDir()
	check := func(name string, data []byte, read func(string) error) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		err := read(path)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error %v does not name the file", name, err)
		}
	}
	check("empty.pdb", nil, func(path string) error { _, err := ReadPDB(path); return err })
	check("bad_page_size.pdb", make([]byte, 4096), func(path string) error { _, err := ReadPDB(path); return err })
	check("empty.db", nil, func(path string) error { _, err := ReadSQLite(context.Background(), path, nil); return err })
	badCipher := make([]byte, sqlcipherPageSize)
	copy(badCipher[:sqlcipherSaltSize], []byte("1234567890abcdef"))
	check("bad_hmac.db", badCipher, func(path string) error { _, err := ReadSQLite(context.Background(), path, nil); return err })
	badPlain := make([]byte, sqlcipherPageSize)
	copy(badPlain[:16], []byte(sqliteHeaderMagic))
	check("corrupt_whole.db", badPlain, func(path string) error { _, err := ReadSQLiteWhole(context.Background(), path); return err })
}

func TestReviewOneLibraryFallbackAndIntegerEdges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edge.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE image (image_id INTEGER, path TEXT);
		INSERT INTO image VALUES (5, '/img/from_image.jpg');
		CREATE TABLE artwork (artwork_id INTEGER, path TEXT);
		INSERT INTO artwork VALUES (5, '/art/from_artwork.jpg');
		CREATE TABLE content (content_id INTEGER, path TEXT, image_id INTEGER, artwork_id INTEGER, file_size REAL);
		INSERT INTO content VALUES (1, '/Music/song.mp3', NULL, 5, 1e30);
	`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	lib, err := (OneLibraryAdapter{}).Read(context.Background(), db)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := lib.Tracks[0].ArtworkPath; got != "/art/from_artwork.jpg" {
		t.Fatalf("ArtworkPath = %q, want artwork fallback", got)
	}
	if lib.Tracks[0].FileSize < 0 {
		t.Fatalf("FileSize overflowed to negative %d", lib.Tracks[0].FileSize)
	}
	if got, ok := driverInt(float64(math.MaxInt64)); ok || got != 0 {
		t.Fatalf("driverInt(MaxInt64 as float) = %d, %v, want rejected", got, ok)
	}
	if got, ok := driverInt(float64(math.MinInt64)); !ok || got != math.MinInt64 {
		t.Fatalf("driverInt(MinInt64 as float) = %d, %v", got, ok)
	}
}
