#!/usr/bin/env python3
"""EngineX OS AZ0x Container Packer / Inspector (#119).

Parses 2nd-generation signed AZ0x firmware containers (0x0000..0x0450 header,
containing records between 0x00c8..0x0348: 10 records of 64 bytes each with
struct.unpack("<4sIQQII", rec[:32]) + 32-byte sha256).

Provides:
- info(data) / CLI: prints container metadata and record descriptors.
- replace_splash(data, splash_bytes): replaces PART splash payload and recalculates SHA-256.
"""
import argparse
import hashlib
import json
import os
import struct
import sys
from pathlib import Path

MAGIC = b'AZ0x'
HEADER_SIZE = 0x450
RECORDS_START_DEFAULT = 0x00c8
RECORDS_END_DEFAULT = 0x0348
RECORD_SIZE = 64
SPLASH_SIZE = 4096000  # 0x3e8000


class SafetyError(ValueError):
    """Refuse corrupted, unexpected or unsafe AZ0x containers."""


def require(condition, message):
    if not condition:
        raise SafetyError(message)


def parse_header(data):
    """Parse AZ0x container header (0x0000..0x0450) and return metadata dictionary."""
    require(len(data) >= HEADER_SIZE, f'truncated AZ0x header: need at least {HEADER_SIZE} bytes')
    require(data[:4] == MAGIC, f'invalid AZ0x magic: expected {MAGIC!r}, got {data[:4]!r}')

    wire_version = struct.unpack('<I', data[4:8])[0]
    require(wire_version == 1, f'unsupported AZ0x wire version: {wire_version}')

    str_base = struct.unpack('<I', data[0x10:0x14])[0] or 0x38
    rec_start = struct.unpack('<I', data[0x18:0x1c])[0] or RECORDS_START_DEFAULT
    rec_end = struct.unpack('<I', data[0x1c:0x20])[0] or RECORDS_END_DEFAULT

    require(0 < rec_start < rec_end <= HEADER_SIZE, f'invalid record bounds: {rec_start:#x}..{rec_end:#x}')
    require((rec_end - rec_start) % RECORD_SIZE == 0, f'record table size must be multiple of {RECORD_SIZE}')

    records = []
    num_records = (rec_end - rec_start) // RECORD_SIZE
    for i in range(num_records):
        roff = rec_start + i * RECORD_SIZE
        rec = data[roff:roff + RECORD_SIZE]
        tag, flags, offset, size, name_off, board_mask = struct.unpack('<4sIQQII', rec[:32])
        sha256 = rec[32:64].hex()

        name = ''
        if name_off != 0:
            spos = str_base + name_off
            if spos < HEADER_SIZE:
                end = data.find(b'\x00', spos)
                if end != -1 and end <= HEADER_SIZE:
                    name = data[spos:end].decode('ascii', errors='replace')
        elif tag == b'BOOT':
            name = 'boot'

        records.append({
            'index': i,
            'magic': tag.decode('ascii', errors='replace'),
            'name': name,
            'flags': flags,
            'offset': offset,
            'size': size,
            'name_offset': name_off,
            'board_mask': board_mask,
            'sha256': sha256,
            'record_offset': roff,
        })

    return {
        'magic': 'AZ0x',
        'wire_version': wire_version,
        'records_start': rec_start,
        'records_end': rec_end,
        'records_count': len(records),
        'records': records,
    }


def info(data):
    """Return JSON-serializable info dictionary describing the AZ0x container."""
    hdr = parse_header(data)
    return {
        'magic': hdr['magic'],
        'wire_version': hdr['wire_version'],
        'file_size': len(data),
        'sha256': hashlib.sha256(data).hexdigest(),
        'records_count': hdr['records_count'],
        'records': hdr['records'],
        'verified': True,
    }


def replace_record(data, magic, name, new_payload, board_mask=None):
    """Replace one record for offline analysis; the container signature is invalidated."""
    require(isinstance(new_payload, bytes) and new_payload, 'replacement payload must be non-empty bytes')
    hdr = parse_header(data)
    records = hdr['records']
    for rec in records:
        start, size = rec['offset'], rec['size']
        require(size > 0 and start >= HEADER_SIZE and start + size <= len(data), 'invalid record bounds')
        require(hashlib.sha256(data[start:start + size]).hexdigest() == rec['sha256'],
                f'record {rec["index"]} SHA-256 mismatch')
    matches = [r for r in records if r['magic'] == magic and r['name'] == name
               and (board_mask is None or r['board_mask'] == board_mask)]
    require(len(matches) == 1, 'expected exactly one matching record; specify board_mask if needed')
    target = matches[0]
    start, end = target['offset'], target['offset'] + target['size']
    for rec in records:
        if rec is not target:
            require(rec['offset'] + rec['size'] <= start or rec['offset'] >= end,
                    'replacement overlaps another record')

    # Preserve the alignment of all later data and the original gaps and trailer.
    padding = (target['size'] - len(new_payload)) % 8
    delta = len(new_payload) + padding - target['size']
    result = bytearray(data[:start] + new_payload + b'\0' * padding + data[end:])
    for rec in records:
        roff = rec['record_offset']
        if rec is target:
            struct.pack_into('<Q', result, roff + 16, len(new_payload))
            result[roff + 32:roff + 64] = hashlib.sha256(new_payload).digest()
        elif rec['offset'] >= end:
            struct.pack_into('<Q', result, roff + 8, rec['offset'] + delta)
    for rec in parse_header(result)['records']:
        payload = result[rec['offset']:rec['offset'] + rec['size']]
        require(len(payload) == rec['size'] and hashlib.sha256(payload).hexdigest() == rec['sha256'],
                f'output record {rec["index"]} SHA-256 mismatch')
    return bytes(result)


def replace_splash(data, splash_bytes):
    """Replace PART splash partition payload and update 32-byte sha256 in header.

    Returns the updated AZ0x image bytes.
    """
    require(data[:4] == MAGIC, f'not an AZ0x container: {data[:4]!r}')
    require(len(splash_bytes) == SPLASH_SIZE,
            f'splash payload must be exactly {SPLASH_SIZE} bytes (got {len(splash_bytes)})')

    hdr = parse_header(data)
    splash_records = [r for r in hdr['records'] if r['magic'] == 'PART' and r['name'] == 'splash']
    require(splash_records, 'no PART splash record found in AZ0x container')

    result = bytearray(data)
    new_sha = hashlib.sha256(splash_bytes).digest()

    for rec in splash_records:
        roff = rec['record_offset']
        part_off = rec['offset']
        part_size = rec['size']

        require(part_size == len(splash_bytes),
                f'splash partition size mismatch: record {rec["index"]} size={part_size}, payload={len(splash_bytes)}')
        require(part_off + part_size <= len(result),
                f'splash partition offset out of file bounds: {part_off} + {part_size} > {len(result)}')

        # Update payload
        result[part_off:part_off + part_size] = splash_bytes
        # Update 32-byte SHA-256 in record
        result[roff + 32:roff + 64] = new_sha

    return bytes(result)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)

    p_info = sub.add_parser('info', help='Print AZ0x container metadata')
    p_info.add_argument('input', type=Path, help='Input AZ0x container (.img)')

    p_replace = sub.add_parser('replace-splash', help='Replace splash in AZ0x container')
    p_replace.add_argument('--original', required=True, type=Path, help='Stock AZ0x container (.img)')
    p_replace.add_argument('--splash', required=True, type=Path, help='4,096,000-byte raw splash file')
    p_replace.add_argument('--output', required=True, type=Path, help='Output AZ0x container (.img)')

    p_record = sub.add_parser('replace-record', help='Replace a record for offline analysis (output is unsigned)')
    p_record.add_argument('--original', required=True, type=Path)
    p_record.add_argument('--magic', required=True)
    p_record.add_argument('--name', required=True)
    p_record.add_argument('--board-mask', type=lambda value: int(value, 0))
    p_record.add_argument('--payload', required=True, type=Path)
    p_record.add_argument('--output', required=True, type=Path)

    args = parser.parse_args(argv)

    try:
        if args.command == 'info':
            require(args.input.is_file(), f'file not found: {args.input}')
            data = args.input.read_bytes()
            meta = info(data)
            print(json.dumps(meta, indent=2))
            return 0
        elif args.command == 'replace-record':
            require(args.original.resolve() != args.output.resolve(), 'output must differ from original')
            new_img = replace_record(args.original.read_bytes(), args.magic, args.name,
                                     args.payload.read_bytes(), args.board_mask)
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_bytes(new_img)
            print(json.dumps({'output': str(args.output), 'record_hashes_verified': True,
                              'signature_verified': False, 'warning': 'not suitable for flashing'}))
            return 0
        elif args.command == 'replace-splash':
            require(args.original.is_file(), f'original file not found: {args.original}')
            require(args.splash.is_file(), f'splash file not found: {args.splash}')
            orig_data = args.original.read_bytes()
            splash_data = args.splash.read_bytes()
            new_img = replace_splash(orig_data, splash_data)
            args.output.parent.mkdir(parents=True, exist_ok=True)
            args.output.write_bytes(new_img)
            print(json.dumps({'output': str(args.output), 'verified': True}))
            return 0
    except (SafetyError, OSError) as exc:
        parser.exit(2, f'refused: {exc}\n')


if __name__ == '__main__':
    sys.exit(main())
