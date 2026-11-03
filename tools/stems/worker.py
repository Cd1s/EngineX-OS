#!/usr/bin/env python3
"""Engine 5.0.4 child protocol + bounded, causal spectral approximation.

This is NOT a neural/semantic vocal separator. A center-panned harmonic mask
approximates vocals; HPSS estimates drums, a low-band mask estimates bass, and
the remainder is melody. Masks sum to one; the stems sum back to the input.
"""
import argparse
import io
import os
import select
import sys

os.environ.setdefault('OPENBLAS_NUM_THREADS', '4')
os.environ.setdefault('OMP_NUM_THREADS', '4')
import numpy as np
from numpy.lib.stride_tricks import sliding_window_view
from scipy.fft import rfft, irfft
from scipy.ndimage import median_filter

MAX_HEADER = 8192
MAX_BLOCK = 65536
MAX_RATE = 192000
BATCH = 64  # blocks separated together when they are already waiting
HISTORY = 17


class Input:
    """Buffered input that can tell whether more bytes are already waiting."""

    def __init__(self, stream):
        self.stream, self.buf, self.pos = stream, bytearray(), 0
        try:
            self.fd = stream.fileno()
        except (AttributeError, OSError, io.UnsupportedOperation):
            self.fd = None

    def _fill(self):
        if self.pos:
            del self.buf[:self.pos]
            self.pos = 0
        chunk = os.read(self.fd, 1 << 20) if self.fd is not None else self.stream.read(1 << 20)
        self.buf += chunk
        return bool(chunk)

    def waiting(self):
        if self.pos < len(self.buf) or self.fd is None:
            return True
        return bool(select.select([self.fd], [], [], 0)[0])

    def readline(self, limit):
        while True:
            end = self.buf.find(b'\n', self.pos, self.pos + limit)
            if end >= 0 or len(self.buf) - self.pos >= limit or not self._fill():
                break
        n = end + 1 - self.pos if end >= 0 else min(len(self.buf) - self.pos, limit)
        line = bytes(self.buf[self.pos:self.pos + n])
        self.pos += n
        return line

    def read(self, count):
        if self.pos == len(self.buf):
            self._fill()
        chunk = bytes(self.buf[self.pos:self.pos + count])
        self.pos += len(chunk)
        return chunk


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
        self.history = np.empty((0, n//2+1), np.float32)
        self.skip = n-h
        self.received = self.returned = 0
        self.finished = False
        self.workers = int(os.environ.get('STEMS_WORKERS', '4'))
        f = np.fft.rfftfreq(n, 1/sample_rate)
        self.low = (1 / (1 + (f/220)**6)).astype(np.float32)
        self.vocal_band = ((1/(1+(180/np.maximum(f, 1))**6)) *
                           (1/(1+(f/5000)**6))).astype(np.float32)

    def _harmonic(self, mag):
        """Median over each frame's last HISTORY magnitudes, as one streaming deque would."""
        hist = np.concatenate((self.history, mag))
        start = len(self.history)
        out = np.empty_like(mag)
        early = max(0, min(len(mag), HISTORY - 1 - start))
        for i in range(early):  # only the first frames of a session
            out[i] = np.median(hist[max(0, start+i+1-HISTORY):start+i+1], axis=0)
        if early < len(mag):
            # One 1-D median over every bin's history laid end to end: scipy's 1-D
            # path is far faster than np.median over windows or an n-D filter.
            # origin ends each window at its own frame; the outputs kept never
            # reach into the neighbouring bin.
            seg = np.ascontiguousarray(hist[start+early+1-HISTORY:].T)
            flat = median_filter(seg.ravel(), size=HISTORY, origin=HISTORY//2, mode='nearest')
            out[early:] = flat.reshape(seg.shape)[:, HISTORY-1:].T
        self.history = hist[-HISTORY:]
        return out

    def _process(self, x):
        self.pending = np.concatenate((self.pending, x), axis=1)
        n, h = self.size, self.hop
        count = (self.pending.shape[1] - n) // h + 1 if self.pending.shape[1] >= n else 0
        if not count:
            return np.empty((4, self.channels, 0), np.float32)
        # Every complete window at once: (channels, frames, n).
        frames = sliding_window_view(self.pending, n, axis=1)[:, :count*h:h]
        z = rfft(frames*self.window, workers=self.workers)
        mag = np.mean(abs(z), axis=0)
        harmonic = self._harmonic(mag)
        # Each frame's median across frequency, as one 1-D pass: 15 edge copies on
        # both sides of every row are exactly mode='nearest' for a 31-bin window.
        padded = np.pad(mag, ((0, 0), (15, 15)), mode='edge')
        percussive = median_filter(padded.ravel(), size=31, mode='nearest').reshape(padded.shape)[:, 15:-15]
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
        waves = irfft(masks[:, None]*z[None], n=n, workers=self.workers)*self.window
        pieces = []
        for i in range(count):
            self.ola += waves[:, :, i]
            self.norm += self.window**2
            out = self.ola[:, :, :h] / np.maximum(self.norm[:h], 1e-12)
            self.ola[:, :, :-h] = self.ola[:, :, h:]
            self.ola[:, :, -h:] = 0
            self.norm[:-h] = self.norm[h:]
            self.norm[-h:] = 0
            if self.skip:
                take = min(self.skip, h)
                out = out[:, :, take:]
                self.skip -= take
            if out.shape[-1]:
                pieces.append(out.copy())
        self.pending = self.pending[:, count*h:]
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

    def reply(frames):
        nonlocal queued
        chunk = queued[:, :, :frames]
        queued = queued[:, :, frames:]
        header_write(dst, returnFrames=chunk.shape[-1])
        dst.write(chunk.astype('<f4', copy=False).tobytes(order='C'))
        dst.flush()

    fields = header_read(src)
    while fields is not None:
        # Blocks already waiting (a pipelining client sends ahead) are separated
        # together; each still gets its own reply. A client that waits for every
        # reply leaves nothing waiting, so it is served block by block.
        blocks, pcm = 0, []
        while True:
            action = fields.get(b'action')
            if action == b'done':
                break
            if action != b'block':
                raise ValueError('unsupported action')
            count = int(fields[b'numSamples'])
            if not 0 <= count <= max_block:
                raise ValueError('invalid PCM frame count')
            if count:
                raw = read_exact(src, count*channels*4)
                pcm.append(np.frombuffer(raw, dtype='<f4').reshape(channels, count))
            blocks += 1
            if blocks >= BATCH or not src.waiting():
                fields = {}
                break
            fields = header_read(src)
            if fields is None:
                break
        if pcm:
            queued = np.concatenate((queued, separator.feed(np.concatenate(pcm, axis=1))), axis=2)
        for _ in range(blocks):
            reply(max_block)
        if fields is not None and fields.get(b'action') == b'done':
            queued = np.concatenate((queued, separator.drain()), axis=2)
            while queued.shape[-1]:
                reply(max_block)
            reply(0)
            return
        if fields is not None:
            fields = header_read(src)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('-s', action='store_true')
    parser.add_argument('-hv')
    parser.add_argument('-pv')
    parser.parse_args()
    try:
        serve(Input(sys.stdin.buffer), sys.stdout.buffer)
    except (ValueError, KeyError, BrokenPipeError, OSError) as exc:
        print('stems worker:', str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
