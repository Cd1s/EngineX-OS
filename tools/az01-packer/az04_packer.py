#!/usr/bin/env python3
"""Inspect AZ04 images and manipulate FIT data for offline analysis.

Modified FITs are not signed and must not be flashed to a device.
"""
import argparse
import hashlib
import json
import os
import re
import shutil
import struct
import subprocess
from pathlib import Path

import az0x_packer as az

SafetyError = az.SafetyError
ZSTD_MAGIC = b'\x28\xb5\x2f\xfd'


def inspect_image(path):
    path = Path(path)
    with path.open('rb') as stream:
        header = stream.read(az.HEADER_SIZE)
        meta = az.parse_header(header)
        size = os.fstat(stream.fileno()).st_size
        for rec in meta['records']:
            az.require(rec['magic'] in ('BOOT', 'PARR', 'PART'), 'unknown record type')
            az.require(rec['size'] > 0 and rec['offset'] >= meta['records_end']
                       and rec['offset'] + rec['size'] <= size, 'invalid record bounds')
            stream.seek(rec['offset'])
            remaining, digest = rec['size'], hashlib.sha256()
            while remaining:
                chunk = stream.read(min(remaining, 1024 * 1024))
                az.require(chunk, 'truncated record')
                remaining -= len(chunk)
                digest.update(chunk)
            az.require(digest.hexdigest() == rec['sha256'], 'record SHA-256 mismatch')
        meta.update(file_size=size, record_hashes_verified=True, signature_verified=False)
        return meta


def _zstd(data, decompress):
    try:
        return subprocess.run(['zstd', '-q', '-c'] + (['-d'] if decompress else []),
                              input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              check=True).stdout
    except (OSError, subprocess.CalledProcessError) as exc:
        raise SafetyError('zstd failed') from exc


def unpack_zstd_cpio_ramdisk(data):
    az.require(data.startswith(ZSTD_MAGIC), 'not a zstd frame')
    raw = _zstd(data, True)
    az.require(raw.startswith((b'070701', b'070702')), 'not a newc cpio archive')
    return raw


def repack_zstd_cpio_ramdisk(data):
    az.require(data.startswith((b'070701', b'070702')), 'not a newc cpio archive')
    return _zstd(data, False)


def _fdt(data):
    az.require(len(data) >= 40, 'truncated FDT header')
    magic, total, off, strings, _, version, _, _, strings_len, tree_len = struct.unpack_from('>10I', data)
    az.require(magic == 0xd00dfeed and version == 17 and 40 <= total <= len(data), 'invalid FDT')
    az.require(40 <= off <= total - tree_len and 40 <= strings <= total - strings_len,
               'invalid FDT blocks')
    end, strings_end = off + tree_len, strings + strings_len
    stack, nodes = [], {}

    def word():
        nonlocal off
        az.require(off + 4 <= end, 'truncated FDT token')
        value = struct.unpack_from('>I', data, off)[0]
        off += 4
        return value

    while True:
        token = word()
        if token == 1:
            stop = data.find(b'\0', off, end)
            az.require(stop >= 0, 'unterminated node')
            try:
                name = data[off:stop].decode('ascii')
            except UnicodeError as exc:
                raise SafetyError('invalid node name') from exc
            az.require('/' not in name and ((not stack and not nodes and not name) or (stack and name)),
                       'invalid node name')
            stack.append(name)
            path = '/'.join(stack)
            az.require(path not in nodes, 'duplicate node')
            nodes[path] = {}
            off = (stop + 4) & ~3
        elif token == 2:
            az.require(stack, 'unbalanced FDT')
            stack.pop()
        elif token == 3:
            length, key = word(), word()
            az.require(stack and off + length <= end and key < strings_len, 'invalid property')
            stop = data.find(b'\0', strings + key, strings_end)
            az.require(stop >= 0, 'invalid property name')
            try:
                name = data[strings + key:stop].decode('ascii')
            except UnicodeError as exc:
                raise SafetyError('invalid property name') from exc
            props = nodes['/'.join(stack)]
            az.require(name and name not in props, 'duplicate property')
            props[name] = (off, length)
            off = (off + length + 3) & ~3
        elif token == 4:
            continue
        elif token == 9:
            az.require(not stack, 'unclosed FDT node')
            break
        else:
            raise SafetyError('unknown FDT token')
    return total, nodes


def fit_external_data(data):
    """Map FIT image nodes to external data; data-offset is relative to totalsize."""
    total, nodes = _fdt(data)
    result = {}
    for path, props in nodes.items():
        if not path.startswith('/images/') or path.count('/') != 2:
            continue
        if 'data-offset' not in props:
            continue
        az.require('data-size' in props and 'data-position' not in props and 'data' not in props,
                   'unsupported FIT data layout')
        def value(name):
            pos, size = props[name]
            az.require(size == 4, 'unsupported FIT data field')
            return struct.unpack_from('>I', data, pos)[0]
        offset, length = total + value('data-offset'), value('data-size')
        az.require(offset >= total and offset + length <= len(data), 'FIT data out of bounds')
        result[path.rsplit('/', 1)[1]] = (offset, length)
    return result


def replace_fit_data(data, name, payload):
    """Replace equal-sized external data and SHA-256 hash; signatures become invalid."""
    _, nodes = _fdt(data)
    entries = fit_external_data(data)
    az.require(name in entries, 'FIT image missing')
    offset, length = entries[name]
    az.require(len(payload) == length, 'FIT replacement must retain its size')
    output = bytearray(data)
    output[offset:offset + length] = payload
    prefix = '/images/' + name + '/'
    hashes = 0
    for path, props in nodes.items():
        if not path.startswith(prefix) or 'algo' not in props:
            continue
        pos, size = props['algo']
        az.require(data[pos:pos + size] == b'sha256\0' and 'value' in props, 'unsupported FIT hash')
        pos, size = props['value']
        az.require(size == 32, 'invalid FIT SHA-256')
        output[pos:pos + size] = hashlib.sha256(payload).digest()
        hashes += 1
    az.require(hashes, 'FIT image has no SHA-256 hash')
    return bytes(output)


def update_verity_root_hash(cmdline, old_hash, new_hash):
    """Replace one exact dm-verity root hash while preserving the DTB size."""
    az.require(re.fullmatch(r'[0-9a-fA-F]{64}', old_hash) is not None and
               re.fullmatch(r'[0-9a-fA-F]{64}', new_hash) is not None, 'invalid SHA-256 root hash')
    old = old_hash.encode('ascii')
    az.require(cmdline.count(old) == 1, 'expected exactly one old root hash')
    return cmdline.replace(old, new_hash.encode('ascii'), 1)


class VerityTreeResult(tuple):
    """dm-verity result supporting both (tree_bytes, root_hash) unpacking and dict keys."""

    @property
    def tree(self):
        return super().__getitem__(0)

    @property
    def root_hash(self):
        return super().__getitem__(1)

    def __getitem__(self, key):
        if isinstance(key, str):
            if key in ('tree', 'hash_tree', 'tree_bytes'):
                return super().__getitem__(0)
            if key == 'root_hash':
                return super().__getitem__(1)
            raise KeyError(key)
        return super().__getitem__(key)


def compute_dm_verity_tree(data_bytes, salt_hex, block_size=1024):
    """Compute standard Linux dm-verity v1 SHA-256 hash tree and root_hash (hex)."""
    az.require(isinstance(block_size, int) and not isinstance(block_size, bool)
               and block_size >= 64 and block_size % 32 == 0
               and (block_size & (block_size - 1)) == 0,
               'invalid dm-verity block_size')
    az.require(isinstance(data_bytes, (bytes, bytearray, memoryview))
               and len(data_bytes) > 0 and len(data_bytes) % block_size == 0,
               'data_bytes length must be a positive multiple of block_size')
    az.require(isinstance(salt_hex, str) and len(salt_hex) >= 2 and len(salt_hex) % 2 == 0
               and re.fullmatch(r'[0-9a-fA-F]+', salt_hex) is not None,
               'invalid dm-verity salt_hex')
    salt = bytes.fromhex(salt_hex)
    data_view = memoryview(data_bytes)

    def hash_level(buf):
        out = bytearray()
        for off in range(0, len(buf), block_size):
            digest = hashlib.sha256()
            digest.update(salt)
            digest.update(buf[off:off + block_size])
            out.extend(digest.digest())
        rem = len(out) % block_size
        if rem:
            out.extend(b'\x00' * (block_size - rem))
        return bytes(out)

    level = hash_level(data_view)
    levels = [level]
    while len(level) > block_size:
        level = hash_level(level)
        levels.append(level)
    tree_bytes = b''.join(reversed(levels))
    root_digest = hashlib.sha256()
    root_digest.update(salt)
    root_digest.update(level)
    return VerityTreeResult((tree_bytes, root_digest.hexdigest()))


def repack_az04_container(data, replacements):
    """Replace top-level AZ0x records by index or (magic, name) and recalculate offsets/sizes/SHA-256."""
    if isinstance(data, (str, Path)):
        data = Path(data).read_bytes()
    az.require(isinstance(data, (bytes, bytearray, memoryview)), 'invalid container data')
    az.require(isinstance(replacements, dict) and replacements, 'replacements must be a non-empty dict')

    raw = memoryview(data)
    meta = az.parse_header(bytes(raw[:az.HEADER_SIZE]))
    total_size = len(raw)
    az.require(meta['records'], 'AZ0x container has no records')

    for rec in meta['records']:
        az.require(rec['magic'] in ('BOOT', 'PARR', 'PART'), 'unknown record type')
        az.require(rec['size'] > 0 and rec['offset'] >= meta['records_end']
                   and rec['offset'] + rec['size'] <= total_size, 'invalid record bounds')
        actual_sha = hashlib.sha256(raw[rec['offset']:rec['offset'] + rec['size']]).hexdigest()
        az.require(actual_sha == rec['sha256'], 'record SHA-256 mismatch')

    used_keys = set()
    record_payloads = []
    for rec in meta['records']:
        candidate_keys = [
            rec['index'],
            (rec['magic'], rec['name']),
            (rec['magic'], rec['name'], rec['board_mask']),
        ]
        if rec['magic'] == 'BOOT':
            if rec['board_mask'] == 1:
                candidate_keys.append(('BOOT', 'JP24'))
            elif rec['board_mask'] == 2:
                candidate_keys.append(('BOOT', 'RMZ2'))
        matched = [k for k in candidate_keys if k in replacements]
        az.require(len(matched) <= 1, f'conflicting replacement keys for record {rec["index"]}')
        if matched:
            key = matched[0]
            used_keys.add(key)
            payload = replacements[key]
            az.require(isinstance(payload, (bytes, bytearray, memoryview)) and len(payload) > 0,
                       f'invalid replacement payload for {key!r}')
            record_payloads.append(bytes(payload))
        else:
            record_payloads.append(bytes(raw[rec['offset']:rec['offset'] + rec['size']]))

    az.require(used_keys == set(replacements.keys()), 'unmatched replacement key')

    first_offset = min(r['offset'] for r in meta['records'])
    last_orig_end = max(r['offset'] + r['size'] for r in meta['records'])
    pad_eof_to_8 = (total_size == ((last_orig_end + 7) & ~7))

    header_buf = bytearray(raw[:first_offset])
    body_chunks = []
    cursor = first_offset

    for idx, (rec, payload) in enumerate(zip(meta['records'], record_payloads)):
        if idx > 0:
            aligned = (cursor + 7) & ~7
            if aligned > cursor:
                body_chunks.append(b'\x00' * (aligned - cursor))
                cursor = aligned
        new_offset = cursor
        new_size = len(payload)
        new_sha = hashlib.sha256(payload).digest()
        roff = rec['record_offset']
        struct.pack_into('<4sIQQII', header_buf, roff,
                         rec['magic'].encode('ascii'),
                         rec['flags'],
                         new_offset,
                         new_size,
                         rec['name_offset'],
                         rec['board_mask'])
        header_buf[roff + 32:roff + 64] = new_sha
        body_chunks.append(payload)
        cursor += new_size

    if pad_eof_to_8 and (cursor % 8 != 0):
        body_chunks.append(b'\x00' * (((cursor + 7) & ~7) - cursor))

    return bytes(header_buf) + b''.join(body_chunks)


def build_usb_bundle(image, output_dir):
    """Copy an image, write SHA256SUMS.txt and manifest.json; device update discovery is not verified."""
    image, output_dir = Path(image), Path(output_dir)
    meta = inspect_image(image)
    output_dir.mkdir(parents=True, exist_ok=False)
    target = output_dir / 'update.img'
    with image.open('rb') as source, target.open('xb') as dest:
        shutil.copyfileobj(source, dest, length=1024 * 1024)
    target_meta = inspect_image(target)
    digest = hashlib.sha256()
    with target.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(block)
    image_sha256 = digest.hexdigest()
    (output_dir / 'SHA256SUMS.txt').write_text(image_sha256 + '  update.img\n', encoding='ascii')
    manifest = {
        'image': 'update.img',
        'file_size': target_meta['file_size'],
        'sha256': image_sha256,
        'records_count': target_meta['records_count'],
        'records': [
            {
                'index': rec['index'],
                'magic': rec['magic'],
                'name': rec['name'],
                'board_mask': rec['board_mask'],
                'flags': rec['flags'],
                'offset': rec['offset'],
                'size': rec['size'],
                'sha256': rec['sha256'],
            }
            for rec in target_meta['records']
        ],
    }
    (output_dir / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')
    (output_dir / 'README.txt').write_text(
        'Signature verification and device USB update discovery have not been verified.\n'
        'Record hashes and this checksum do not prove the image is trusted or flashable.\n',
        encoding='ascii')
    return manifest


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('image', type=Path)
    parser.add_argument('--build-usb-bundle', type=Path, metavar='OUTPUT_DIR')
    args = parser.parse_args(argv)
    try:
        meta = inspect_image(args.image)
        if args.build_usb_bundle:
            build_usb_bundle(args.image, args.build_usb_bundle)
            meta['usb_bundle'] = str(args.build_usb_bundle)
            meta['usb_discovery_verified'] = False
        print(json.dumps(meta, indent=2))
    except (SafetyError, OSError) as exc:
        parser.exit(2, f'refused: {exc}\n')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
