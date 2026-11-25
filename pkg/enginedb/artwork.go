package enginedb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
)

// Engine 5.3.0 answers GET_ALBUM_ART(hash) and GET_ALBUM_ART_SIZE(hash), which
// serve covers to other decks over Engine Link, only for a 20-byte BLOB hash
// (the raw SHA-1), reading Engine Library/Artwork/<base64url(hash)>.jpg. A
// 40-character TEXT hash yields nothing, so remote decks show no cover.
const (
	artworkDir               = "Engine Library/Artwork"
	selectAlbumArtIndexQuery = "SELECT id, hash FROM AlbumArt ORDER BY hash"
)

// putArtwork writes data as the file Engine looks up for sum, unless it exists.
func putArtwork(dir string, sum, data []byte) error {
	p := filepath.Join(dir, base64.RawURLEncoding.EncodeToString(sum)+".jpg")
	if _, e := os.Lstat(p); e == nil {
		return nil
	}
	if isPNG(data) { // Engine's .jpg reader rejects PNG bytes
		cfg, e := png.DecodeConfig(bytes.NewReader(data))
		if e != nil {
			return e
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 16<<20 {
			return errors.New("artwork: PNG dimensions exceed safe limit")
		}
		img, e := png.Decode(bytes.NewReader(data))
		if e != nil {
			return e
		}
		var buf bytes.Buffer
		if e = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); e != nil {
			return e
		}
		data = buf.Bytes()
	}
	if e := os.WriteFile(p+".tmp", data, 0o644); e != nil {
		return e
	}
	return os.Rename(p+".tmp", p)
}

// migrateArtwork turns earlier builds' TEXT hashes into BLOB hashes plus an
// Artwork file. A row whose file cannot be written keeps its TEXT hash, so
// the next run retries it.
func migrateArtwork(ctx context.Context, tx *sql.Tx, dir string, deadline time.Time, now func() time.Time) (migrated int, err error) {
	past := func() bool { return !deadline.IsZero() && !now().Before(deadline) }
	if past() {
		return 0, nil
	}
	// Index existing regular files once, instead of an Lstat per cover. Read
	// bounded batches so cancellation or a mount's read budget stops the scan.
	f, err := os.Open(dir)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		log.Printf("rb-bridge: artwork directory scan skipped: %v", err)
		return 0, nil
	}
	defer f.Close()
	exists := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if past() {
			return 0, nil
		}
		entries, e := f.ReadDir(128)
		for _, ent := range entries {
			if ent.Type().IsRegular() {
				exists[ent.Name()] = true
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			log.Printf("rb-bridge: artwork directory scan skipped: %v", e)
			return 0, nil
		}
	}
	type artCandidate struct {
		id  int64
		raw any
	}
	// Scan index_AlbumArt_hash as a covering index in one pass so stamp-bypassed
	// cold mounts never walk scattered AlbumArt table pages containing JPEG blobs.
	rows, e := tx.QueryContext(ctx, selectAlbumArtIndexQuery)
	if e != nil {
		return 0, e
	}
	var need []artCandidate
	for rows.Next() {
		var c artCandidate
		if e = rows.Scan(&c.id, &c.raw); e != nil {
			rows.Close()
			return 0, e
		}
		if b, ok := c.raw.([]byte); ok && len(b) == 20 && exists[base64.RawURLEncoding.EncodeToString(b)+".jpg"] {
			continue
		}
		need = append(need, c)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return 0, e
	}
	rows.Close()
	if len(need) == 0 {
		return 0, nil
	}
	sort.Slice(need, func(i, j int) bool { return need[i].id < need[j].id })
	for _, c := range need {
		if err := ctx.Err(); err != nil {
			return migrated, err
		}
		if past() {
			return migrated, nil
		}
		id, raw := c.id, c.raw
		var data []byte
		e := tx.QueryRowContext(ctx, `SELECT albumArt FROM AlbumArt WHERE id=? AND albumArt IS NOT NULL`, id).Scan(&data)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return migrated, e
		}
		if past() {
			return migrated, nil
		}
		var sum []byte
		if b, ok := raw.([]byte); ok && len(b) == 20 {
			sum = b
		} else {
			// Anything else (TEXT hex, wrong length) is rehashed from the image itself.
			h := sha1.Sum(data)
			sum = h[:]
		}
		if e = putArtwork(dir, sum, data); e != nil {
			log.Printf("rb-bridge: artwork %d left as is: %v", id, e)
			continue
		}
		exists[base64.RawURLEncoding.EncodeToString(sum)+".jpg"] = true
		if b, ok := raw.([]byte); ok && bytes.Equal(b, sum) {
			continue
		}
		if _, e = tx.ExecContext(ctx, `UPDATE AlbumArt SET hash=? WHERE id=?`, sum, id); e != nil {
			return migrated, e
		}
		migrated++
	}
	return migrated, nil
}

// MigrateArtwork upgrades the library under mount in place. Engine must not
// have m.db open: bridge only ever writes it before Engine attaches the volume.
func MigrateArtwork(ctx context.Context, mount string) (int, error) {
	return MigrateArtworkBefore(ctx, mount, time.Time{})
}

// MigrateArtworkBefore upgrades covers until deadline, committing progress for
// the next mount. A zero deadline allows a complete explicit migration.
func MigrateArtworkBefore(ctx context.Context, mount string, deadline time.Time) (int, error) {
	dir, err := compat.SafeOutputDir(mount, artworkDir)
	if err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(mount, "Engine Library/Database2/m.db")}).String()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := migrateArtwork(ctx, tx, dir, deadline, time.Now)
	if err != nil || n == 0 {
		return 0, err
	}
	return n, tx.Commit()
}

var (
	ErrNoArtwork = errors.New("artwork: no embedded artwork found")
	ErrBadFormat = errors.New("artwork: unsupported image format")
)

func isJPEG(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}

func isPNG(b []byte) bool {
	return len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
}

func validateImage(b []byte) ([]byte, error) {
	if isJPEG(b) || isPNG(b) {
		return b, nil
	}
	return nil, ErrBadFormat
}

func findImageOffset(b []byte) int {
	jpegMagic := []byte{0xFF, 0xD8, 0xFF}
	pngMagic := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	jIdx := bytes.Index(b, jpegMagic)
	pIdx := bytes.Index(b, pngMagic)
	if jIdx >= 0 && (pIdx < 0 || jIdx < pIdx) {
		return jIdx
	}
	if pIdx >= 0 {
		return pIdx
	}
	return -1
}

// ExtractEmbeddedArtwork extracts embedded cover artwork from an audio file (MP3 ID3v2 APIC,
// FLAC METADATA_BLOCK_PICTURE, or MP4/M4A covr->data atoms), verifying PNG or JPEG headers.
func ExtractEmbeddedArtwork(audioPath string) ([]byte, error) {
	return ExtractEmbeddedArtworkContext(context.Background(), audioPath)
}

const extractReadChunkSize = 64 << 10

type ctxReadSeeker struct {
	ctx context.Context
	rs  io.ReadSeeker
}

func (c *ctxReadSeeker) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > extractReadChunkSize {
		p = p[:extractReadChunkSize]
	}
	return c.rs.Read(p)
}

func (c *ctxReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.rs.Seek(offset, whence)
}

// ExtractEmbeddedArtworkContext extracts embedded cover artwork from audioPath while
// honoring ctx cancellation before opening and during bounded tag/atom reads.
func ExtractEmbeddedArtworkContext(ctx context.Context, audioPath string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(audioPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return extractEmbeddedArtworkFromReadSeeker(ctx, f, filepath.Ext(audioPath))
}

func extractEmbeddedArtworkFromReadSeeker(ctx context.Context, rs io.ReadSeeker, ext string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	crs := &ctxReadSeeker{ctx: ctx, rs: rs}
	if _, err := crs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	hdr := make([]byte, 12)
	n, err := io.ReadFull(crs, hdr)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n < 4 {
		return nil, ErrNoArtwork
	}

	tryParse := func(fn func(context.Context, io.ReadSeeker) ([]byte, error)) ([]byte, bool, error) {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if _, err := crs.Seek(0, io.SeekStart); err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, nil
		}
		art, err := fn(ctx, crs)
		if err == nil {
			return art, true, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, nil
	}

	if string(hdr[:3]) == "ID3" {
		if art, ok, err := tryParse(extractMP3Artwork); err != nil {
			return nil, err
		} else if ok {
			return art, nil
		}
	}
	if string(hdr[:4]) == "fLaC" {
		if art, ok, err := tryParse(extractFLACArtwork); err != nil {
			return nil, err
		} else if ok {
			return art, nil
		}
	}
	if (n >= 8 && (string(hdr[4:8]) == "ftyp" || string(hdr[4:8]) == "moov")) ||
		ext == ".m4a" || ext == ".mp4" {
		if art, ok, err := tryParse(extractMP4Artwork); err != nil {
			return nil, err
		} else if ok {
			return art, nil
		}
	}

	// General fallbacks in case header check was indeterminate
	for _, fn := range []func(context.Context, io.ReadSeeker) ([]byte, error){
		extractMP3Artwork,
		extractFLACArtwork,
		extractMP4Artwork,
	} {
		if art, ok, err := tryParse(fn); err != nil {
			return nil, err
		} else if ok {
			return art, nil
		}
	}

	return nil, ErrNoArtwork
}

// maxArtworkBytes bounds what one file's tag or cover atom may make us read: a 10-byte header must
// not allocate 256 MiB on a 32-bit deck with 1-2 GB of RAM (BH-GC1-14).
const maxArtworkBytes = 32 << 20

// remaining is how many bytes f holds after its current position.
func remaining(f io.ReadSeeker) (int64, error) {
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	_, err = f.Seek(pos, io.SeekStart)
	return end - pos, err
}

func extractMP3Artwork(ctx context.Context, f io.ReadSeeker) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, err
	}
	if string(hdr[:3]) != "ID3" {
		return nil, ErrNoArtwork
	}
	ver := hdr[3]
	flags := hdr[5]
	tagSize := int(hdr[6]&0x7F)<<21 | int(hdr[7]&0x7F)<<14 | int(hdr[8]&0x7F)<<7 | int(hdr[9]&0x7F)
	// The header's size is a claim: read no more than the file holds, and no more than the cap
	// (a cover past it is not found, nothing more).
	left, err := remaining(f)
	if err != nil {
		return nil, err
	}
	tagSize = int(min(int64(tagSize), left, maxArtworkBytes))
	tagData := make([]byte, tagSize)
	if _, err := io.ReadFull(f, tagData); err != nil {
		return nil, err
	}
	if ver == 2 {
		return id3v22Artwork(ctx, tagData)
	}

	// Every length below is a 32-bit claim of the file: compared in int64, so that on the
	// first-generation decks' 32-bit build (int is 32 bits) none can wrap past a bound.
	offset := 0
	if flags&0x40 != 0 {
		var extSize int64
		if ver == 4 && len(tagData) >= 4 {
			extSize = int64(tagData[0]&0x7F)<<21 | int64(tagData[1]&0x7F)<<14 | int64(tagData[2]&0x7F)<<7 | int64(tagData[3]&0x7F)
		} else if ver == 3 && len(tagData) >= 4 {
			extSize = 4 + int64(binary.BigEndian.Uint32(tagData[:4]))
		}
		if extSize > int64(len(tagData)) {
			return nil, ErrNoArtwork
		}
		offset = int(extSize)
	}

	for frames := 0; offset+10 <= len(tagData); frames++ {
		if frames&31 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if tagData[offset] == 0 {
			break
		}
		frameID := string(tagData[offset : offset+4])
		var size int64
		if ver == 4 {
			size = int64(tagData[offset+4]&0x7F)<<21 | int64(tagData[offset+5]&0x7F)<<14 | int64(tagData[offset+6]&0x7F)<<7 | int64(tagData[offset+7]&0x7F)
		} else {
			size = int64(binary.BigEndian.Uint32(tagData[offset+4 : offset+8]))
		}
		if size <= 0 || size > int64(len(tagData)-offset-10) {
			break
		}
		frameSize := int(size)
		frameBody := tagData[offset+10 : offset+10+frameSize]
		if frameID == "APIC" {
			if idx := findImageOffset(frameBody); idx >= 0 {
				return validateImage(frameBody[idx:])
			}
		}
		offset += 10 + frameSize
	}
	return nil, ErrNoArtwork
}

// id3v22Artwork finds a PIC frame in an ID3v2.2 tag, whose frame headers are 6 bytes (a 3-byte ID
// and a 3-byte size) instead of v2.3/v2.4's 10 (BH-GC1-14).
func id3v22Artwork(ctx context.Context, tagData []byte) ([]byte, error) {
	for offset, frames := 0, 0; offset+6 <= len(tagData) && tagData[offset] != 0; frames++ {
		if frames&31 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		frameSize := int(tagData[offset+3])<<16 | int(tagData[offset+4])<<8 | int(tagData[offset+5])
		if frameSize <= 0 || offset+6+frameSize > len(tagData) {
			break
		}
		if string(tagData[offset:offset+3]) == "PIC" {
			body := tagData[offset+6 : offset+6+frameSize]
			if idx := findImageOffset(body); idx >= 0 {
				return validateImage(body[idx:])
			}
		}
		offset += 6 + frameSize
	}
	return nil, ErrNoArtwork
}

func extractFLACArtwork(ctx context.Context, f io.ReadSeeker) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return nil, err
	}
	if string(magic) != "fLaC" {
		return nil, ErrNoArtwork
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(f, hdr); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrNoArtwork
		}
		isLast := (hdr[0] & 0x80) != 0
		blockType := hdr[0] & 0x7F
		blockLen := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])

		if blockType == 6 {
			// The block header's length is a claim too: no more than the file holds.
			left, err := remaining(f)
			if err != nil {
				return nil, err
			}
			if int64(blockLen) > left {
				return nil, ErrNoArtwork
			}
			data := make([]byte, min(int64(blockLen), maxArtworkBytes))
			if _, err := io.ReadFull(f, data); err != nil {
				return nil, err
			}
			if n := int64(len(data)); n >= 32 {
				mimeLen := int64(binary.BigEndian.Uint32(data[4:8]))
				if 8+mimeLen+4 <= n {
					descLen := int64(binary.BigEndian.Uint32(data[8+mimeLen : 8+mimeLen+4]))
					dataOffset := 8 + mimeLen + 4 + descLen + 16
					if dataOffset+4 <= n {
						imgLen := int64(binary.BigEndian.Uint32(data[dataOffset : dataOffset+4]))
						imgStart := dataOffset + 4
						if imgStart+imgLen <= n {
							return validateImage(data[imgStart : imgStart+imgLen])
						}
					}
				}
			}
			if idx := findImageOffset(data); idx >= 0 {
				return validateImage(data[idx:])
			}
		} else {
			if _, err := f.Seek(int64(blockLen), io.SeekCurrent); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, ErrNoArtwork
			}
		}
		if isLast {
			break
		}
	}
	return nil, ErrNoArtwork
}

func extractMP4Artwork(ctx context.Context, f io.ReadSeeker) ([]byte, error) {
	return parseMP4Atoms(ctx, f, 0, 0)
}

func parseMP4Atoms(ctx context.Context, f io.ReadSeeker, start, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if length == 0 {
		size, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, err
		}
		length = size
	}
	curr := start
	for curr+8 <= start+length {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := f.Seek(curr, io.SeekStart); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			break
		}
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(f, hdr); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			break
		}
		atomSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		atomType := string(hdr[4:8])
		headerLen := int64(8)
		if atomSize == 1 {
			extHdr := make([]byte, 8)
			if _, err := io.ReadFull(f, extHdr); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				break
			}
			atomSize = int64(binary.BigEndian.Uint64(extHdr))
			headerLen = 16
		} else if atomSize == 0 {
			atomSize = (start + length) - curr
		}
		if atomSize < headerLen || atomSize > start+length-curr { // a 64-bit size must not wrap curr
			break
		}

		switch atomType {
		case "moov", "udta", "ilst":
			art, err := parseMP4Atoms(ctx, f, curr+headerLen, atomSize-headerLen)
			if err == nil && art != nil {
				return art, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		case "meta":
			metaOffset := headerLen + 4
			if atomSize > metaOffset {
				art, err := parseMP4Atoms(ctx, f, curr+metaOffset, atomSize-metaOffset)
				if err == nil && art != nil {
					return art, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
		case "covr":
			art, err := parseMP4CovrContext(ctx, f, curr+headerLen, atomSize-headerLen)
			if err == nil && art != nil {
				return art, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		curr += atomSize
	}
	return nil, ErrNoArtwork
}

func parseMP4Covr(f io.ReadSeeker, start, length int64) ([]byte, error) {
	return parseMP4CovrContext(context.Background(), f, start, length)
}

func parseMP4CovrContext(ctx context.Context, f io.ReadSeeker, start, length int64) ([]byte, error) {
	curr := start
	for curr+8 <= start+length {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := f.Seek(curr, io.SeekStart); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			break
		}
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(f, hdr); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			break
		}
		atomSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		atomType := string(hdr[4:8])
		if atomSize < 8 || curr+atomSize > start+length {
			break
		}
		if atomType == "data" {
			if atomSize > 16 && atomSize-16 <= maxArtworkBytes {
				payloadLen := atomSize - 16
				payload := make([]byte, payloadLen)
				if _, err := f.Seek(curr+16, io.SeekStart); err == nil {
					if _, err := io.ReadFull(f, payload); err == nil {
						return validateImage(payload)
					} else if ctx.Err() != nil {
						return nil, ctx.Err()
					}
				} else if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
		}
		curr += atomSize
	}
	return nil, ErrNoArtwork
}
