"""Offline AZ04 parsing and FIT data tests."""
import hashlib
import json
import struct
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import az04_packer as az
import az04_uboot_patch as ub_patch
from test_az0x_packer import make_az0x_fixture


def make_az04_5rec_fixture():
    """Construct a valid 5-record AZ04 container (BOOT JP24, BOOT RMZ2, PARR recovery, PART boot, PART rootfs)."""
    hdr = bytearray(0x450)
    struct.pack_into('<4sI', hdr, 0, b'AZ0x', 1)
    rec_start = 0xc8
    rec_end = rec_start + 5 * 64
    struct.pack_into('<IIII', hdr, 0x10, 0x38, 0xa8, rec_start, rec_end)
    hdr[0x70:0x79] = b'recovery\0'
    hdr[0x79:0x7e] = b'boot\0'
    hdr[0x7e:0x85] = b'rootfs\0'

    spec = [
        (b'BOOT', 0x80000000, 0, 1, b'JP24_BOOT_FIT_PAYLOAD' * 8),
        (b'BOOT', 0x80000000, 0, 2, b'RMZ2_BOOT_FIT_PAYLOAD' * 8),
        (b'PARR', 0x1, 0x70 - 0x38, 0, b'RECOVERY_EXT4_XZ' * 10),
        (b'PART', 0x1, 0x79 - 0x38, 0, b'BOOT_EXT4_XZ' * 12),
        (b'PART', 0x1, 0x7e - 0x38, 0, b'ROOTFS_EXT4_XZ' * 16),
    ]
    body = bytearray()
    cursor = 0x450
    for i, (magic, flags, name_off, bmask, payload) in enumerate(spec):
        if i > 0 and (cursor % 8):
            pad = ((cursor + 7) & ~7) - cursor
            body.extend(b'\x00' * pad)
            cursor += pad
        roff = rec_start + i * 64
        struct.pack_into('<4sIQQII', hdr, roff, magic, flags, cursor, len(payload), name_off, bmask)
        hdr[roff + 32:roff + 64] = hashlib.sha256(payload).digest()
        body.extend(payload)
        cursor += len(payload)
    if cursor % 8:
        body.extend(b'\x00' * (((cursor + 7) & ~7) - cursor))
    return bytes(hdr) + bytes(body)


def make_fit(payload):
    strings = b'data-offset\0data-size\0algo\0value\0'
    names = {name: strings.index(name.encode() + b'\0') for name in
             ('data-offset', 'data-size', 'algo', 'value')}
    def word(value):
        return struct.pack('>I', value)
    def node(name):
        raw = name.encode() + b'\0'
        return word(1) + raw + b'\0' * (-len(raw) % 4)
    def prop(name, value):
        return word(3) + word(len(value)) + word(names[name]) + value + b'\0' * (-len(value) % 4)
    tree = (node('') + node('images') + node('ramdisk') +
            prop('data-offset', word(0)) + prop('data-size', word(len(payload))) +
            node('hash-1') + prop('algo', b'sha256\0') +
            prop('value', hashlib.sha256(payload).digest()) +
            word(2) + word(2) + word(2) + word(2) + word(9))
    off = 56
    strings_off = off + len(tree)
    total = strings_off + len(strings)
    header = struct.pack('>10I', 0xd00dfeed, total, off, strings_off, 40, 17, 16, 0,
                         len(strings), len(tree))
    return header + b'\0' * 16 + tree + strings + payload


class AZ04Tests(unittest.TestCase):
    def test_parr_record_and_corruption(self):
        data = bytearray(make_az0x_fixture())
        data[0x70:0x79] = b'recovery\0'
        data[0xc8:0xcc] = b'PARR'
        struct.pack_into('<I', data, 0xc8 + 24, 0x38)
        with tempfile.TemporaryDirectory() as tmp:
            image = Path(tmp) / 'update.img'
            image.write_bytes(data)
            meta = az.inspect_image(image)
            self.assertEqual((meta['records'][0]['magic'], meta['records'][0]['name']),
                             ('PARR', 'recovery'))
            self.assertTrue(meta['record_hashes_verified'])
            self.assertFalse(meta['signature_verified'])
            data[-1] ^= 1
            image.write_bytes(data)
            with self.assertRaises(az.SafetyError):
                az.inspect_image(image)

    def test_zstd_ramdisk(self):
        raw = b'070701' + b'0' * 104 + b'TRAILER!!!\0'
        encoded = az.repack_zstd_cpio_ramdisk(raw)
        self.assertTrue(encoded.startswith(az.ZSTD_MAGIC))
        self.assertEqual(az.unpack_zstd_cpio_ramdisk(encoded), raw)
        with self.assertRaises(az.SafetyError):
            az.unpack_zstd_cpio_ramdisk(b'not zstd')

    def test_external_data_and_update(self):
        fit = make_fit(b'old data')
        total = struct.unpack_from('>I', fit, 4)[0]
        self.assertEqual(az.fit_external_data(fit)['ramdisk'], (total, 8))
        changed = az.replace_fit_data(fit, 'ramdisk', b'new data')
        self.assertEqual(changed[total:], b'new data')
        self.assertIn(hashlib.sha256(b'new data').digest(), changed[:total])
        with self.assertRaises(az.SafetyError):
            az.replace_fit_data(fit, 'ramdisk', b'wrong length')
        with self.assertRaises(az.SafetyError):
            az.fit_external_data(fit[:-1])

    def test_verity_hash(self):
        old, new = 'a' * 64, 'b' * 64
        self.assertEqual(az.update_verity_root_hash(b'dm=' + old.encode(), old, new),
                         b'dm=' + new.encode())
        with self.assertRaises(az.SafetyError):
            az.update_verity_root_hash(old.encode() * 2, old, new)

    def test_compute_dm_verity_tree(self):
        salt_hex = '306a938d151f51cb1a26665a1194524d4d8c6333d56214417c165a6852a9cbb3'
        salt = bytes.fromhex(salt_hex)
        # 33 blocks of 1024 bytes -> 2 leaf hash blocks -> 1 top hash block (total 3 blocks = 3072 bytes)
        data = b''.join(bytes([i & 0xff]) * 1024 for i in range(33))
        res = az.compute_dm_verity_tree(data, salt_hex, block_size=1024)
        tree, root_hash = res
        self.assertEqual(res['tree'], tree)
        self.assertEqual(res['root_hash'], root_hash)
        self.assertEqual(res.tree, tree)
        self.assertEqual(res.root_hash, root_hash)
        self.assertEqual(len(tree), 3 * 1024)

        leaf0 = b''.join(hashlib.sha256(salt + data[i * 1024:(i + 1) * 1024]).digest() for i in range(32))
        leaf1 = hashlib.sha256(salt + data[32 * 1024:33 * 1024]).digest() + b'\x00' * (1024 - 32)
        top = hashlib.sha256(salt + leaf0).digest() + hashlib.sha256(salt + leaf1).digest() + b'\x00' * (1024 - 64)
        self.assertEqual(tree[:1024], top)
        self.assertEqual(tree[1024:2048], leaf0)
        self.assertEqual(tree[2048:3072], leaf1)
        self.assertEqual(root_hash, hashlib.sha256(salt + top).hexdigest())

        with self.assertRaises(az.SafetyError):
            az.compute_dm_verity_tree(b'not aligned', salt_hex, block_size=1024)
        with self.assertRaises(az.SafetyError):
            az.compute_dm_verity_tree(data, 'xyz', block_size=1024)
        with self.assertRaises(az.SafetyError):
            az.compute_dm_verity_tree(data, salt_hex, block_size=1000)

    def test_repack_az04_container(self):
        raw = make_az04_5rec_fixture()
        new_jp24 = b'PATCHED_JP24_BOOT_FIT_DATA_LONGER_PAYLOAD' * 5
        new_rmz2 = b'PATCHED_RMZ2_BOOT_FIT_DATA' * 7
        new_rootfs = b'PATCHED_ROOTFS_PAYLOAD' * 20

        repacked = az.repack_az04_container(raw, {
            0: new_jp24,
            ('BOOT', 'RMZ2'): new_rmz2,
            ('PART', 'rootfs'): new_rootfs,
        })
        with tempfile.TemporaryDirectory() as tmp:
            img = Path(tmp) / 'repacked.img'
            img.write_bytes(repacked)
            meta = az.inspect_image(img)
            self.assertEqual(meta['records_count'], 5)
            self.assertTrue(meta['record_hashes_verified'])
            recs = meta['records']
            self.assertEqual(recs[0]['size'], len(new_jp24))
            self.assertEqual(recs[0]['sha256'], hashlib.sha256(new_jp24).hexdigest())
            self.assertEqual(recs[1]['size'], len(new_rmz2))
            self.assertEqual(recs[1]['sha256'], hashlib.sha256(new_rmz2).hexdigest())
            self.assertEqual(recs[4]['size'], len(new_rootfs))
            self.assertEqual(recs[4]['sha256'], hashlib.sha256(new_rootfs).hexdigest())
            for r in recs:
                self.assertEqual(r['offset'] % 8, 0)

        with self.assertRaises(az.SafetyError):
            az.repack_az04_container(raw, {('PART', 'nonexistent'): b'data'})
        with self.assertRaises(az.SafetyError):
            az.repack_az04_container(raw, {0: b'a', ('BOOT', 'JP24'): b'b'})

    def test_usb_bundle(self):
        data = make_az04_5rec_fixture()
        with tempfile.TemporaryDirectory() as tmp:
            image, output = Path(tmp) / 'source.img', Path(tmp) / 'usb'
            image.write_bytes(data)
            self.assertEqual(az.main([str(image), '--build-usb-bundle', str(output)]), 0)
            self.assertEqual((output / 'update.img').read_bytes(), data)
            self.assertEqual((output / 'SHA256SUMS.txt').read_text(),
                             hashlib.sha256(data).hexdigest() + '  update.img\n')
            manifest = json.loads((output / 'manifest.json').read_text())
            self.assertEqual(manifest['image'], 'update.img')
            self.assertEqual(manifest['sha256'], hashlib.sha256(data).hexdigest())
            self.assertEqual(manifest['records_count'], 5)
            self.assertEqual([(r['index'], r['magic'], r['name']) for r in manifest['records']],
                             [(0, 'BOOT', 'boot'),
                              (1, 'BOOT', 'boot'),
                              (2, 'PARR', 'recovery'),
                              (3, 'PART', 'boot'),
                              (4, 'PART', 'rootfs')])
            for r in manifest['records']:
                self.assertTrue(r['size'] > 0 and len(r['sha256']) == 64)
            self.assertIn('not been verified', (output / 'README.txt').read_text())
            with self.assertRaises(FileExistsError):
                az.build_usb_bundle(image, output)

    def test_inject_fdt_signature_key_and_fit_resize(self):
        strings = b'key-name-hint\0'
        def word(value):
            return struct.pack('>I', value)
        def node(name):
            raw = name.encode('ascii') + b'\0'
            return word(1) + raw + b'\0' * (-len(raw) % 4)
        def prop(name_off, value):
            return word(3) + word(len(value)) + word(name_off) + value + b'\0' * (-len(value) % 4)
        tree = (node('') + node('signature') + node('key-denon-1') +
                prop(0, b'denon-1\0') + word(2) + word(2) + word(2) + word(9))
        off = 56
        strings_off = off + len(tree)
        total = strings_off + len(strings) + 1024
        fdt = (struct.pack('>10I', 0xd00dfeed, total, off, strings_off, 40, 17, 16, 0,
                           len(strings), len(tree)) +
               b'\0' * 16 + tree + strings + b'\0' * 1024)
        injected = ub_patch.inject_fdt_signature_key(fdt, node_name='key-enginex')
        self.assertEqual(len(injected), len(fdt))
        self.assertEqual(ub_patch.inject_fdt_signature_key(injected, node_name='key-enginex'), injected)
        nodes = ub_patch.fdt_nodes(injected)
        self.assertIn('/signature/key-denon-1', nodes)
        self.assertIn('/signature/key-enginex', nodes)
        self.assertEqual(nodes['/signature/key-enginex']['key-name-hint'], b'key-enginex\0')

        fit = make_fit(b'old data')
        # Align FDT totalsize to 4 bytes for U-Boot FIT external data convention
        fdt_total = struct.unpack_from('>I', fit, 4)[0]
        aligned_total = (fdt_total + 3) & ~3
        fit_aligned = (fit[:4] + struct.pack('>I', aligned_total) + fit[8:fdt_total] +
                       b'\0' * (aligned_total - fdt_total) + fit[fdt_total:])
        resized = ub_patch.update_fit_image_hash(fit_aligned, 'ramdisk', b'longer new payload', allow_resize=True)
        self.assertEqual(ub_patch.fit_images(resized)['ramdisk'], b'longer new payload')


if __name__ == '__main__':
    unittest.main()
