#!/usr/bin/env python3
"""EngineX OS boot splash / recovery splash generator and splash.bin codec.

Pure standard library (zlib PNG I/O), so it runs anywhere python3 does.

The firmware splash partitions hold a raw 800x1280 portrait BGRA8888 framebuffer
(4,096,000 bytes). The 1280x800 landscape picture is rotated 90 degrees
counter-clockwise into it: x_rot = y_src, y_rot = 1279 - x_src.

  splash_tool.py render --preset enginex --stock stock-splash.png --png out.png --bin splash.bin --bin-rot180 splash-rot180.bin
  splash_tool.py render --preset enginex-recovery --stock stock-recoverysplash.png --bin recoverysplash.bin
  splash_tool.py rotate180 --input splash.bin --output splash-rot180.bin
  splash_tool.py encode --input picture.png --output splash.bin [--rotate 180]
  splash_tool.py decode --input splash.bin --output picture.png [--rotate 180]

--stock takes the official picture (PNG, or the raw splash .bin) because the
engine icon, "engine" lettering and version digits are lifted from it rather than redrawn.
"""
import argparse
import array
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


def rotate_bin_180(data):
    """Rotate an 800x1280 portrait BGRA8888 framebuffer 180 degrees (#226).

    PRIME 4 (JC11), PRIME 2 (JC16) and MIXSTREAM PRO (NH08) mount their LCD
    panels physically rotated 180 degrees relative to SC5000 / SC6000 / PRIME GO.
    Reversing the sequence of 32-bit (BGRA) pixels rotates the 2D framebuffer 180°.
    """
    if len(data) != BIN_SIZE:
        raise ValueError(f"splash bin must be {BIN_SIZE} bytes, got {len(data)}")
    arr = array.array("I")
    assert arr.itemsize == 4
    arr.frombytes(data)
    arr.reverse()
    return arr.tobytes()


def _read(path):
    with open(path, "rb") as f:
        return f.read()


def _write(path, data):
    with open(path, "wb") as f:
        f.write(data)


def load_image(path, rotate=0):
    if path.endswith(".bin"):
        data = _read(path)
        if rotate == 180:
            data = rotate_bin_180(data)
        return W, H, decode_bin(data)
    w, h, rgba = read_png(path)
    if rotate == 180:
        arr = array.array("I")
        assert arr.itemsize == 4
        arr.frombytes(rgba)
        arr.reverse()
        return w, h, bytearray(arr.tobytes())
    return w, h, rgba


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
# Straight-stroke digits the stock art lacks, drawn to the stock metrics: 30 px wide,
# rows 727-774, ~6 px strokes, grey 126 (5.3.0.4 shipped "5.3.0.3" for want of a "4").
DIGIT_W, DIGIT_TOP, DIGIT_BOT, DIGIT_GREY = 30, 727, 775, 126
DRAWN_GLYPHS = {
    "4": [[(18, 727), (24, 727), (24, 775), (18, 775)],             # stem
          [(0, 759), (30, 759), (30, 765), (0, 765)],               # bar
          [(18, 727), (24, 727), (6.5, 759), (0, 759)]],            # diagonal
    "1": [[(17, 727), (23, 727), (23, 775), (17, 775)],
          [(17, 727), (21, 731), (7, 741), (5, 736)]],
    "7": [[(0, 727), (30, 727), (30, 733), (0, 733)],
          [(23, 733), (30, 733), (14, 775), (7, 775)]],
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
        # shortcut: only the glyphs the stock "5.3.0" has; other digits need another stock source
        bad = set(version) - set(VERSION_GLYPHS) - set(DRAWN_GLYPHS)
        if bad:
            raise ValueError(f"version glyphs not in stock art: {sorted(bad)}")
        adv = lambda c: VERSION_GLYPHS[c][1] - VERSION_GLYPHS[c][0] if c in VERSION_GLYPHS else DIGIT_W
        width = sum(adv(c) for c in version) + 8 * (len(version) - 1)
        pen = 1258 - width
        for c in version:
            if c in VERSION_GLYPHS:
                a, b = VERSION_GLYPHS[c]
                blit(canvas, stock, (a, 700, b, 800), pen - a)
            else:
                for poly in DRAWN_GLYPHS[c]:
                    polygon(canvas, [(pen + x, y) for x, y in poly], DIGIT_GREY)
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
    r.add_argument("--stock-rotate", type=int, choices=[0, 180], default=0,
                   help="rotate stock before lifting glyphs (default: 0; use 180 if stock is from PRIME 4/2 or Mixstream Pro)")
    r.add_argument("--version", required=True, help="bottom-right version text (splash only; '' hides it)")
    r.add_argument("--png", action="append", default=[], help="PNG preview path (repeatable)")
    r.add_argument("--bin", action="append", default=[], help="raw BGRA framebuffer path (repeatable)")
    r.add_argument("--bin-rot180", action="append", default=[],
                   help="180-deg rotated raw BGRA framebuffer path for inverted panel models (PRIME 4 / PRIME 2 / MIXSTREAM PRO)")
    for name in ("encode", "decode"):
        p = sub.add_parser(name, help=f"{name} PNG {'->' if name == 'encode' else '<-'} splash .bin")
        p.add_argument("--input", required=True)
        p.add_argument("--output", required=True)
        p.add_argument("--rotate", type=int, choices=[0, 180], default=0,
                       help="rotation angle (0 or 180 deg, default: 0)")
    p_rot = sub.add_parser("rotate180", help="rotate 800x1280 raw splash .bin 180 degrees")
    p_rot.add_argument("--input", required=True)
    p_rot.add_argument("--output", required=True)
    args = ap.parse_args(argv)

    if args.cmd == "render":
        stock = load_image(args.stock, rotate=args.stock_rotate)
        if stock[:2] != (W, H):
            ap.error(f"--stock must be {W}x{H}")
        canvas = PRESETS[args.preset](stock, args.version)
        for p in args.png:
            write_png(p, W, H, canvas)
        if args.bin or args.bin_rot180:
            bin_data = encode_bin(W, H, canvas)
            for p in args.bin:
                _write(p, bin_data)
            if args.bin_rot180:
                bin_rot = rotate_bin_180(bin_data)
                for p in args.bin_rot180:
                    _write(p, bin_rot)
        if not args.png and not args.bin and not args.bin_rot180:
            ap.error("give --png, --bin and/or --bin-rot180")
    elif args.cmd == "encode":
        raw = encode_bin(*read_png(args.input))
        if args.rotate == 180:
            raw = rotate_bin_180(raw)
        _write(args.output, raw)
    elif args.cmd == "decode":
        data = _read(args.input)
        if args.rotate == 180:
            data = rotate_bin_180(data)
        write_png(args.output, W, H, decode_bin(data))
    elif args.cmd == "rotate180":
        _write(args.output, rotate_bin_180(_read(args.input)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
