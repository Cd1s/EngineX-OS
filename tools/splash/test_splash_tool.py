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
            with open(p, "wb") as f:
                f.write(filtered_png(w, h, rgb))
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

    def test_rotate_bin_180(self):
        px = pattern()
        data = st.encode_bin(W, H, px)
        rot = st.rotate_bin_180(data)
        self.assertEqual(len(rot), 4096000)
        # Double rotation identity
        self.assertEqual(st.rotate_bin_180(rot), data)
        # Check geometric coordinates in 800x1280 portrait buffer:
        # pixel (x, y) should move to (799 - x, 1279 - y)
        pw, ph = 800, 1280
        for px_x, px_y in ((0, 0), (799, 1279), (100, 200), (799, 0), (0, 1279)):
            src_o = (px_y * pw + px_x) * 4
            dst_o = ((ph - 1 - px_y) * pw + (pw - 1 - px_x)) * 4
            self.assertEqual(rot[dst_o:dst_o + 4], data[src_o:src_o + 4])
        # Equivalence: rotate 180 in landscape RGBA before encode_bin
        # matches rotate_bin_180 on the encoded bin
        px_rot180 = bytearray(W * H * 4)
        for y in range(H):
            for x in range(W):
                src_o = (y * W + x) * 4
                dst_o = ((H - 1 - y) * W + (W - 1 - x)) * 4
                px_rot180[dst_o:dst_o + 4] = px[src_o:src_o + 4]
        self.assertEqual(st.encode_bin(W, H, px_rot180), rot)
        # Bad size
        with self.assertRaises(ValueError):
            st.rotate_bin_180(b"\0" * 100)

    def test_cli_rot180_and_prime4_hardware_parity(self):
        with tempfile.TemporaryDirectory() as d:
            src = os.path.join(d, "splash.png")
            bin_std = os.path.join(d, "splash.bin")
            bin_rot = os.path.join(d, "splash-rot180.bin")
            bin_rot2 = os.path.join(d, "splash-rot2.bin")
            png_dec_std = os.path.join(d, "dec_std.png")
            png_dec_rot = os.path.join(d, "dec_rot.png")

            px = pattern()
            st.write_png(src, W, H, px)

            # Test render with --bin and --bin-rot180
            st.main(["render", "--preset", "enginex", "--stock", src, "--version", "5.3.0.4",
                     "--png", src, "--bin", bin_std, "--bin-rot180", bin_rot])
            self.assertEqual(os.path.getsize(bin_std), 4096000)
            self.assertEqual(os.path.getsize(bin_rot), 4096000)
            with open(bin_std, "rb") as f1, open(bin_rot, "rb") as f2:
                data_std = f1.read()
                data_rot = f2.read()
            self.assertEqual(data_rot, st.rotate_bin_180(data_std))

            # Test rotate180 command
            st.main(["rotate180", "--input", bin_std, "--output", bin_rot2])
            with open(bin_rot2, "rb") as f3:
                self.assertEqual(f3.read(), data_rot)

            # Test decode with --rotate 180
            st.main(["decode", "--input", bin_rot, "--rotate", "180", "--output", png_dec_rot])
            st.main(["decode", "--input", bin_std, "--output", png_dec_std])
            with open(png_dec_std, "rb") as f1, open(png_dec_rot, "rb") as f2:
                self.assertEqual(f1.read(), f2.read())

            # Test encode with --rotate 180
            enc_rot = os.path.join(d, "enc_rot.bin")
            st.main(["encode", "--input", src, "--rotate", "180", "--output", enc_rot])
            with open(enc_rot, "rb") as f:
                self.assertEqual(f.read(), st.rotate_bin_180(st.encode_bin(*st.read_png(src))))

            # Test render --stock-rotate 180 from both .bin and .png stocks
            _, _, stock_rgba = fake_stock(10)
            stock_png = os.path.join(d, "stock.png")
            stock_rot_png = os.path.join(d, "stock_rot180.png")
            stock_bin = os.path.join(d, "stock.bin")
            stock_rot_bin = os.path.join(d, "stock_rot180.bin")
            st.write_png(stock_png, W, H, stock_rgba)
            st.main(["encode", "--input", stock_png, "--output", stock_bin])
            st.main(["encode", "--input", stock_png, "--rotate", "180", "--output", stock_rot_bin])
            st.main(["decode", "--input", stock_rot_bin, "--output", stock_rot_png])
            out_std = os.path.join(d, "out_std.bin")
            out_from_bin = os.path.join(d, "out_from_rot_bin.bin")
            out_from_png = os.path.join(d, "out_from_rot_png.bin")
            st.main(["render", "--preset", "enginex", "--stock", stock_bin, "--version", "5.3.0.4", "--bin", out_std])
            st.main(["render", "--preset", "enginex", "--stock", stock_rot_bin, "--stock-rotate", "180", "--version", "5.3.0.4", "--bin", out_from_bin])
            st.main(["render", "--preset", "enginex", "--stock", stock_rot_png, "--stock-rotate", "180", "--version", "5.3.0.4", "--bin", out_from_png])
            with open(out_std, "rb") as f0, open(out_from_bin, "rb") as f1, open(out_from_png, "rb") as f2:
                expected = f0.read()
                self.assertEqual(f1.read(), expected)
                self.assertEqual(f2.read(), expected)

    def test_stock_firmware_prime4_sc5000_splash_relationship(self):
        fw_p4 = os.path.join("/root/build/sc5000-lab/fw-5.3.0/PRIME4-5.3.0-Update.img")
        fw_sc = os.path.join("/root/build/sc5000-lab/fw-5.3.0/SC5000-5.3.0-Update.img")
        if not (os.path.isfile(fw_p4) and os.path.isfile(fw_sc)):
            self.skipTest("stock firmware lab images not present")
        sys.path.insert(0, os.path.join(os.path.dirname(__file__), "../az01-packer"))
        import az01_packer
        with open(fw_sc, "rb") as f:
            sc_splash = az01_packer.Image(f.read()).payload("splash")
        with open(fw_p4, "rb") as f:
            p4_splash = az01_packer.Image(f.read()).payload("splash")
        self.assertEqual(st.rotate_bin_180(sc_splash), p4_splash)


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

    def test_drawn_digits(self):
        # 5.3.0.4 shipped a splash reading "5.3.0.3": the stock art has no "4".
        c = st.render_splash(fake_stock(10), "5.3.0.4")
        at = lambda x, y: c[(y * W + x) * 4]  # noqa: E731
        lit = [at(x, 762) > 60 for x in range(1000, 1280)]  # the 4's bar row
        self.assertEqual(sum(a and not b for a, b in zip(lit, [False] + lit)), 7)
        self.assertEqual(max(i for i, v in enumerate(lit) if v) + 1000, 1257)
        self.assertGreater(at(1249, 740), 100)              # 4: stem
        self.assertEqual(at(1238, 735), 10)                 # 4: open left of the diagonal
        self.assertLessEqual(max(c[(y * W + x) * 4] for x in range(1228, 1258) for y in range(720, 780)), 126)
        for v in ("5.3.0.1", "5.3.0.7", "5.4.1.7"):
            st.render_splash(fake_stock(10), v)

    def test_unknown_version_glyph(self):
        with self.assertRaises(ValueError):
            st.render_splash(fake_stock(10), "5.3.2")
        with self.assertRaises(SystemExit):  # no stale default version
            st.main(["render", "--preset", "enginex", "--stock", "x.bin", "--bin", "y.bin"])


if __name__ == "__main__":
    unittest.main()
