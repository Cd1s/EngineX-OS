#!/usr/bin/env python3
"""Read-only bounded FIT/FDT inspector for the official SC5000 4.3.4 sample.

No FIT rewrite or signing support. Inline SHA-1 data only; no external offsets.
"""
import argparse
import hashlib
import json
import lzma
import os
from pathlib import Path
import struct
import tempfile

from az01_packer import MODELS, ExtFS, SafetyError, inflate, new_output, require

KNOWN_COMPATIBLE = {c.encode() for c in MODELS}


def inspect(data):
    require(len(data) >= 40, 'truncated FDT')
    magic, total, offset, strings_at, reserve, version, compat, cpu, strings_size, tree_size = (
        struct.unpack_from('>10I', data))
    require(magic == 0xd00dfeed and total == len(data) and version == 17,
            'unsupported/truncated FDT header')
    require(40 <= offset <= total-tree_size and 40 <= strings_at <= total-strings_size,
            'FDT block outside file')
    strings = data[strings_at:strings_at+strings_size]
    end = offset + tree_size
    stack, nodes = [], {}

    def decode(value, encoding='utf-8'):
        try:
            return value.decode(encoding)
        except UnicodeDecodeError as exc:
            raise SafetyError('invalid FDT text encoding') from exc

    def prop(path, name):
        require(path in nodes, 'missing FDT node: '+path)
        require(name in nodes[path], 'missing FDT property: '+path+'/'+name)
        return nodes[path][name][1]

    def word():
        nonlocal offset
        require(offset+4 <= end, 'truncated FDT token')
        result = struct.unpack_from('>I', data, offset)[0]
        offset += 4
        return result

    while True:
        token = word()
        if token == 1:
            stop = data.find(b'\0', offset, end)
            require(stop >= offset, 'unterminated node')
            name = decode(data[offset:stop], 'ascii')
            require('/' not in name and name not in ('.', '..'), 'invalid node name')
            require((not nodes and name == '') if not stack else name != '',
                    'invalid FDT root/child node')
            stack.append(name)
            offset = (stop+4) & ~3
            path = '/'.join(stack)
            require(path not in nodes, 'duplicate node')
            nodes[path] = {}
        elif token == 2:
            require(stack, 'unbalanced FDT')
            stack.pop()
        elif token == 3:
            size, key = word(), word()
            require(offset+size <= end and key < len(strings), 'property outside FDT')
            stop = strings.find(b'\0', key)
            require(stop >= key and stack, 'invalid property name')
            name = decode(strings[key:stop], 'ascii')
            require(name, 'empty FDT property name')
            path = '/'.join(stack)
            require(name not in nodes[path], 'duplicate property')
            nodes[path][name] = (offset, data[offset:offset+size])
            offset = (offset+size+3) & ~3
        elif token == 9:
            require(not stack, 'unclosed FDT node')
            break
        else:
            require(token == 4, 'unknown FDT token')
    compatible = prop('', 'compatible').split(b'\0')
    require(any(c in KNOWN_COMPATIBLE for c in compatible), 'not an official supported-model FIT layout')
    parts = []
    for path, props in nodes.items():
        if 'data' not in props:
            continue
        hashes = [p for p in nodes if p.startswith(path+'/')]
        require(hashes and all(prop(p, 'algo') == b'sha1\0' for p in hashes), 'unknown FIT hash')
        pos, payload = props['data']
        digest = hashlib.sha1(payload).digest()
        require(all(prop(p, 'value') == digest for p in hashes), 'FIT hash mismatch')
        parts.append(dict(name=decode(prop(path, 'partition').rstrip(b'\0')),
                          compression=decode(prop(path, 'compression').rstrip(b'\0')),
                          offset=pos, size=len(payload), sha1=digest.hex()))
    require([p['name'] for p in parts] == ['splash', 'recoverysplash', 'rootfs'],
            'unknown/protected FIT partitions')
    require(not any('signature' in p for p in nodes), 'signed FIT not supported')
    return dict(magic='FIT/FDT', fdt_version=version, size=total,
                description=decode(prop('', 'description').rstrip(b'\0')),
                version=decode(prop('', 'inmusic,version').rstrip(b'\0')),
                sha256=hashlib.sha256(data).hexdigest(), partitions=parts, verified=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('image', type=Path)
    parser.add_argument('--extract-rootfs', help='new ext2 file outside git')
    args = parser.parse_args()
    try:
        data = args.image.read_bytes()
        metadata = inspect(data)
        if args.extract_rootfs:
            output = new_output(args.extract_rootfs)
            root = next(p for p in metadata['partitions'] if p['name'] == 'rootfs')
            require(root['compression'] == 'xz', 'unsupported rootfs compression')
            with tempfile.TemporaryDirectory(prefix='fit-read-', dir=output.parent) as tmp:
                temp = Path(tmp)/'rootfs.ext2'
                with temp.open('wb') as f:
                    inflate(data[root['offset']:root['offset']+root['size']], f)
                ExtFS(temp)
                os.link(temp, output)
        print(json.dumps(metadata, indent=2))
    except (SafetyError, ValueError, OSError, lzma.LZMAError) as exc:
        parser.exit(2, 'refused: '+str(exc)+'\n')


if __name__ == '__main__':
    main()
