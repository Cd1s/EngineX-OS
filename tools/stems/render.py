#!/usr/bin/env python3
"""Native-protocol driver: 16-bit WAV -> remote child -> four 16-bit WAVs.

Also used to verify the ARM executable under qemu without running Engine.
Credentials are inherited as a file path; never passed on the command line.
"""
import argparse
import ctypes
import json
import os
from pathlib import Path
import select
import subprocess
import time
import wave
import numpy as np
from worker import header_write

# Native level callback order: Vocal=s0, Bass=s1, Drums=s2, Melody=s3.
NATIVE_NAMES = ('vocals', 'bass', 'drums', 'melody')


def _wait_pipe_ready(pipe, timeout):
    fd = pipe.fileno()
    if os.name == 'nt':
        try:
            import msvcrt
            handle = msvcrt.get_osfhandle(fd)
            avail = ctypes.c_ulong(0)
            deadline = time.monotonic() + timeout
            while True:
                ok = ctypes.windll.kernel32.PeekNamedPipe(
                    ctypes.c_void_p(handle), None, 0, None, ctypes.byref(avail), None
                )
                if not ok or avail.value > 0:
                    return
                if time.monotonic() >= deadline:
                    raise TimeoutError('remote renderer stalled')
                time.sleep(min(0.005, max(0.0, deadline - time.monotonic())))
        except TimeoutError:
            raise
        except (ImportError, AttributeError, OSError, ValueError):
            return
    try:
        if not select.select([pipe], [], [], timeout)[0]:
            raise TimeoutError('remote renderer stalled')
    except TimeoutError:
        raise
    except OSError:
        return


class DeadlineReader:
    def __init__(self, pipe, timeout=30):
        self.pipe, self.buf, self.timeout = pipe, bytearray(), timeout

    def _fill(self):
        _wait_pipe_ready(self.pipe, self.timeout)
        chunk = os.read(self.pipe.fileno(), 65536)
        if not chunk:
            raise EOFError('remote child closed its output')
        self.buf.extend(chunk)

    def read(self, size):
        while len(self.buf) < size:
            self._fill()
        value = bytes(self.buf[:size])
        del self.buf[:size]
        return value

    def header(self):
        fields = {}
        used = 0
        while True:
            while b'\n' not in self.buf:
                self._fill()
                if b'\n' not in self.buf and len(self.buf) > 8192:
                    raise ValueError('oversized response header')
            end = self.buf.index(b'\n')+1
            line = bytes(self.buf[:end]).rstrip(b'\r\n')
            del self.buf[:end]
            used += end
            if used > 8192:
                raise ValueError('oversized response header')
            if not line:
                return fields
            k, sep, v = line.partition(b'=')
            if not sep or k in fields:
                raise ValueError('bad response field')
            fields[k] = v


def run(command, blocks, channels=2, rate=44100, consume=None, timeout=30, max_block=1024):
    child = subprocess.Popen(command + ['-s', '-hv', '1.0.0', '-pv', '1.6.0'],
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    reader = DeadlineReader(child.stdout, timeout)
    incoming = outgoing = 0
    start = time.monotonic()
    try:
        hello = reader.header()
        assert b'session' in hello
        # Our backend does not validate the original processor's session proof.
        header_write(child.stdin, version='1.6.0', time=int(time.time()),
                     numChannels=channels, sampleRate=rate, maxBlockSize=max_block,
                     highQuality=1, residual=0, numThreads=1,
                     session='FFFFFFFFFFFFFFFF')
        assert reader.header().get(b'preparing') != b'failed'
        ready = reader.header()
        assert int(ready[b'numReturnChannels']) == 4*channels

        def receive():
            nonlocal outgoing
            meta = reader.header()
            frames = int(meta[b'returnFrames'])
            if not 0 <= frames <= max_block:
                raise ValueError('invalid return frame count')
            raw = reader.read(frames*channels*4*4)
            y = np.frombuffer(raw, '<f4').reshape(4, channels, frames)
            if not np.isfinite(y).all():
                raise ValueError('non-finite response PCM')
            outgoing += frames
            if consume and frames:
                consume(y)
            return frames

        for x in blocks:
            if x.shape[0] != channels or not 0 <= x.shape[1] <= max_block:
                raise ValueError('input block shape')
            header_write(child.stdin, action='block', numSamples=x.shape[1])
            child.stdin.write(x.astype('<f4').tobytes(order='C'))
            child.stdin.flush()
            incoming += x.shape[1]
            receive()
        header_write(child.stdin, action='done')
        while receive():
            pass
        child.stdin.close()
        if child.wait(timeout=10) != 0:
            raise ValueError('remote child failed')
        if incoming != outgoing:
            raise ValueError(f'frame count mismatch {incoming} != {outgoing}')
        return dict(input_frames=incoming, output_frames=outgoing,
                    channels=channels, sample_rate=rate,
                    elapsed_seconds=round(time.monotonic()-start, 3))
    finally:
        if child.poll() is None:
            child.kill()
            child.wait()
        child.stdout.close()
        if not child.stdin.closed:
            child.stdin.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('input', type=Path)
    parser.add_argument('output', type=Path)
    parser.add_argument('--client', default=str(Path(__file__).parent/'bin/stems-processor-linux-amd64'))
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    outputs = []
    with wave.open(str(args.input), 'rb') as src:
        channels, rate = src.getnchannels(), src.getframerate()
        if channels not in (1, 2) or src.getsampwidth() != 2:
            raise ValueError('input must be mono/stereo 16-bit PCM WAV')
        try:
            for name in NATIVE_NAMES:
                w = wave.open(str(args.output/(name+'.wav')), 'wb')
                w.setparams((channels, 2, rate, 0, 'NONE', 'not compressed'))
                outputs.append(w)
            def blocks():
                while True:
                    raw = src.readframes(1024)
                    if not raw:
                        break
                    yield np.frombuffer(raw, '<i2').reshape(-1, channels).T.astype(np.float32)/32768
            def save(y):
                for w, stem in zip(outputs, y):
                    w.writeframesraw(np.rint(np.clip(stem.T, -1, 32767/32768)*32768).astype('<i2').tobytes())
            print(json.dumps(run([args.client], blocks(), channels, rate, save)))
        finally:
            for w in outputs:
                w.close()


if __name__ == '__main__':
    main()
