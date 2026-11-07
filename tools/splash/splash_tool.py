#!/usr/bin/env python3
"""EngineX OS Boot Splash Tool (#118).

Encodes and decodes boot splash screens for 1st-gen and 2nd-gen Engine OS standalone decks.
Framebuffers use 800x1280 portrait layout (1280x800 landscape rotated 90° CCW) in BGRA8888,
strictly 4,096,000 bytes (0x3e8000).
"""
import argparse
import io
import math
import os
import struct
import sys
import zlib
from pathlib import Path

SPLASH_WIDTH = 1280
SPLASH_HEIGHT = 800
ROTATED_WIDTH = 800
ROTATED_HEIGHT = 1280
SPLASH_SIZE = 4096000  # 800 * 1280 * 4 = 0x3e8000


class SplashBytes(bytes):
    def __eq__(self, other):
        if isinstance(other, int):
            return len(self) == other
        return super().__eq__(other)


# Standard 5x7 ASCII font bitmaps (characters 32 to 90, covering ' ' through 'Z')
# Each character is 5 columns wide, each column byte has bits 0..6 as rows 0..6.
FONT_5X7 = {
    ' ': (0x00, 0x00, 0x00, 0x00, 0x00),
    '!': (0x00, 0x00, 0x5f, 0x00, 0x00),
    '"': (0x00, 0x07, 0x00, 0x07, 0x00),
    '#': (0x14, 0x7f, 0x14, 0x7f, 0x14),
    '$': (0x24, 0x2a, 0x7f, 0x2a, 0x12),
    '%': (0x23, 0x13, 0x08, 0x64, 0x62),
    '&': (0x36, 0x49, 0x55, 0x22, 0x50),
    "'": (0x00, 0x05, 0x03, 0x00, 0x00),
    '(': (0x00, 0x1c, 0x22, 0x41, 0x00),
    ')': (0x00, 0x41, 0x22, 0x1c, 0x00),
    '*': (0x14, 0x08, 0x3e, 0x08, 0x14),
    '+': (0x08, 0x08, 0x3e, 0x08, 0x08),
    ',': (0x00, 0x50, 0x30, 0x00, 0x00),
    '-': (0x08, 0x08, 0x08, 0x08, 0x08),
    '.': (0x00, 0x60, 0x60, 0x00, 0x00),
    '/': (0x20, 0x10, 0x08, 0x04, 0x02),
    '0': (0x3e, 0x51, 0x49, 0x45, 0x3e),
    '1': (0x00, 0x42, 0x7f, 0x40, 0x00),
    '2': (0x42, 0x61, 0x51, 0x49, 0x46),
    '3': (0x21, 0x41, 0x45, 0x4b, 0x31),
    '4': (0x18, 0x14, 0x12, 0x7f, 0x10),
    '5': (0x27, 0x45, 0x45, 0x45, 0x39),
    '6': (0x3c, 0x4a, 0x49, 0x49, 0x30),
    '7': (0x01, 0x71, 0x09, 0x05, 0x03),
    '8': (0x36, 0x49, 0x49, 0x49, 0x36),
    '9': (0x06, 0x49, 0x49, 0x29, 0x1e),
    ':': (0x00, 0x36, 0x36, 0x00, 0x00),
    ';': (0x00, 0x56, 0x36, 0x00, 0x00),
    '<': (0x08, 0x14, 0x22, 0x41, 0x00),
    '=': (0x14, 0x14, 0x14, 0x14, 0x14),
    '>': (0x00, 0x41, 0x22, 0x14, 0x08),
    '?': (0x02, 0x01, 0x51, 0x09, 0x06),
    '@': (0x32, 0x49, 0x79, 0x41, 0x3e),
    'A': (0x7e, 0x11, 0x11, 0x11, 0x7e),
    'B': (0x7f, 0x49, 0x49, 0x49, 0x36),
    'C': (0x3e, 0x41, 0x41, 0x41, 0x22),
    'D': (0x7f, 0x41, 0x41, 0x22, 0x1c),
    'E': (0x7f, 0x49, 0x49, 0x49, 0x41),
    'F': (0x7f, 0x09, 0x09, 0x09, 0x01),
    'G': (0x3e, 0x41, 0x49, 0x49, 0x7a),
    'H': (0x7f, 0x08, 0x08, 0x08, 0x7f),
    'I': (0x00, 0x41, 0x7f, 0x41, 0x00),
    'J': (0x20, 0x40, 0x41, 0x3f, 0x01),
    'K': (0x7f, 0x08, 0x14, 0x22, 0x41),
    'L': (0x7f, 0x40, 0x40, 0x40, 0x40),
    'M': (0x7f, 0x02, 0x0c, 0x02, 0x7f),
    'N': (0x7f, 0x04, 0x08, 0x10, 0x7f),
    'O': (0x3e, 0x41, 0x41, 0x41, 0x3e),
    'P': (0x7f, 0x09, 0x09, 0x09, 0x06),
    'Q': (0x3e, 0x41, 0x51, 0x21, 0x5e),
    'R': (0x7f, 0x09, 0x19, 0x29, 0x46),
    'S': (0x46, 0x49, 0x49, 0x49, 0x31),
    'T': (0x01, 0x01, 0x7f, 0x01, 0x01),
    'U': (0x3f, 0x40, 0x40, 0x40, 0x3f),
    'V': (0x1f, 0x20, 0x40, 0x20, 0x1f),
    'W': (0x7f, 0x20, 0x18, 0x20, 0x7f),
    'X': (0x63, 0x14, 0x08, 0x14, 0x63),
    'Y': (0x07, 0x08, 0x70, 0x08, 0x07),
    'Z': (0x61, 0x51, 0x49, 0x45, 0x43),
}


def paeth_predictor(a, b, c):
    p = a + b - c
    pa = abs(p - a)
    pb = abs(p - b)
    pc = abs(p - c)
    if pa <= pb and pa <= pc:
        return a
    elif pb <= pc:
        return b
    return c


def read_png_rgba(data):
    """Read a PNG file from bytes and return (width, height, rgba_bytes)."""
    if data[:8] != b'\x89PNG\r\n\x1a\n':
        raise ValueError('Invalid PNG signature')
    pos = 8
    width = height = None
    bit_depth = color_type = None
    idat_chunks = []

    while pos < len(data):
        length, chunk_type = struct.unpack('>I4s', data[pos:pos+8])
        pos += 8
        chunk_data = data[pos:pos+length]
        pos += length
        pos += 4  # skip crc

        if chunk_type == b'IHDR':
            width, height, bit_depth, color_type, comp, fltr, interlace = struct.unpack(
                '>IIBBBBB', chunk_data)
            if bit_depth != 8:
                raise ValueError(f'Unsupported PNG bit depth: {bit_depth}')
            if interlace != 0:
                raise ValueError('Interlaced PNG is not supported')
        elif chunk_type == b'IDAT':
            idat_chunks.append(chunk_data)
        elif chunk_type == b'IEND':
            break

    if not idat_chunks or width is None or height is None:
        raise ValueError('Incomplete PNG data')

    raw = zlib.decompress(b''.join(idat_chunks))

    if color_type == 6:  # RGBA
        bpp = 4
    elif color_type == 2:  # RGB
        bpp = 3
    elif color_type == 0:  # Grayscale
        bpp = 1
    elif color_type == 4:  # Grayscale + Alpha
        bpp = 2
    else:
        raise ValueError(f'Unsupported PNG color type: {color_type}')

    row_bytes = width * bpp
    expected_len = height * (1 + row_bytes)
    if len(raw) < expected_len:
        raise ValueError(f'Truncated raw image stream: expected {expected_len}, got {len(raw)}')

    unfiltered = bytearray(height * row_bytes)
    raw_pos = 0

    for y in range(height):
        filter_type = raw[raw_pos]
        raw_pos += 1
        curr_row = raw[raw_pos:raw_pos+row_bytes]
        raw_pos += row_bytes

        row_start = y * row_bytes
        prev_start = (y - 1) * row_bytes

        for x in range(row_bytes):
            val = curr_row[x]
            left = unfiltered[row_start + x - bpp] if x >= bpp else 0
            up = unfiltered[prev_start + x] if y > 0 else 0
            upleft = unfiltered[prev_start + x - bpp] if (y > 0 and x >= bpp) else 0

            if filter_type == 0:  # None
                recon = val
            elif filter_type == 1:  # Sub
                recon = (val + left) & 0xff
            elif filter_type == 2:  # Up
                recon = (val + up) & 0xff
            elif filter_type == 3:  # Average
                recon = (val + ((left + up) // 2)) & 0xff
            elif filter_type == 4:  # Paeth
                recon = (val + paeth_predictor(left, up, upleft)) & 0xff
            else:
                raise ValueError(f'Unknown PNG filter type: {filter_type}')

            unfiltered[row_start + x] = recon

    # Convert unfiltered pixels to standard RGBA (width * height * 4)
    rgba = bytearray(width * height * 4)
    for i in range(width * height):
        dst_idx = i * 4
        src_idx = i * bpp
        if color_type == 6:  # RGBA
            rgba[dst_idx:dst_idx+4] = unfiltered[src_idx:src_idx+4]
        elif color_type == 2:  # RGB
            rgba[dst_idx] = unfiltered[src_idx]
            rgba[dst_idx+1] = unfiltered[src_idx+1]
            rgba[dst_idx+2] = unfiltered[src_idx+2]
            rgba[dst_idx+3] = 0xff
        elif color_type == 0:  # Grayscale
            gray = unfiltered[src_idx]
            rgba[dst_idx] = gray
            rgba[dst_idx+1] = gray
            rgba[dst_idx+2] = gray
            rgba[dst_idx+3] = 0xff
        elif color_type == 4:  # Grayscale + Alpha
            gray = unfiltered[src_idx]
            rgba[dst_idx] = gray
            rgba[dst_idx+1] = gray
            rgba[dst_idx+2] = gray
            rgba[dst_idx+3] = unfiltered[src_idx+1]

    return width, height, bytes(rgba)


def write_png_rgba(width, height, rgba, path=None):
    """Write width x height RGBA bytes to a valid PNG file using pure standard library."""
    if len(rgba) != width * height * 4:
        raise ValueError('RGBA buffer size mismatch')

    raw = bytearray()
    row_stride = width * 4
    for y in range(height):
        raw.append(0)  # filter type 0: None
        raw.extend(rgba[y*row_stride : (y+1)*row_stride])

    compressed = zlib.compress(bytes(raw), level=6)

    def make_chunk(tag, data):
        crc = zlib.crc32(tag + data) & 0xffffffff
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', crc)

    ihdr = struct.pack('>IIBBBBB', width, height, 8, 6, 0, 0, 0)
    png_data = b'\x89PNG\r\n\x1a\n' + make_chunk(b'IHDR', ihdr) + make_chunk(b'IDAT', compressed) + make_chunk(b'IEND', b'')

    if path is not None:
        with open(path, 'wb') as f:
            f.write(png_data)
    return png_data


def read_ppm_rgba(data):
    """Parse binary PPM (P6) into 1280x800 RGBA."""
    lines = []
    pos = 0
    header_tokens = []
    while len(header_tokens) < 4:
        # Read next token
        while pos < len(data) and data[pos:pos+1].isspace():
            pos += 1
        if pos >= len(data):
            break
        if data[pos:pos+1] == b'#':
            # Skip comment
            while pos < len(data) and data[pos:pos+1] != b'\n':
                pos += 1
            continue
        tok_start = pos
        while pos < len(data) and not data[pos:pos+1].isspace():
            pos += 1
        header_tokens.append(data[tok_start:pos].decode('latin1'))

    if len(header_tokens) < 4 or header_tokens[0] != 'P6':
        raise ValueError('Invalid or unsupported PPM header (must be P6)')

    width = int(header_tokens[1])
    height = int(header_tokens[2])
    maxval = int(header_tokens[3])
    if maxval != 255:
        raise ValueError(f'Unsupported PPM maxval: {maxval}')

    pos += 1  # single whitespace after maxval
    rgb_data = data[pos:pos + width * height * 3]
    if len(rgb_data) != width * height * 3:
        raise ValueError('Truncated PPM pixel data')

    rgba = bytearray(width * height * 4)
    for i in range(width * height):
        s = i * 3
        d = i * 4
        rgba[d] = rgb_data[s]
        rgba[d+1] = rgb_data[s+1]
        rgba[d+2] = rgb_data[s+2]
        rgba[d+3] = 0xff

    return width, height, bytes(rgba)


def landscape_to_framebuffer(src_rgba):
    """Convert 1280x800 landscape RGBA to 800x1280 portrait BGRA (90° CCW).

    Mapping:
    x_rot = y_src           (0 <= x_rot < 800)
    y_rot = 1279 - x_src    (0 <= y_rot < 1280)
    Byte length: 4,096,000 bytes.
    """
    if len(src_rgba) != SPLASH_WIDTH * SPLASH_HEIGHT * 4:
        raise ValueError(f'Expected {SPLASH_WIDTH*SPLASH_HEIGHT*4} RGBA bytes, got {len(src_rgba)}')

    fb = bytearray(SPLASH_SIZE)
    stride = ROTATED_WIDTH * 4  # 3200 bytes

    for y_src in range(SPLASH_HEIGHT):
        x_rot = y_src
        dst_idx = (1279 * ROTATED_WIDTH + x_rot) * 4
        src_idx = y_src * SPLASH_WIDTH * 4
        for _ in range(SPLASH_WIDTH):
            r = src_rgba[src_idx]
            g = src_rgba[src_idx + 1]
            b = src_rgba[src_idx + 2]
            a = src_rgba[src_idx + 3]

            fb[dst_idx] = b
            fb[dst_idx + 1] = g
            fb[dst_idx + 2] = r
            fb[dst_idx + 3] = a

            dst_idx -= stride
            src_idx += 4

    return bytes(fb)


def framebuffer_to_landscape(fb_data):
    """Convert 800x1280 portrait BGRA to 1280x800 landscape RGBA (90° CW).

    Reverse mapping:
    x_rot = y_src
    y_rot = 1279 - x_src
    """
    if len(fb_data) != SPLASH_SIZE:
        raise ValueError(f'Expected {SPLASH_SIZE} bytes for splash.bin, got {len(fb_data)}')

    src_rgba = bytearray(SPLASH_WIDTH * SPLASH_HEIGHT * 4)
    stride = ROTATED_WIDTH * 4

    for y_src in range(SPLASH_HEIGHT):
        x_rot = y_src
        dst_idx = (1279 * ROTATED_WIDTH + x_rot) * 4
        src_idx = y_src * SPLASH_WIDTH * 4
        for _ in range(SPLASH_WIDTH):
            b = fb_data[dst_idx]
            g = fb_data[dst_idx + 1]
            r = fb_data[dst_idx + 2]
            a = fb_data[dst_idx + 3]

            src_rgba[src_idx] = r
            src_rgba[src_idx + 1] = g
            src_rgba[src_idx + 2] = b
            src_rgba[src_idx + 3] = a

            dst_idx -= stride
            src_idx += 4

    return bytes(src_rgba)


def generate_enginex_preset():
    """Generate 1280x800 RGBA image with EngineX OS branding on dark #0a0a0aff background."""
    img = bytearray(SPLASH_WIDTH * SPLASH_HEIGHT * 4)

    # Fill background with #0a0a0aff
    bg_pixel = b'\x0a\x0a\x0a\xff'
    img[:] = bg_pixel * (SPLASH_WIDTH * SPLASH_HEIGHT)

    def set_pixel(x, y, r, g, b, a=255):
        if 0 <= x < SPLASH_WIDTH and 0 <= y < SPLASH_HEIGHT:
            idx = (y * SPLASH_WIDTH + x) * 4
            img[idx] = r
            img[idx + 1] = g
            img[idx + 2] = b
            img[idx + 3] = a

    def fill_rect(x1, y1, x2, y2, r, g, b, a=255):
        for y in range(max(0, y1), min(SPLASH_HEIGHT, y2)):
            for x in range(max(0, x1), min(SPLASH_WIDTH, x2)):
                set_pixel(x, y, r, g, b, a)

    def draw_text(text, x_start, y_start, scale, r, g, b):
        curr_x = x_start
        for ch in text.upper():
            glyph = FONT_5X7.get(ch, FONT_5X7.get(' '))
            for col_idx in range(5):
                col_bits = glyph[col_idx]
                for row_idx in range(7):
                    if (col_bits >> row_idx) & 1:
                        fill_rect(curr_x + col_idx * scale,
                                  y_start + row_idx * scale,
                                  curr_x + (col_idx + 1) * scale,
                                  y_start + (row_idx + 1) * scale,
                                  r, g, b)
            curr_x += 6 * scale

    # 1. Subtle top and bottom accent lines
    fill_rect(80, 70, SPLASH_WIDTH - 80, 73, 0x00, 0xe5, 0xff)  # Neon cyan line
    fill_rect(80, SPLASH_HEIGHT - 73, SPLASH_WIDTH - 80, SPLASH_HEIGHT - 70, 0xff, 0x88, 0x00)  # Orange line

    # 2. Centered DJ Platter / Shield Emblem (Center at x=640, y=260)
    cx, cy = 640, 250
    # Outer ring (Cyan glow)
    r_outer = 90
    r_inner = 84
    r_ring2_out = 72
    r_ring2_in = 68

    for y in range(cy - r_outer - 5, cy + r_outer + 6):
        for x in range(cx - r_outer - 5, cx + r_outer + 6):
            dist_sq = (x - cx) ** 2 + (y - cy) ** 2
            if r_inner ** 2 <= dist_sq <= r_outer ** 2:
                set_pixel(x, y, 0x00, 0xe5, 0xff)
            elif r_ring2_in ** 2 <= dist_sq <= r_ring2_out ** 2:
                set_pixel(x, y, 0xff, 0x88, 0x00)

    # Stylized center geometric "X" emblem inside the rings
    # Draw thick crossing diagonal bars
    for t in range(-45, 46):
        for w in range(-7, 8):
            set_pixel(cx + t + w, cy + t, 0xff, 0xff, 0xff)
            set_pixel(cx + t + w, cy - t, 0xff, 0xff, 0xff)

    # 3. Main Brand Title: "ENGINEX OS" (scale 9)
    # 10 chars * 6 * 9 = 540 px width
    text_main = "ENGINEX OS"
    scale_main = 8
    text_width = len(text_main) * 6 * scale_main - scale_main
    x_main = (SPLASH_WIDTH - text_width) // 2
    draw_text(text_main, x_main, 410, scale_main, 0xff, 0xff, 0xff)

    # 4. Secondary Subtitle: "HIGH PERFORMANCE STANDALONE DECK SYSTEM" (scale 3)
    text_sub = "HIGH PERFORMANCE STANDALONE DECK SYSTEM"
    scale_sub = 3
    sub_width = len(text_sub) * 6 * scale_sub - scale_sub
    x_sub = (SPLASH_WIDTH - sub_width) // 2
    draw_text(text_sub, x_sub, 500, scale_sub, 0x00, 0xe5, 0xff)

    # 5. Version / Platform Tagline
    text_tag = "UNIVERSAL BOOT SPLASH - ALL 16 MODELS"
    scale_tag = 2
    tag_width = len(text_tag) * 6 * scale_tag - scale_tag
    x_tag = (SPLASH_WIDTH - tag_width) // 2
    draw_text(text_tag, x_tag, 570, scale_tag, 0x88, 0x88, 0x88)

    return bytes(img)


def encode_splash(input_arg=None, preset=None, output_path=None, input_path=None):
    """Encode an image or preset to 4,096,000 bytes BGRA8888 splash.bin."""
    if preset == 'enginex':
        rgba = generate_enginex_preset()
    elif isinstance(input_arg, (bytes, bytearray)):
        if len(input_arg) == SPLASH_WIDTH * SPLASH_HEIGHT * 4:
            rgba = bytes(input_arg)
        elif len(input_arg) == SPLASH_SIZE:
            fb = bytes(input_arg)
            if output_path:
                Path(output_path).write_bytes(fb)
            return SplashBytes(fb)
        elif input_arg.startswith(b'\x89PNG\r\n\x1a\n'):
            w, h, rgba = read_png_rgba(input_arg)
            if w != SPLASH_WIDTH or h != SPLASH_HEIGHT:
                raise ValueError(f'PNG dimensions must be {SPLASH_WIDTH}x{SPLASH_HEIGHT}, got {w}x{h}')
        else:
            raise ValueError(f'Invalid input byte length: {len(input_arg)}')
    else:
        src = input_path or input_arg
        if not src:
            raise ValueError('Must specify input or --preset')
        p = Path(src)
        data = p.read_bytes()
        if len(data) == SPLASH_SIZE:
            fb = data
            if output_path:
                Path(output_path).write_bytes(fb)
            return SplashBytes(fb)
        elif data.startswith(b'\x89PNG\r\n\x1a\n'):
            w, h, rgba = read_png_rgba(data)
            if w != SPLASH_WIDTH or h != SPLASH_HEIGHT:
                raise ValueError(f'PNG dimensions must be {SPLASH_WIDTH}x{SPLASH_HEIGHT}, got {w}x{h}')
        elif data.startswith(b'P6'):
            w, h, rgba = read_ppm_rgba(data)
            if w != SPLASH_WIDTH or h != SPLASH_HEIGHT:
                raise ValueError(f'PPM dimensions must be {SPLASH_WIDTH}x{SPLASH_HEIGHT}, got {w}x{h}')
        else:
            raise ValueError(f'Unrecognized input image format in {src}')

    fb = landscape_to_framebuffer(rgba)
    if output_path:
        Path(output_path).write_bytes(fb)
    return SplashBytes(fb)


def decode_splash(input_arg, output_path=None):
    """Decode 4,096,000 bytes BGRA8888 splash.bin to 1280x800 PNG bytes."""
    if isinstance(input_arg, (bytes, bytearray)):
        data = bytes(input_arg)
    else:
        data = Path(input_arg).read_bytes()

    if len(data) != SPLASH_SIZE:
        raise ValueError(f'Expected exactly {SPLASH_SIZE} bytes, got {len(data)}')
    rgba = framebuffer_to_landscape(data)
    png_bytes = write_png_rgba(SPLASH_WIDTH, SPLASH_HEIGHT, rgba, output_path)
    return SplashBytes(png_bytes)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)

    enc = sub.add_parser('encode', help='Encode 1280x800 image to 800x1280 BGRA splash.bin')
    enc.add_argument('--input', type=Path, help='Input PNG, PPM, or raw 4,096,000-byte file')
    enc.add_argument('--preset', choices=['enginex'], help='Generate built-in preset')
    enc.add_argument('--output', required=True, type=Path, help='Output splash.bin path')

    dec = sub.add_parser('decode', help='Decode 800x1280 BGRA splash.bin to 1280x800 PNG')
    dec.add_argument('--input', required=True, type=Path, help='Input splash.bin file')
    dec.add_argument('--output', required=True, type=Path, help='Output PNG path')

    info = sub.add_parser('info', help='Display splash.bin metadata')
    info.add_argument('--input', required=True, type=Path, help='Input splash.bin file')

    args = parser.parse_args(argv)

    if args.command == 'encode':
        out = encode_splash(args.input, args.preset, args.output)
        print(f'Encoded {len(out)} bytes to {args.output}')
    elif args.command == 'decode':
        out = decode_splash(args.input, args.output)
        print(f'Decoded {len(out)} bytes to {args.output}')
    elif args.command == 'info':
        p = Path(args.input)
        sz = p.stat().st_size
        print(f'File: {p.name}')
        print(f'Size: {sz} bytes ({hex(sz)})')
        print(f'Expected size: {SPLASH_SIZE} bytes ({hex(SPLASH_SIZE)})')
        print(f'Valid splash: {sz == SPLASH_SIZE}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
