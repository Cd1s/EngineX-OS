"""Unit tests for splash_tool.py (#118)."""
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

# Add tools/splash to sys.path
SPLASH_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(SPLASH_DIR))
import splash_tool


class SplashToolTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory(prefix='splash-test-')
        self.work = Path(self.tmpdir.name)

    def tearDown(self):
        self.tmpdir.cleanup()

    def test_enginex_preset_exact_size(self):
        out_bin = self.work / 'splash.bin'
        size = splash_tool.encode_splash(preset='enginex', output_path=out_bin)
        self.assertEqual(size, splash_tool.SPLASH_SIZE)
        self.assertEqual(out_bin.stat().st_size, 4096000)

    def test_rotation_and_pixel_mapping(self):
        # Create a test 1280x800 RGBA image with distinct test pixels
        rgba = bytearray(splash_tool.SPLASH_WIDTH * splash_tool.SPLASH_HEIGHT * 4)

        # Mark pixel at (x_src=100, y_src=200) with RGBA = (11, 22, 33, 255)
        x_src, y_src = 100, 200
        src_idx = (y_src * splash_tool.SPLASH_WIDTH + x_src) * 4
        rgba[src_idx] = 11      # R
        rgba[src_idx + 1] = 22  # G
        rgba[src_idx + 2] = 33  # B
        rgba[src_idx + 3] = 255 # A

        fb = splash_tool.landscape_to_framebuffer(rgba)
        self.assertEqual(len(fb), 4096000)

        # Expected in portrait framebuffer:
        # x_rot = y_src = 200
        # y_rot = 1279 - x_src = 1279 - 100 = 1179
        x_rot = 200
        y_rot = 1179
        dst_idx = (y_rot * splash_tool.ROTATED_WIDTH + x_rot) * 4
        # Format is BGRA8888
        self.assertEqual(fb[dst_idx], 33)      # B
        self.assertEqual(fb[dst_idx + 1], 22)  # G
        self.assertEqual(fb[dst_idx + 2], 11)  # R
        self.assertEqual(fb[dst_idx + 3], 255) # A

        # Reverse back to landscape
        restored = splash_tool.framebuffer_to_landscape(fb)
        self.assertEqual(restored[src_idx:src_idx+4], bytes([11, 22, 33, 255]))
        self.assertEqual(restored, bytes(rgba))

    def test_encode_decode_roundtrip(self):
        splash_bin = self.work / 'splash.bin'
        splash_tool.encode_splash(preset='enginex', output_path=splash_bin)

        out_png = self.work / 'splash.png'
        splash_tool.decode_splash(splash_bin, out_png)
        self.assertTrue(out_png.exists())

        reencoded_bin = self.work / 'reencoded.bin'
        splash_tool.encode_splash(input_path=out_png, output_path=reencoded_bin)

        self.assertEqual(splash_bin.read_bytes(), reencoded_bin.read_bytes())

    def test_ppm_encode(self):
        # Create a valid 1280x800 P6 PPM file
        ppm_file = self.work / 'test.ppm'
        header = b'P6\n1280 800\n255\n'
        # Fill with blue pixels (0, 128, 255)
        pixel = b'\x00\x80\xff'
        body = pixel * (1280 * 800)
        ppm_file.write_bytes(header + body)

        out_bin = self.work / 'ppm_splash.bin'
        splash_tool.encode_splash(input_path=ppm_file, output_path=out_bin)
        self.assertEqual(out_bin.stat().st_size, 4096000)

        # Check a pixel in the framebuffer: (B=255, G=128, R=0, A=255)
        data = out_bin.read_bytes()
        self.assertEqual(data[:4], b'\xff\x80\x00\xff')

    def test_raw_passthrough(self):
        raw_file = self.work / 'raw_4mb.bin'
        sample_bytes = os.urandom(4096000)
        raw_file.write_bytes(sample_bytes)

        out_bin = self.work / 'out.bin'
        splash_tool.encode_splash(input_path=raw_file, output_path=out_bin)
        self.assertEqual(out_bin.read_bytes(), sample_bytes)

    def test_validation_errors(self):
        # Wrong size for decode
        bad_bin = self.work / 'short.bin'
        bad_bin.write_bytes(b'too short')
        with self.assertRaises(ValueError):
            splash_tool.decode_splash(bad_bin, self.work / 'fail.png')

        # Wrong dimensions in PNG
        bad_png = self.work / 'small.png'
        splash_tool.write_png_rgba(100, 100, bytes(100 * 100 * 4), bad_png)
        with self.assertRaises(ValueError):
            splash_tool.encode_splash(input_path=bad_png, output_path=self.work / 'fail.bin')

    def test_cli_commands(self):
        bin_path = self.work / 'cli_splash.bin'
        png_path = self.work / 'cli_splash.png'

        # CLI encode preset
        ret = splash_tool.main(['encode', '--preset', 'enginex', '--output', str(bin_path)])
        self.assertEqual(ret, 0)
        self.assertEqual(bin_path.stat().st_size, 4096000)

        # CLI info
        ret = splash_tool.main(['info', '--input', str(bin_path)])
        self.assertEqual(ret, 0)

        # CLI decode
        ret = splash_tool.main(['decode', '--input', str(bin_path), '--output', str(png_path)])
        self.assertEqual(ret, 0)
        self.assertTrue(png_path.exists())

    def test_bytes_api_and_return_types(self):
        # 1. encode_splash(rgba_1280x800_bytes) -> bytes (4096000 bytes)
        rgba = bytearray(1280 * 800 * 4)
        rgba[0:4] = b'\xaa\xbb\xcc\xff'
        fb = splash_tool.encode_splash(bytes(rgba))
        self.assertIsInstance(fb, bytes)
        self.assertEqual(len(fb), 4096000)

        # 2. decode_splash(bgra_800x1280_bytes) -> bytes (valid PNG bytes)
        png_stream = splash_tool.decode_splash(fb)
        self.assertIsInstance(png_stream, bytes)
        self.assertTrue(png_stream.startswith(b'\x89PNG\r\n\x1a\n'))
        w, h, decoded_rgba = splash_tool.read_png_rgba(png_stream)
        self.assertEqual(w, 1280)
        self.assertEqual(h, 800)
        self.assertEqual(decoded_rgba[0:4], b'\xaa\xbb\xcc\xff')


if __name__ == '__main__':
    unittest.main()
