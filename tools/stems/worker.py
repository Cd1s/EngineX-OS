#!/usr/bin/env python3
"""Engine 5.0.4 child protocol + bounded, causal spectral approximation.

This is NOT a neural/semantic vocal separator. A center-panned harmonic mask
approximates vocals; HPSS estimates drums, a low-band mask estimates bass, and
the remainder is melody. Masks sum to one; the stems sum back to the input.
"""
import argparse
from collections import deque
import os
import sys

os.environ.setdefault('OPENBLAS_NUM_THREADS', '1')
os.environ.setdefault('OMP_NUM_THREADS', '1')
import numpy as np
from scipy.fft import rfft, irfft
from scipy.ndimage import median_filter

MAX_HEADER = 8192
MAX_BLOCK = 65536
MAX_RATE = 192000


def header_read(stream):
    fields = {}
    used = 0
    while True:
        line = stream.readline(MAX_HEADER + 1)
        if not line:
            if not fields:
                return None
            raise ValueError('truncated header')
        used += len(line)
        if used > MAX_HEADER or not line.endswith(b'\n'):
            raise ValueError('header too large or unterminated')
        line = line.rstrip(b'\r\n')
        if not line:
            return fields
        key, sep, value = line.partition(b'=')
        if not sep or not key or key in fields:
            raise ValueError('invalid or duplicate header field')
        fields[key] = value


def header_write(stream, **fields):
    stream.write(b''.join(str(k).encode() + b'=' + str(v).encode() + b'\n'
                          for k, v in fields.items()) + b'\n')
    stream.flush()


def read_exact(stream, count):
    result = bytearray()
    while len(result) < count:
        chunk = stream.read(count - len(result))
        if not chunk:
            raise ValueError('truncated PCM payload')
        result.extend(chunk)
    return result


class Separator:
    """Streaming WOLA, 2048-point Hann/512 hop; at most 2047 frames latency."""
    size, hop = 2048, 512

    def __init__(self, channels, sample_rate):
        self.channels = channels
        n, h = self.size, self.hop
        self.window = np.hanning(n).astype(np.float32)
        self.pending = np.zeros((channels, n-h), np.float32)
        self.ola = np.zeros((4, channels, n), np.float32)
        self.norm = np.zeros(n, np.float32)
        self.history = deque(maxlen=17)
        self.skip = n-h
        self.received = self.returned = 0
        self.finished = False
        f = np.fft.rfftfreq(n, 1/sample_rate)
        self.low = (1 / (1 + (f/220)**6)).astype(np.float32)
        self.vocal_band = ((1/(1+(180/np.maximum(f, 1))**6)) *
                           (1/(1+(f/5000)**6))).astype(np.float32)

    def _process(self, x):
        self.pending = np.concatenate((self.pending, x), axis=1)
        pieces = []
        n, h = self.size, self.hop
        while self.pending.shape[1] >= n:
            z = rfft(self.pending[:, :n]*self.window, workers=1)
            mag = np.mean(abs(z), axis=0)
            self.history.append(mag)
            harmonic = np.median(np.stack(self.history), axis=0)
            percussive = median_filter(mag, size=31, mode='nearest')
            p_mask = percussive**2 / (harmonic**2 + percussive**2 + 1e-20)
            h_mask = 1-p_mask
            bass_mask = h_mask*self.low
            if self.channels == 2:
                # Mid/side coherence favors centered material; this also leaks
                # centered instruments and cannot identify a singer semantically.
                mid = abs((z[0]+z[1])*0.5)
                side = abs((z[0]-z[1])*0.5)
                center = np.clip((mid-side)/(mid+side+1e-10), 0, 1)
            else:
                center = np.ones_like(mag)
            vocal_mask = h_mask*(1-self.low)*self.vocal_band*center*0.8
            melody_mask = 1-p_mask-bass_mask-vocal_mask
            # Canonical worker order; native output mapping is documented in PROTOCOL.md.
            masks = np.stack((vocal_mask, bass_mask, p_mask, melody_mask))
            waves = irfft(masks[:, None, :]*z[None, :, :], n=n, workers=1)
            self.ola += waves*self.window
            self.norm += self.window**2
            out = self.ola[:, :, :h] / np.maximum(self.norm[:h], 1e-12)
            self.ola[:, :, :-h] = self.ola[:, :, h:]
            self.ola[:, :, -h:] = 0
            self.norm[:-h] = self.norm[h:]
            self.norm[-h:] = 0
            self.pending = self.pending[:, h:]
            if self.skip:
                take = min(self.skip, h)
                out = out[:, :, take:]
                self.skip -= take
            if out.shape[-1]:
                pieces.append(out.copy())
        if not pieces:
            return np.empty((4, self.channels, 0), np.float32)
        return np.concatenate(pieces, axis=2).astype(np.float32)

    def feed(self, x):
        if self.finished:
            raise ValueError('PCM after drain')
        if not np.isfinite(x).all():
            raise ValueError('non-finite PCM')
        self.received += x.shape[1]
        out = self._process(x)
        self.returned += out.shape[-1]
        return out

    def drain(self):
        if self.finished:
            return np.empty((4, self.channels, 0), np.float32)
        left = self.received-self.returned
        out = self._process(np.zeros((self.channels, self.size+self.hop), np.float32))
        out = out[:, :, :left]
        self.returned += out.shape[-1]
        self.finished = True
        if self.returned != self.received:
            raise ValueError('drain frame accounting mismatch')
        return out


def serve(src, dst):
    # The original parent treats initReply/ack/feedback/reply as log labels;
    # they are not required keys on the wire.
    header_write(dst, session='FFFFFFFFFFFFFFFF')
    init = header_read(src)
    if init is None:
        return
    channels = int(init[b'numChannels'])
    sample_rate = float(init[b'sampleRate'])
    max_block = int(init[b'maxBlockSize'])
    if channels not in (1, 2) or not 8000 <= sample_rate <= MAX_RATE:
        raise ValueError('unsupported channel count or sample rate')
    if not 1 <= max_block <= MAX_BLOCK:
        raise ValueError('unsupported block size')
    separator = Separator(channels, sample_rate)
    queued = np.empty((4, channels, 0), np.float32)
    header_write(dst, preparing='ready')
    header_write(dst, preparing='ready', numReturnChannels=channels*4)
    while True:
        fields = header_read(src)
        if fields is None:
            return
        action = fields.get(b'action')
        if action == b'done':
            tail = np.concatenate((queued, separator.drain()), axis=2)
            for start in range(0, tail.shape[-1], max_block):
                chunk = tail[:, :, start:start+max_block]
                header_write(dst, returnFrames=chunk.shape[-1])
                dst.write(chunk.astype('<f4', copy=False).tobytes(order='C'))
                dst.flush()
            header_write(dst, returnFrames=0)
            return
        if action != b'block':
            raise ValueError('unsupported action')
        count = int(fields[b'numSamples'])
        if not 0 <= count <= max_block:
            raise ValueError('invalid PCM frame count')
        if count:
            raw = read_exact(src, count*channels*4)
            x = np.frombuffer(raw, dtype='<f4').reshape(channels, count)
            stems = separator.feed(x)
        else:
            stems = np.empty((4, channels, 0), np.float32)
        queued = np.concatenate((queued, stems), axis=2)
        reply = queued[:, :, :max_block]
        queued = queued[:, :, max_block:]
        header_write(dst, returnFrames=reply.shape[-1])
        dst.write(reply.astype('<f4', copy=False).tobytes(order='C'))
        dst.flush()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('-s', action='store_true')
    parser.add_argument('-hv')
    parser.add_argument('-pv')
    parser.parse_args()
    try:
        serve(sys.stdin.buffer, sys.stdout.buffer)
    except (ValueError, KeyError, BrokenPipeError, OSError) as exc:
        print('stems worker:', str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
