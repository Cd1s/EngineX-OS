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

	"github.com/Cd1s/EngineX-OS/pkg/compat"
)

// Engine 5.3.0 answers GET_ALBUM_ART(hash) and GET_ALBUM_ART_SIZE(hash), which
// serve covers to other decks over Engine Link, only for a 20-byte BLOB hash
// (the raw SHA-1), reading Engine Library/Artwork/<base64url(hash)>.jpg. A
// 40-character TEXT hash yields nothing, so remote decks show no cover.
const artworkDir = "Engine Library/Artwork"

// putArtwork writes data as the file Engine looks up for sum, unless it exists.
func putArtwork(dir string, sum, data []byte) error {
	p := filepath.Join(dir, base64.RawURLEncoding.EncodeToString(sum)+".jpg")
	if _, e := os.Lstat(p); e == nil {
		return nil
	}
	if isPNG(data) { // Engine's .jpg reader rejects PNG bytes
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
func migrateArtwork(ctx context.Context, tx *sql.Tx, dir string) (migrated int, err error) {
	var last int64
	for {
		var id int64
		var raw any
		var data []byte
		e := tx.QueryRowContext(ctx, `SELECT id,hash,albumArt FROM AlbumArt WHERE id>? AND albumArt IS NOT NULL ORDER BY id LIMIT 1`, last).Scan(&id, &raw, &data)
		if e == sql.ErrNoRows {
			return migrated, nil
		}
		if e != nil {
			return migrated, e
		}
		last = id
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
		if b, ok := raw.([]byte); ok && bytes.Equal(b, sum) {
			continue
		}
		if _, e = tx.ExecContext(ctx, `UPDATE AlbumArt SET hash=? WHERE id=?`, sum, id); e != nil {
			return migrated, e
		}
		migrated++
	}
}

// MigrateArtwork upgrades the library under mount in place. Engine must not
// have m.db open: bridge only ever writes it before Engine attaches the volume.
func MigrateArtwork(ctx context.Context, mount string) (int, error) {
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
	n, err := migrateArtwork(ctx, tx, dir)
	if err != nil {
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
	f, err := os.Open(audioPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hdr := make([]byte, 12)
	n, err := f.ReadAt(hdr, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n < 4 {
		return nil, ErrNoArtwork
	}

	if string(hdr[:3]) == "ID3" {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			if art, err := extractMP3Artwork(f); err == nil {
				return art, nil
			}
		}
	}
	if string(hdr[:4]) == "fLaC" {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			if art, err := extractFLACArtwork(f); err == nil {
				return art, nil
			}
		}
	}
	if (n >= 8 && (string(hdr[4:8]) == "ftyp" || string(hdr[4:8]) == "moov")) ||
		filepath.Ext(audioPath) == ".m4a" || filepath.Ext(audioPath) == ".mp4" {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			if art, err := extractMP4Artwork(f); err == nil {
				return art, nil
			}
		}
	}

	// General fallbacks in case header check was indeterminate
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		if art, err := extractMP3Artwork(f); err == nil {
			return art, nil
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		if art, err := extractFLACArtwork(f); err == nil {
			return art, nil
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		if art, err := extractMP4Artwork(f); err == nil {
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

func extractMP3Artwork(f io.ReadSeeker) ([]byte, error) {
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
		return id3v22Artwork(tagData)
	}

	offset := 0
	if flags&0x40 != 0 {
		if ver == 4 && len(tagData) >= 4 {
			extSize := int(tagData[0]&0x7F)<<21 | int(tagData[1]&0x7F)<<14 | int(tagData[2]&0x7F)<<7 | int(tagData[3]&0x7F)
			offset = extSize
		} else if ver == 3 && len(tagData) >= 4 {
			extSize := int(binary.BigEndian.Uint32(tagData[:4]))
			offset = 4 + extSize
		}
	}

	for offset+10 <= len(tagData) {
		if tagData[offset] == 0 {
			break
		}
		frameID := string(tagData[offset : offset+4])
		var frameSize int
		if ver == 4 {
			frameSize = int(tagData[offset+4]&0x7F)<<21 | int(tagData[offset+5]&0x7F)<<14 | int(tagData[offset+6]&0x7F)<<7 | int(tagData[offset+7]&0x7F)
		} else {
			frameSize = int(binary.BigEndian.Uint32(tagData[offset+4 : offset+8]))
		}
		if frameSize <= 0 || offset+10+frameSize > len(tagData) {
			break
		}
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
func id3v22Artwork(tagData []byte) ([]byte, error) {
	for offset := 0; offset+6 <= len(tagData) && tagData[offset] != 0; {
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

func extractFLACArtwork(f io.ReadSeeker) ([]byte, error) {
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return nil, err
	}
	if string(magic) != "fLaC" {
		return nil, ErrNoArtwork
	}

	for {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(f, hdr); err != nil {
			return nil, ErrNoArtwork
		}
		isLast := (hdr[0] & 0x80) != 0
		blockType := hdr[0] & 0x7F
		blockLen := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])

		if blockType == 6 {
			data := make([]byte, blockLen)
			if _, err := io.ReadFull(f, data); err != nil {
				return nil, err
			}
			if len(data) >= 32 {
				mimeLen := int(binary.BigEndian.Uint32(data[4:8]))
				if 8+mimeLen+4 <= len(data) {
					descLen := int(binary.BigEndian.Uint32(data[8+mimeLen : 8+mimeLen+4]))
					dataOffset := 8 + mimeLen + 4 + descLen + 16
					if dataOffset+4 <= len(data) {
						imgLen := int(binary.BigEndian.Uint32(data[dataOffset : dataOffset+4]))
						imgStart := dataOffset + 4
						if imgStart+imgLen <= len(data) {
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
				return nil, ErrNoArtwork
			}
		}
		if isLast {
			break
		}
	}
	return nil, ErrNoArtwork
}

func extractMP4Artwork(f io.ReadSeeker) ([]byte, error) {
	return parseMP4Atoms(f, 0, 0)
}

func parseMP4Atoms(f io.ReadSeeker, start, length int64) ([]byte, error) {
	if length == 0 {
		size, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, err
		}
		length = size
	}
	curr := start
	for curr+8 <= start+length {
		if _, err := f.Seek(curr, io.SeekStart); err != nil {
			break
		}
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		atomSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		atomType := string(hdr[4:8])
		headerLen := int64(8)
		if atomSize == 1 {
			extHdr := make([]byte, 8)
			if _, err := io.ReadFull(f, extHdr); err != nil {
				break
			}
			atomSize = int64(binary.BigEndian.Uint64(extHdr))
			headerLen = 16
		} else if atomSize == 0 {
			atomSize = (start + length) - curr
		}
		if atomSize < headerLen || curr+atomSize > start+length {
			break
		}

		switch atomType {
		case "moov", "udta", "ilst":
			if art, err := parseMP4Atoms(f, curr+headerLen, atomSize-headerLen); err == nil && art != nil {
				return art, nil
			}
		case "meta":
			metaOffset := headerLen + 4
			if atomSize > metaOffset {
				if art, err := parseMP4Atoms(f, curr+metaOffset, atomSize-metaOffset); err == nil && art != nil {
					return art, nil
				}
			}
		case "covr":
			if art, err := parseMP4Covr(f, curr+headerLen, atomSize-headerLen); err == nil && art != nil {
				return art, nil
			}
		}
		curr += atomSize
	}
	return nil, ErrNoArtwork
}

func parseMP4Covr(f io.ReadSeeker, start, length int64) ([]byte, error) {
	curr := start
	for curr+8 <= start+length {
		if _, err := f.Seek(curr, io.SeekStart); err != nil {
			break
		}
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(f, hdr); err != nil {
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
					}
				}
			}
		}
		curr += atomSize
	}
	return nil, ErrNoArtwork
}
