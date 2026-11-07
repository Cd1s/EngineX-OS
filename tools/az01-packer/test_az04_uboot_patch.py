"""Tests of real AZ04 5.3.0 boot FITs; no firmware is modified."""
import hashlib
import struct
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import az04_uboot_patch as patcher

FIRMWARE = Path('/root/build/sc5000-lab/fw-5.3.0-gen2/SYSTEMONE-5.3.0-Update.img')


@unittest.skipUnless(FIRMWARE.is_file(), 'SYSTEMONE 5.3.0 firmware not available')
class AZ04BootTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.boards = patcher.extract_boot_records(FIRMWARE)

    def test_both_nested_fit_images_and_hashes(self):
        self.assertEqual(set(self.boards), {'JP24', 'RMZ2'})
        for model, board in self.boards.items():
            with self.subTest(model=model):
                outer = patcher.fit_images(board['outer'])
                self.assertEqual(outer['u-boot'], board['inner'])
                self.assertEqual(outer['idbloader'], board['idbloader'])
                inner = patcher.fit_images(board['inner'])
                self.assertEqual(inner, board['images'])
                self.assertIn('fdt-1', inner)
                self.assertEqual(hashlib.sha256(inner['u-boot']).hexdigest(),
                                 patcher.PROFILES[model][0])
                self.assertIn('/signature/key-denon-1' if model == 'JP24' else
                              '/signature/key-rane-1', patcher.fdt_nodes(inner['fdt-1']))

    def test_branch_patch_is_idempotent_and_version_bound(self):
        for model, board in self.boards.items():
            with self.subTest(model=model):
                binary = board['images']['u-boot']
                self.assertEqual(patcher.patch_state(binary, model), 'original')
                modified = patcher.patch_binary(binary, model)
                self.assertEqual(patcher.patch_state(modified, model), 'patched')
                self.assertEqual(patcher.patch_binary(modified, model), modified)
                self.assertEqual(len(binary), len(modified))
                changed = {i for i, (a, b) in enumerate(zip(binary, modified)) if a != b}
                allowed = {offset + i for offset, _, _ in patcher.PROFILES[model][1] for i in range(4)}
                self.assertTrue(changed)
                self.assertLessEqual(changed, allowed)
                corrupted = bytearray(modified)
                corrupted[-1] ^= 1
                with self.assertRaises(patcher.SafetyError):
                    patcher.patch_state(corrupted, model)
                other = 'RMZ2' if model == 'JP24' else 'JP24'
                with self.assertRaises(patcher.SafetyError):
                    patcher.patch_binary(binary, other)

    def test_partial_patch_is_rejected(self):
        binary = bytearray(self.boards['RMZ2']['images']['u-boot'])
        offset, _, replacement = patcher.PROFILES['RMZ2'][1][0]
        binary[offset:offset + 4] = bytes.fromhex(replacement)
        with self.assertRaises(patcher.SafetyError):
            patcher.patch_state(binary, 'RMZ2')

    def test_modified_external_data_is_rejected_at_both_levels(self):
        board = self.boards['RMZ2']
        for fit in (board['outer'], board['inner']):
            with self.subTest(size=len(fit)):
                corrupted = bytearray(fit)
                nodes = patcher.fdt_nodes(fit)
                image = nodes['/images/u-boot']
                offset = ((struct.unpack_from('>I', fit, 4)[0] + 3) & ~3) + struct.unpack('>I', image['data-offset'])[0]
                corrupted[offset] ^= 1
                with self.assertRaisesRegex(patcher.SafetyError, 'hash mismatch'):
                    patcher.fit_images(corrupted)

    def test_truncated_external_data_is_rejected(self):
        with self.assertRaises(patcher.SafetyError):
            patcher.fit_images(self.boards['RMZ2']['inner'][:-1])

    def test_fdt_rsa_key_props_matches_vendor_dtbs(self):
        for model, key_node, hint in (('JP24', '/signature/key-denon-1', 'denon-1'),
                                      ('RMZ2', '/signature/key-rane-1', 'rane-1')):
            with self.subTest(model=model):
                nodes = patcher.fdt_nodes(self.boards[model]['images']['fdt-1'])
                expected = nodes[key_node]
                computed = patcher.build_fdt_rsa_key_props(expected['rsa,modulus'],
                                                           exponent=65537,
                                                           key_name_hint=hint)
                self.assertEqual(computed, expected)

    def test_patch_boot_record_and_update_fit_image_hash(self):
        for model, board in self.boards.items():
            with self.subTest(model=model):
                patched_boot = patcher.patch_boot_record(board['outer'], model)
                self.assertEqual(len(patched_boot), len(board['outer']))
                self.assertNotEqual(patched_boot, board['outer'])
                # Idempotent when applied again
                self.assertEqual(patcher.patch_boot_record(patched_boot, model), patched_boot)

                outer_imgs = patcher.fit_images(patched_boot)
                self.assertEqual(outer_imgs['idbloader'], board['idbloader'])
                inner_imgs = patcher.fit_images(outer_imgs['u-boot'])
                self.assertEqual(patcher.verify_uboot_state(inner_imgs['u-boot'], model), 'patched')
                fdt1_nodes = patcher.fdt_nodes(inner_imgs['fdt-1'])
                self.assertIn('/signature/key-denon-1' if model == 'JP24' else '/signature/key-rane-1', fdt1_nodes)
                self.assertIn('/signature/key-enginex', fdt1_nodes)
                self.assertEqual(fdt1_nodes['/signature/key-enginex']['key-name-hint'], b'key-enginex\0')

                with self.assertRaises(patcher.SafetyError):
                    patcher.update_fit_image_hash(board['inner'], 'u-boot', b'short')
                with self.assertRaises(patcher.SafetyError):
                    patcher.update_fit_image_hash(board['inner'], 'missing', board['images']['u-boot'])


class FDTValidationTests(unittest.TestCase):
    def test_bad_header_is_rejected(self):
        with self.assertRaises(patcher.SafetyError):
            patcher.fdt_nodes(b'bad')

    def test_build_fdt_rsa_key_props_math_and_validation(self):
        # Construct deterministic 2048-bit odd modulus with MSB set
        n = (1 << 2047) | ((1 << 1024) + 0x12345679)
        modulus = n.to_bytes(256, 'big')
        props = patcher.build_fdt_rsa_key_props(modulus)
        self.assertEqual(props['algo'], b'sha256,rsa2048\0')
        self.assertEqual(props['key-name-hint'], b'enginex-1\0')
        self.assertEqual(props['rsa,num-bits'], struct.pack('>I', 2048))
        self.assertEqual(props['rsa,modulus'], modulus)
        self.assertEqual(props['rsa,exponent'], struct.pack('>Q', 65537))
        n0_inv = struct.unpack('>I', props['rsa,n0-inverse'])[0]
        self.assertEqual((n * n0_inv) & 0xffffffff, 0xffffffff)
        r_sq = int.from_bytes(props['rsa,r-squared'], 'big')
        self.assertEqual(r_sq, pow(2, 4096, n))

        with self.assertRaises(patcher.SafetyError):
            patcher.build_fdt_rsa_key_props(b'\xff' * 128)
        with self.assertRaises(patcher.SafetyError):
            patcher.build_fdt_rsa_key_props((n - 1).to_bytes(256, 'big'))
        with self.assertRaises(patcher.SafetyError):
            patcher.build_fdt_rsa_key_props(modulus, exponent=65536)


if __name__ == '__main__':
    unittest.main()
