package dbhealer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func s11JPEG(t *testing.T, w, h, quality int, noisy bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if noisy {
				v := uint32(x*2654435761) ^ uint32(y*2246822519)
				v ^= v >> 13
				v *= 1274126177
				img.SetRGBA(x, y, color.RGBA{uint8(v), uint8(v >> 8), uint8(v >> 16), 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 80, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func s11PNG(t *testing.T, w, h int, alpha uint8, noisy bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if noisy {
				v := uint32(x*2654435761) ^ uint32(y*2246822519)
				v ^= v >> 13
				v *= 1274126177
				img.SetNRGBA(x, y, color.NRGBA{uint8(v), uint8(v >> 8), uint8(v >> 16), alpha})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{20, 40, 200, alpha})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func s11PNGHeaderBomb(w, h uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	ihdr := make([]byte, 17)
	copy(ihdr[:4], "IHDR")
	binary.BigEndian.PutUint32(ihdr[4:8], w)
	binary.BigEndian.PutUint32(ihdr[8:12], h)
	ihdr[12] = 8 // bit depth
	ihdr[13] = 2 // RGB
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 13)
	buf.Write(lenBuf[:])
	buf.Write(ihdr)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc32.ChecksumIEEE(ihdr))
	buf.Write(crcBuf[:])
	return buf.Bytes()
}

func s11ArtFile(mount string, sum []byte) string {
	return filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum)+".jpg")
}

func imgBounds(img image.Image) image.Rectangle {
	if img == nil {
		return image.Rectangle{}
	}
	return img.Bounds()
}

func TestS11ReconstructMissingAndZeroByteArtworkFiles(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	artDir := filepath.Join(mount, "Engine Library", "Artwork")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Row 1: stock sentinel (hash IS NULL, albumArt IS NULL) referenced by Track 1.
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)`)
	mustExec(t, db, `INSERT INTO Track(id,path,albumArtId,isAnalyzed) VALUES(1,'../Music/1.mp3',1,1)`)

	// Row 2: valid 20-byte BLOB hash + JPEG blob, missing external Artwork/<b64>.jpg.
	jpg2 := s11JPEG(t, 300, 300, 85, false)
	sum2 := sha1.Sum(jpg2)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(2,?,?)`, sum2[:], jpg2)
	mustExec(t, db, `INSERT INTO Track(id,path,albumArtId,isAnalyzed) VALUES(2,'../Music/2.mp3',2,1)`)

	// Row 3: valid 20-byte BLOB hash + transparent PNG blob, 0-byte external file.
	png3 := s11PNG(t, 400, 400, 0, false)
	sum3 := sha1.Sum(png3)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(3,?,?)`, sum3[:], png3)
	mustExec(t, db, `INSERT INTO Track(id,path,albumArtId,isAnalyzed) VALUES(3,'../Music/3.mp3',3,1)`)
	if err := os.WriteFile(s11ArtFile(mount, sum3[:]), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Row 4: oversized 1600x800 PNG blob + existing oversized PNG masquerading as .jpg.
	png4 := s11PNG(t, 1600, 800, 255, false)
	sum4 := sha1.Sum(png4)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(4,?,?)`, sum4[:], png4)
	mustExec(t, db, `INSERT INTO Track(id,path,albumArtId,isAnalyzed) VALUES(4,'../Music/4.mp3',4,1)`)
	if err := os.WriteFile(s11ArtFile(mount, sum4[:]), png4, 0o644); err != nil {
		t.Fatal(err)
	}

	// Rows 5 & 6: stock streaming and fileart URI hashes must remain untouched.
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(5,'image://streaming/track-abc',NULL)`)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(6,'image://fileart/%2Fmedia%2Fdemo.mp3',NULL)`)
	db.Close()

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("HealMountDataOnly: %+v err=%v", r, err)
	}

	// Row 2 file reconstructed as valid JPEG.
	got2, err := os.ReadFile(s11ArtFile(mount, sum2[:]))
	if err != nil {
		t.Fatalf("missing row 2 external artwork: %v", err)
	}
	if !bytes.Equal(got2, jpg2) {
		t.Fatalf("row 2 external JPEG altered unnecessarily: got %d bytes want %d", len(got2), len(jpg2))
	}

	// Row 3 0-byte file reconstructed as valid JPEG (PNG converted, alpha composited over white).
	got3, err := os.ReadFile(s11ArtFile(mount, sum3[:]))
	if err != nil {
		t.Fatalf("missing row 3 external artwork: %v", err)
	}
	img3, fmt3, err := image.Decode(bytes.NewReader(got3))
	if err != nil || fmt3 != "jpeg" || img3.Bounds().Dx() != 400 || img3.Bounds().Dy() != 400 {
		t.Fatalf("row 3 external artwork fmt=%q bounds=%v err=%v", fmt3, imgBounds(img3), err)
	}
	cr, cg, cb, _ := img3.At(10, 10).RGBA()
	if cr>>8 < 240 || cg>>8 < 240 || cb>>8 < 240 {
		t.Fatalf("row 3 transparent PNG not composited over white: rgb=(%d,%d,%d)", cr>>8, cg>>8, cb>>8)
	}

	// Row 4 external file synced with downscaled 512x256 JPEG while preserving hash.
	got4, err := os.ReadFile(s11ArtFile(mount, sum4[:]))
	if err != nil {
		t.Fatalf("missing row 4 external artwork: %v", err)
	}
	img4, fmt4, err := image.Decode(bytes.NewReader(got4))
	if err != nil || fmt4 != "jpeg" || img4.Bounds().Dx() != 512 || img4.Bounds().Dy() != 256 {
		t.Fatalf("row 4 external artwork not downscaled JPEG: fmt=%q bounds=%v err=%v", fmt4, imgBounds(img4), err)
	}

	// Verify sentinel, Track 1 reference, row 4 hash preservation, and URI rows.
	check, err := Open(filepath.Join(mount, "Engine Library", "Database2", "m.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var t1Art sql.NullInt64
	if err := check.QueryRow(`SELECT albumArtId FROM Track WHERE id=1`).Scan(&t1Art); err != nil || !t1Art.Valid || t1Art.Int64 != 1 {
		t.Fatalf("sentinel Track.albumArtId=%v err=%v", t1Art, err)
	}
	var h1Type, a1Type string
	if err := check.QueryRow(`SELECT typeof(hash), typeof(albumArt) FROM AlbumArt WHERE id=1`).Scan(&h1Type, &a1Type); err != nil || h1Type != "null" || a1Type != "null" {
		t.Fatalf("sentinel modified: hash=%s albumArt=%s err=%v", h1Type, a1Type, err)
	}
	var h4 []byte
	if err := check.QueryRow(`SELECT hash FROM AlbumArt WHERE id=4`).Scan(&h4); err != nil || !bytes.Equal(h4, sum4[:]) {
		t.Fatalf("row 4 hash changed: got %x want %x err=%v", h4, sum4, err)
	}
	for _, tc := range []struct {
		id   int
		want string
	}{
		{5, "image://streaming/track-abc"},
		{6, "image://fileart/%2Fmedia%2Fdemo.mp3"},
	} {
		var got string
		if err := check.QueryRow(`SELECT hash FROM AlbumArt WHERE id=?`, tc.id).Scan(&got); err != nil || got != tc.want {
			t.Fatalf("URI row %d changed: got %q want %q err=%v", tc.id, got, tc.want, err)
		}
	}

	// Idempotence: second heal makes no DB changes.
	mdbPath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	beforeHdr, err := os.ReadFile(mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := HealMountDataOnly(mount, 0)
	if err != nil || !r2.Healthy || r2.Artwork != 0 {
		t.Fatalf("second heal: %+v err=%v", r2, err)
	}
	afterHdr, err := os.ReadFile(mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeHdr[:100], afterHdr[:100]) {
		t.Fatal("idempotent second heal modified m.db header")
	}
}

func TestS11LegacyTextHashEmptyBlobAndInvalidHashTypes(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	artDir := filepath.Join(mount, "Engine Library", "Artwork")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Row 1: sentinel.
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)`)

	// Row 2: 40-char hex TEXT hash + populated JPEG blob, missing external file.
	jpg2 := s11JPEG(t, 240, 240, 85, false)
	sum2 := sha1.Sum(jpg2)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(2,?,?)`, hex.EncodeToString(sum2[:]), jpg2)

	// Row 3: 40-char uppercase hex TEXT hash + NULL albumArt, external file already on disk.
	jpg3 := s11JPEG(t, 260, 260, 85, false)
	sum3 := sha1.Sum(jpg3)
	if err := os.WriteFile(s11ArtFile(mount, sum3[:]), jpg3, 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(3,?,NULL)`, strings.ToUpper(hex.EncodeToString(sum3[:])))

	// Row 4: 20-byte BLOB hash + empty non-NULL albumArt (X'') shadowing IFNULL(albumArt, GET_ALBUM_ART(hash)).
	jpg4 := s11JPEG(t, 220, 220, 85, false)
	sum4 := sha1.Sum(jpg4)
	if err := os.WriteFile(s11ArtFile(mount, sum4[:]), jpg4, 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(4,?,?)`, sum4[:], []byte{})

	// Row 5: non-40-hex garbage TEXT hash + NULL albumArt (Engine safely returns NULL; preserved).
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(5,'not-a-valid-hash',NULL)`)

	// Row 6: non-40-hex TEXT hash + populated JPEG blob -> rehashed to 20-byte BLOB + external file written.
	jpg6 := s11JPEG(t, 200, 200, 85, false)
	sum6 := sha1.Sum(jpg6)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(6,'67890',?)`, jpg6)

	// Row 7: NULL hash + populated JPEG blob (non-sentinel!) -> rehashed to 20-byte BLOB + external file written.
	jpg7 := s11JPEG(t, 180, 180, 85, false)
	sum7 := sha1.Sum(jpg7)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(7,NULL,?)`, jpg7)
	db.Close()

	// Also seed hm.db with a legacy TEXT hash row to confirm hm.db reconciliation too.
	hdbPath := filepath.Join(mount, "Engine Library", "Database2", "hm.db")
	mdbBytes, err := os.ReadFile(filepath.Join(mount, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hdbPath, mdbBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	hdb, err := Open(hdbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	jpgH := s11JPEG(t, 190, 190, 85, false)
	sumH := sha1.Sum(jpgH)
	mustExec(t, hdb, `DELETE FROM AlbumArt; INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL),(2,?,?)`, hex.EncodeToString(sumH[:]), jpgH)
	hdb.Close()

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("HealMountDataOnly: %+v err=%v", r, err)
	}

	check, err := Open(filepath.Join(mount, "Engine Library", "Database2", "m.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()

	for _, tc := range []struct {
		id   int
		want []byte
	}{
		{2, sum2[:]},
		{3, sum3[:]},
		{4, sum4[:]},
		{6, sum6[:]},
		{7, sum7[:]},
	} {
		var typ string
		var got []byte
		if err := check.QueryRow(`SELECT typeof(hash), hash FROM AlbumArt WHERE id=?`, tc.id).Scan(&typ, &got); err != nil {
			t.Fatalf("row %d: %v", tc.id, err)
		}
		if typ != "blob" || !bytes.Equal(got, tc.want) {
			t.Fatalf("row %d hash=(%s, %x), want (blob, %x)", tc.id, typ, got, tc.want)
		}
		if st, err := os.Stat(s11ArtFile(mount, tc.want)); err != nil || st.Size() == 0 {
			t.Fatalf("row %d external artwork missing/empty: st=%v err=%v", tc.id, st, err)
		}
	}

	// Row 4 empty non-NULL albumArt normalized to NULL so IFNULL reaches GET_ALBUM_ART.
	var a4Type string
	if err := check.QueryRow(`SELECT typeof(albumArt) FROM AlbumArt WHERE id=4`).Scan(&a4Type); err != nil || a4Type != "null" {
		t.Fatalf("row 4 albumArt typeof=%q, want null (err=%v)", a4Type, err)
	}

	// Row 5 non-40-hex TEXT hash with empty albumArt stays preserved (safe in Engine).
	var h5Type string
	if err := check.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=5`).Scan(&h5Type); err != nil || h5Type != "text" {
		t.Fatalf("row 5 garbage text hash typeof=%q, want text (err=%v)", h5Type, err)
	}

	// Verify true SQLITE_INTEGER/REAL hash (which makes Engine's GET_ALBUM_ART UDF throw)
	// is cleared to NULL when albumArt is empty.
	_, mountNum, dbNum := testlibrary.Create(t)
	mustExec(t, dbNum, `PRAGMA foreign_keys=OFF; DROP TABLE AlbumArt; CREATE TABLE AlbumArt (id INTEGER PRIMARY KEY AUTOINCREMENT, hash, albumArt BLOB); CREATE INDEX index_AlbumArt_hash ON AlbumArt (hash); PRAGMA foreign_keys=ON`)
	mustExec(t, dbNum, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL),(2,12345,NULL),(3,3.14,NULL)`)
	var preType string
	if err := dbNum.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=2`).Scan(&preType); err != nil || preType != "integer" {
		t.Fatalf("fixture row 2 typeof=%q, want integer", preType)
	}
	dbNum.Close()
	rNum, err := HealMountDataOnly(mountNum, 0)
	if err != nil || !rNum.Healthy {
		t.Fatalf("numeric hash heal: %+v err=%v", rNum, err)
	}
	checkNum, err := Open(filepath.Join(mountNum, "Engine Library", "Database2", "m.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkNum.Close()
	for _, id := range []int{2, 3} {
		var postType string
		if err := checkNum.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=?`, id).Scan(&postType); err != nil || postType != "null" {
			t.Fatalf("numeric row %d typeof=%q, want null (err=%v)", id, postType, err)
		}
	}

	// Verify hm.db row 2 converted and file written.
	hcheck, err := Open(hdbPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer hcheck.Close()
	var hType string
	var hSum []byte
	if err := hcheck.QueryRow(`SELECT typeof(hash), hash FROM AlbumArt WHERE id=2`).Scan(&hType, &hSum); err != nil || hType != "blob" || !bytes.Equal(hSum, sumH[:]) {
		t.Fatalf("hm.db row 2 hash=(%s, %x), want (blob, %x) err=%v", hType, hSum, sumH, err)
	}
	if st, err := os.Stat(s11ArtFile(mount, sumH[:])); err != nil || st.Size() == 0 {
		t.Fatalf("hm.db external artwork missing: %v", err)
	}
}

func TestS11PostMigrationOversizedAndPNGExternalArtworkDownscaled(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	artDir := filepath.Join(mount, "Engine Library", "Artwork")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Row 2: post-5.0 migrated (albumArt IS NULL), oversized 1600x1200 noisy JPEG (>256 KiB).
	bigJPG := s11JPEG(t, 1600, 1200, 95, true)
	if len(bigJPG) <= 256<<10 {
		t.Fatalf("test fixture bigJPG too small: %d", len(bigJPG))
	}
	sum2 := sha1.Sum(bigJPG)
	if err := os.WriteFile(s11ArtFile(mount, sum2[:]), bigJPG, 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(2,?,NULL)`, sum2[:])

	// Row 3: post-5.0 migrated (albumArt IS NULL), noisy 420x420 PNG (>256 KiB) saved with .jpg extension.
	bigPNG := s11PNG(t, 420, 420, 255, true)
	if len(bigPNG) <= 256<<10 {
		t.Fatalf("test fixture bigPNG too small: %d", len(bigPNG))
	}
	sum3 := sha1.Sum(bigPNG)
	if err := os.WriteFile(s11ArtFile(mount, sum3[:]), bigPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(3,?,NULL)`, sum3[:])

	// Row 4: healthy small JPEG on disk must stay byte-identical.
	smallJPG := s11JPEG(t, 256, 256, 80, false)
	sum4 := sha1.Sum(smallJPG)
	if err := os.WriteFile(s11ArtFile(mount, sum4[:]), smallJPG, 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(4,?,NULL)`, sum4[:])
	db.Close()

	mdbPath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	beforeHdr, err := os.ReadFile(mdbPath)
	if err != nil {
		t.Fatal(err)
	}

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("HealMountDataOnly: %+v err=%v", r, err)
	}

	// Row 2 downscaled in place to 512x384 JPEG at exact same <base64url(sum2)>.jpg path.
	got2, err := os.ReadFile(s11ArtFile(mount, sum2[:]))
	if err != nil {
		t.Fatal(err)
	}
	img2, fmt2, err := image.Decode(bytes.NewReader(got2))
	if err != nil || fmt2 != "jpeg" || img2.Bounds().Dx() != 512 || img2.Bounds().Dy() != 384 {
		t.Fatalf("row 2 external artwork fmt=%q bounds=%v err=%v", fmt2, imgBounds(img2), err)
	}

	// Row 3 PNG converted in place to valid 420x420 JPEG.
	got3, err := os.ReadFile(s11ArtFile(mount, sum3[:]))
	if err != nil {
		t.Fatal(err)
	}
	img3, fmt3, err := image.Decode(bytes.NewReader(got3))
	if err != nil || fmt3 != "jpeg" || img3.Bounds().Dx() != 420 || img3.Bounds().Dy() != 420 {
		t.Fatalf("row 3 external PNG not converted to JPEG: fmt=%q bounds=%v err=%v", fmt3, imgBounds(img3), err)
	}

	// Row 4 untouched.
	got4, err := os.ReadFile(s11ArtFile(mount, sum4[:]))
	if err != nil || !bytes.Equal(got4, smallJPG) {
		t.Fatalf("row 4 healthy JPEG changed")
	}

	// m.db header untouched when only external disk files were rewritten (no DB mutations needed).
	afterHdr, err := os.ReadFile(mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeHdr[:100], afterHdr[:100]) {
		t.Fatal("external-only artwork downscale unnecessarily wrote m.db")
	}
}

func TestS11PixelBombCorruptSymlinkBudgetAndCoveringIndex(t *testing.T) {
	// 1. Pixel-bomb (>16M pixels) and corrupt bytes in DB + external file.
	_, mount, db := testlibrary.Create(t)
	artDir := filepath.Join(mount, "Engine Library", "Artwork")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Verify covering index plan on index_AlbumArt_hash.
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT id, hash FROM AlbumArt ORDER BY hash`)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	rows.Close()
	if !strings.Contains(plan.String(), "COVERING INDEX index_AlbumArt_hash") {
		t.Fatalf("expected COVERING INDEX index_AlbumArt_hash, got %q", plan.String())
	}

	// 7000x7000 = 49M pixels (> 36M safe limit) with TEXT hash.
	bomb := s11PNGHeaderBomb(7000, 7000)
	sumBomb := sha1.Sum(bomb)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(2,?,?)`, hex.EncodeToString(sumBomb[:]), bomb)

	// External disk pixel-bomb PNG (> 256 KiB) with NULL albumArt must be rejected before decode.
	diskBomb := append(append([]byte(nil), bomb...), bytes.Repeat([]byte{0}, 300<<10)...)
	sumDiskBomb := sha1.Sum(diskBomb)
	if err := os.WriteFile(s11ArtFile(mount, sumDiskBomb[:]), diskBomb, 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(5,?,NULL)`, sumDiskBomb[:])

	// Corrupt bytes with TEXT hash.
	corrupt := []byte("not-a-valid-image-payload")
	sumCorrupt := sha1.Sum(corrupt)
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(3,?,?)`, hex.EncodeToString(sumCorrupt[:]), corrupt)

	// Symlinked target file inside Artwork/ must never be followed or overwritten.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("keep-outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	jpgSym := s11JPEG(t, 200, 200, 85, false)
	sumSym := sha1.Sum(jpgSym)
	if err := os.Symlink(outside, s11ArtFile(mount, sumSym[:])); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(4,?,?)`, sumSym[:], jpgSym)
	db.Close()

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("HealMountDataOnly: %+v err=%v", r, err)
	}
	if _, err := os.Stat(s11ArtFile(mount, sumBomb[:])); err == nil {
		t.Fatal("pixel-bomb PNG was written to Artwork/")
	}
	if _, err := os.Stat(s11ArtFile(mount, sumCorrupt[:])); err == nil {
		t.Fatal("corrupt bytes were written to Artwork/")
	}
	gotOutside, err := os.ReadFile(outside)
	if err != nil || string(gotOutside) != "keep-outside" {
		t.Fatalf("symlink target modified: %q %v", gotOutside, err)
	}

	check, err := Open(filepath.Join(mount, "Engine Library", "Database2", "m.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	var h2Type, h3Type string
	_ = check.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=2`).Scan(&h2Type)
	_ = check.QueryRow(`SELECT typeof(hash) FROM AlbumArt WHERE id=3`).Scan(&h3Type)
	check.Close()
	if h2Type != "text" || h3Type != "text" {
		t.Fatalf("unwritable rows should keep TEXT hash: bomb=%s corrupt=%s", h2Type, h3Type)
	}
	warningsJoined := strings.Join(r.Warnings, "\n")
	if !strings.Contains(warningsJoined, "AlbumArt 2 exceeds safe decoding budget") {
		t.Fatalf("7000x7000 DB pixel bomb was not rejected before decode: %v", r.Warnings)
	}
	if !strings.Contains(warningsJoined, "Artwork "+filepath.Base(s11ArtFile(mount, sumDiskBomb[:]))+" exceeds safe decoding budget") {
		t.Fatalf("7000x7000 external pixel bomb was not rejected before decode: %v", r.Warnings)
	}
	if gotDiskBomb, err := os.ReadFile(s11ArtFile(mount, sumDiskBomb[:])); err != nil || !bytes.Equal(gotDiskBomb, diskBomb) {
		t.Fatalf("external pixel bomb was altered: len=%d err=%v", len(gotDiskBomb), err)
	}
	if len(r.Warnings) < 4 {
		t.Fatalf("expected warnings for DB bomb, disk bomb, corrupt, and symlink: %v", r.Warnings)
	}

	// 2. Symlinked Artwork directory must be refused cleanly.
	_, mount2, db2 := testlibrary.Create(t)
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(mount2, "Engine Library", "Artwork")); err != nil {
		t.Fatal(err)
	}
	jpgOK := s11JPEG(t, 128, 128, 85, false)
	sumOK := sha1.Sum(jpgOK)
	mustExec(t, db2, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(2,?,?)`, sumOK[:], jpgOK)
	db2.Close()

	rSymDir, err := HealMountDataOnly(mount2, 0)
	if err != nil || !rSymDir.Healthy {
		t.Fatalf("symlink Artwork dir heal: %+v err=%v", rSymDir, err)
	}
	entries, err := os.ReadDir(outsideDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlinked Artwork dir was written into: %v %v", entries, err)
	}

	// 3. Canceled context stops cleanly with Healthy=true, next mount completes reconciliation.
	_, mount3, db3 := testlibrary.Create(t)
	jpgC := s11JPEG(t, 160, 160, 85, false)
	sumC := sha1.Sum(jpgC)
	mustExec(t, db3, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(2,?,?)`, hex.EncodeToString(sumC[:]), jpgC)
	db3.Close()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	rc, err := HealMountDataOnlyContext(canceled, mount3, 0)
	if err != nil || !rc.Healthy {
		t.Fatalf("canceled heal: %+v err=%v", rc, err)
	}
	if _, err := os.Stat(s11ArtFile(mount3, sumC[:])); err == nil {
		t.Fatal("canceled context still wrote Artwork file")
	}
	rn, err := HealMountDataOnly(mount3, 0)
	if err != nil || !rn.Healthy {
		t.Fatalf("follow-up heal: %+v err=%v", rn, err)
	}
	if _, err := os.Stat(s11ArtFile(mount3, sumC[:])); err != nil {
		t.Fatalf("follow-up heal did not complete missing Artwork file: %v", err)
	}
}
