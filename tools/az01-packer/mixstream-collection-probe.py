#!/usr/bin/env python3
"""Sandbox only: distinguish live native SweepFxCollection from NhSweepFxCollection.

Resolve vtables from ELF RTTI rather than hard-coded heap or vtable addresses.
Run in the sandbox's PID namespace while its Engine is alive.
"""
import os
from pathlib import Path
import struct
import sys
import time


def word(data, offset):
    return struct.unpack_from('<I', data, offset)[0]


def vtables(data, name):
    phoff = word(data, 28)
    phsize, phnum = struct.unpack_from('<HH', data, 42)
    loads = []
    for i in range(phnum):
        kind, offset, va, _, size = struct.unpack_from('<5I', data, phoff + i * phsize)
        if kind == 1:
            loads.append((offset, offset + size, va - offset))

    def va(offset):
        return next(offset + delta for start, end, delta in loads if start <= offset < end)

    end = data.index(name + b'\0') + len(name)
    start = data.rfind(b'\0', 0, end - len(name)) + 1
    type_name = data[start:end]
    assert type_name.startswith(b'N7Effects') and type_name.endswith(name)
    def refs(value):
        needle, at = struct.pack('<I', value), 0
        while (at := data.find(needle, at)) >= 0:
            if at % 4 == 0:
                yield at
            at += 1

    references = [va(at - 4) for at in refs(va(start))]  # type_info's second word
    result = []
    for info in references:
        for at in refs(info):
            if at >= 4 and word(data, at - 4) == 0:
                result.append(va(at + 4))  # Itanium vtable address point
    assert result, name
    return result


def main():
    root, expected = Path(sys.argv[1]), sys.argv[2]
    data = (root / 'usr/Engine/Engine').read_bytes()
    full = vtables(data, b'17SweepFxCollectionE')
    nh = vtables(data, b'19NhSweepFxCollectionE')
    process = None
    for _ in range(25):
        # The outer /proc still belongs to the build host's PID namespace.
        # Only the chroot's newly mounted procfs describes this sandbox.
        for entry in (root / 'proc').iterdir():
            if entry.name.isdigit():
                try:
                    if (entry / 'comm').read_text().strip() == 'Engine':
                        process = entry
                        break
                except FileNotFoundError:
                    pass
        if process:
            break
        time.sleep(1)
    assert process, 'Engine process not found'
    time.sleep(8)  # mixer construction follows QML/startup registration
    try:
        maps = (process / 'maps').read_text().splitlines()
    except FileNotFoundError:
        raise SystemExit('Engine exited before the native mixer could be inspected') from None
    base = next(int(line.split('-')[0], 16) for line in maps
                if '/usr/Engine/Engine' in line and int(line.split()[2], 16) == 0)
    counts = [0, 0]
    needles = [[struct.pack('<I', base + address) for address in group] for group in (full, nh)]
    fd = os.open(process / 'mem', os.O_RDONLY)
    try:
        for line in maps:
            fields = line.split()
            if fields[1] != 'rw-p' or (len(fields) > 5 and not fields[5].startswith('[')):
                continue
            start, end = (int(v, 16) for v in fields[0].split('-'))
            for offset in range(start, end, 1024 * 1024):
                block = os.pread(fd, min(1024 * 1024, end - offset), offset)
                for i, group in enumerate(needles):
                    counts[i] += sum(block.count(needle) for needle in group)
    finally:
        os.close(fd)
    print(f'X166 COLLECTION|SweepFxCollection={counts[0]}|NhSweepFxCollection={counts[1]}|expected={expected}|', flush=True)
    matches = counts[0] >= 2 and counts[1] == 0 if expected == 'full' else counts[1] >= 2 and counts[0] == 0
    if not matches:
        raise SystemExit('native Sweep FX collection does not match the expected collection')


if __name__ == '__main__':
    main()
