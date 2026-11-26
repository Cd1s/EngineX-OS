package rekordbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unicode/utf8"
)

func TestRecheckArtworkTableWithImageIDColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artwork.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE artwork (artwork_id INTEGER, path TEXT);
		INSERT INTO artwork VALUES (7, '/art/from_artwork.jpg');
		CREATE TABLE content (content_id INTEGER, path TEXT, image_id INTEGER);
		INSERT INTO content VALUES (1, '/Music/song.mp3', 7);
	`); err != nil {
		t.Fatal(err)
	}
	lib, err := (OneLibraryAdapter{}).Read(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got := lib.Tracks[0].ArtworkPath; got != "/art/from_artwork.jpg" {
		t.Fatalf("ArtworkPath = %q, want /art/from_artwork.jpg", got)
	}
}

func TestRecheckSharedKeyDirDeduplicatesColdReaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
	before := KeyDerivations()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kc := NewKeyCacheAt(dir)
			f, err := os.Open(path)
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()
			st, err := f.Stat()
			if err == nil {
				err = decryptSQLCipher4(context.Background(), f, st.Size(), io.Discard, []byte(oneLibraryPassphrase), kc)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := KeyDerivations() - before; got != 1 {
		t.Fatalf("cold readers derived %d keys, want 1", got)
	}
}

func TestRecheckWarmSharedKeyReadDoesNotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
	kc := NewKeyCacheAt(dir)
	read := func() {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if err := decryptSQLCipher4(context.Background(), f, st.Size(), io.Discard, []byte(oneLibraryPassphrase), kc); err != nil {
			t.Fatal(err)
		}
	}
	read()
	salt, err := ReadSQLCipherSalt(path)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, sharedKeyName(salt, []byte(oneLibraryPassphrase)))
	before, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	read()
	after, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("warm cache read rewrote the shared key file")
	}
}

func TestRecheckFlockRetriesEINTR(t *testing.T) {
	dir := t.TempDir()
	original := flockFile
	defer func() { flockFile = original }()
	calls := 0
	flockFile = func(fd int, operation int) error {
		calls++
		if calls == 1 {
			return syscall.EINTR
		}
		return original(fd, operation)
	}
	f, err := lockSharedDir(dir, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	unlockSharedDir(f)
	if calls < 3 {
		t.Fatalf("flock calls = %d, want EINTR retry and unlock", calls)
	}
}

func TestRecheckDeferredKeyCleanupTracksRetry(t *testing.T) {
	old := &cipherKeys{enc: bytes.Repeat([]byte{1}, 32), mac: bytes.Repeat([]byte{2}, 32)}
	current := old
	cleanup := cleanupKey(nil, &current)
	current = &cipherKeys{enc: bytes.Repeat([]byte{3}, 32), mac: bytes.Repeat([]byte{4}, 32)}
	cleanup()
	if !isAllZero(current.enc) || !isAllZero(current.mac) {
		t.Fatal("deferred cleanup did not clear the reassigned key copy")
	}
	if isAllZero(old.enc) || isAllZero(old.mac) {
		t.Fatal("test setup unexpectedly cleared the original key copy")
	}
}

func reflectTrack(seed uint64) Track {
	next := func() uint64 {
		seed ^= seed << 7
		seed ^= seed >> 9
		seed ^= seed << 8
		return seed
	}
	var track Track
	if err := fillReflectValue(reflect.ValueOf(&track).Elem(), next); err != nil {
		panic(err)
	}
	return track
}

func fillReflectValue(v reflect.Value, next func() uint64) error {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := fillReflectValue(v.Field(i), next); err != nil {
				return err
			}
		}
	case reflect.String:
		switch next() % 4 {
		case 0:
			v.SetString("ASCII fast path")
		case 1:
			v.SetString("line\u2028paragraph\u2029")
		case 2:
			v.SetString(string([]byte{0xff, 0xfe, 'x', '\n', '<'}))
		default:
			v.SetString(string([]byte{byte(next()), byte(next()), 'x', '\n', '<'}))
		}
	case reflect.Bool:
		v.SetBool(next()&1 != 0)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(next()%1000000) - 500000)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(next() % 1000000)
	case reflect.Float32, reflect.Float64:
		values := []float64{
			math.SmallestNonzeroFloat64, 1e-10, 1e-7, 9.999999999999999e-7, -1e-7,
			math.Copysign(0, -1), 1e-6, 1e6, 1e20, 9.999999999999999e20,
			1e21, 1e22, math.MaxFloat64, -1e21,
		}
		v.SetFloat(values[next()%uint64(len(values))])
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := fillReflectValue(v.Index(i), next); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if v.Type() == reflect.TypeOf([]Cue{}) {
			length := int(next() % 3)
			v.Set(reflect.MakeSlice(v.Type(), length, length))
			for i := 0; i < v.Len(); i++ {
				if err := fillReflectValue(v.Index(i), next); err != nil {
					return err
				}
			}
		} else {
			return fmt.Errorf("unhandled reflected slice type %s", v.Type())
		}
	default:
		return fmt.Errorf("unhandled reflected kind %s", v.Kind())
	}
	return nil
}

func pickJSONSentinel(v reflect.Value) string {
	sentinel := "__BADUTF8__"
	var check func(reflect.Value)
	check = func(x reflect.Value) {
		switch x.Kind() {
		case reflect.String:
			for strings.Contains(x.String(), sentinel) {
				sentinel += "_"
			}
		case reflect.Struct:
			for i := 0; i < x.NumField(); i++ {
				check(x.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < x.Len(); i++ {
				check(x.Index(i))
			}
		}
	}
	check(v)
	return sentinel
}

func replaceInvalidUTF8Bytes(s, sentinel string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && n == 1 {
			b.WriteString(sentinel)
		} else {
			b.WriteString(s[:n])
		}
		s = s[n:]
	}
	return b.String()
}

// marshalTrackLegacyInvalidUTF8 preserves Go <1.27 invalid-UTF-8 `\ufffd` escaping on top of
// stdlib json.Marshal while keeping valid U+FFFD (`\xef\xbf\xbd`) and every other field identical.
func marshalTrackLegacyInvalidUTF8(t *testing.T, track Track) []byte {
	t.Helper()
	if len(track.Cues) > 0 {
		track.Cues = append([]Cue(nil), track.Cues...)
	}
	sentinel := pickJSONSentinel(reflect.ValueOf(track))
	var rewrite func(reflect.Value)
	rewrite = func(x reflect.Value) {
		switch x.Kind() {
		case reflect.String:
			if s := x.String(); !utf8.ValidString(s) {
				x.SetString(replaceInvalidUTF8Bytes(s, sentinel))
			}
		case reflect.Struct:
			for i := 0; i < x.NumField(); i++ {
				rewrite(x.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < x.Len(); i++ {
				rewrite(x.Index(i))
			}
		}
	}
	rewrite(reflect.ValueOf(&track).Elem())
	want, err := json.Marshal(track)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(want, []byte(sentinel), []byte(`\ufffd`))
}

func assertTrackJSONMatches(t *testing.T, track Track) {
	t.Helper()
	want := marshalTrackLegacyInvalidUTF8(t, track)
	got := appendTrackJSON(nil, track)
	if !bytes.Equal(got, want) {
		t.Fatalf("track JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestTrackJSONDifferentialAllFields(t *testing.T) {
	for seed := uint64(1); seed <= 128; seed++ {
		assertTrackJSONMatches(t, reflectTrack(seed))
	}
}

func TestTrackJSONDifferentialSpecialValues(t *testing.T) {
	floatValues := []float64{
		math.SmallestNonzeroFloat64, 1e-10, 1e-7, 9.999999999999999e-7, -1e-7,
		math.Copysign(0, -1), 1e-6, 1e6, 1e20, 9.999999999999999e20,
		1e21, 1e22, math.MaxFloat64, -1e21,
	}
	for _, value := range floatValues {
		assertTrackJSONMatches(t, Track{ID: 1, Path: "/track.mp3", BPM: value, Duration: value})
	}
	for i := 0; i < 256; i++ {
		assertTrackJSONMatches(t, Track{ID: 2, Path: "/track.mp3", Title: string([]byte{byte(i)})})
	}
	for _, value := range []string{
		"ASCII fast path", "line\u2028paragraph\u2029", string([]byte{0xc0, 0x80}), string([]byte{0xed, 0xa0, 0x80}),
		"ok\ufffd" + string([]byte{0xff}) + "end",
	} {
		assertTrackJSONMatches(t, Track{ID: 3, Path: "/track.mp3", Title: value})
	}
	assertTrackJSONMatches(t, Track{})
	assertTrackJSONMatches(t, Track{ID: 4, Path: "/track.mp3", StartOffsetSamples: -1})
	var unknown struct{ Value complex64 }
	if err := fillReflectValue(reflect.ValueOf(&unknown).Elem(), func() uint64 { return 1 }); err == nil {
		t.Fatal("unknown reflected type was silently skipped")
	}
}

func TestTrackJSONUsesTheAllocationLightEncoder(t *testing.T) {
	track := reflectTrack(17)
	var buffer [2048]byte
	allocs := testing.AllocsPerRun(100, func() {
		_ = appendTrackJSON(buffer[:0], track)
	})
	if allocs > 2 {
		t.Fatalf("appendTrackJSON allocations = %.1f, want at most 2", allocs)
	}
}

func FuzzTrackJSONDifferential(f *testing.F) {
	f.Add(uint64(1))
	f.Add(uint64(0x9e3779b97f4a7c15))
	f.Fuzz(func(t *testing.T, seed uint64) {
		assertTrackJSONMatches(t, reflectTrack(seed))
	})
}
