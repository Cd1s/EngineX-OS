#!/usr/bin/env python3
# Measure /live.wav from the receiving computer: drift against real time, the first burst, and gaps.
# usage: [BIND_IF=en0] python3 live_check.py 'http://<deck address>:8080/live.wav[?buffer=<ms>]' [seconds, default 190]
# BIND_IF (macOS) pins the test to one network interface, e.g. Wi-Fi while a cable is also connected.
import os, socket, sys, time, urllib.parse

u = urllib.parse.urlparse(sys.argv[1])
dur = float(sys.argv[2]) if len(sys.argv) > 2 else 190.0
bif = os.environ.get("BIND_IF")
s = socket.socket()
if bif: s.setsockopt(socket.IPPROTO_IP, 25, socket.if_nametoindex(bif))  # IP_BOUND_IF on macOS
s.settimeout(20)  # a silently dropped connection must not hang the test
s.connect((u.hostname, u.port or 80))
s.sendall(("GET %s HTTP/1.1\r\nHost: %s\r\n\r\n" % (u.path + ("?" + u.query if u.query else ""), u.hostname)).encode())
head = b""
while b"\r\n\r\n" not in head:
    head += s.recv(4096)
t0 = last = time.monotonic()
n = max(0, len(head.split(b"\r\n\r\n", 1)[1]) - 44)  # audio bytes after the 44-byte WAV header
first, gaps, env = None, [], {}
while time.monotonic() - t0 < dur:
    d = s.recv(65536)
    if not d:
        break
    t = time.monotonic()
    n += len(d)
    if first is None and t - t0 > 0.02:
        first = (n - len(d)) / 192.0  # ms of audio that arrived in the first 20 ms
    gaps.append(t - last)
    last = t
    b = int((t - t0) // 10)
    env[b] = max(env.get(b, -1e9), n / 192.0 - (t - t0) * 1000)  # ms of audio ahead of real time
gaps.sort()
ks = sorted(env)
print("first 20 ms brought %.0f ms of audio (the ?buffer= cushion plus up to 20 ms of live audio)" % (first or 0))
print("on-time envelope, ms ahead of real time per 10 s:", " ".join("%.0f" % env[k] for k in ks))
if len(ks) >= 6:
    a, b = ks[2], ks[-2]
    print("drift %.0f ppm (from %d s to %d s)" % ((env[b] - env[a]) / ((b - a) * 10.0) * 1000, a * 10, b * 10))
print("gap between receives: p50 %.1f ms, p99 %.1f ms, max %.1f ms; over 60 ms: %d, over 120 ms: %d" % (
    gaps[len(gaps) // 2] * 1000, gaps[int(len(gaps) * 0.99)] * 1000, gaps[-1] * 1000,
    sum(g > 0.06 for g in gaps), sum(g > 0.12 for g in gaps)))
