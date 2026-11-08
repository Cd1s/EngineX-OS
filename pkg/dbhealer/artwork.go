package dbhealer

import (
	"bytes"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
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

func healArtwork(tx *sql.Tx, r *Report) error {
	var last int64 = math.MinInt64
	for {
		rows, err := tx.Query("SELECT id,substr(albumArt,1,33554433) FROM AlbumArt WHERE id>? ORDER BY id LIMIT 250", last)
		if err != nil {
			return err
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
				return err
			}
			page = append(page, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
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
				r.Warnings = append(r.Warnings, fmt.Sprintf("AlbumArt %d: unsupported image preserved", a.id))
				continue
			}
			if cfg.Width <= 1024 && cfg.Height <= 1024 && len(a.blob) <= 1<<20 {
				continue
			}
			if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 64<<20 || len(a.blob) > maxBlobSize {
				r.Warnings = append(r.Warnings, fmt.Sprintf("AlbumArt %d exceeds safe decoding budget; preserved", a.id))
				continue
			}
			src, _, e := image.Decode(bytes.NewReader(a.blob))
			if e != nil {
				r.Warnings = append(r.Warnings, fmt.Sprintf("AlbumArt %d: corrupt image preserved", a.id))
				continue
			}
			var out bytes.Buffer
			if err = jpeg.Encode(&out, thumbnail(src), &jpeg.Options{Quality: 80}); err != nil {
				return err
			}
			// Hashes identify the original source for bridge deduplication.
			if _, err = tx.Exec("UPDATE AlbumArt SET albumArt=? WHERE id=?", out.Bytes(), a.id); err != nil {
				return err
			}
			r.Artwork++
		}
	}
	result, err := tx.Exec("UPDATE Track SET albumArtId=NULL WHERE albumArtId IS NOT NULL AND NOT EXISTS(SELECT 1 FROM AlbumArt WHERE id=Track.albumArtId)")
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	r.OrphanArtwork = int(n)
	return err
}
