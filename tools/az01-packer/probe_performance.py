#!/usr/bin/env python3
"""Read-only research decoder for the shipped DemoDevice Database2/m.db.

Observed fields are named conservatively. Unknown fields/tails remain explicit;
this is not a production encoder or a guarantee for other library revisions.
"""
import argparse
import hashlib
import json
from pathlib import Path
import sqlite3
import struct
from urllib.parse import quote
import zlib


def uncompress(blob):
    if len(blob) < 4:
        raise ValueError('truncated qCompress header')
    size = struct.unpack_from('>I', blob)[0]
    if size > 16*1024**2:
        raise ValueError('oversized performance blob')
    dec = zlib.decompressobj()
    data = dec.decompress(blob[4:], size+1)
    if len(data) != size or not dec.eof or dec.unused_data or dec.unconsumed_tail:
        raise ValueError('invalid qCompress stream/length/trailer')
    return data


class Cursor:
    def __init__(self, data):
        self.data, self.pos = data, 0

    def take(self, size):
        if not 0 <= size <= len(self.data)-self.pos:
            raise ValueError('truncated performance structure')
        data = self.data[self.pos:self.pos+size]
        self.pos += size
        return data

    def unpack(self, fmt):
        return struct.unpack(fmt, self.take(struct.calcsize(fmt)))

    def label(self):
        n, = self.unpack('B')
        raw = self.take(n)
        return dict(text=raw.decode('utf-8', errors='replace'), bytes_hex=raw.hex())

    def tail(self):
        return self.data[self.pos:].hex()


def decode(column, blob):
    raw = blob if column == 'loops' else uncompress(blob)
    result = dict(blob_length=len(blob), raw_length=len(raw),
                  blob_sha256=hashlib.sha256(blob).hexdigest())
    c = Cursor(raw)
    if column == 'trackData':
        sr, length, candidate, a, b, d = c.unpack('>dQI3d')
        result.update(sample_rate=sr, sample_length=length,
                      offset16_u32_candidate_analyzed_key=candidate,
                      unknown_doubles=[a, b, d])
    elif column == 'overviewWaveFormData':
        n, m, ratio = c.unpack('>QQd')
        result.update(header_u64=[n, m], offset16_double=ratio,
                      data_length=len(raw)-24, triple_count=(len(raw)-24)//3,
                      first_eight_triples=list(raw[24:48]))
        c.take(len(raw)-24)
    elif column == 'beatData':
        sr, length, is_set = c.unpack('>ddB')
        grids = []
        for _ in range(2):
            n, = c.unpack('>Q')
            if n > 10000:
                raise ValueError('unreasonable beat marker count')
            grid = []
            for _ in range(n):
                position, index, until_next, unknown = c.unpack('<dqII')
                grid.append(dict(sample_position=position, beat_index=index,
                                 beats_until_next=until_next, unknown_u32=unknown))
            grids.append(grid)
        result.update(sample_rate=sr, sample_length=length, is_set=is_set,
                      default_grid=grids[0], adjusted_grid=grids[1])
    elif column == 'quickCues':
        n, = c.unpack('>Q')
        if n > 64:
            raise ValueError('unreasonable cue count')
        cues = []
        for _ in range(n):
            label = c.label()
            position, = c.unpack('>d')
            cues.append(dict(label=label, sample_position=position, argb=c.take(4).hex()))
        main, overridden, default = c.unpack('>dBd')
        result.update(cues=cues, main_cue_position=main,
                      main_cue_overridden=overridden, default_cue_position=default)
    elif column == 'loops':
        n, = c.unpack('B')
        padding = c.take(7)
        if n > 64:
            raise ValueError('unreasonable loop count')
        loops = []
        for _ in range(n):
            label = c.label()
            start, end, start_set, end_set = c.unpack('<ddBB')
            loops.append(dict(label=label, start_sample=start, end_sample=end,
                              start_set=start_set, end_set=end_set, argb=c.take(4).hex()))
        result.update(loops=loops, padding_hex=padding.hex())
    result.update(consumed=c.pos, unknown_tail_hex=c.tail())
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('database', type=Path)
    args = parser.parse_args()
    path = args.database.resolve(strict=True)
    con = sqlite3.connect('file:'+quote(str(path))+'?mode=ro&immutable=1', uri=True)
    con.row_factory = sqlite3.Row
    results = []
    for row in con.execute('SELECT * FROM PerformanceData ORDER BY trackId'):
        record = {}
        for column in row.keys():
            value = row[column]
            record[column] = decode(column, value) if isinstance(value, bytes) else value
        results.append(record)
    print(json.dumps(dict(database=str(path), database_sha256=hashlib.sha256(path.read_bytes()).hexdigest(),
                          scope='shipped demo samples, observed layouts only', records=results), indent=2,
                     allow_nan=False))


if __name__ == '__main__':
    main()
