#!/usr/bin/env python3
"""EngineX OS boot splash / recovery splash generator and splash.bin codec.

Pure standard library (zlib PNG I/O), so it runs anywhere python3 does.

The firmware splash partitions hold a raw 800x1280 portrait BGRA8888 framebuffer
(4,096,000 bytes). The 1280x800 landscape picture is rotated 90 degrees into it
(90 deg CCW on SC5000/SC6000 panels, or 90 deg CW — 180 degrees opposite — on
panels mounted the other way round such as PRIME 4 / PRIME 2 / Mixstream Pro).

  splash_tool.py render --preset enginex --stock stock-splash.bin --png out.png --bin splash.bin
  splash_tool.py render --preset enginex-recovery --stock stock-recoverysplash.bin --bin recoverysplash.bin
  splash_tool.py encode --input picture.png [--stock stock-splash.bin] --output splash.bin
  splash_tool.py decode --input splash.bin [--stock stock-splash.bin] --output picture.png

--stock takes the official picture (PNG, or the raw splash .bin/.payload) because
the engine icon, "engine" lettering, version digits and panel framebuffer
orientation are derived directly from it rather than redrawn or listed by model.
"""
import argparse
import base64
import math
import os
import struct
import sys
import zlib

W, H = 1280, 800
BIN_SIZE = W * H * 4  # 0x3e8000


# ---------------------------------------------------------------- PNG / bin I/O

def read_png(path):
    """Return (w, h, RGBA bytearray). 8-bit, non-interlaced PNGs only."""
    data = _read(path)
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError(f"{path}: not a PNG")
    pos, idat, hdr = 8, [], None
    while pos < len(data):
        n, kind = struct.unpack(">I4s", data[pos:pos + 8])
        chunk = data[pos + 8:pos + 8 + n]
        pos += 12 + n
        if kind == b"IHDR":
            hdr = struct.unpack(">IIBBBBB", chunk)
        elif kind == b"IDAT":
            idat.append(chunk)
        elif kind == b"IEND":
            break
    w, h, depth, ctype, _, _, interlace = hdr
    bpp = {0: 1, 2: 3, 4: 2, 6: 4}.get(ctype)
    if depth != 8 or bpp is None or interlace:
        raise ValueError(f"{path}: need 8-bit gray/RGB/RGBA non-interlaced PNG (depth={depth} type={ctype})")
    raw = zlib.decompress(b"".join(idat))
    stride = w * bpp
    prev = bytearray(stride)
    rows = []
    for y in range(h):
        o = y * (stride + 1)
        f, line = raw[o], bytearray(raw[o + 1:o + 1 + stride])
        if f == 1:
            for i in range(bpp, stride):
                line[i] = (line[i] + line[i - bpp]) & 255
        elif f == 2:
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 255
        elif f == 3:
            for i in range(stride):
                line[i] = (line[i] + ((line[i - bpp] if i >= bpp else 0) + prev[i]) // 2) & 255
        elif f == 4:
            for i in range(stride):
                a = line[i - bpp] if i >= bpp else 0
                b, c = prev[i], (prev[i - bpp] if i >= bpp else 0)
                pa, pb, pc = abs(b - c), abs(a - c), abs(a + b - 2 * c)
                line[i] = (line[i] + (a if pa <= pb and pa <= pc else b if pb <= pc else c)) & 255
        elif f != 0:
            raise ValueError(f"{path}: bad filter {f}")
        rows.append(line)
        prev = line
    out = bytearray(w * h * 4)
    for c in range(3):  # gray fans out to RGB
        src = c if ctype in (2, 6) else 0
        out[c::4] = b"".join(bytes(r[src::bpp]) for r in rows)
    out[3::4] = b"".join(bytes(r[bpp - 1::bpp]) for r in rows) if ctype in (4, 6) else b"\xff" * (w * h)
    return w, h, out


def write_png(path, w, h, rgba):
    """Write RGB when fully opaque, else RGBA."""
    opaque = rgba[3::4] == b"\xff" * (w * h)
    bpp = 3 if opaque else 4
    if opaque:
        px = bytearray(w * h * 3)
        for c in range(3):
            px[c::3] = rgba[c::4]
    else:
        px = rgba
    stride = w * bpp
    raw = b"".join(b"\x00" + bytes(px[y * stride:(y + 1) * stride]) for y in range(h))

    def chunk(kind, body):
        return struct.pack(">I", len(body)) + kind + body + struct.pack(">I", zlib.crc32(kind + body))

    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    with open(path, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 6 - 4 * opaque, 0, 0, 0))
                + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


def encode_bin(w, h, rgba):
    """1280x800 RGBA -> 800x1280 BGRA framebuffer (rotated 90 deg CCW)."""
    if (w, h) != (W, H):
        raise ValueError(f"splash must be {W}x{H}, got {w}x{h}")
    out = bytearray(BIN_SIZE)
    step = H * 4
    for y in range(H):
        row = rgba[y * W * 4:(y + 1) * W * 4]
        # Source row y becomes framebuffer column y; x runs bottom-to-top, so
        # walking the column top-down (step = one portrait row) reads x reversed.
        for dst, src in ((0, 2), (1, 1), (2, 0), (3, 3)):
            out[y * 4 + dst::step] = row[src::4][::-1]
    return out


def decode_bin(data):
    if len(data) != BIN_SIZE:
        raise ValueError(f"splash bin must be {BIN_SIZE} bytes, got {len(data)}")
    rgba = bytearray(BIN_SIZE)
    step = H * 4
    for y in range(H):
        o = y * W * 4
        for dst, src in ((0, 2), (1, 1), (2, 0), (3, 3)):
            rgba[o + dst:o + W * 4:4] = data[y * 4 + src::step][::-1]
    return rgba


def _read(path):
    with open(path, "rb") as f:
        return f.read()


def _write(path, data):
    with open(path, "wb") as f:
        f.write(data)


def rotate_180(buf):
    """Rotate a 4-byte-per-pixel buffer (1280x800 RGBA or 800x1280 BGRA) by 180 degrees."""
    out = bytearray(len(buf))
    for c in range(4):
        out[c::4] = buf[c::4][::-1]
    return out


def is_rotated_180(rgba):
    """Return True when a decode_bin() 1280x800 RGBA stock frame is upside down (90 deg CW panel).

    Stock boot splashes carry the version string in the bottom-right corner while the
    opposite top-left corner is flat background; stock recovery splashes place the large
    update icon in the upper half above the single-line caption in the lower half.
    """
    red = rgba[0::4]
    bg = sorted((red[0], red[W - 1], red[(H - 1) * W], red[W * H - 1]))[1]
    br = sum(abs(red[y * W + x] - bg) for y in range(700, 790) for x in range(1080, 1270))
    tl = sum(abs(red[y * W + x] - bg) for y in range(10, 100) for x in range(10, 200))
    if br != tl:
        return tl > br
    upper = sum(abs(red[i] - bg) > 20 for i in range(0, W * (H // 2)))
    lower = sum(abs(red[i] - bg) > 20 for i in range(W * (H // 2), W * H))
    return lower > upper


def load_image(path):
    data = _read(path)
    if data[:8] != b"\x89PNG\r\n\x1a\n" and (path.endswith((".bin", ".payload")) or len(data) == BIN_SIZE):
        return W, H, decode_bin(data)
    return read_png(path)


def load_stock(path):
    """Load --stock as upright (w, h, rgba) plus whether the stock frame is rotated 180 degrees."""
    w, h, rgba = load_image(path)
    rot180 = (w, h) == (W, H) and is_rotated_180(rgba)
    if rot180:
        rgba = rotate_180(rgba)
    return w, h, rgba, rot180


# ---------------------------------------------------------------- drawing

def polygon(canvas, pts, grey):
    """Fill a convex polygon flat (half-plane distances: sharp corners, 1 px AA edges)."""
    area = sum(pts[i - 1][0] * p[1] - p[0] * pts[i - 1][1] for i, p in enumerate(pts))
    sgn = 1.0 if area > 0 else -1.0
    edges = []
    for i, (bx, by) in enumerate(pts):
        ax, ay = pts[i - 1]
        ln = math.hypot(bx - ax, by - ay)
        edges.append((ax, ay, -(by - ay) / ln * sgn, (bx - ax) / ln * sgn))
    for y in range(int(min(p[1] for p in pts)) - 1, int(max(p[1] for p in pts)) + 2):
        for x in range(int(min(p[0] for p in pts)) - 1, int(max(p[0] for p in pts)) + 2):
            a = min(nx * (x + 0.5 - ax) + ny * (y + 0.5 - ay) for ax, ay, nx, ny in edges) + 0.5
            if a <= 0:
                continue
            a = min(a, 1.0)
            o = (y * W + x) * 4
            for c in range(3):
                canvas[o + c] = int(canvas[o + c] + (grey - canvas[o + c]) * a + 0.5)


def blit(canvas, img, box, dx, dy=0):
    """Copy a stock rectangle unchanged (same background, so no seams)."""
    sw, _, px = img
    x0, y0, x1, y1 = box
    for y in range(y0, y1):
        s, d = (y * sw + x0) * 4, ((y + dy) * W + x0 + dx) * 4
        canvas[d:d + (x1 - x0) * 4] = px[s:s + (x1 - x0) * 4]


# stock 5.3.0 splash, bottom-right "5.3.0": glyph column spans, rows 700-800, 8 px gaps
VERSION_GLYPHS = {"5": (1122, 1151), ".": (1158, 1166), "3": (1174, 1204), "0": (1228, 1258)}
# Missing digits are rasterized from the official Engine 5.3.0 QRC font
# Helvetica-LT-Condensed-Medium at 62 px. The cells use the stock 30x48 metrics,
# baseline and grey level, so no hand-drawn digit geometry is introduced here.
DIGIT_W, DIGIT_H, DIGIT_GREY = 30, 48, 126
FONT_GLYPHS = {
    '1': zlib.decompress(base64.b64decode('eNpjYEAHRf///1/BgAtMxSt7Ap+s8G88skzZ/3HKMjLzvcQqy8jIxMzCwLj+PxZZJlZmRiClu+vff6x6maUts9a9+P8fm2zEzTsPX3z6/x+7bC5E9B922ZRvQPAdBH5jkS3+jwLQZCXtYGDOf3whWYRFlpEZBurw6q0ZlR2VHZUdKrIAXWAlHQ==')),
    '2': zlib.decompress(base64.b64decode('eNp91D1IHEEUAOA39xN/0PNOTRB/kDM2guA1nigJgaAQTJF0KUQkWKmFCIeNhSGFvaggoqCCck26cEggiYgEwxVJtDjDVVEE0fiDoPizu+O8N+c53L7kNftmvnmzO8vMAGQi79XE+v7F1d7PxTclkBOF7w+vLduR0nHsm+NhvzCxKW1JJbYKNUJavyruWTSckEkd+PxdnWVfknqPRqKVlZH+78Sfs6V9aE4q7MeWNzBMY/vuilPYOg7fjfaPY/tPRqM0dig7FYSOsOOlbg5hfl5kLGEBe2Z1voz5R3OBXdiT0vk3zN+ZGsEeq5jybcy7Ta2iL3lM+R6mr00NkjZTvnOpotPUMtLnlNvG9+uoJX1C+dsBFVWmttDPiwAbnl6qLecV4oh/9a8UGIb5gqc48Qpb6IUxmniQfSm0neEOuqxhsXaTSuMsPlqRqtS6aBAMln+QagNacpSrLIvLG8REgceNpUsaN+uYytJFjelWEC4MzWewQy06F4NzGrfbwZeDAoIz8hoPwtYzBkumNf6IcjilMdnoQnVCJxAduVHv+iB1OMYQpVyvdqOAmMavD8Hj/oOduBQpPwU4LNwlTBQIBkFvsi8PPILbDmt4V6QDXhYr8EaxX/CV0IOlSeARJvGtMa/IDa0bqK3/2PhwgNoUcgXdFfmO5GMVNawvOMsViewlwQUdpKf/0Vt8s9R2')),
    '4': zlib.decompress(base64.b64decode('eNrllL9KA0EQxmc2kYCIRQrRQgsrBauAloJEUBuJkuKCHIgvoD6BtoIWPoEg+ACKlY2kSpEg2vmnEMFGiyAhwaC3O85GE+WYuRfwq2bmd9/eze7sAcSVc0R0DYouSae4Qgm07z6B4jbpFLP1BAqHRFajZqJNTveeMSgr1OQt2bsNmSJecb1YVOg6UVRJl0RqBp75k2YhkL273M05oEhTow3noilQvMdcPAKZmplPZ5tjMkXwm7AHirdALnrJokhN5pZLm6B4t7ibh34jUjP0yucWgOLlY3W1FIoUJ9+to3kQvQinnF8AyivPEe9hDkUvpqo8LSe9Z2PekHgPx41IcfApIjoAxbvDydtIJt3Vmqc3PmI43LAUhX96X/K09pMscvxRWi30tO/pIwfLTPPf4y3J04jVav6q7YHloN59jeqdrsTUucAtDsrSXVrwtNobi5jCTr8+krwBJf1z/jP9AshMwQI=')),
    '6': zlib.decompress(base64.b64decode('eNpjYIABifzNdz//fnNrXaEcAzrg6P35HwZ+LxRFldS9+h8ZPDZGllR99R8VvJBFSArfA4m8L1dj41FLPAeW3ouQnQ3iP1KBcJhngaXtYZJm/4C8b+owLus1kOwCGHcjiFeMMCobxH8C5cj8AXJecyBktcFGi0M4VSB2J5IPuMGyBhDOERDbEtmD+QVAAPET/2+Qb5gYsANPkNYDOCQZKkCyk3HJLgfJpuGSPQuStcIl+xIkq4BDkg0UjP95cMhKgiR/gFgWE85+/PPx6tIopGDTgAaq9kF45L6Mhsuag/g3GLy/Ikf+JphNjiDeeaNvqGljH9R0dxDn1KX//38tdZZjl7Ke8gksvQQi6wtV/doWapjSeTDfBcwJgEj+sYG7RPQxSOAymB0MkUUOaH+wiDmIGQSRVUKSZbwOEpmKsPc6Sgh1gISuwt2MSIFg4AES+ssLZDmAZRtRZJXgFhuDWTkosqxgMS+4uiTUqAHnR1BoC4BlY1Flv4DEokDOByXJ/zEokixgHZ4g5i0QKxVFVhQsawHPRRUosqqIAAJ7fRqKrD1I6A2YGQZiHkORLQAJbQEzeX+AokgYWXY9SLYcwt4KYicgSQqCvPtHBsJJAMleQ8pmzeCkBSuqnoF4JYhU+h2l3EgF8f6lQ3mK4KQxH2HUdrD/DiUqc3JqV38AsZ8KIWT5L6MXZhrIXhA+jCJ5Sxs1UliKP8DlfnVxYOQ2zoi55179+PVsd6kkQhAAGBcUgA==')),
    '7': zlib.decompress(base64.b64decode('eNp90zEoRWEUB/Dvu9e9oZAXvQWDJJMRpRBXBixEXilRMpjkDUaLMhi8SXplYjBKeRPLY1FM3JEyyPCUQnrce49zvtH7/r7p9n6d/znf+XoqIHQCpdT/Oly2nB/REdZxVBqNsTYOVJ6hdUbqUuCcUkJ7Hn9o7+/x1RZR/JgGlb2fcUwzStvMqbni3CPl2lCrLMVJqcWx57a+RUQb9lytDviuD7WgtIeb0ry2Y9VlTHSjgc5ybhIAdO9Zr8EedEb2PwXUCTk3dAGa0iUULF2fqkHpqAy8jZ61INpuv47uKPO7FlHpjsyUAZuof+ENlxrAwNPS9RAFn/H+aRIEp74Y3+tA6TJ3pRMUfCETL4DgtHSNmkHpopTeouBj0V2A7qvcFr17n/lXNgHdFAxR26JoHqD/IboKtNu0HQS6ImukFHi9vOAzGupOtACW7H+L5kBppxkqC3TC6BzQNaP9QPeNtgE9F4y9it9/AeI7RrM=')),
    '8': zlib.decompress(base64.b64decode('eNpllFtIVFEYhU+KTRdDzQaqkQYVKwg0FIZGzJCRCCQQCUEMmgcvTyW+iOEFU/AlM8yal4gUH0QNGkvLBBERezBUsAuCIpIMXhAsNNO5OM36Z88+/z79T+tb68w5e5+95miamGPFr2a3Dv7+nGrN1owTVb4RlDNhU8PEqSCfgwoenl0IGqZUD498JGe48PzRMzd6SP+5KNO7ZNQIchK9lukMcFRiF9BrFnSFLr4p08vEdwRVAnZi9HUswngq4AXgM9vCAIx3AvoB/SxtgzEpYEhdo6Y1wfgqoA/Qw9IWGNMCngPcLH0G44OAB4DvLH0P47GATID/tJ6uwbgdoWVQmQxtwG1TBMuBSxLfAhvkxdGj4F7xtqoAP07qD4ofgzPvvGBKzKMXtXqJH3909Ro/+yGLoVimgplIbdptxtYlu3blL3+789Wwek+t1Scz65ULjq/bYTFZHN0+wEqKTB+BPZGS2z30Zk8JzAmEaC9TXnyVltAmaBLQzFbRSEs/F/4p6XiWxtIaq0g/gRxRtjCoV3gesl5Ja2FtkvTy9oankDYdG1JxpHKVNJe8pJCykspS0izycBRmUnYlzZF3jjH8izC3qGkkd9VWYSpgLZKc5v0MTwesNyQ7Ib8o6RysOpIlkAErC1NpKemkT2xDd7L0JbWSP8XvkOF1v34Impb0i8pfIPDaFnBBfgvu0XMO3UWpxxPsLp/hVlq78WN26ORbeBhQwh2nWtn0YRaOWP/7yqbdH/y27t33jLdk6OY/+fgtmw==')),
    '9': zlib.decompress(base64.b64decode('eNpjYIACgazNj378eHl+XjQXAzpgKvj8HwY+lrOiSkoc/o8MToghSwpd+o8KboogJFlOgER+TTLjYVNMvQWWPsQEly0F8b/YQzgcm8HSxTBJ2S8grieMy7YfxH3HB+X2gHjLERap/AAJFEDVvgJxDJAcOQkkcATCDgaxLyF7wRAk8lcCzJ4KYk9A8f59kFAqmHkaxIxEkV0JEpoOYrH/AjHNUGSrQEIHQCxlsPckUWTjQEIvQSxjsCw3iqwrWIwXyHIGs5hQZK3AYvJAVgCYxY4iCzHPEG6KEIqsLVjMCcgyBbO0UGR9wWL+oDgAs9xRZAvBYmEg5hsQqxJFdi5YNgbE3AVibUGRfQiWTQQxi8AJQxRJ0hqSfJLAKe4PiNmCJLsNIhsP5qwDa0ZEcDo06cUgpYVXRlDJjF//XxyAu5mBoQ6s9Hu3CTeHcjhIIhMsGwCRZZyHmpp3Mp4EUW5Qw5inIUse52W4CqLN4S5xfwCT+9ULjJGXIJYGUn4IWnD5458PJxpB8cb4GyQrzIAdCIIkv+GQZNAG5zRcshGYQY8E2kCybbhkwdkwHJfsE5CsFJgpVgICbEiSOiDJKxC2IkZyrwYJdEMzO1jWCilgHoEENKG86yBOFUI2FcQ/iJKZ73PCuDLvkSOIwQhs9BIWCE/6Joi3FmHURrD05TR1HlbFfHA58VwGqaR7glaYfUQuRRj0H6BI3kXN6wxCc3/C5f7M5MEIPOn8Nbfe/vn6YEe5HJIoAOCsGnQ=')),
}


def render_splash(stock, version):
    """Stock layout and pixels: headphone icon + "engine", then a flat white x in
    place of "dj", centred; version drawn with the stock version digits."""
    canvas = bytearray([0x0a, 0x0a, 0x0a, 255] * (W * H))
    # stock geometry: icon 173-287, "engine" 354-914, x-height 366-426, stems 18.5 px,
    # word space 41 px before "dj" at 955.
    xw, xl = 96, 955                      # x as wide as the stock "n" (459-554)
    dx = (W - (xl + xw - 173)) // 2 - 173
    blit(canvas, stock, (160, 320, 300, 476), dx)   # headphone icon
    blit(canvas, stock, (340, 336, 935, 460), dx)   # "engine"
    x0, top, bot, t = xl + dx, 366, 427, 18.0
    # horizontal stroke width that gives a perpendicular thickness of t
    th = t
    for _ in range(20):
        th = t * math.hypot(1, (xw - th) / (bot - top))
    polygon(canvas, [(x0, top), (x0 + th, top), (x0 + xw, bot), (x0 + xw - th, bot)], 255)
    polygon(canvas, [(x0 + xw - th, top), (x0 + xw, top), (x0 + th, bot), (x0, bot)], 255)

    if version:
        bad = set(version) - set(VERSION_GLYPHS) - set(FONT_GLYPHS)
        if bad:
            raise ValueError(f"version glyphs not in stock art or the embedded stock font: {sorted(bad)}")
        adv = lambda c: VERSION_GLYPHS[c][1] - VERSION_GLYPHS[c][0] if c in VERSION_GLYPHS else DIGIT_W
        width = sum(adv(c) for c in version) + 8 * (len(version) - 1)
        pen = 1258 - width
        for c in version:
            if c in VERSION_GLYPHS:
                a, b = VERSION_GLYPHS[c]
                blit(canvas, stock, (a, 700, b, 800), pen - a)
            else:
                mask = FONT_GLYPHS[c]
                for y in range(DIGIT_H):
                    for x in range(DIGIT_W):
                        alpha = mask[y * DIGIT_W + x]
                        if not alpha:
                            continue
                        o = ((y + 727) * W + pen + x) * 4
                        blend = alpha / 255.0
                        for channel in range(3):
                            canvas[o + channel] = int(canvas[o + channel] + (DIGIT_GREY - canvas[o + channel]) * blend + 0.5)
            pen += adv(c) + 8
    return canvas


def render_recovery(stock):
    """The stock recovery screen as is: flat white icon, "Computer Update" and hint on black."""
    return bytearray(stock[2])


PRESETS = {
    "enginex": lambda stock, version: render_splash(stock, version),
    "enginex-recovery": lambda stock, version: render_recovery(stock),
}


# ---------------------------------------------------------------- CLI

def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("render", help="generate a preset splash")
    r.add_argument("--preset", choices=sorted(PRESETS), required=True)
    r.add_argument("--stock", required=True, help="official splash of the same kind (PNG or raw .bin)")
    r.add_argument("--version", required=True, help="bottom-right version text (splash only; '' hides it)")
    r.add_argument("--png", action="append", default=[], help="PNG preview path (repeatable)")
    r.add_argument("--bin", action="append", default=[], help="raw BGRA framebuffer path (repeatable)")
    for name in ("encode", "decode"):
        p = sub.add_parser(name, help=f"{name} PNG {'->' if name == 'encode' else '<-'} splash .bin")
        p.add_argument("--input", required=True)
        p.add_argument("--output", required=True)
        p.add_argument("--stock", help="official splash (.bin/.payload) to match panel orientation")
    args = ap.parse_args(argv)

    if args.cmd == "render":
        w, h, px, rot180 = load_stock(args.stock)
        if (w, h) != (W, H):
            ap.error(f"--stock must be {W}x{H}")
        canvas = PRESETS[args.preset]((w, h, px), args.version)
        for p in args.png:
            write_png(p, W, H, canvas)
        if args.bin:
            raw = encode_bin(W, H, canvas)
            if rot180:
                raw = rotate_180(raw)
            for p in args.bin:
                _write(p, raw)
        if not args.png and not args.bin:
            ap.error("give --png and/or --bin")
    elif args.cmd == "encode":
        raw = encode_bin(*read_png(args.input))
        if args.stock and load_stock(args.stock)[3]:
            raw = rotate_180(raw)
        _write(args.output, raw)
    else:
        rgba = decode_bin(_read(args.input))
        if args.stock and load_stock(args.stock)[3]:
            rgba = rotate_180(rgba)
        write_png(args.output, W, H, rgba)
    return 0


if __name__ == "__main__":
    sys.exit(main())
