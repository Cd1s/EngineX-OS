"""Unit tests for az0x_packer.py (#119)."""
import hashlib
import json
import os
import struct
import sys
import tempfile
import unittest
from pathlib import Path

# Add tools/az01-packer to sys.path
AZ01_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(AZ01_DIR))
import az0x_packer


def make_az0x_fixture(splash_payload=None, num_records=10, include_splash=True):
    """Construct a valid synthetic AZ0x container image for testing."""
    if splash_payload is None:
        splash_payload = b'S' * az0x_packer.SPLASH_SIZE

    hdr = bytearray(0x450)
    struct.pack_into('<4sI', hdr, 0, b'AZ0x', 1)
    rec_start = 0xc8
    rec_end = rec_start + num_records * 64
    struct.pack_into('<IIII', hdr, 0x10, 0x38, 0xa8, rec_start, rec_end)

    # String table entries at 0x38:
    hdr[0x7e:0x7e + 7] = b'splash\x00'
    hdr[0x85:0x85 + 13] = b'updatesplash\x00'

    boot = b'BOOTLOADER_TEST' * 64
    boot_off = 0x450
    s1_off = boot_off + len(boot)
    s2_off = s1_off + len(splash_payload)

    for i in range(num_records):
        roff = rec_start + i * 64
        if include_splash and i in (4, 5):
            magic = b'PART'
            off = s1_off if i == 4 else s2_off
            sz = len(splash_payload)
            name_off = 0x46  # 0x38 + 0x46 = 0x7e -> 'splash'
            sha = hashlib.sha256(splash_payload).digest()
            bmask = 1 if i == 4 else 0xe
        else:
            magic = b'BOOT'
            off = boot_off
            sz = len(boot)
            name_off = 0
            sha = hashlib.sha256(boot).digest()
            bmask = 1 << (i % 8)

        hdr[roff:roff + 32] = struct.pack('<4sIQQII', magic, 0x80000000, off, sz, name_off, bmask)
        hdr[roff + 32:roff + 64] = sha

    payloads = boot
    if include_splash:
        payloads += splash_payload + splash_payload
    return bytes(hdr) + payloads


class AZ0xPackerTests(unittest.TestCase):
    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory(prefix='az0x-test-')
        self.work = Path(self.tmpdir.name)

    def tearDown(self):
        self.tmpdir.cleanup()

    def test_parse_and_info(self):
        raw = make_az0x_fixture()
        meta = az0x_packer.info(raw)
        self.assertEqual(meta['magic'], 'AZ0x')
        self.assertEqual(meta['wire_version'], 1)
        self.assertEqual(meta['records_count'], 10)
        self.assertTrue(meta['verified'])

        splash_recs = [r for r in meta['records'] if r['name'] == 'splash']
        self.assertEqual(len(splash_recs), 2)
        self.assertEqual(splash_recs[0]['size'], az0x_packer.SPLASH_SIZE)
        self.assertEqual(splash_recs[1]['size'], az0x_packer.SPLASH_SIZE)

    def test_replace_splash(self):
        raw = make_az0x_fixture()
        new_splash = b'N' * az0x_packer.SPLASH_SIZE

        new_img = az0x_packer.replace_splash(raw, new_splash)
        self.assertEqual(len(new_img), len(raw))

        new_meta = az0x_packer.info(new_img)
        expected_sha = hashlib.sha256(new_splash).hexdigest()

        for rec in new_meta['records']:
            if rec['name'] == 'splash':
                self.assertEqual(rec['sha256'], expected_sha)
                # Verify payload bytes in the returned container
                part_off = rec['offset']
                part_size = rec['size']
                self.assertEqual(new_img[part_off:part_off + part_size], new_splash)
            elif rec['name'] == 'boot':
                # Boot records must remain intact
                self.assertNotEqual(rec['sha256'], expected_sha)

    def test_replace_record(self):
        raw = make_az0x_fixture(splash_payload=b'splash data')
        for payload in (b'x', b'larger replacement payload'):
            result = az0x_packer.replace_record(raw, 'PART', 'splash', payload, 1)
            before = az0x_packer.parse_header(raw)['records']
            after = az0x_packer.parse_header(result)['records']
            self.assertEqual(result[after[4]['offset']:after[4]['offset'] + after[4]['size']], payload)
            self.assertEqual(result[after[5]['offset']:after[5]['offset'] + after[5]['size']], b'splash data')
            self.assertEqual((after[5]['offset'] - before[5]['offset']) % 8, 0)
            for rec in after:
                data = result[rec['offset']:rec['offset'] + rec['size']]
                self.assertEqual(hashlib.sha256(data).hexdigest(), rec['sha256'])

        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.replace_record(raw, 'PART', 'splash', b'')
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.replace_record(raw, 'PART', 'splash', b'x')
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.replace_record(raw, 'BOOT', 'boot', b'x', 1)
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.replace_record(raw[:-1] + b'X', 'PART', 'splash', b'x', 1)

    def test_cli_replace_record(self):
        original = self.work / 'original.img'
        payload = self.work / 'payload'
        output = self.work / 'output.img'
        original.write_bytes(make_az0x_fixture(splash_payload=b'splash data'))
        payload.write_bytes(b'new payload')
        self.assertEqual(az0x_packer.main(['replace-record', '--original', str(original),
                         '--magic', 'PART', '--name', 'splash', '--board-mask', '0x1',
                         '--payload', str(payload), '--output', str(output)]), 0)
        self.assertTrue(output.exists())
        with self.assertRaises(SystemExit) as cm:
            az0x_packer.main(['replace-record', '--original', str(original),
                              '--magic', 'PART', '--name', 'splash', '--board-mask', '0x1',
                              '--payload', str(payload), '--output', str(original)])
        self.assertEqual(cm.exception.code, 2)

    def test_validation_errors(self):
        raw = make_az0x_fixture()

        # 1. Invalid magic
        bad_magic = b'AZ01' + raw[4:]
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.info(bad_magic)
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.replace_splash(bad_magic, b'X' * az0x_packer.SPLASH_SIZE)

        # 2. Invalid splash payload size
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.replace_splash(raw, b'short')
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.replace_splash(raw, b'L' * (az0x_packer.SPLASH_SIZE + 1))

        # 3. Truncated header
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.info(raw[:100])

        # 4. Container with no splash record
        no_splash_img = make_az0x_fixture(include_splash=False)
        with self.assertRaises(az0x_packer.SafetyError):
            az0x_packer.replace_splash(no_splash_img, b'X' * az0x_packer.SPLASH_SIZE)

    def test_cli_info_and_replace_splash(self):
        raw = make_az0x_fixture()
        img_path = self.work / 'stock_az0x.img'
        img_path.write_bytes(raw)

        splash_bin = self.work / 'new_splash.bin'
        new_splash = b'C' * az0x_packer.SPLASH_SIZE
        splash_bin.write_bytes(new_splash)

        out_img = self.work / 'output_az0x.img'

        # CLI info
        ret = az0x_packer.main(['info', str(img_path)])
        self.assertEqual(ret, 0)

        # CLI replace-splash
        ret = az0x_packer.main(['replace-splash',
                                '--original', str(img_path),
                                '--splash', str(splash_bin),
                                '--output', str(out_img)])
        self.assertEqual(ret, 0)
        self.assertTrue(out_img.exists())

        out_meta = az0x_packer.info(out_img.read_bytes())
        expected_sha = hashlib.sha256(new_splash).hexdigest()
        splash_recs = [r for r in out_meta['records'] if r['name'] == 'splash']
        self.assertEqual(len(splash_recs), 2)
        for r in splash_recs:
            self.assertEqual(r['sha256'], expected_sha)

    def test_real_sclive2_header_if_present(self):
        real_path = Path('/root/build/sc5000-lab/fw-5.3.0/SCLIVE2-5.3.0-Update.img')
        if not real_path.exists():
            self.skipTest('Real SCLIVE2 firmware not present in test lab')

        with real_path.open('rb') as f:
            hdr = f.read(0x450)
        meta = az0x_packer.info(hdr)
        self.assertEqual(meta['magic'], 'AZ0x')
        self.assertEqual(meta['records_count'], 10)
        names = [r['name'] for r in meta['records']]
        self.assertIn('splash', names)
        self.assertIn('updatesplash', names)
        self.assertIn('rootfs', names)


if __name__ == '__main__':
    unittest.main()
