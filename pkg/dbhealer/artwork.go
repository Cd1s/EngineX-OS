package dbhealer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// maxArtworkPixels caps image decoding at 36M pixels (~6144x6144 = ~150 MiB transient
	// RGBA): large enough to downscale real 5000x5000 (25M) Denon/Beatport master covers
	// while rejecting crafted pixel bombs (>=7000x7000) before image.Decode allocates.
	maxArtworkPixels = 36 << 20

	// externalArtCheckMinSize skips opening healthy small external JPEG files during
	// mount reconciliation; files above 256 KiB are header-inspected for >1024 px,
	// >1 MiB, or PNG-as-.jpg payloads that slow QQuickPixmapRea or fail Engine's reader.
	externalArtCheckMinSize = 256 << 10

	// maxEmbeddedTracksPerArt caps per-row audio file probes when recovering missing
	// post-5.0 external artwork files from embedded audio tags.
	maxEmbeddedTracksPerArt = 3

	selectAlbumArtIndexQuery = "SELECT id, hash FROM AlbumArt ORDER BY hash"
)

func thumbnail(src image.Image) image.Image {
	b := src.Bounds()
	scale := math.Min(1, 512/float64(max(b.Dx(), b.Dy())))
	w, h := max(1, int(math.Round(float64(b.Dx())*scale))), max(1, int(math.Round(float64(b.Dy())*scale)))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx, sy := (float64(x)+0.5)/scale-0.5, (float64(y)+0.5)/scale-0.5
			sx = math.Max(0, sx)
			sy = math.Max(0, sy)
			x0, y0 := int(sx), int(sy)
			dx, dy := sx-float64(x0), sy-float64(y0)
			var channels [3]float64
			for j := 0; j < 2; j++ {
				for i := 0; i < 2; i++ {
					weight := []float64{1 - dx, dx}[i] * []float64{1 - dy, dy}[j]
					r, g, bb, a := src.At(b.Min.X+min(x0+i, b.Dx()-1), b.Min.Y+min(y0+j, b.Dy()-1)).RGBA()
					// Composite transparent artwork over white before JPEG encoding.
					channels[0] += float64(r+65535-a) * weight
					channels[1] += float64(g+65535-a) * weight
					channels[2] += float64(bb+65535-a) * weight
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(math.Round(channels[0] / 257)), uint8(math.Round(channels[1] / 257)), uint8(math.Round(channels[2] / 257)), 255})
		}
	}
	return dst
}

func opaqueOverWhite(src image.Image) image.Image {
	if op, ok := src.(interface{ Opaque() bool }); ok && op.Opaque() {
		return src
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bb, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8((r + 65535 - a) / 257),
				G: uint8((g + 65535 - a) / 257),
				B: uint8((bb + 65535 - a) / 257),
				A: 255,
			})
		}
	}
	return dst
}

func appendWarningUnique(r *Report, msg string) {
	for _, existing := range r.Warnings {
		if existing == msg {
			return
		}
	}
	r.Warnings = append(r.Warnings, msg)
}

func healArtwork(tx *sql.Tx, r *Report) (map[int64]bool, error) {
	// Engine 5.3.0 reads artwork with IFNULL(AlbumArt.albumArt, GET_ALBUM_ART(AlbumArt.hash)).
	// An empty non-NULL blob (X'') short-circuits IFNULL and hides external Artwork/<hash>.jpg.
	emptyRes, err := tx.Exec("UPDATE AlbumArt SET albumArt=NULL WHERE albumArt IS NOT NULL AND length(albumArt)=0")
	if err != nil {
		return nil, err
	}
	if n, err := emptyRes.RowsAffected(); err == nil && n > 0 {
		r.ArtworkReconciled += int(n)
	}

	var resized map[int64]bool
	var last int64 = math.MinInt64
	for {
		rows, err := tx.Query("SELECT id,substr(albumArt,1,33554433) FROM AlbumArt WHERE id>? AND albumArt IS NOT NULL ORDER BY id LIMIT 250", last)
		if err != nil {
			return nil, err
		}
		type art struct {
			id   int64
			blob []byte
		}
		var page []art
		for rows.Next() {
			var a art
			if err = rows.Scan(&a.id, &a.blob); err != nil {
				rows.Close()
				return nil, err
			}
			page = append(page, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			last = a.id
			if len(a.blob) == 0 {
				continue
			}
			cfg, _, e := image.DecodeConfig(bytes.NewReader(a.blob))
			if e != nil {
				appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: unsupported image preserved", a.id))
				continue
			}
			if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxArtworkPixels || len(a.blob) > maxBlobSize {
				appendWarningUnique(r, fmt.Sprintf("AlbumArt %d exceeds safe decoding budget; preserved", a.id))
				continue
			}
			if cfg.Width <= 1024 && cfg.Height <= 1024 && len(a.blob) <= 1<<20 {
				continue
			}
			src, _, e := image.Decode(bytes.NewReader(a.blob))
			if e != nil {
				appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: corrupt image preserved", a.id))
				continue
			}
			var out bytes.Buffer
			if err = jpeg.Encode(&out, thumbnail(src), &jpeg.Options{Quality: 80}); err != nil {
				return nil, err
			}
			// Hashes identify the original source for bridge deduplication.
			if _, err = tx.Exec("UPDATE AlbumArt SET albumArt=? WHERE id=?", out.Bytes(), a.id); err != nil {
				return nil, err
			}
			if resized == nil {
				resized = map[int64]bool{}
			}
			resized[a.id] = true
			r.Artwork++
		}
	}
	result, err := tx.Exec("UPDATE Track SET albumArtId=NULL WHERE albumArtId IS NOT NULL AND NOT EXISTS(SELECT 1 FROM AlbumArt WHERE id=Track.albumArtId)")
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	r.OrphanArtwork = int(n)
	return resized, err
}

type artFileInfo struct {
	regular bool
	size    int64
}

type artCandidateKind uint8

const (
	artKindBlob20 artCandidateKind = iota
	artKindTextHex
	artKindNumeric
	artKindOther
)

type artCandidate struct {
	id             int64
	kind           artCandidateKind
	sum            []byte
	needHashUpdate bool
}

type artHashUpdate struct {
	id  int64
	sum []byte // nil clears invalid numeric hash to NULL
}

type diskArtStatus uint8

const (
	diskArtOK diskArtStatus = iota
	diskArtUpdated
	diskArtBad
	diskArtUnsafe
)

func isArtworkURIHash(raw any) bool {
	var s string
	switch v := raw.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return false
	}
	return strings.HasPrefix(s, "image://streaming/") || strings.HasPrefix(s, "image://fileart/")
}

func decodeHexSHA1(s string) ([]byte, bool) {
	if len(s) != 40 {
		return nil, false
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return nil, false
	}
	return b, true
}

func artworkFileName(sum []byte) string {
	return base64.RawURLEncoding.EncodeToString(sum) + ".jpg"
}

func normalizeExternalArtwork(data []byte, id int64, r *Report) (out []byte, resized bool, ok bool) {
	if len(data) == 0 {
		return nil, false, false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: unsupported image preserved", id))
		return nil, false, false
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxArtworkPixels || len(data) > maxBlobSize {
		appendWarningUnique(r, fmt.Sprintf("AlbumArt %d exceeds safe decoding budget; preserved", id))
		return nil, false, false
	}
	oversized := cfg.Width > 1024 || cfg.Height > 1024 || len(data) > 1<<20
	if format == "jpeg" && !oversized {
		return data, false, true
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: corrupt image preserved", id))
		return nil, false, false
	}
	var img image.Image
	quality := 90
	if oversized {
		img = thumbnail(src)
		quality = 80
		resized = true
	} else {
		img = opaqueOverWhite(src)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, false, false
	}
	return buf.Bytes(), resized, true
}

func writeArtworkFileAtomic(artDir, fname string, data []byte) error {
	dst := filepath.Join(artDir, fname)
	if st, err := os.Lstat(dst); err == nil {
		if !st.Mode().IsRegular() {
			return fmt.Errorf("artwork destination %s is not a regular file", fname)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := dst + ".tmp"
	if st, err := os.Lstat(tmp); err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("artwork temp path %s is not a regular file", fname+".tmp")
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func normalizeExistingArtworkFile(artDir, fname string, size int64, r *Report) (status diskArtStatus, resized bool, newSize int64) {
	if size > maxBlobSize {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s exceeds safe decoding budget; preserved", fname))
		return diskArtUnsafe, false, size
	}
	path := filepath.Join(artDir, fname)
	st, err := os.Lstat(path)
	if err != nil {
		return diskArtBad, false, size
	}
	if !st.Mode().IsRegular() {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
		return diskArtUnsafe, false, size
	}
	f, err := os.Open(path)
	if err != nil {
		return diskArtBad, false, size
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(io.LimitReader(f, maxBlobSize+1))
	if err != nil {
		return diskArtBad, false, size
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxArtworkPixels {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s exceeds safe decoding budget; preserved", fname))
		return diskArtUnsafe, false, size
	}
	oversized := cfg.Width > 1024 || cfg.Height > 1024 || size > 1<<20
	if format == "jpeg" && !oversized {
		return diskArtOK, false, size
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return diskArtBad, false, size
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBlobSize+1))
	if err != nil {
		return diskArtBad, false, size
	}
	if int64(len(raw)) > maxBlobSize {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s exceeds safe decoding budget; preserved", fname))
		return diskArtUnsafe, false, size
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return diskArtBad, false, size
	}
	var img image.Image
	quality := 90
	if oversized {
		img = thumbnail(src)
		quality = 80
		resized = true
	} else {
		img = opaqueOverWhite(src)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return diskArtBad, false, size
	}
	out := buf.Bytes()
	if err := writeArtworkFileAtomic(artDir, fname, out); err != nil {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
		return diskArtUnsafe, false, size
	}
	return diskArtUpdated, resized, int64(len(out))
}

func recoverEmbeddedArtworkForRow(ctx context.Context, db *sql.DB, mount string, artID int64, extract func(string) ([]byte, error), r *Report) ([]byte, bool) {
	if extract == nil || ctx.Err() != nil {
		return nil, false
	}
	rows, err := db.QueryContext(ctx, `SELECT path FROM Track WHERE albumArtId=? AND path IS NOT NULL AND path<>'' AND COALESCE(streamingSource,'')='' ORDER BY id LIMIT ?`, artID, maxEmbeddedTracksPerArt)
	if err != nil {
		return nil, false
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			paths = append(paths, p)
		}
	}
	rows.Close()
	for _, rel := range paths {
		if ctx.Err() != nil {
			return nil, false
		}
		full, ok := trackPath(mount, rel)
		if !ok {
			continue
		}
		st, err := os.Lstat(full)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		raw, err := extract(full)
		if err != nil || len(raw) == 0 {
			continue
		}
		jpegBytes, _, ok := normalizeExternalArtwork(raw, artID, r)
		if ok {
			return jpegBytes, true
		}
	}
	return nil, false
}

// reconcileArtwork aligns AlbumArt rows and Engine Library/Artwork/<base64url(hash)>.jpg
// with Engine 5.3.0's GET_ALBUM_ART(hash) and PlanckImageProvider expectations:
//   - reconstructs missing or zero-byte Artwork/<hash>.jpg from populated AlbumArt blobs
//     (and from embedded audio artwork when albumArt is NULL after a partial 5.0 migration);
//   - upgrades legacy 40-char hex TEXT hashes to 20-byte BLOB hashes once the file is present,
//     and clears invalid numeric hashes that make Engine's GET_ALBUM_ART UDF throw;
//   - downscales oversized (>1024 px or >1 MiB) and converts PNG-as-.jpg external files;
//   - preserves the stock id=1 NULL sentinel and image://streaming/ / image://fileart/ rows;
//   - scans index_AlbumArt_hash as a covering index and stops cleanly on context expiry.
func reconcileArtwork(ctx context.Context, db *sql.DB, mount string, resizedIDs map[int64]bool, extractEmbedded func(string) ([]byte, error), r *Report) error {
	if ctx.Err() != nil {
		return nil
	}
	libDir := filepath.Join(mount, "Engine Library")
	if st, err := os.Lstat(libDir); err != nil || !st.Mode().IsDir() {
		return nil
	}
	artDir := filepath.Join(libDir, "Artwork")
	artDirExists := false
	if st, err := os.Lstat(artDir); err == nil {
		if !st.Mode().IsDir() {
			appendWarningUnique(r, "Engine Library/Artwork is not a regular directory; skipped artwork reconciliation")
			return nil
		}
		artDirExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		appendWarningUnique(r, fmt.Sprintf("Engine Library/Artwork unavailable: %v", err))
		return nil
	}

	type rawRow struct {
		id  int64
		raw any
	}
	rows, err := db.QueryContext(ctx, selectAlbumArtIndexQuery)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	var all []rawRow
	hasNullHash := false
	nonNullCount := 0
	for rows.Next() {
		var row rawRow
		if err := rows.Scan(&row.id, &row.raw); err != nil {
			rows.Close()
			return err
		}
		all = append(all, row)
		if row.raw == nil {
			hasNullHash = true
		} else {
			nonNullCount++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	rows.Close()
	if len(all) == 0 {
		return nil
	}

	var nullHashWithBlob map[int64]bool
	if hasNullHash {
		if ctx.Err() != nil {
			return nil
		}
		nrows, err := db.QueryContext(ctx, `SELECT id FROM AlbumArt WHERE hash IS NULL AND albumArt IS NOT NULL AND length(albumArt)>0`)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for nrows.Next() {
			var id int64
			if err := nrows.Scan(&id); err != nil {
				nrows.Close()
				return err
			}
			if nullHashWithBlob == nil {
				nullHashWithBlob = map[int64]bool{}
			}
			nullHashWithBlob[id] = true
		}
		if err := nrows.Err(); err != nil {
			nrows.Close()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		nrows.Close()
	}

	if nonNullCount == 0 && len(nullHashWithBlob) == 0 {
		return nil
	}

	artFiles := map[string]artFileInfo{}
	if artDirExists {
		f, err := os.Open(artDir)
		if err != nil {
			appendWarningUnique(r, fmt.Sprintf("Engine Library/Artwork scan skipped: %v", err))
			return nil
		}
		for {
			if ctx.Err() != nil {
				f.Close()
				return nil
			}
			entries, e := f.ReadDir(128)
			for _, ent := range entries {
				if !ent.Type().IsRegular() {
					artFiles[ent.Name()] = artFileInfo{regular: false}
					continue
				}
				info, ierr := ent.Info()
				if ierr != nil {
					continue
				}
				artFiles[ent.Name()] = artFileInfo{regular: true, size: info.Size()}
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				f.Close()
				if ctx.Err() != nil {
					return nil
				}
				appendWarningUnique(r, fmt.Sprintf("Engine Library/Artwork scan skipped: %v", e))
				return nil
			}
		}
		f.Close()
	}

	var work []artCandidate
	for _, row := range all {
		if row.raw == nil {
			if nullHashWithBlob[row.id] {
				work = append(work, artCandidate{id: row.id, kind: artKindOther, needHashUpdate: true})
			}
			continue
		}
		if isArtworkURIHash(row.raw) {
			continue
		}
		switch v := row.raw.(type) {
		case []byte:
			if len(v) == 20 {
				fname := artworkFileName(v)
				fi, exists := artFiles[fname]
				if exists && !fi.regular {
					appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
					continue
				}
				if !exists || fi.size == 0 || fi.size > externalArtCheckMinSize || resizedIDs[row.id] {
					sum := append([]byte(nil), v...)
					work = append(work, artCandidate{id: row.id, kind: artKindBlob20, sum: sum})
				}
			} else {
				work = append(work, artCandidate{id: row.id, kind: artKindOther, needHashUpdate: true})
			}
		case string:
			if sum, ok := decodeHexSHA1(v); ok {
				work = append(work, artCandidate{id: row.id, kind: artKindTextHex, sum: sum, needHashUpdate: true})
			} else {
				work = append(work, artCandidate{id: row.id, kind: artKindOther, needHashUpdate: true})
			}
		case int64, float64:
			work = append(work, artCandidate{id: row.id, kind: artKindNumeric, needHashUpdate: true})
		default:
			work = append(work, artCandidate{id: row.id, kind: artKindOther, needHashUpdate: true})
		}
	}
	if len(work) == 0 {
		return nil
	}
	sort.Slice(work, func(i, j int) bool { return work[i].id < work[j].id })

	ensureArtDir := func() error {
		if artDirExists {
			return nil
		}
		if st, err := os.Lstat(libDir); err != nil || !st.Mode().IsDir() {
			return fmt.Errorf("Engine Library is not a regular directory")
		}
		if err := os.Mkdir(artDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		st, err := os.Lstat(artDir)
		if err != nil || !st.Mode().IsDir() {
			return fmt.Errorf("Engine Library/Artwork is not a regular directory")
		}
		artDirExists = true
		return nil
	}

	var pending []artHashUpdate
	flush := func() error {
		if len(pending) == 0 || ctx.Err() != nil {
			pending = pending[:0]
			return nil
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			pending = pending[:0]
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, u := range pending {
			var e error
			if u.sum == nil {
				_, e = tx.ExecContext(ctx, `UPDATE AlbumArt SET hash=NULL WHERE id=?`, u.id)
			} else {
				_, e = tx.ExecContext(ctx, `UPDATE AlbumArt SET hash=? WHERE id=?`, u.sum, u.id)
			}
			if e != nil {
				_ = tx.Rollback()
				pending = pending[:0]
				if ctx.Err() != nil {
					return nil
				}
				return e
			}
		}
		pending = pending[:0]
		if err := tx.Commit(); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		return nil
	}

	writeArt := func(fname string, data []byte) error {
		if err := ensureArtDir(); err != nil {
			return err
		}
		if err := writeArtworkFileAtomic(artDir, fname, data); err != nil {
			return err
		}
		artFiles[fname] = artFileInfo{regular: true, size: int64(len(data))}
		return nil
	}

	for _, c := range work {
		if ctx.Err() != nil {
			break
		}
		var blob []byte
		err := db.QueryRowContext(ctx, `SELECT substr(albumArt,1,33554433) FROM AlbumArt WHERE id=?`, c.id).Scan(&blob)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			_ = flush()
			return err
		}

		// Rows without a usable 20-byte hash (NULL+blob, invalid numeric, wrong-length blob/text).
		if len(c.sum) != 20 {
			if len(blob) == 0 {
				if c.kind == artKindNumeric {
					pending = append(pending, artHashUpdate{id: c.id, sum: nil})
					appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: cleared invalid numeric hash", c.id))
					r.ArtworkReconciled++
				}
				if len(pending) >= 64 {
					if err := flush(); err != nil {
						return err
					}
				}
				continue
			}
			jpegBytes, _, ok := normalizeExternalArtwork(blob, c.id, r)
			if !ok {
				if c.kind == artKindNumeric {
					pending = append(pending, artHashUpdate{id: c.id, sum: nil})
					appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: cleared invalid numeric hash", c.id))
					r.ArtworkReconciled++
				}
				continue
			}
			h := sha1.Sum(blob)
			sum := h[:]
			fname := artworkFileName(sum)
			if fi, exists := artFiles[fname]; exists && !fi.regular {
				appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
				continue
			}
			if fi, exists := artFiles[fname]; !exists || fi.size == 0 || resizedIDs[c.id] {
				if err := writeArt(fname, jpegBytes); err != nil {
					appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
					continue
				}
			}
			pending = append(pending, artHashUpdate{id: c.id, sum: sum})
			r.ArtworkReconciled++
			if len(pending) >= 64 {
				if err := flush(); err != nil {
					return err
				}
			}
			continue
		}

		// Rows with a valid 20-byte hash (native BLOB or decoded 40-char hex TEXT).
		fname := artworkFileName(c.sum)
		fi, exists := artFiles[fname]
		if exists && !fi.regular {
			appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
			continue
		}

		changed := false
		if len(blob) > 0 {
			if !exists || fi.size == 0 || resizedIDs[c.id] {
				jpegBytes, _, ok := normalizeExternalArtwork(blob, c.id, r)
				if !ok {
					continue
				}
				if err := writeArt(fname, jpegBytes); err != nil {
					appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
					continue
				}
				changed = true
			} else if fi.size > externalArtCheckMinSize || c.needHashUpdate {
				status, resizedDisk, newSize := normalizeExistingArtworkFile(artDir, fname, fi.size, r)
				switch status {
				case diskArtUpdated:
					artFiles[fname] = artFileInfo{regular: true, size: newSize}
					if resizedDisk {
						r.Artwork++
					}
					changed = true
				case diskArtBad:
					jpegBytes, _, ok := normalizeExternalArtwork(blob, c.id, r)
					if !ok {
						continue
					}
					if err := writeArt(fname, jpegBytes); err != nil {
						appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
						continue
					}
					changed = true
				case diskArtUnsafe:
					continue
				}
			}
			if c.needHashUpdate {
				pending = append(pending, artHashUpdate{id: c.id, sum: c.sum})
				changed = true
			}
			if changed {
				r.ArtworkReconciled++
			}
		} else {
			// AlbumArt.albumArt is NULL (post-5.0 migrated library).
			if exists && fi.size > 0 {
				if fi.size > externalArtCheckMinSize || c.needHashUpdate {
					status, resizedDisk, newSize := normalizeExistingArtworkFile(artDir, fname, fi.size, r)
					switch status {
					case diskArtUpdated:
						artFiles[fname] = artFileInfo{regular: true, size: newSize}
						if resizedDisk {
							r.Artwork++
						}
						changed = true
					case diskArtBad, diskArtUnsafe:
						continue
					}
				}
				if c.needHashUpdate {
					pending = append(pending, artHashUpdate{id: c.id, sum: c.sum})
					changed = true
				}
				if changed {
					r.ArtworkReconciled++
				}
			} else if jpegBytes, ok := recoverEmbeddedArtworkForRow(ctx, db, mount, c.id, extractEmbedded, r); ok {
				if err := writeArt(fname, jpegBytes); err != nil {
					appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
					continue
				}
				if c.needHashUpdate {
					pending = append(pending, artHashUpdate{id: c.id, sum: c.sum})
				}
				r.ArtworkReconciled++
			}
		}

		if len(pending) >= 64 {
			if err := flush(); err != nil {
				return err
			}
		}
	}

	return flush()
}
