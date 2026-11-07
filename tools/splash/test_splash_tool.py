import os
import struct
import sys
import tempfile
import unittest
import zlib

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import splash_tool as st  # noqa: E402

W, H = st.W, st.H


def pattern():
    px = bytearray(W * H * 4)
    for i in range(W * H):
        px[i * 4:i * 4 + 4] = bytes(((i * 7) & 255, (i >> 3) & 255, (i * 13 + 5) & 255, 255))
    return px


def filtered_png(w, h, rgb):
    """RGB PNG whose rows cycle through filter types 0-4 (decoder coverage)."""
    stride, prev, raw = w * 3, bytearray(w * 3), b""
    for y in range(h):
        line, f = rgb[y * stride:(y + 1) * stride], y % 5
        out = bytearray(stride)
        for i in range(stride):
            a = line[i - 3] if i >= 3 else 0
            b, c = prev[i], (prev[i - 3] if i >= 3 else 0)
            pa, pb, pc = abs(b - c), abs(a - c), abs(a + b - 2 * c)
            pred = [0, a, b, (a + b) // 2, a if pa <= pb and pa <= pc else b if pb <= pc else c][f]
            out[i] = (line[i] - pred) & 255
        raw += bytes([f]) + bytes(out)
        prev = line

    def chunk(k, d):
        return struct.pack(">I", len(d)) + k + d + struct.pack(">I", zlib.crc32(k + d))
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


def fake_stock(bg):
    """White blocks where the stock art sits, so presets can be rendered without firmware."""
    px = bytearray([bg, bg, bg, 255] * (W * H))
    for x0, y0, x1, y1 in ((180, 330, 280, 465), (360, 366, 910, 426), (960, 345, 1100, 451),
                           (590, 265, 690, 405), (440, 522, 840, 538)) + tuple(
            (a, 727, b, 774) for a, b in st.VERSION_GLYPHS.values()):
        for y in range(y0, y1):
            px[(y * W + x0) * 4:(y * W + x1) * 4] = b"\xff\xff\xff\xff" * (x1 - x0)
    return W, H, px


class Codec(unittest.TestCase):
    def test_bin_layout(self):
        px = pattern()
        data = st.encode_bin(W, H, px)
        self.assertEqual(len(data), 4096000)
        for x, y in ((0, 0), (1279, 799), (10, 20), (1279, 0), (0, 799)):
            r, g, b, a = px[(y * W + x) * 4:(y * W + x) * 4 + 4]
            o = ((1279 - x) * 800 + y) * 4  # x_rot = y, y_rot = 1279 - x, 800-wide rows
            self.assertEqual(tuple(data[o:o + 4]), (b, g, r, a), (x, y))
        self.assertEqual(st.decode_bin(data), px)

    def test_bad_sizes(self):
        with self.assertRaises(ValueError):
            st.decode_bin(b"\0" * 100)
        with self.assertRaises(ValueError):
            st.encode_bin(800, 1280, bytearray(st.BIN_SIZE))

    def test_png_filters_and_cli_roundtrip(self):
        w, h = 7, 11
        rgb = bytes((i * 37 + 11) & 255 for i in range(w * h * 3))
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "f.png")
            open(p, "wb").write(filtered_png(w, h, rgb))
            _, _, rgba = st.read_png(p)
            self.assertEqual(bytes(rgba[0::4] + rgba[1::4]), bytes(rgb[0::3] + rgb[1::3]))
            self.assertEqual(bytes(rgba[2::4]), bytes(rgb[2::3]))

            src, binf, back = (os.path.join(d, n) for n in ("a.png", "a.bin", "b.png"))
            px = pattern()
            px[3] = 128  # one translucent pixel forces the RGBA path
            st.write_png(src, W, H, px)
            st.main(["encode", "--input", src, "--output", binf])
            st.main(["decode", "--input", binf, "--output", back])
            self.assertEqual(os.path.getsize(binf), 4096000)
            self.assertEqual(st.read_png(back)[2], px)


class Presets(unittest.TestCase):
    def test_splash(self):
        c = st.render_splash(fake_stock(10), "5.3.0.3")
        at = lambda x, y: tuple(c[(y * W + x) * 4:(y * W + x) * 4 + 4])  # noqa: E731
        self.assertEqual(at(0, 0), (10, 10, 10, 255))
        self.assertEqual(at(640, 200), (10, 10, 10, 255))
        self.assertEqual(at(1031, 396), (255, 255, 255, 255))  # x crossing (shifted right by 28)
        self.assertEqual(at(1031, 370), (10, 10, 10, 255))     # open between the x arms
        self.assertEqual(at(1100, 445), (10, 10, 10, 255))     # stock "dj" descender not copied
        self.assertGreater(at(260, 440)[0], 60)                # icon kept
        # "5.3.0.3" right-aligned from stock glyphs: 7 blocks, last ends at x=1257
        lit = [at(x, 750)[0] > 60 for x in range(1000, 1280)]
        self.assertEqual(sum(a and not b for a, b in zip(lit, [False] + lit)), 7)
        self.assertEqual(max(i for i, v in enumerate(lit) if v) + 1000, 1257)
        self.assertEqual(len(st.encode_bin(W, H, c)), 4096000)

    def test_recovery(self):
        stock = fake_stock(0)
        self.assertEqual(st.render_recovery(stock), stock[2])  # stock screen kept as is

    def test_unknown_version_glyph(self):
        with self.assertRaises(ValueError):
            st.render_splash(fake_stock(10), "5.3.1")


if __name__ == "__main__":
    unittest.main()
