package enginedb

import (
	"bytes"
	"crypto/sha1"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestPutArtworkRejectsOversizedPNGDimensions(t *testing.T) {
	// 4200x4200 (17.64M pixels > 16M limit) compresses to a tiny paletted PNG
	// yet expands to >70 MB during decode/JPEG encode on 32-bit armhf.
	pal := image.NewPaletted(image.Rect(0, 0, 4200, 4200), color.Palette{color.Black})
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, pal); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(buf.Bytes())
	if err := putArtwork(t.TempDir(), sum[:], buf.Bytes()); err == nil {
		t.Fatal("putArtwork accepted oversized 4200x4200 PNG decompression bomb")
	}
}
