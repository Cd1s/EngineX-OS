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

	artworkMaxDimension    = 1024
	artworkMaxBytes        = 1 << 20
	artworkTargetMaxDim    = 512
	artworkHeaderProbeSize = 64 << 10
	artworkIDPageSize      = 256
	embeddedTrackPageSize  = 64
	artworkHashFlushBatch  = 64

	selectAlbumArtIndexQuery = "SELECT id, hash FROM AlbumArt ORDER BY hash"
)

// ctxReader aborts image.DecodeConfig / image.Decode promptly when ctx expires.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func thumbnail(ctx context.Context, src image.Image) (image.Image, error) {
	b := src.Bounds()
	scale := math.Min(1, float64(artworkTargetMaxDim)/float64(max(b.Dx(), b.Dy())))
	w, h := max(1, int(math.Round(float64(b.Dx())*scale))), max(1, int(math.Round(float64(b.Dy())*scale)))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		if y&31 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
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
	return dst, nil
}

func opaqueOverWhite(ctx context.Context, src image.Image) (image.Image, error) {
	if op, ok := src.(interface{ Opaque() bool }); ok && op.Opaque() {
		return src, nil
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		if y&31 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
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
	return dst, nil
}

func encodeNormalizedJPEG(ctx context.Context, src image.Image, oversized bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var (
		img     image.Image
		quality int
		err     error
	)
	if oversized {
		img, err = thumbnail(ctx, src)
		quality = 80
	} else {
		img, err = opaqueOverWhite(ctx, src)
		quality = 90
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func artworkOversized(cfg image.Config, byteSize int64) bool {
	return cfg.Width > artworkMaxDimension || cfg.Height > artworkMaxDimension || byteSize > artworkMaxBytes
}

func artworkDimsWithinBudget(cfg image.Config) bool {
	return cfg.Width > 0 && cfg.Height > 0 && int64(cfg.Width)*int64(cfg.Height) <= maxArtworkPixels
}

func appendWarningUnique(r *Report, msg string) {
	for _, existing := range r.Warnings {
		if existing == msg {
			return
		}
	}
	r.Warnings = append(r.Warnings, msg)
}

// healArtworkMetadata fixes lightweight AlbumArt/Track metadata inside the main heal transaction:
//   - normalizes empty non-NULL AlbumArt.albumArt (zero-length blob) to NULL so Engine 5.3.0's
//     IFNULL(AlbumArt.albumArt, GET_ALBUM_ART(AlbumArt.hash)) reaches external Artwork files;
//   - nulls dangling Track.albumArtId references without altering Track.albumArtSourceHash.
func healArtworkMetadata(ctx context.Context, tx *sql.Tx, r *Report) error {
	emptyRes, err := tx.ExecContext(ctx, "UPDATE AlbumArt SET albumArt=NULL WHERE albumArt IS NOT NULL AND length(albumArt)=0")
	if err != nil {
		return err
	}
	if n, err := emptyRes.RowsAffected(); err == nil && n > 0 {
		r.ArtworkReconciled += int(n)
	}
	result, err := tx.ExecContext(ctx, "UPDATE Track SET albumArtId=NULL WHERE albumArtId IS NOT NULL AND NOT EXISTS(SELECT 1 FROM AlbumArt WHERE id=Track.albumArtId)")
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	r.OrphanArtwork = int(n)
	return err
}

type artworkBlobMeta struct {
	id   int64
	size int64
}

type sqlQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func listArtworkBlobMetaPage(ctx context.Context, q sqlQueryer, afterID int64) ([]artworkBlobMeta, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, length(albumArt) FROM AlbumArt WHERE id>? AND albumArt IS NOT NULL AND length(albumArt)>0 ORDER BY id LIMIT ?", afterID, artworkIDPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []artworkBlobMeta
	for rows.Next() {
		var m artworkBlobMeta
		if err := rows.Scan(&m.id, &m.size); err != nil {
			return nil, err
		}
		page = append(page, m)
	}
	return page, rows.Err()
}

// resizeSingleArtworkBlob inspects one DB blob using a 64 KiB header probe first and
// loads at most one full blob (<= 32 MiB) into memory when downscaling is required.
func resizeSingleArtworkBlob(ctx context.Context, q sqlQueryer, update func([]byte) error, m artworkBlobMeta, r *Report) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if m.size <= 0 {
		return false, nil
	}
	if m.size > maxBlobSize {
		appendWarningUnique(r, fmt.Sprintf("AlbumArt %d exceeds safe decoding budget; preserved", m.id))
		return false, nil
	}
	var prefix []byte
	if err := q.QueryRowContext(ctx, "SELECT substr(albumArt,1,?) FROM AlbumArt WHERE id=?", artworkHeaderProbeSize, m.id).Scan(&prefix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if len(prefix) == 0 {
		return false, nil
	}
	var full []byte
	cfg, _, err := image.DecodeConfig(bytes.NewReader(prefix))
	if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && m.size > int64(len(prefix)) {
		if qerr := q.QueryRowContext(ctx, "SELECT albumArt FROM AlbumArt WHERE id=?", m.id).Scan(&full); qerr != nil {
			if errors.Is(qerr, sql.ErrNoRows) {
				return false, nil
			}
			return false, qerr
		}
		cfg, _, err = image.DecodeConfig(&ctxReader{ctx: ctx, r: bytes.NewReader(full)})
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: unsupported image preserved", m.id))
		return false, nil
	}
	if !artworkDimsWithinBudget(cfg) {
		appendWarningUnique(r, fmt.Sprintf("AlbumArt %d exceeds safe decoding budget; preserved", m.id))
		return false, nil
	}
	if !artworkOversized(cfg, m.size) {
		return false, nil
	}
	if full == nil {
		if int64(len(prefix)) == m.size {
			full = prefix
		} else if err := q.QueryRowContext(ctx, "SELECT albumArt FROM AlbumArt WHERE id=?", m.id).Scan(&full); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	src, _, err := image.Decode(&ctxReader{ctx: ctx, r: bytes.NewReader(full)})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: corrupt image preserved", m.id))
		return false, nil
	}
	out, err := encodeNormalizedJPEG(ctx, src, true)
	if err != nil {
		return false, err
	}
	if err := update(out); err != nil {
		return false, err
	}
	r.Artwork++
	return true, nil
}

// resizeOversizedArtworkBlobsTx streams AlbumArt rows one blob at a time inside an
// explicit Heal(db) transaction so all non-mount repairs remain atomic.
func resizeOversizedArtworkBlobsTx(ctx context.Context, tx *sql.Tx, r *Report) (map[int64]bool, error) {
	var resized map[int64]bool
	var last int64 = math.MinInt64
	for {
		page, err := listArtworkBlobMetaPage(ctx, tx, last)
		if err != nil {
			return resized, err
		}
		if len(page) == 0 {
			return resized, nil
		}
		for _, m := range page {
			last = m.id
			ok, err := resizeSingleArtworkBlob(ctx, tx, func(out []byte) error {
				// Hashes identify the original source for bridge deduplication.
				_, e := tx.ExecContext(ctx, "UPDATE AlbumArt SET albumArt=? WHERE id=?", out, m.id)
				return e
			}, m, r)
			if err != nil {
				return resized, err
			}
			if ok {
				if resized == nil {
					resized = map[int64]bool{}
				}
				resized[m.id] = true
			}
		}
	}
}

// resizeOversizedArtworkBlobsMount streams AlbumArt blobs one row at a time with
// per-row commits so mount-time --data-only heals obey ctx deadlines without holding
// an active SQLite transaction open or buffering large blob batches in RAM.
func resizeOversizedArtworkBlobsMount(ctx context.Context, db *sql.DB, r *Report) (map[int64]bool, error) {
	var resized map[int64]bool
	var last int64 = math.MinInt64
	for {
		if ctx.Err() != nil {
			return resized, nil
		}
		page, err := listArtworkBlobMetaPage(ctx, db, last)
		if err != nil {
			if ctx.Err() != nil {
				return resized, nil
			}
			return resized, err
		}
		if len(page) == 0 {
			return resized, nil
		}
		for _, m := range page {
			if ctx.Err() != nil {
				return resized, nil
			}
			last = m.id
			ok, err := resizeSingleArtworkBlob(ctx, db, func(out []byte) error {
				return transactionContext(ctx, db, func(tx *sql.Tx) error {
					// Hashes identify the original source for bridge deduplication.
					_, e := tx.ExecContext(ctx, "UPDATE AlbumArt SET albumArt=? WHERE id=?", out, m.id)
					return e
				})
			}, m, r)
			if err != nil {
				if ctx.Err() != nil {
					return resized, nil
				}
				return resized, err
			}
			if ok {
				if resized == nil {
					resized = map[int64]bool{}
				}
				resized[m.id] = true
			}
		}
	}
}

type artFileInfo struct {
	regular bool
	size    int64
}

type artworkDirCache struct {
	loaded   bool
	exists   bool
	files    map[string]artFileInfo
	verified map[string]bool
}

func newArtworkDirCache() *artworkDirCache {
	return &artworkDirCache{
		files:    map[string]artFileInfo{},
		verified: map[string]bool{},
	}
}

func (c *artworkDirCache) ensureScanned(ctx context.Context, artDir string, r *Report) (bool, error) {
	if c.loaded {
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	st, err := os.Lstat(artDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.loaded = true
			c.exists = false
			return true, nil
		}
		appendWarningUnique(r, fmt.Sprintf("Engine Library/Artwork unavailable: %v", err))
		return false, nil
	}
	if !st.Mode().IsDir() {
		appendWarningUnique(r, "Engine Library/Artwork is not a regular directory; skipped artwork reconciliation")
		return false, nil
	}
	f, err := os.Open(artDir)
	if err != nil {
		appendWarningUnique(r, fmt.Sprintf("Engine Library/Artwork scan skipped: %v", err))
		return false, nil
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		entries, e := f.ReadDir(128)
		for _, ent := range entries {
			if !ent.Type().IsRegular() {
				c.files[ent.Name()] = artFileInfo{regular: false}
				continue
			}
			info, ierr := ent.Info()
			if ierr != nil {
				continue
			}
			c.files[ent.Name()] = artFileInfo{regular: true, size: info.Size()}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			appendWarningUnique(r, fmt.Sprintf("Engine Library/Artwork scan skipped: %v", e))
			return false, nil
		}
	}
	c.loaded = true
	c.exists = true
	return true, nil
}

func (c *artworkDirCache) ensureDir(libDir, artDir string) error {
	if c.exists {
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
	c.exists = true
	return nil
}

func (c *artworkDirCache) writeAtomic(libDir, artDir, fname string, data []byte) error {
	if err := c.ensureDir(libDir, artDir); err != nil {
		return err
	}
	if err := writeArtworkFileAtomic(artDir, fname, data); err != nil {
		return err
	}
	c.files[fname] = artFileInfo{regular: true, size: int64(len(data))}
	c.verified[fname] = true
	return nil
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

type artHashFlusher struct {
	db      *sql.DB
	pending []artHashUpdate
}

func (f *artHashFlusher) add(ctx context.Context, id int64, sum []byte) error {
	f.pending = append(f.pending, artHashUpdate{id: id, sum: sum})
	if len(f.pending) >= artworkHashFlushBatch {
		return f.flush(ctx)
	}
	return nil
}

func (f *artHashFlusher) flush(ctx context.Context) error {
	if len(f.pending) == 0 || ctx.Err() != nil {
		f.pending = f.pending[:0]
		return nil
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		f.pending = f.pending[:0]
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	for _, u := range f.pending {
		var e error
		if u.sum == nil {
			_, e = tx.ExecContext(ctx, `UPDATE AlbumArt SET hash=NULL WHERE id=?`, u.id)
		} else {
			_, e = tx.ExecContext(ctx, `UPDATE AlbumArt SET hash=? WHERE id=?`, u.sum, u.id)
		}
		if e != nil {
			_ = tx.Rollback()
			f.pending = f.pending[:0]
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
	}
	f.pending = f.pending[:0]
	if err := tx.Commit(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return nil
}

type diskArtStatus uint8

const (
	diskArtOK diskArtStatus = iota
	diskArtUpdated
	diskArtBad
	diskArtUnsafe
	diskArtCanceled
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

func probeFileHeader(ctx context.Context, f *os.File, size int64, buf []byte) (image.Config, string, error) {
	if err := ctx.Err(); err != nil {
		return image.Config{}, "", err
	}
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return image.Config{}, "", err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(buf[:n]))
	if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && int64(n) < size {
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return image.Config{}, "", serr
		}
		return image.DecodeConfig(&ctxReader{ctx: ctx, r: io.LimitReader(f, maxBlobSize+1)})
	}
	return cfg, format, err
}

func normalizeArtworkBytes(ctx context.Context, data []byte, id int64, r *Report) (out []byte, ok bool) {
	if len(data) == 0 || ctx.Err() != nil {
		return nil, false
	}
	cfg, format, err := image.DecodeConfig(&ctxReader{ctx: ctx, r: bytes.NewReader(data)})
	if err != nil {
		if ctx.Err() == nil {
			appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: unsupported image preserved", id))
		}
		return nil, false
	}
	if !artworkDimsWithinBudget(cfg) || len(data) > maxBlobSize {
		appendWarningUnique(r, fmt.Sprintf("AlbumArt %d exceeds safe decoding budget; preserved", id))
		return nil, false
	}
	oversized := artworkOversized(cfg, int64(len(data)))
	if format == "jpeg" && !oversized {
		return data, true
	}
	src, _, err := image.Decode(&ctxReader{ctx: ctx, r: bytes.NewReader(data)})
	if err != nil {
		if ctx.Err() == nil {
			appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: corrupt image preserved", id))
		}
		return nil, false
	}
	out, err = encodeNormalizedJPEG(ctx, src, oversized)
	if err != nil {
		return nil, false
	}
	return out, true
}

func inspectAndNormalizeArtworkFile(ctx context.Context, cache *artworkDirCache, libDir, artDir, fname string, size int64, probeBuf []byte, r *Report) (status diskArtStatus, resized bool) {
	if ctx.Err() != nil {
		return diskArtCanceled, false
	}
	if cache.verified[fname] {
		return diskArtOK, false
	}
	if size > maxBlobSize {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s exceeds safe decoding budget; preserved", fname))
		return diskArtUnsafe, false
	}
	path := filepath.Join(artDir, fname)
	st, err := os.Lstat(path)
	if err != nil {
		return diskArtBad, false
	}
	if !st.Mode().IsRegular() {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
		return diskArtUnsafe, false
	}
	f, err := os.Open(path)
	if err != nil {
		return diskArtBad, false
	}
	defer f.Close()
	cfg, format, err := probeFileHeader(ctx, f, size, probeBuf)
	if err != nil {
		if ctx.Err() != nil {
			return diskArtCanceled, false
		}
		return diskArtBad, false
	}
	if !artworkDimsWithinBudget(cfg) {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s exceeds safe decoding budget; preserved", fname))
		return diskArtUnsafe, false
	}
	oversized := artworkOversized(cfg, size)
	if format == "jpeg" && !oversized {
		cache.verified[fname] = true
		return diskArtOK, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return diskArtBad, false
	}
	raw, err := io.ReadAll(&ctxReader{ctx: ctx, r: io.LimitReader(f, maxBlobSize+1)})
	if err != nil {
		if ctx.Err() != nil {
			return diskArtCanceled, false
		}
		return diskArtBad, false
	}
	if int64(len(raw)) > maxBlobSize {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s exceeds safe decoding budget; preserved", fname))
		return diskArtUnsafe, false
	}
	src, _, err := image.Decode(&ctxReader{ctx: ctx, r: bytes.NewReader(raw)})
	if err != nil {
		if ctx.Err() != nil {
			return diskArtCanceled, false
		}
		return diskArtBad, false
	}
	out, err := encodeNormalizedJPEG(ctx, src, oversized)
	if err != nil {
		if ctx.Err() != nil {
			return diskArtCanceled, false
		}
		return diskArtBad, false
	}
	if err := cache.writeAtomic(libDir, artDir, fname, out); err != nil {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
		return diskArtUnsafe, false
	}
	return diskArtUpdated, oversized
}

// recoverEmbeddedArtworkForRow probes referencing non-streaming tracks in bounded ID
// pages under the mount context until embedded artwork is found or ctx expires.
func recoverEmbeddedArtworkForRow(ctx context.Context, db *sql.DB, mount string, artID int64, extract func(string) ([]byte, error), r *Report) ([]byte, bool) {
	if extract == nil || ctx.Err() != nil {
		return nil, false
	}
	var lastID int64 = math.MinInt64
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		rows, err := db.QueryContext(ctx, `SELECT id, path FROM Track WHERE albumArtId=? AND id>? AND path IS NOT NULL AND path<>'' AND COALESCE(streamingSource,'')='' ORDER BY id LIMIT ?`, artID, lastID, embeddedTrackPageSize)
		if err != nil {
			return nil, false
		}
		type trackRef struct {
			id   int64
			path string
		}
		var page []trackRef
		for rows.Next() {
			var tr trackRef
			if err := rows.Scan(&tr.id, &tr.path); err == nil {
				page = append(page, tr)
			}
		}
		rows.Close()
		if len(page) == 0 {
			return nil, false
		}
		for _, tr := range page {
			lastID = tr.id
			if ctx.Err() != nil {
				return nil, false
			}
			full, ok := trackPath(mount, tr.path)
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
			if jpegBytes, ok := normalizeArtworkBytes(ctx, raw, artID, r); ok {
				return jpegBytes, true
			}
		}
	}
}

type rawArtIndexRow struct {
	id  int64
	raw any
}

func scanAlbumArtIndex(ctx context.Context, db *sql.DB) (rowsOut []rawArtIndexRow, hasNullHash bool, nonNullCount int, err error) {
	rows, err := db.QueryContext(ctx, selectAlbumArtIndexQuery)
	if err != nil {
		return nil, false, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(rowsOut)&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, 0, err
			}
		}
		var row rawArtIndexRow
		if err := rows.Scan(&row.id, &row.raw); err != nil {
			return nil, false, 0, err
		}
		rowsOut = append(rowsOut, row)
		if row.raw == nil {
			hasNullHash = true
		} else {
			nonNullCount++
		}
	}
	return rowsOut, hasNullHash, nonNullCount, rows.Err()
}

func queryNullHashBlobIDs(ctx context.Context, db *sql.DB) (map[int64]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM AlbumArt WHERE hash IS NULL AND albumArt IS NOT NULL AND length(albumArt)>0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids map[int64]bool
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if ids == nil {
			ids = map[int64]bool{}
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func buildArtworkCandidates(all []rawArtIndexRow, nullHashWithBlob map[int64]bool, cache *artworkDirCache, r *Report) []artCandidate {
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
				if fi, exists := cache.files[fname]; exists && !fi.regular {
					appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
					continue
				}
				sum := append([]byte(nil), v...)
				work = append(work, artCandidate{id: row.id, kind: artKindBlob20, sum: sum})
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
	sort.Slice(work, func(i, j int) bool { return work[i].id < work[j].id })
	return work
}

func readAlbumArtBlobCapped(ctx context.Context, db *sql.DB, id int64) ([]byte, bool, error) {
	var blob []byte
	err := db.QueryRowContext(ctx, `SELECT substr(albumArt,1,33554433) FROM AlbumArt WHERE id=?`, id).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return blob, true, nil
}

func reconcileUnresolvedHashRow(
	ctx context.Context,
	db *sql.DB,
	mount, libDir, artDir string,
	c artCandidate,
	resizedIDs map[int64]bool,
	extractEmbedded func(string) ([]byte, error),
	cache *artworkDirCache,
	flusher *artHashFlusher,
	r *Report,
) error {
	blob, found, err := readAlbumArtBlobCapped(ctx, db, c.id)
	if err != nil || !found {
		return err
	}
	if len(blob) == 0 {
		if jpegBytes, ok := recoverEmbeddedArtworkForRow(ctx, db, mount, c.id, extractEmbedded, r); ok {
			h := sha1.Sum(jpegBytes)
			sum := h[:]
			fname := artworkFileName(sum)
			if fi, exists := cache.files[fname]; exists && !fi.regular {
				appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
				return nil
			}
			if fi, exists := cache.files[fname]; !exists || fi.size == 0 {
				if err := cache.writeAtomic(libDir, artDir, fname, jpegBytes); err != nil {
					appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
					return nil
				}
			}
			r.ArtworkReconciled++
			return flusher.add(ctx, c.id, sum)
		}
		if ctx.Err() == nil && c.kind == artKindNumeric {
			appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: cleared invalid numeric hash", c.id))
			r.ArtworkReconciled++
			return flusher.add(ctx, c.id, nil)
		}
		return nil
	}
	jpegBytes, ok := normalizeArtworkBytes(ctx, blob, c.id, r)
	if !ok {
		if ctx.Err() == nil && c.kind == artKindNumeric {
			appendWarningUnique(r, fmt.Sprintf("AlbumArt %d: cleared invalid numeric hash", c.id))
			r.ArtworkReconciled++
			return flusher.add(ctx, c.id, nil)
		}
		return nil
	}
	h := sha1.Sum(blob)
	sum := h[:]
	fname := artworkFileName(sum)
	if fi, exists := cache.files[fname]; exists && !fi.regular {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
		return nil
	}
	if fi, exists := cache.files[fname]; !exists || fi.size == 0 || resizedIDs[c.id] {
		if err := cache.writeAtomic(libDir, artDir, fname, jpegBytes); err != nil {
			appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
			return nil
		}
	}
	r.ArtworkReconciled++
	return flusher.add(ctx, c.id, sum)
}

func reconcileValidHashRow(
	ctx context.Context,
	db *sql.DB,
	mount, libDir, artDir string,
	c artCandidate,
	resizedIDs map[int64]bool,
	extractEmbedded func(string) ([]byte, error),
	cache *artworkDirCache,
	flusher *artHashFlusher,
	probeBuf []byte,
	r *Report,
) error {
	fname := artworkFileName(c.sum)
	fi, exists := cache.files[fname]
	if exists && !fi.regular {
		appendWarningUnique(r, fmt.Sprintf("Artwork %s: non-regular file skipped", fname))
		return nil
	}

	// Fast path: probe the existing external file header first before touching AlbumArt blobs in SQLite.
	if exists && fi.size > 0 && !resizedIDs[c.id] {
		status, resizedDisk := inspectAndNormalizeArtworkFile(ctx, cache, libDir, artDir, fname, fi.size, probeBuf, r)
		switch status {
		case diskArtCanceled:
			return ctx.Err()
		case diskArtUnsafe:
			return nil
		case diskArtOK:
			if c.needHashUpdate {
				r.ArtworkReconciled++
				return flusher.add(ctx, c.id, c.sum)
			}
			return nil
		case diskArtUpdated:
			if resizedDisk {
				r.Artwork++
			}
			r.ArtworkReconciled++
			if c.needHashUpdate {
				return flusher.add(ctx, c.id, c.sum)
			}
			return nil
		case diskArtBad:
			// Corrupt external file: fall through to reconstruct from DB blob or embedded tags if available.
		}
	}

	blob, found, err := readAlbumArtBlobCapped(ctx, db, c.id)
	if err != nil || !found {
		return err
	}
	if len(blob) > 0 {
		jpegBytes, ok := normalizeArtworkBytes(ctx, blob, c.id, r)
		if !ok {
			return nil
		}
		if err := cache.writeAtomic(libDir, artDir, fname, jpegBytes); err != nil {
			appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
			return nil
		}
		r.ArtworkReconciled++
		if c.needHashUpdate {
			return flusher.add(ctx, c.id, c.sum)
		}
		return nil
	}

	// AlbumArt.albumArt is NULL (post-5.0 migrated): recover from embedded audio tags if available.
	if jpegBytes, ok := recoverEmbeddedArtworkForRow(ctx, db, mount, c.id, extractEmbedded, r); ok {
		if err := cache.writeAtomic(libDir, artDir, fname, jpegBytes); err != nil {
			appendWarningUnique(r, fmt.Sprintf("Artwork %s: write skipped: %v", fname, err))
			return nil
		}
		r.ArtworkReconciled++
		if c.needHashUpdate {
			return flusher.add(ctx, c.id, c.sum)
		}
	}
	return nil
}

// reconcileArtwork aligns AlbumArt rows and Engine Library/Artwork/<base64url(hash)>.jpg
// with Engine 5.3.0's GET_ALBUM_ART(hash) and PlanckImageProvider expectations:
//   - scans index_AlbumArt_hash linearly and indexes Artwork/ in bounded ReadDir(128) batches
//     (shared across m.db and hm.db via cache);
//   - probes every existing external file's 64 KiB header (even below 256 KiB) and downscales
//     >1024 px / >1 MiB covers and PNG/GIF-as-.jpg payloads to <=512 px JPEGs in place;
//   - reconstructs missing, zero-byte, or corrupt Artwork/<hash>.jpg atomically from populated
//     AlbumArt blobs or from referenced tracks' embedded tags under the mount ctx budget;
//   - upgrades legacy 40-char hex TEXT hashes to 20-byte BLOB hashes once the JPEG exists,
//     rehashes invalid/numeric/NULL-with-blob rows, and clears unrecoverable numeric hashes;
//   - preserves the stock id=1 NULL sentinel and image://streaming/ / image://fileart/ rows.
func reconcileArtwork(ctx context.Context, db *sql.DB, mount string, resizedIDs map[int64]bool, extractEmbedded func(string) ([]byte, error), cache *artworkDirCache, r *Report) error {
	if ctx.Err() != nil {
		return nil
	}
	libDir := filepath.Join(mount, "Engine Library")
	if st, err := os.Lstat(libDir); err != nil || !st.Mode().IsDir() {
		return nil
	}
	artDir := filepath.Join(libDir, "Artwork")
	if cache == nil {
		cache = newArtworkDirCache()
	}

	all, hasNullHash, nonNullCount, err := scanAlbumArtIndex(ctx, db)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if len(all) == 0 {
		return nil
	}

	var nullHashWithBlob map[int64]bool
	if hasNullHash {
		nullHashWithBlob, err = queryNullHashBlobIDs(ctx, db)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	if nonNullCount == 0 && len(nullHashWithBlob) == 0 {
		return nil
	}

	ok, err := cache.ensureScanned(ctx, artDir, r)
	if err != nil || !ok {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	work := buildArtworkCandidates(all, nullHashWithBlob, cache, r)
	if len(work) == 0 {
		return nil
	}

	flusher := &artHashFlusher{db: db}
	probeBuf := make([]byte, artworkHeaderProbeSize)
	for _, c := range work {
		if ctx.Err() != nil {
			break
		}
		if len(c.sum) != 20 {
			err = reconcileUnresolvedHashRow(ctx, db, mount, libDir, artDir, c, resizedIDs, extractEmbedded, cache, flusher, r)
		} else {
			err = reconcileValidHashRow(ctx, db, mount, libDir, artDir, c, resizedIDs, extractEmbedded, cache, flusher, probeBuf, r)
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			_ = flusher.flush(ctx)
			return err
		}
	}
	return flusher.flush(ctx)
}
