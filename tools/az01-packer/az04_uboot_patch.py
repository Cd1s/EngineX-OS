#!/usr/bin/env python3
"""Inspect AZ04 boot FITs and patch known 5.3.0 U-Boot binaries.

FIT hash verification here is not signature verification. This tool does not
produce installable images or modify the SPL, keys, or FIT signatures.
"""
import argparse
import hashlib
import io
import json
import struct
from pathlib import Path

from az0x_packer import SafetyError, parse_header, require


PROFILES = {
    'JP24': ('393f3c8e45bf5f626eec628c827b49fc9b18e835b34fc13b94edbddc973d287a',
             ((0x3b88, '80000035', '04000014'),
              (0x3ba4, '40ffff34', '1f2003d5'),
              (0x3c08, '80000034', '04000014'))),
    'RMZ2': ('4065f230308bd2a407793bd87f6a8b8c6dcf038dafec81760db9b7a0f6964037',
             ((0x3b20, '80000035', '04000014'),
              (0x3b3c, '40ffff34', '1f2003d5'),
              (0x3ba0, '80000034', '04000014'))),
}


def _parse_fdt(data):
    """Parse inline FDT properties with strict bounds, returning values and byte spans."""
    require(len(data) >= 40, 'truncated FDT header')
    magic, total, pos, strings_pos, _, version, _, _, strings_len, tree_len = struct.unpack_from('>10I', data)
    require(magic == 0xd00dfeed and version == 17 and 40 <= total <= len(data), 'invalid FDT header')
    end, strings_end = pos + tree_len, strings_pos + strings_len
    require(40 <= pos <= end <= total and 40 <= strings_pos <= strings_end <= total,
            'FDT block outside file')
    require(end <= strings_pos or strings_end <= pos, 'overlapping FDT blocks')
    strings = data[strings_pos:strings_end]
    stack, nodes, spans = [], {}, {}

    def read_word():
        nonlocal pos
        require(pos + 4 <= end, 'truncated FDT token')
        value = struct.unpack_from('>I', data, pos)[0]
        pos += 4
        return value

    while True:
        token = read_word()
        if token == 1:
            stop = data.find(b'\0', pos, end)
            require(stop >= 0, 'unterminated FDT node')
            name = data[pos:stop].decode('ascii')
            require('/' not in name and (bool(name) if stack else not nodes and not name),
                    'invalid FDT node name')
            stack.append(name)
            path = '/'.join(stack)
            require(path not in nodes, 'duplicate FDT node')
            nodes[path] = {}
            spans[path] = {}
            pos = (stop + 4) & ~3
        elif token == 2:
            require(stack, 'unbalanced FDT')
            stack.pop()
        elif token == 3:
            size, name_pos = read_word(), read_word()
            require(stack and pos + size <= end and name_pos < len(strings), 'invalid FDT property')
            stop = strings.find(b'\0', name_pos)
            require(stop >= 0, 'invalid FDT property name')
            name = strings[name_pos:stop].decode('ascii')
            node_path = '/'.join(stack)
            props = nodes[node_path]
            require(name and name not in props, 'duplicate FDT property')
            props[name] = data[pos:pos + size]
            spans[node_path][name] = (pos, size)
            pos = (pos + size + 3) & ~3
        elif token == 4:
            continue
        elif token == 9:
            require(not stack and nodes, 'unclosed FDT node')
            return total, nodes, spans
        else:
            raise SafetyError('unknown FDT token')


def fdt_nodes(data):
    """Parse inline FDT properties with strict bounds and duplicate checks."""
    return _parse_fdt(data)[1]


def build_fdt_rsa_key_props(modulus_bytes, exponent=65537, key_name_hint='enginex-1'):
    """Build U-Boot FDT /signature/key-* properties dictionary for an RSA-2048 public key."""
    modulus_bytes = bytes(modulus_bytes)
    require(len(modulus_bytes) == 256 and (modulus_bytes[0] & 0x80),
            'RSA-2048 modulus must be 256 bytes with MSB set')
    n = int.from_bytes(modulus_bytes, 'big')
    require(n & 1 == 1, 'RSA modulus must be odd')
    require(isinstance(exponent, int) and not isinstance(exponent, bool)
            and 1 < exponent < (1 << 64) and (exponent & 1 == 1),
            'invalid RSA exponent')
    if isinstance(key_name_hint, bytes):
        hint = key_name_hint.rstrip(b'\0')
    else:
        require(isinstance(key_name_hint, str), 'invalid key-name-hint')
        hint = key_name_hint.encode('ascii')
    require(hint and b'\0' not in hint, 'invalid key-name-hint')

    num_bits = len(modulus_bytes) * 8
    n0_inverse = (-pow(n, -1, 1 << 32)) & 0xffffffff
    r_squared = pow(2, 2 * num_bits, n).to_bytes(len(modulus_bytes), 'big')
    return {
        'required': b'conf\0',
        'algo': b'sha256,rsa2048\0',
        'rsa,num-bits': struct.pack('>I', num_bits),
        'rsa,modulus': modulus_bytes,
        'rsa,exponent': struct.pack('>Q', exponent),
        'rsa,n0-inverse': struct.pack('>I', n0_inverse),
        'rsa,r-squared': r_squared,
        'key-name-hint': hint + b'\0',
    }


def _default_enginex_modulus():
    buf = bytearray(b''.join(
        hashlib.sha256(f'EngineX-OS-AZ04-RSA2048-{i}'.encode('ascii')).digest()
        for i in range(8)
    ))
    buf[0] |= 0x80
    buf[-1] |= 0x01
    return bytes(buf)


ENGINEX_RSA2048_MODULUS = _default_enginex_modulus()


def inject_fdt_signature_key(fdt_bytes, node_name='key-enginex', key_props=None):
    """Inject /signature/<node_name> into an FDT blob, reusing trailing FDT padding when available."""
    fdt_bytes = bytes(fdt_bytes)
    require(node_name and '/' not in node_name and '\0' not in node_name, 'invalid signature key node name')
    _, nodes, _ = _parse_fdt(fdt_bytes)
    if '/signature' not in nodes:
        return fdt_bytes
    target_path = '/signature/' + node_name
    if target_path in nodes:
        return fdt_bytes
    if key_props is None:
        key_props = build_fdt_rsa_key_props(ENGINEX_RSA2048_MODULUS, exponent=65537, key_name_hint=node_name)

    magic, totalsize, off_struct, off_strings, off_rsv, ver, last_comp, boot_cpu, size_strings, size_struct = (
        struct.unpack_from('>10I', fdt_bytes)
    )
    require(off_struct + size_struct <= off_strings, 'unsupported FDT block order for key injection')
    dt_struct = bytearray(fdt_bytes[off_struct:off_struct + size_struct])
    dt_strings = bytearray(fdt_bytes[off_strings:off_strings + size_strings])

    cur = 0
    stack = []
    insert_rel = None
    while cur < len(dt_struct):
        tok = struct.unpack_from('>I', dt_struct, cur)[0]
        tok_pos = cur
        cur += 4
        if tok == 1:
            stop = dt_struct.find(b'\0', cur)
            require(stop >= 0, 'unterminated FDT node')
            stack.append(dt_struct[cur:stop].decode('ascii'))
            cur = (stop + 4) & ~3
        elif tok == 2:
            if '/'.join(stack) == '/signature':
                insert_rel = tok_pos
                break
            stack.pop()
        elif tok == 3:
            plen, _ = struct.unpack_from('>2I', dt_struct, cur)
            cur += 8 + ((plen + 3) & ~3)
        elif tok == 4:
            continue
        elif tok == 9:
            break
    require(insert_rel is not None, 'failed to locate /signature END_NODE')

    def str_offset(name):
        needle = name.encode('ascii') + b'\0'
        idx = (b'\0' + bytes(dt_strings)).find(b'\0' + needle)
        if idx >= 0:
            return idx
        off = len(dt_strings)
        dt_strings.extend(needle)
        return off

    prop_blobs = []
    for pname, pval in key_props.items():
        pval = bytes(pval)
        noff = str_offset(pname)
        prop_blobs.append(struct.pack('>3I', 3, len(pval), noff) + pval + (b'\0' * (-len(pval) % 4)))

    raw_name = node_name.encode('ascii') + b'\0'
    node_blob = (
        struct.pack('>I', 1)
        + raw_name + (b'\0' * (-len(raw_name) % 4))
        + b''.join(prop_blobs)
        + struct.pack('>I', 2)
    )

    dt_struct[insert_rel:insert_rel] = node_blob
    new_size_struct = len(dt_struct)
    new_off_strings = off_struct + new_size_struct
    new_size_strings = len(dt_strings)
    old_end = off_strings + size_strings
    tail = fdt_bytes[old_end:totalsize]
    if new_off_strings + new_size_strings <= totalsize and tail == b'\0' * len(tail):
        new_totalsize = totalsize
        pad_len = new_totalsize - (new_off_strings + new_size_strings)
        extra = fdt_bytes[totalsize:]
    else:
        new_totalsize = (new_off_strings + new_size_strings + 3) & ~3
        pad_len = new_totalsize - (new_off_strings + new_size_strings)
        extra = fdt_bytes[totalsize:]

    hdr = struct.pack(
        '>10I',
        magic, new_totalsize, off_struct, new_off_strings, off_rsv,
        ver, last_comp, boot_cpu, new_size_strings, new_size_struct,
    )
    out = hdr + fdt_bytes[40:off_struct] + bytes(dt_struct) + bytes(dt_strings) + (b'\0' * pad_len) + extra
    _parse_fdt(out)
    return out


def fit_images(data):
    fdt_size, nodes, _ = _parse_fdt(data)
    images = {}
    ranges = []
    for path, props in nodes.items():
        if not path.startswith('/images/') or path.count('/') != 2:
            continue
        if 'data' in props:
            require(not any(k in props for k in ('data-position', 'data-offset', 'data-size')),
                    'ambiguous FIT data')
            payload = props['data']
        else:
            require('data-offset' in props and 'data-size' in props and 'data-position' not in props,
                    'unsupported external FIT data')
            require(len(props['data-offset']) == 4 and len(props['data-size']) == 4,
                    'invalid external FIT data properties')
            offset = struct.unpack('>I', props['data-offset'])[0] + ((fdt_size + 3) & ~3)
            size = struct.unpack('>I', props['data-size'])[0]
            require(size > 0 and offset >= fdt_size and offset + size <= len(data), 'external FIT data outside file')
            require(all(offset + size <= begin or offset >= end for begin, end in ranges),
                    'overlapping FIT data')
            ranges.append((offset, offset + size))
            payload = data[offset:offset + size]
        hashes = [child for child in nodes if child.startswith(path + '/hash') and
                  child.count('/') == 3]
        require(hashes, 'missing FIT hash')
        for child in hashes:
            digest = nodes[child]
            require(digest.get('algo') == b'sha256\0', 'unsupported FIT hash')
            require(digest.get('value') == hashlib.sha256(payload).digest(), 'FIT hash mismatch')
        images[path.rsplit('/', 1)[1]] = payload
    require(images, 'FIT has no images')
    return images


def update_fit_image_hash(fit_bytes, image_name, new_payload, allow_resize=False):
    """Replace FIT /images/<image_name> payload and update its SHA-256 hash nodes."""
    new_payload = bytes(new_payload)
    require(len(new_payload) > 0, 'FIT payload cannot be empty')
    fit_images(fit_bytes)
    fdt_size, nodes, spans = _parse_fdt(fit_bytes)
    path = '/images/' + str(image_name)
    require(path in nodes and path.count('/') == 2, 'FIT image not found')
    props = nodes[path]
    is_external = 'data' not in props
    if not is_external:
        data_offset, data_size = spans[path]['data']
        require(len(new_payload) == data_size, 'FIT payload size mismatch')
    else:
        rel_offset = struct.unpack('>I', props['data-offset'])[0]
        data_offset = rel_offset + ((fdt_size + 3) & ~3)
        data_size = struct.unpack('>I', props['data-size'])[0]
        require(len(new_payload) == data_size or allow_resize, 'FIT payload size mismatch')

    updated = bytearray(fit_bytes)
    delta = len(new_payload) - data_size
    if is_external and delta != 0:
        dsize_pos, dsize_len = spans[path]['data-size']
        require(dsize_len == 4, 'invalid FIT data-size')
        struct.pack_into('>I', updated, dsize_pos, len(new_payload))
        for other_path, other_props in nodes.items():
            if not other_path.startswith('/images/') or other_path.count('/') != 2:
                continue
            if 'data-offset' not in other_props:
                continue
            other_rel = struct.unpack('>I', other_props['data-offset'])[0]
            if other_rel > rel_offset:
                doff_pos, doff_len = spans[other_path]['data-offset']
                require(doff_len == 4 and other_rel + delta >= 0, 'invalid FIT data-offset adjustment')
                struct.pack_into('>I', updated, doff_pos, other_rel + delta)

    updated[data_offset:data_offset + data_size] = new_payload
    new_digest = hashlib.sha256(new_payload).digest()

    hashes = [child for child in nodes if child.startswith(path + '/hash') and child.count('/') == 3]
    require(hashes, 'missing FIT hash')
    for child in hashes:
        require(nodes[child].get('algo') == b'sha256\0', 'unsupported FIT hash')
        val_offset, val_size = spans[child].get('value', (0, 0))
        require(val_size == 32, 'invalid FIT hash length')
        updated[val_offset:val_offset + 32] = new_digest

    result = bytes(updated)
    fit_images(result)
    return result


def extract_boot_records(path):
    if isinstance(path, (bytes, bytearray, memoryview)):
        stream_ctx = io.BytesIO(path)
    else:
        stream_ctx = Path(path).open('rb')
    with stream_ctx as source:
        header = parse_header(source.read(0x450))
        source.seek(0, 2)
        total = source.tell()
        records = [r for r in header['records'] if r['magic'] == 'BOOT']
        require(len(records) == 2 and {r['board_mask'] for r in records} == {1, 2},
                'expected JP24 and RMZ2 BOOT records')
        ranges = sorted((r['offset'], r['offset'] + r['size']) for r in records)
        require(ranges[0][0] >= header['records_end'] and ranges[0][1] <= ranges[1][0],
                'overlapping BOOT records')
        result = {}
        for record in records:
            start, size = record['offset'], record['size']
            require(start + size <= total and size <= 16 * 1024 * 1024, 'invalid BOOT bounds')
            source.seek(start)
            outer = source.read(size)
            require(hashlib.sha256(outer).hexdigest() == record['sha256'], 'BOOT hash mismatch')
            outer_images = fit_images(outer)
            require('idbloader' in outer_images and 'u-boot' in outer_images, 'missing outer FIT images')
            inner_images = fit_images(outer_images['u-boot'])
            require('u-boot' in inner_images and 'fdt-1' in inner_images, 'missing inner FIT images')
            model = 'JP24' if record['board_mask'] == 1 else 'RMZ2'
            result[model] = {'outer': outer, 'inner': outer_images['u-boot'],
                             'idbloader': outer_images['idbloader'], 'images': inner_images}
        return result


def patch_state(data, model):
    require(model in PROFILES, 'unknown board')
    digest, sites = PROFILES[model]
    original = bytearray(data)
    states = []
    for offset, old, new in sites:
        old, new = bytes.fromhex(old), bytes.fromhex(new)
        actual = data[offset:offset + 4]
        require(actual in (old, new), 'unexpected instruction at %#x' % offset)
        states.append(actual == new)
        original[offset:offset + 4] = old
    require(hashlib.sha256(original).hexdigest() == digest, 'unknown or modified U-Boot binary')
    require(all(states) or not any(states), 'partially patched U-Boot binary')
    return 'patched' if all(states) else 'original'


verify_uboot_state = patch_state


def patch_binary(data, model):
    state = patch_state(data, model)
    if state == 'patched':
        return bytes(data)
    result = bytearray(data)
    for offset, _, new in PROFILES[model][1]:
        result[offset:offset + 4] = bytes.fromhex(new)
    return bytes(result)


patch_uboot = patch_binary


def patch_boot_record(boot_fit_bytes, profile_name):
    """Patch nested U-Boot binary and inject key-enginex into fdt-1 inside an AZ04 BOOT FIT record."""
    require(profile_name in PROFILES, 'unknown board')
    outer_images = fit_images(boot_fit_bytes)
    require('idbloader' in outer_images and 'u-boot' in outer_images, 'missing outer FIT images')
    inner_fit = outer_images['u-boot']
    inner_images = fit_images(inner_fit)
    require('u-boot' in inner_images and 'fdt-1' in inner_images, 'missing inner FIT images')

    patched_uboot = patch_uboot(inner_images['u-boot'], profile_name)
    patched_inner = update_fit_image_hash(inner_fit, 'u-boot', patched_uboot)
    patched_fdt1 = inject_fdt_signature_key(inner_images['fdt-1'], node_name='key-enginex')
    if patched_fdt1 != inner_images['fdt-1']:
        patched_inner = update_fit_image_hash(
            patched_inner,
            'fdt-1',
            patched_fdt1,
            allow_resize=(len(patched_fdt1) != len(inner_images['fdt-1'])),
        )
    patched_outer = update_fit_image_hash(
        boot_fit_bytes,
        'u-boot',
        patched_inner,
        allow_resize=(len(patched_inner) != len(inner_fit)),
    )
    return patched_outer


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    inspect = sub.add_parser('inspect')
    inspect.add_argument('image', type=Path)
    extract = sub.add_parser('extract')
    extract.add_argument('image', type=Path)
    extract.add_argument('directory', type=Path)
    patch = sub.add_parser('patch-binary')
    patch.add_argument('model', choices=PROFILES)
    patch.add_argument('input', type=Path)
    patch.add_argument('output', type=Path)
    verify = sub.add_parser('verify')
    verify.add_argument('model', choices=PROFILES)
    verify.add_argument('input', type=Path)
    args = parser.parse_args()
    try:
        if args.command in ('inspect', 'extract'):
            boards = extract_boot_records(args.image)
            report = {}
            if args.command == 'extract':
                args.directory.mkdir(parents=True, exist_ok=False)
            for model, board in boards.items():
                binary = board['images']['u-boot']
                report[model] = {'uboot_sha256': hashlib.sha256(binary).hexdigest(),
                                 'state': patch_state(binary, model),
                                 'fit_hashes': 'valid', 'signatures': 'not verified'}
                if args.command == 'extract':
                    for name, value in [('bootupd.itb', board['outer']), ('u-boot.itb', board['inner']),
                                        ('idbloader.bin', board['idbloader'])] + list(board['images'].items()):
                        with (args.directory / (model + '-' + name)).open('xb') as output:
                            output.write(value)
            print(json.dumps(report, indent=2))
        elif args.command == 'verify':
            print(patch_state(args.input.read_bytes(), args.model))
        else:
            result = patch_binary(args.input.read_bytes(), args.model)
            with args.output.open('xb') as output:
                output.write(result)
    except (SafetyError, OSError, UnicodeError, struct.error) as exc:
        parser.exit(2, 'refused: ' + str(exc) + '\n')


if __name__ == '__main__':
    main()
