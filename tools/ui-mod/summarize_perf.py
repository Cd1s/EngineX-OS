#!/usr/bin/env python3
"""Summarize perf_spy's IP samples using matching /proc/PID/maps and ELF DSOs.

Samples count 1 ms of on-CPU time. They do not measure frames or off-CPU waits.
Usage: summarize_perf.py MAPS SAMPLES --libs DIRECTORY
"""
import argparse
import bisect
from collections import Counter
import json
from pathlib import Path
import subprocess


def symbols(path):
    result = subprocess.run(["readelf", "-Ws", "--wide", str(path)],
                            check=True, capture_output=True, text=True)
    found = {}
    for line in result.stdout.splitlines():
        fields = line.split()
        if len(fields) >= 8 and fields[3] == "FUNC" and fields[6] != "UND":
            start, size = int(fields[1], 16) & ~1, int(fields[2])
            if size:
                found[start] = (start + size, fields[7].split("@")[0])
    return sorted(found), found


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("maps", type=Path)
    parser.add_argument("samples", type=Path)
    parser.add_argument("--libs", type=Path, required=True)
    args = parser.parse_args()
    mappings, tables = [], {}
    for line in args.maps.read_text().splitlines():
        fields = line.split(maxsplit=5)
        if len(fields) < 6 or "x" not in fields[1]:
            continue
        start, end = (int(v, 16) for v in fields[0].split("-"))
        name = Path(fields[5]).name
        mappings.append((start, end, int(fields[2], 16), name))
        lib = args.libs / name
        if name not in tables and lib.is_file():
            tables[name] = symbols(lib)
    objects, functions, gc_bins = Counter(), Counter(), Counter()
    total = 0
    for line in args.samples.read_text().splitlines():
        fields = line.split()
        if len(fields) != 2 or not fields[1].startswith("0x"):
            continue
        stamp, ip = int(fields[0]), int(fields[1], 16) & ~1
        total += 1
        for start, end, offset, name in mappings:
            if not start <= ip < end:
                continue
            objects[name] += 1
            # These ARM Qt DSOs have PT_LOAD p_vaddr == p_offset.
            address = ip - start + offset
            if name in tables:
                starts, table = tables[name]
                i = bisect.bisect_right(starts, address) - 1
                if i >= 0 and address < table[starts[i]][0]:
                    symbol = table[starts[i]][1]
                    functions[symbol] += 1
                    if any(key in symbol for key in ("MemoryManager", "MarkStack", "markOnce", "markObjects", "GCStateMachine")):
                        gc_bins[stamp // 100] += 1
            break
        else:
            objects["unmapped"] += 1
    top = functions.most_common(15)
    names = subprocess.run(["c++filt", *[n for n, _ in top]],
                           capture_output=True, text=True, check=True).stdout.splitlines() if top else []
    print(json.dumps({"file": str(args.samples), "samples_1ms": total,
                      "objects": objects.most_common(10),
                      "resolved_functions": [[name, count] for name, (_, count) in zip(names, top)],
                      "gc_core_samples": sum(gc_bins.values()),
                      "gc_100ms_bins": sorted(gc_bins.items())}, indent=2))


if __name__ == "__main__":
    main()
