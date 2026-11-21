#!/usr/bin/env python3
"""Score a Stems child against MUSDB18 7-STEMS excerpts.

The dataset is supplied through ``MUSDB18_STEMS`` and is never downloaded by
this script.  Each ``*.stem.mp4`` contains mixture, drums, bass, other and
vocals in that order.  The separator is an Engine child-protocol command.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time

import numpy as np
from scipy.linalg import toeplitz
from scipy.signal import fftconvolve

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
from render import run  # noqa: E402

STEMS = ("vocals", "bass", "drums", "melody")
STREAMS = (4, 2, 1, 3)
EPS = 1e-12


def decode(path, stream):
    """Decode one stereo stream from a MUSDB18 STEMS container."""
    proc = subprocess.run(
        ["ffmpeg", "-v", "error", "-i", str(path), "-map", f"0:a:{stream}",
         "-ac", "2", "-ar", "44100", "-f", "f32le", "pipe:1"],
        check=True,
        stdout=subprocess.PIPE,
    )
    return np.frombuffer(proc.stdout, "<f4").reshape(-1, 2).T.copy()


class BssEval:
    """BSS Eval time-invariant filter scorer with a reusable reference matrix."""

    def __init__(self, references, filter_length=512):
        self.references = np.asarray(references, dtype=np.float64)
        self.filter_length = filter_length
        self._gram = self._make_gram(self.references, filter_length)

    @staticmethod
    def _make_gram(references, filter_length):
        nsrc, samples = references.shape
        padded = np.pad(references, ((0, 0), (0, filter_length - 1)))
        nfft = 1 << (samples + filter_length - 2).bit_length()
        spectrum = np.fft.rfft(padded, n=nfft, axis=1)
        gram = np.zeros((nsrc * filter_length, nsrc * filter_length))
        for i in range(nsrc):
            for j in range(i, nsrc):
                corr = np.fft.irfft(spectrum[i] * spectrum[j].conj(), n=nfft).real
                block = toeplitz(
                    np.r_[corr[0], corr[-1:-filter_length:-1]],
                    corr[:filter_length],
                )
                gram[i * filter_length:(i + 1) * filter_length,
                     j * filter_length:(j + 1) * filter_length] = block
                gram[j * filter_length:(j + 1) * filter_length,
                     i * filter_length:(i + 1) * filter_length] = block.T
        return gram

    def project(self, estimated):
        references = self.references
        flen = self.filter_length
        nsrc, samples = references.shape
        padded = np.pad(references, ((0, 0), (0, flen - 1)))
        estimate = np.pad(estimated, (0, flen - 1))
        nfft = 1 << (samples + flen - 2).bit_length()
        sf = np.fft.rfft(padded, n=nfft, axis=1)
        ef = np.fft.rfft(estimate, n=nfft)
        d = np.empty(nsrc * flen)
        for i in range(nsrc):
            corr = np.fft.irfft(sf[i] * ef.conj(), n=nfft).real
            d[i * flen:(i + 1) * flen] = np.r_[corr[0], corr[-1:-flen:-1]]
        try:
            coeff = np.linalg.solve(self._gram, d).reshape((flen, nsrc), order="F")
        except np.linalg.LinAlgError:
            coeff = np.linalg.lstsq(self._gram, d, rcond=None)[0].reshape((flen, nsrc), order="F")
        parts = [fftconvolve(coeff[:, i], padded[i])[:samples + flen - 1]
                 for i in range(nsrc)]
        return np.asarray(parts), estimate


def bss_metrics(references, estimated, filter_length=512):
    """Return SDR, SIR and SAR for four stereo estimates."""
    references = np.asarray(references)
    estimated = np.asarray(estimated)
    if references.shape != estimated.shape or references.ndim != 3:
        raise ValueError("references and estimated must both be (stems, channels, frames)")
    values = np.empty((len(STEMS), 3), dtype=np.float64)
    per_channel = []
    for channel in range(references.shape[1]):
        scorer = BssEval(references[:, channel], filter_length)
        channel_values = []
        for stem in range(len(STEMS)):
            projected, estimate = scorer.project(estimated[stem, channel])
            target = projected[stem]
            interference = projected.sum(axis=0) - target
            artifact = estimate - projected.sum(axis=0)
            signal = target + EPS
            db = lambda numerator, denominator: 10 * np.log10(
                (np.dot(numerator, numerator) + EPS) /
                (np.dot(denominator, denominator) + EPS)
            )
            channel_values.append((db(signal, interference + artifact),
                                   db(signal, interference),
                                   db(target + interference, artifact)))
        per_channel.append(channel_values)
    values[:] = np.mean(np.asarray(per_channel), axis=0)
    return values


def score_track(path, separator, filter_length):
    mix = decode(path, 0)
    references = np.stack([decode(path, stream) for stream in STREAMS])
    frames = min(mix.shape[1], references.shape[2])
    mix, references = mix[:, :frames], references[:, :, :frames]
    estimated_parts = []
    result = run(separator, (mix[:, i:i + 1024] for i in range(0, frames, 1024)),
                 channels=2, rate=44100, consume=estimated_parts.append, max_block=1024)
    estimated = np.concatenate(estimated_parts, axis=2)[:, :, :frames]
    return result, bss_metrics(references, estimated, filter_length)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--split", default="test")
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--filter-length", type=int, default=512)
    parser.add_argument("--json", action="store_true")
    parser.add_argument("--separator", nargs="+", required=True)
    args = parser.parse_args(argv)
    dataset = os.environ.get("MUSDB18_STEMS")
    if not dataset:
        parser.error("MUSDB18_STEMS must point to the extracted MUSDB18-7-STEMS directory")
    tracks = sorted((Path(dataset) / args.split).glob("*.stem.mp4"))
    if args.limit:
        tracks = tracks[:args.limit]
    if not tracks:
        parser.error(f"no *.stem.mp4 excerpts under {dataset}/{args.split}")
    rows, started = [], time.monotonic()
    for index, track in enumerate(tracks, 1):
        timing, metrics = score_track(track, args.separator, args.filter_length)
        rows.append(metrics)
        print(f"{index}/{len(tracks)} {track.name} {timing['elapsed_seconds']:.3f}s", file=sys.stderr)
    median = np.median(np.asarray(rows), axis=0)
    if args.json:
        print(json.dumps({"dataset": str(Path(dataset).resolve()), "split": args.split,
                          "tracks": len(tracks), "filter_length": args.filter_length,
                          "elapsed_seconds": time.monotonic() - started,
                          "median": {name: dict(zip(("sdr", "sir", "sar"), values.tolist()))
                                     for name, values in zip(STEMS, median)}}))
        return 0
    print(f"dataset={Path(dataset).resolve()} split={args.split} tracks={len(tracks)} filter={args.filter_length}")
    print("stem     SDR      SIR      SAR")
    for name, values in zip(STEMS, median):
        print(f"{name:<7} {values[0]:6.2f}  {values[1]:6.2f}  {values[2]:6.2f}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
