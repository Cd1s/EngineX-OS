#!/usr/bin/env python3
"""Unlock model-ID soft-gated features (Stems pre-rendered playback, Stems FX,
Stems integration flag, and native HFS+ support) for JP07 (SC5000) in Engine OS 5.0.4.
"""
import argparse
import hashlib
import struct
from pathlib import Path

OFFICIAL_ENGINE_SHA256 = "e1eb0e2b858880016daa3ad14538426204f8e698d855a274aebc924a5f9a5caa"
UNLOCKED_STR_OFF = 0x1D99A18  # b"Unlocked\x00"
FALSE_STR_OFF = 0x1D99720     # b"False\x00"

PATCHES = [
    # 1. hasStemsFx (this + 0x17c): length 5 -> 8 at 0xf62ae0 (mov r2, #5 -> mov r2, #8)
    (0xF62AE0, struct.pack("<I", 0xE3A02005), struct.pack("<I", 0xE3A02008), "hasStemsFx length 5 -> 8"),
    # 2. hasStemsFx literal at 0xf62ecc (PC at 0xf62af4 + 8 = 0xf62afc): "False" -> "Unlocked"
    (0xF62ECC, struct.pack("<I", FALSE_STR_OFF - 0xF62AFC), struct.pack("<I", UNLOCKED_STR_OFF - 0xF62AFC), "hasStemsFx False -> Unlocked"),
    # 3. stemsPreRenderedPlayback (this + 0x9c): length 5 -> 8 at 0xf62b10 (mov r3, #5 -> mov r3, #8)
    (0xF62B10, struct.pack("<I", 0xE3A03005), struct.pack("<I", 0xE3A03008), "stemsPreRenderedPlayback length 5 -> 8"),
    # 4. stemsPreRenderedPlayback literal at 0xf62ed0 (PC at 0xf62b40 + 8 = 0xf62b48): "False" -> "Unlocked"
    (0xF62ED0, struct.pack("<I", FALSE_STR_OFF - 0xF62B48), struct.pack("<I", UNLOCKED_STR_OFF - 0xF62B48), "stemsPreRenderedPlayback False -> Unlocked"),
    # 5. disableHfsPlus (this + 0x289 = 649): strb r3(1), [r4, #649] -> strb r1(0), [r4, #649] at 0xf62ba0
    (0xF62BA0, struct.pack("<I", 0xE5C43289), struct.pack("<I", 0xE5C41289), "disableHfsPlus 1 -> 0"),
    # 6. JP07 hasStems integration flag (this + 0xdb): mov r2, #0x100 -> movw r2, #0x101 at 0xf6592c
    (0xF6592C, struct.pack("<I", 0xE3A02C01), struct.pack("<I", 0xE3002101), "JP07 hasStems 0 -> 1"),
]


def patch_engine_bytes(raw: bytes) -> bytes:
    buf = bytearray(raw)
    if buf[UNLOCKED_STR_OFF:UNLOCKED_STR_OFF + 9] != b"Unlocked\x00":
        raise ValueError("Unlocked string not found at expected offset 0x1d99a18")
    if buf[FALSE_STR_OFF:FALSE_STR_OFF + 6] != b"False\x00":
        raise ValueError("False string not found at expected offset 0x1d99720")
    for offset, expected, replacement, desc in PATCHES:
        actual = bytes(buf[offset:offset + len(expected)])
        if actual != expected:
            raise ValueError(f"patch mismatch at 0x{offset:x} ({desc}): got {actual.hex()}, expected {expected.hex()}")
        buf[offset:offset + len(replacement)] = replacement
    return bytes(buf)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", type=Path, help="path to stock 5.0.4 /usr/Engine/Engine binary")
    parser.add_argument("--output", type=Path, required=True, help="output path for unlocked Engine binary")
    args = parser.parse_args()
    raw = args.input.read_bytes()
    sha = hashlib.sha256(raw).hexdigest()
    if sha != OFFICIAL_ENGINE_SHA256:
        raise SystemExit(f"refused: input SHA-256 {sha} does not match official 5.0.4 Engine ({OFFICIAL_ENGINE_SHA256})")
    patched = patch_engine_bytes(raw)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_bytes(patched)
    args.output.chmod(0o755)
    print(f"Patched 6 JP07 soft-gate sites: {sha} -> {hashlib.sha256(patched).hexdigest()}")


if __name__ == "__main__":
    main()
