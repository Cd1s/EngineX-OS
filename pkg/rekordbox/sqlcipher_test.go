package rekordbox

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

// encryptSQLCipher4ForTest encrypts a 4096-byte-paged SQLite file (created with
// reserve=80, or reserve=0 when each page uses <= 4016 bytes) into SQLCipher 4 format.
func encryptSQLCipher4ForTest(t *testing.T, plain []byte, passphrase []byte) []byte {
	t.Helper()
	if len(plain) < sqlcipherPageSize || len(plain)%sqlcipherPageSize != 0 {
		t.Fatalf("plaintext size %d is not a positive multiple of %d", len(plain), sqlcipherPageSize)
	}
	var salt [sqlcipherSaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		t.Fatal(err)
	}
	encKey, err := pbkdf2.Key(sha512.New, string(passphrase), salt[:], sqlcipherKDFIter, 32)
	if err != nil {
		t.Fatal(err)
	}
	var macSalt [sqlcipherSaltSize]byte
	for i := range salt {
		macSalt[i] = salt[i] ^ sqlcipherHMACSaltMask
	}
	macKey, err := pbkdf2.Key(sha512.New, string(encKey), macSalt[:], sqlcipherFastKDFIter, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha512.New, macKey)
	out := make([]byte, len(plain))
	pages := uint32(len(plain) / sqlcipherPageSize)
	const usableEnd = sqlcipherPageSize - sqlcipherReserveSize
	const ivEnd = usableEnd + sqlcipherIVSize

	for pgno := uint32(1); pgno <= pages; pgno++ {
		inPage := append([]byte(nil), plain[int(pgno-1)*sqlcipherPageSize:int(pgno)*sqlcipherPageSize]...)
		dst := out[int(pgno-1)*sqlcipherPageSize : int(pgno)*sqlcipherPageSize]
		off := 0
		if pgno == 1 {
			inPage[20] = sqlcipherReserveSize
			copy(dst[:sqlcipherSaltSize], salt[:])
			off = sqlcipherSaltSize
		}
		if _, err := rand.Read(dst[usableEnd:ivEnd]); err != nil {
			t.Fatal(err)
		}
		cipher.NewCBCEncrypter(block, dst[usableEnd:ivEnd]).CryptBlocks(dst[off:usableEnd], inPage[off:usableEnd])
		mac.Reset()
		_, _ = mac.Write(dst[off:ivEnd])
		var pgLE [4]byte
		binary.LittleEndian.PutUint32(pgLE[:], pgno)
		_, _ = mac.Write(pgLE[:])
		copy(dst[ivEnd:sqlcipherPageSize], mac.Sum(nil))
	}
	return out
}

func listTempOneLibraryFiles(t *testing.T) map[string]bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "rb-onelibrary-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[string]bool, len(matches))
	for _, p := range matches {
		m[p] = true
	}
	return m
}

func TestSQLCipher4ByteExactAgainstCSQLCipher(t *testing.T) {
	encBytes, err := os.ReadFile(filepath.Join("testdata", "sqlcipher4_onelibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	wantDec, err := os.ReadFile(filepath.Join("testdata", "sqlcipher4_onelibrary_decrypted.db"))
	if err != nil {
		t.Fatal(err)
	}

	var gotDec bytes.Buffer
	if err := DecryptSQLCipher4(context.Background(), bytes.NewReader(encBytes), int64(len(encBytes)), &gotDec, []byte(oneLibraryPassphrase)); err != nil {
		t.Fatalf("DecryptSQLCipher4 failed on C SQLCipher 4 fixture: %v", err)
	}
	if !bytes.Equal(gotDec.Bytes(), wantDec) {
		t.Fatalf("decrypted bytes mismatch C SQLCipher 4 sqlite3Codec output: got sha256=%x, want sha256=%x",
			sha256.Sum256(gotDec.Bytes()), sha256.Sum256(wantDec))
	}

	// Verify 0600 temp file creation off the USB directory and cleanup after ReadSQLite.
	usbDir := t.TempDir()
	dbPath := filepath.Join(usbDir, "exportLibrary.db")
	if err := os.WriteFile(dbPath, encBytes, 0o444); err != nil {
		t.Fatal(err)
	}

	tmpPath, err := decryptSQLCipher4ToTemp(context.Background(), dbPath, []byte(oneLibraryPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(tmpPath) == usbDir {
		_ = os.Remove(tmpPath)
		t.Fatalf("temp file %s created inside source USB directory %s", tmpPath, usbDir)
	}
	st, err := os.Stat(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		_ = os.Remove(tmpPath)
		t.Fatalf("temp file permissions = %04o, want 0600", perm)
	}
	_ = os.Remove(tmpPath)

	tempsBefore := listTempOneLibraryFiles(t)
	usbEntriesBefore, err := os.ReadDir(usbDir)
	if err != nil {
		t.Fatal(err)
	}

	lib, err := ReadSQLite(context.Background(), dbPath, nil)
	if err != nil {
		t.Fatalf("ReadSQLite on C SQLCipher 4 fixture failed: %v", err)
	}

	usbEntriesAfter, err := os.ReadDir(usbDir)
	if err != nil || len(usbEntriesAfter) != len(usbEntriesBefore) {
		t.Fatalf("source USB directory modified: %v vs %v (err=%v)", usbEntriesBefore, usbEntriesAfter, err)
	}
	for p := range listTempOneLibraryFiles(t) {
		if !tempsBefore[p] {
			t.Fatalf("ReadSQLite leaked temporary decrypted file %s", p)
		}
	}

	if len(lib.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(lib.Tracks))
	}
	t0 := lib.Tracks[0]
	if t0.ID != 101 || t0.BPM != 128.5 || t0.Duration != 294 || t0.SampleRate != 48000 || t0.Comment != "Peak" {
		t.Fatalf("unexpected track 101 metadata (verify length in seconds): %+v", t0)
	}
	if len(t0.Cues) != 3 {
		t.Fatalf("track 101 cues = %+v, want 3 cues", t0.Cues)
	}
	if c := t0.Cues[0]; c.HotCue != 0 || c.TimeMS != 250 || c.Loop || c.ColorID != 2 || c.Name != "Intro" {
		t.Fatalf("unexpected memory cue: %+v", c)
	}
	if c := t0.Cues[1]; c.HotCue != 0 || c.TimeMS != 500 || c.EndMS != 1500 || !c.Loop || !c.Active || c.LoopNumerator != 4 || c.LoopDenominator != 1 || c.ColorID != 3 || c.Name != "Loop" {
		t.Fatalf("unexpected active memory loop: %+v", c)
	}
	if c := t0.Cues[2]; c.HotCue != 1 || c.TimeMS != 125 || c.Loop || c.ColorCode != 49 || c.Color != [4]byte{255, 0xde, 0x44, 0xcf} || c.Name != "Hot A" {
		t.Fatalf("unexpected Hot Cue A: %+v", c)
	}
	if lib.Tracks[1].BPM != 0 || lib.Tracks[1].Duration != 180 {
		t.Fatalf("0x7fffffff bpmx100 sentinel not normalized: %+v", lib.Tracks[1])
	}
	if lib.Tracks[2].BPM != 0 || lib.Tracks[2].Duration != 210 {
		t.Fatalf("0xffffffff bpmx100 sentinel not normalized: %+v", lib.Tracks[2])
	}
}

func TestSQLCipher4TamperAndBounds(t *testing.T) {
	encBytes, err := os.ReadFile(filepath.Join("testdata", "sqlcipher4_onelibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	if err := DecryptSQLCipher4(context.Background(), bytes.NewReader(encBytes), int64(len(encBytes)), &sink, []byte("wrong-passphrase")); err == nil {
		t.Fatal("accepted wrong passphrase")
	}
	if err := DecryptSQLCipher4(context.Background(), bytes.NewReader(encBytes), int64(len(encBytes)), &sink, nil); err == nil {
		t.Fatal("accepted empty passphrase")
	}
	for _, badSize := range []int64{0, 16, 4095, 5000, MaxSQLCipherBytes + sqlcipherPageSize} {
		if err := DecryptSQLCipher4(context.Background(), bytes.NewReader(encBytes), badSize, &sink, []byte(oneLibraryPassphrase)); err == nil {
			t.Fatalf("accepted invalid size %d", badSize)
		}
	}
	for _, flipOffset := range []int{0, 24, 4020, 4050, 4096 + 100, 8192 + 4080} {
		tampered := append([]byte(nil), encBytes...)
		tampered[flipOffset] ^= 0x01
		sink.Reset()
		if err := DecryptSQLCipher4(context.Background(), bytes.NewReader(tampered), int64(len(tampered)), &sink, []byte(oneLibraryPassphrase)); err == nil {
			t.Fatalf("accepted tampered byte at offset %d", flipOffset)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := DecryptSQLCipher4(ctx, bytes.NewReader(encBytes), int64(len(encBytes)), &sink, []byte(oneLibraryPassphrase)); err == nil {
		t.Fatal("cancelled context succeeded")
	}

	// Uncheckpointed sidecars next to encrypted exportLibrary.db must still be rejected.
	dir := t.TempDir()
	p := filepath.Join(dir, "exportLibrary.db")
	if err := os.WriteFile(p, encBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+"-wal", []byte("uncheckpointed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSQLite(context.Background(), p, nil); err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("expected sidecar rejection, got %v", err)
	}
}

func TestOneLibraryFullSchemaCuesAndRecordHash(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "plain.db")
	seed, err := os.ReadFile(filepath.Join("testdata", "sqlcipher4_onelibrary_decrypted.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", plainPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		DROP TABLE IF EXISTS content;
		DROP TABLE IF EXISTS cue;
		CREATE TABLE artist(artist_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO artist VALUES(1,'Main Artist'),(2,'Track Composer'),(3,'Club Remixer');
		CREATE TABLE album(album_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO album VALUES(10,'Test Album');
		CREATE TABLE genre(genre_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO genre VALUES(20,'Techno');
		CREATE TABLE label(label_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO label VALUES(30,'Test Label');
		CREATE TABLE "key"(key_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO "key" VALUES(40,'F#m');
		CREATE TABLE image(image_id INTEGER PRIMARY KEY, path TEXT);
		INSERT INTO image VALUES(100,'/PIONEER/Artwork/00001/a1.jpg');
		CREATE TABLE content(
			content_id INTEGER PRIMARY KEY, title TEXT, bpmx100 INTEGER, length INTEGER,
			artist_id_artist INTEGER, artist_id_composer INTEGER, artist_id_remixer INTEGER,
			album_id INTEGER, genre_id INTEGER, label_id INTEGER, key_id INTEGER, image_id INTEGER,
			djComment TEXT, rating INTEGER, releaseYear INTEGER, path TEXT, fileSize INTEGER,
			bitrate INTEGER, bitDepth INTEGER, samplingRate INTEGER, analysisDataFilePath TEXT
		);
		INSERT INTO content VALUES
			(1,'Peak',13000,320,1,2,3,10,20,30,40,100,'Note',5,2025,'/Contents/a.wav',1024000,2304,24,48000,'/PIONEER/USBANLZ/P001/ANLZ0000.DAT'),
			(2,'UnanalyzedNull',NULL,150,1,NULL,NULL,10,20,30,NULL,NULL,'',0,0,'/Contents/b.wav',512000,1411,16,44100,'');
		CREATE TABLE cue(
			cue_id INTEGER PRIMARY KEY, content_id INTEGER, kind INTEGER, colorTableIndex INTEGER,
			cueComment TEXT, isActiveLoop INTEGER, beatLoopNumerator INTEGER, beatLoopDenominator INTEGER,
			inUsec INTEGER, outUsec INTEGER
		);
		INSERT INTO cue VALUES
			(1,1,0,1,'Drop Memory',0,0,0,1000000,-1),
			(2,1,2,1,'Hot B Loop',0,8,1,2000000,4000000),
			(3,1,8,38,'Hot H',0,0,0,0,4294967295),
			(4,1,12,5,'Desktop Pad L Ignored',0,0,0,500000,-1),
			(5,1,3,0,'Unset In Ignored',0,0,0,-1,-1),
			(6,1,4,0,'Null In Ignored',0,0,0,NULL,NULL);
		CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY, sequenceNo INTEGER, name TEXT, attribute INTEGER, playlist_id_parent INTEGER);
		INSERT INTO playlist VALUES(10,1,'Root Folder',1,0),(11,2,'Sub Playlist',0,10),(12,1,'Root Smart',4,NULL);
		CREATE TABLE playlist_content(playlist_id INTEGER, content_id INTEGER, sequenceNo INTEGER);
		INSERT INTO playlist_content VALUES(11,2,1),(11,1,2),(12,1,1);
	`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}

	plainBytes, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(dir, "exportLibrary.db")
	if err := os.WriteFile(encPath, encryptSQLCipher4ForTest(t, plainBytes, []byte(oneLibraryPassphrase)), 0o600); err != nil {
		t.Fatal(err)
	}

	lib1, err := ReadSQLite(context.Background(), encPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := lib1.Tracks[0]
	if tr.Artist != "Main Artist" || tr.Composer != "Track Composer" || tr.Remixer != "Club Remixer" || tr.Album != "Test Album" || tr.Genre != "Techno" || tr.Label != "Test Label" || tr.Key != "F#m" || tr.ArtworkPath != "/PIONEER/Artwork/00001/a1.jpg" || tr.Duration != 320 || tr.BPM != 130 {
		t.Fatalf("unexpected OneLibrary track: %+v", tr)
	}
	if len(tr.Cues) != 3 || tr.Cues[2].HotCue != 8 || tr.Cues[2].TimeMS != 0 || tr.Cues[2].Loop {
		t.Fatalf("unexpected parsed cues: %+v", tr.Cues)
	}
	if lib1.Tracks[1].BPM != 0 || lib1.Tracks[1].Duration != 150 {
		t.Fatalf("NULL bpmx100 track unexpected: %+v", lib1.Tracks[1])
	}
	if len(lib1.Playlists) != 3 || !reflect.DeepEqual(lib1.Playlists[2].Tracks, []uint32{2, 1}) {
		t.Fatalf("unexpected playlists: %+v", lib1.Playlists)
	}

	// Mutating cue position/comment must change Track.RecordHash so incremental sync fires.
	db2, err := sql.Open("sqlite", plainPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db2.Exec(`UPDATE cue SET cueComment='Edited Drop', inUsec=1500000 WHERE cue_id=1`)
	_ = db2.Close()
	if err != nil {
		t.Fatal(err)
	}
	plainBytes2, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(encPath, encryptSQLCipher4ForTest(t, plainBytes2, []byte(oneLibraryPassphrase)), 0o600); err != nil {
		t.Fatal(err)
	}
	lib2, err := ReadSQLite(context.Background(), encPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lib2.Tracks[0].RecordHash == lib1.Tracks[0].RecordHash {
		t.Fatal("cue edit did not change Track.RecordHash")
	}
}

func TestPDBMangledISRCUnusedSlotsAndSkippedRows(t *testing.T) {
	b := testfixture.PDB()
	const ps = testfixture.PageSize
	p := b[2*ps : 3*ps]
	row0Off := 40 + int(le.Uint16(p[ps-6:]))
	row1Off := 40 + int(le.Uint16(p[ps-8:]))
	row2Off := 40 + int(le.Uint16(p[ps-10:]))

	// Append a mangled 0x90+0x03 odd-length ISRC string and an invalid unused-slot byte at heap end,
	// pointing track 0's ofs_strings[0] (ISRC) and ofs_strings[5] (unused) to them.
	// Move track 0 to heap tail so its row slice spans the appended strings, and mark rows 3 & 4
	// with ID=0 and empty Path respectively so row-level skipping is exercised.
	le.PutUint32(p[row1Off+72:], 0) // Track ID == 0 -> skipped
	// Point row 2's Path (slot 20) to an empty short ASCII string (0x03 = length 1 header only -> "").
	emptyOff := int(le.Uint16(p[row2Off+94:])) // slot 0 in testfixture is already "" (0x03)
	le.PutUint16(p[row2Off+94+2*20:], uint16(emptyOff))

	// Corrupt unused slot 5 on row 0 to point at an out-of-bounds offset 60000, and slot 0 to mangled ISRC.
	heapUsed := 40 + int(le.Uint16(p[30:]))
	row0Len := row1Off - row0Off
	newRow0 := make([]byte, row0Len)
	copy(newRow0, p[row0Off:row1Off])
	isrcRel := len(newRow0)
	isrcBody := append([]byte{0x03}, []byte("GBCFB2000432\x00\x00")...) // 15 bytes (odd)
	isrcStr := append([]byte{0x90, byte(4 + len(isrcBody)), 0, 0}, isrcBody...)
	newRow0 = append(newRow0, isrcStr...)
	titleRel := len(newRow0)
	titleStr := EncodeDeviceSQLString("七里香") // U+4E03 starts with 0x03 in UTF-16LE
	newRow0 = append(newRow0, titleStr...)
	le.PutUint16(newRow0[94+0:], uint16(isrcRel))
	le.PutUint16(newRow0[94+2*17:], uint16(titleRel))
	le.PutUint16(newRow0[94+2*5:], 60000) // unused slot out-of-bounds

	copy(p[heapUsed:], newRow0)
	le.PutUint16(p[ps-6:], uint16(heapUsed-40))
	le.PutUint16(p[30:], uint16(heapUsed+len(newRow0)-40))

	lib, err := ParsePDB(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("ParsePDB failed on mangled ISRC / unused slot / skipped rows: %v", err)
	}
	// Original 5 tracks minus 2 skipped (ID=0 and empty Path) = 3 tracks.
	if len(lib.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(lib.Tracks))
	}
	foundQiLiXiang := false
	for _, tr := range lib.Tracks {
		if tr.ID == 1 {
			foundQiLiXiang = tr.Title == "七里香"
			if tr.Title != "七里香" {
				t.Fatalf("track 1 title = %q, want %q", tr.Title, "七里香")
			}
		}
	}
	if !foundQiLiXiang {
		t.Fatal("track 1 missing after ParsePDB")
	}
}
