#!/usr/bin/env python3
"""Live dual-unit Engine Link on-demand rekordbox matrix check (7 cases).

Verifies Layer 4a on the real SC5000 5.3.0 twin (A holds USB sticks, B loads over Engine Link):
  1. Cold Link load with 4-deck shadow ON (instant PQTZ grid, cues kept, 0 B OA, A filled.jsonl)
  2. Pre-filled on A before B opens remote source (0 B->A on-demand LAN calls, 0 B OA)
  3. Filled on A after B browses remote source (0 B->A on-demand LAN calls, 0 B OA)
  4. Cold Link load with 4-deck shadow OFF / 2-deck mode (/proc/net/tcp peer discovery, 0 B OA)
  5. Stock Engine stick (pdbImportKey IS NULL: 0 B->A on-demand LAN calls, stock OA unchanged)
  6. Unit A not answering (750/800 ms answer budget -> clean stock Engine fallback, no hang)
  7. Older/non-EngineX peer (plain 404 -> cached peer_unsupported -> 2nd load makes 0 LAN calls)

Also checks browse-time LAN cost == 0, and supports --base-only to prove Case 1 fails on 83627d2c.
"""

from __future__ import annotations

import argparse
import http.server
import json
import math
import os
import shutil
import socket
import socketserver
import sqlite3
import struct
import subprocess
import sys
import threading
import time
import wave
import zlib
from contextlib import closing
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import twin


def now_ms() -> int:
    return int(time.time() * 1000)


def make_click_wav(path: Path, seconds: float = 15.0, bpm: float = 128.0, sr: int = 44100) -> int:
    """Synthesize a percussive WAV whose audio beat 0 is at 0.000 ms (distinct from PQTZ b0 >= 250 ms)."""
    n = int(round(seconds * sr))
    step = sr * 60.0 / bpm
    buf = bytearray(n * 4)
    for i in range(n):
        phase = i % step
        val = 0.0
        if phase < 2200:
            t = phase / sr
            env = math.exp(-t * 45.0)
            val = 0.75 * env * math.sin(2.0 * math.pi * (120.0 * math.exp(-t * 25.0) + 45.0) * t)
            if phase < 90:
                val += 0.25 * (1.0 - phase / 90.0)
        half = (i - step / 2.0) % step
        if 0 <= half < 400:
            val += 0.12 * math.exp(-(half / sr) * 160.0) * (1.0 if (i % 7 < 3) else -1.0)
        s = max(-32767, min(32767, int(val * 30000)))
        struct.pack_into("<hh", buf, i * 4, s, s)
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(2)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(bytes(buf))
    return path.stat().st_size


def build_pioneer_section(tag: bytes, hdr_len: int, body: bytes) -> bytes:
    return tag + struct.pack(">II", hdr_len, 12 + len(body)) + body


def build_pmai(*sections: bytes) -> bytes:
    body = b"".join(sections)
    return b"PMAI" + struct.pack(">II", 28, 28 + len(body)) + b"\x00" * 16 + body


def build_anlz(bpm: float, duration_s: float, b0_ms: float, cue_name: str):
    step_ms = 60000.0 / bpm
    n_beats = max(8, int((duration_s * 1000.0 - b0_ms) / step_ms))
    pqtz = bytearray(12)
    struct.pack_into(">II", pqtz, 4, 0x80000, n_beats)
    for i in range(n_beats):
        cur_bpm = bpm if i < 16 else bpm + 0.25
        t_ms = b0_ms + (i * step_ms if i < 16 else 16 * step_ms + (i - 16) * (60000.0 / cur_bpm))
        pqtz += struct.pack(">HHI", (i % 4) + 1, int(round(cur_bpm * 100)), int(round(t_ms)))
    pqtz_sec = build_pioneer_section(b"PQTZ", 24, bytes(pqtz))

    def pcpt(hot: int, ms: int) -> bytes:
        b = bytearray(56)
        b[0:4] = b"PCPT"
        struct.pack_into(">III", b, 4, 28, 56, hot)
        b[28] = 1
        struct.pack_into(">I", b, 32, ms)
        return bytes(b)

    hot = bytearray(12)
    struct.pack_into(">I", hot, 0, 1)
    struct.pack_into(">H", hot, 6, 1)
    hot += pcpt(1, 1000)
    pcob_sec = build_pioneer_section(b"PCOB", 24, bytes(hot))

    name_u16 = (cue_name + "\x00").encode("utf-16-be")
    pcp2 = bytearray(48 + len(name_u16))
    pcp2[0:4] = b"PCP2"
    struct.pack_into(">III", pcp2, 4, 16, len(pcp2), 1)
    pcp2[16] = 1
    struct.pack_into(">II", pcp2, 20, 1000, 0)
    struct.pack_into(">I", pcp2, 40, len(name_u16))
    pcp2[44 : 44 + len(name_u16)] = name_u16
    q = 44 + len(name_u16)
    pcp2[q : q + 4] = bytes((7, 12, 34, 56))
    pco2 = bytearray(8)
    struct.pack_into(">IH", pco2, 0, 1, 1)
    pco2 += pcp2
    pco2_sec = build_pioneer_section(b"PCO2", 20, bytes(pco2))

    entries = max(150, int(round(duration_s * 150)))
    wave_b = bytearray(12 + 3 * entries)
    struct.pack_into(">III", wave_b, 0, 3, entries, 0)
    for i in range(entries):
        wave_b[12 + 3 * i : 12 + 3 * i + 3] = bytes((180 if i % 2 == 0 else 80, 120, 60))
    pwv7_sec = build_pioneer_section(b"PWV7", 24, bytes(wave_b))
    return build_pmai(pqtz_sec, pcob_sec), build_pmai(pco2_sec), build_pmai(pwv7_sec)


def build_pdb(track_specs) -> bytes:
    page_size = 4096

    def dstr(s: str) -> bytes:
        u = s.encode("utf-16-le")
        b = bytearray(4 + len(u))
        b[0] = 0x90
        struct.pack_into("<H", b, 1, len(b))
        b[4:] = u
        return bytes(b)

    track_rows, entry_rows = [], []
    for idx, (tid, title, rel_path, anlz_path, bpm, duration_s, wav_size) in enumerate(track_specs, 1):
        b = bytearray(136)
        struct.pack_into("<I", b, 8, 44100)
        struct.pack_into("<I", b, 16, wav_size)
        struct.pack_into("<I", b, 48, 128)
        struct.pack_into("<I", b, 56, int(round(bpm * 100)))
        struct.pack_into("<I", b, 72, tid)
        struct.pack_into("<H", b, 84, int(round(duration_s)))
        for j in range(21):
            s = ""
            if j == 14:
                s = anlz_path
            elif j == 17:
                s = title
            elif j == 20:
                s = "/" + rel_path.lstrip("/")
            struct.pack_into("<H", b, 94 + 2 * j, len(b))
            b += dstr(s)
        track_rows.append(bytes(b))
        entry_rows.append(struct.pack("<III", idx, tid, 1))
    pl = bytearray(20)
    struct.pack_into("<IIIII", pl, 0, 0, 0, 1, 1, 0)
    pl += dstr("Link Matrix")
    tables = [(0, track_rows), (7, [bytes(pl)]), (8, entry_rows)]
    out = bytearray(page_size)
    struct.pack_into("<II", out, 4, page_size, len(tables))
    for ti, (typ, rows) in enumerate(tables):
        first = len(out) // page_size
        left = list(rows)
        while left:
            num = len(out) // page_size
            p = bytearray(page_size)
            struct.pack_into("<II", p, 4, num, typ)
            p[27] = 0x24
            off, count = 40, 0
            while count < len(left) and off + len(left[count]) + ((count + 16) // 16) * 36 <= page_size:
                r = left[count]
                p[off : off + len(r)] = r
                base = page_size - (count // 16) * 36
                struct.pack_into("<H", p, base - 6 - 2 * (count % 16), off - 40)
                flags = struct.unpack_from("<H", p, base - 4)[0]
                struct.pack_into("<H", p, base - 4, flags | (1 << (count % 16)))
                off += len(r)
                count += 1
            struct.pack_into("<H", p, 24, count)
            struct.pack_into("<H", p, 30, off - 40)
            left = left[count:]
            if left:
                struct.pack_into("<I", p, 12, num + 1)
            out += p
        struct.pack_into("<IIII", out, 28 + 16 * ti, typ, 0, first, len(out) // page_size - 1)
    struct.pack_into("<I", out, 12, len(out) // page_size)
    return bytes(out)


def parse_beat_blob(blob: bytes | None):
    if not blob:
        return None
    raw = zlib.decompress(blob[4:])
    sr, samples = struct.unpack_from(">dd", raw, 0)
    is_set = raw[16]
    n_def = struct.unpack_from(">Q", raw, 17)[0]
    first_ms = None
    if n_def > 0 and len(raw) >= 25 + 24:
        pos, _, _, _ = struct.unpack_from("<dqII", raw, 25)
        first_ms = round(pos / sr * 1000.0, 3) if sr else 0.0
    return {"sr": sr, "samples": samples, "is_set": is_set, "n_def": n_def, "b0_ms": first_ms}


def read_mdb_row(mdb: Path, filename: str):
    with closing(sqlite3.connect(mdb.as_uri() + "?mode=ro", uri=True)) as conn:
        r = conn.execute(
            "SELECT t.id, t.isAnalyzed, t.pdbImportKey, p.thirdPartySourceId, "
            "length(p.trackData), length(p.overviewWaveFormData), length(p.beatData), length(p.quickCues), p.beatData "
            "FROM Track t LEFT JOIN PerformanceData p ON t.id = p.trackId WHERE t.filename = ? ORDER BY t.id DESC LIMIT 1",
            (filename,),
        ).fetchone()
    if not r:
        return None
    return {
        "id": r[0],
        "analyzed": r[1],
        "importKey": r[2],
        "thirdParty": r[3],
        "td": r[4] or 0,
        "ov": r[5] or 0,
        "bd": r[6] or 0,
        "qc": r[7] or 0,
        "beat": parse_beat_blob(r[8]),
    }


def deck_info(side: str, d: int):
    return twin.evj(
        side,
        f'(() => {{ const g = p => root.rtTr("/Engine/Deck{d}" + p) || {{}};'
        f' return JSON.stringify({{'
        f' loaded: !!g("/Track/SongLoaded").state,'
        f' analyzed: !!g("/Track/SongAnalyzed").state,'
        f' fn: g("/Track/FileName").string || "",'
        f' net: g("/Track/TrackNetworkPath").string || "",'
        f' bpm: g("/OriginalBPM").unnormalized || 0,'
        f' cue1: g("/Track/QuickCue1Name").string || "",'
        f' cue1pos: g("/Track/QuickCue1Position").value || 0,'
        f' sr: g("/Track/SampleRate").value || 0'
        f' }}); }})()',
    )


def wait_for(fn, timeout: float = 45.0, interval: float = 0.1, label: str = "cond"):
    dl = time.monotonic() + timeout
    while time.monotonic() < dl:
        r = fn()
        if r:
            return r
        time.sleep(interval)
    raise AssertionError(f"timeout waiting for {label}")


def oa_cpu_ticks(side: str) -> int:
    """Sum utime+stime ticks across OfflineAnalyzer threads of side's Engine process."""
    out = subprocess.check_output(
        twin.ns_chroot_cmd(
            side,
            "EPID=$(pgrep -f '^/usr/Engine/Engine' | head -n1); "
            "tot=0; "
            "for f in /proc/$EPID/task/*/comm; do "
            "  [ -r \"$f\" ] || continue; "
            "  name=$(cat \"$f\" 2>/dev/null || true); "
            "  case \"$name\" in OfflineAnalyzer*) "
            "    st=$(cat \"${f%/comm}/stat\" 2>/dev/null || true); "
            "    rest=${st#*) }; "
            "    set -- $rest; "
            "    tot=$((tot + ${12:-0} + ${13:-0})); "
            "  ;; esac; "
            "done; echo $tot",
        ),
        text=True,
    ).strip()
    return int(out or "0")


def start_lan_counter(netns: str, log_file: Path):
    """Capture B<->A HTTP POST /peer/rekordbox/ requests on eth0."""
    code = r"""
import socket, struct, sys, time
out = open(sys.argv[1], "w", buffering=1)
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0003))
s.bind(("eth0", 0))
while True:
    pkt, _ = s.recvfrom(65535)
    if len(pkt) < 34: continue
    if struct.unpack_from("!H", pkt, 12)[0] != 0x0800: continue
    if pkt[23] != 6: continue
    ihl = (pkt[14] & 0x0f) * 4
    ip_len = struct.unpack_from("!H", pkt, 16)[0]
    off = 14 + ihl
    if len(pkt) < off + 20: continue
    sp, dp, _, _, doff_fl = struct.unpack_from("!HHIIH", pkt, off)
    if dp != 8080 and sp != 8080: continue
    thl = ((doff_fl >> 12) & 0xf) * 4
    pay = pkt[off + thl : 14 + ip_len]
    if b"/peer/rekordbox/" in pay or b"X-RB-Status:" in pay:
        first = pay.split(b"\r\n", 1)[0].decode("latin1", "replace")
        out.write(f"{int(time.time()*1000)} {sp}->{dp} {first}\n")
"""
    return subprocess.Popen(["ip", "netns", "exec", netns, "python3", "-c", code, str(log_file)])


def count_peer_reqs(log_file: Path, route: str = "/peer/rekordbox/track-data") -> int:
    if not log_file.is_file():
        return 0
    return sum(1 for ln in log_file.read_text(errors="replace").splitlines() if f"POST {route} " in ln)


def read_filled_ids(stage_file: Path) -> set[int]:
    if not stage_file.is_file():
        return set()
    ids = set()
    for ln in stage_file.read_text(errors="replace").splitlines():
        ln = ln.strip()
        if not ln:
            continue
        try:
            obj = json.loads(ln)
            if "id" in obj and obj.get("wr"):
                ids.add(int(obj["id"]))
        except Exception:
            pass
    return ids


def start_side_custom(side: str, src_dir: Path, extra_args: str = ""):
    sdir = twin.WORK / side
    for f in ("shadow-twin-sync", "stream-sync", "alsa-null-fixture.so", "alsa-clock.txt"):
        (sdir / "tmp" / f).unlink(missing_ok=True)
    hook_so = src_dir / "rootfs-overlay/etc/enginex/libengine-ui-hook.so"
    shutil.copy2(hook_so, sdir / "tmp/libengine-ui-hook.so")
    subprocess.run(
        twin.ns_chroot_cmd(side, "cp -f /tmp/libengine-ui-hook.so /etc/enginex/libengine-ui-hook.so && chmod 755 /etc/enginex/libengine-ui-hook.so"),
        check=True,
    )
    shutil.copy2(twin.WORK / "bin/enginex-suite", sdir / "tmp/shadow-twin-sync")
    (sdir / "tmp/stream-sync").symlink_to("shadow-twin-sync")
    shutil.copy2(twin.WORK / "bin/alsa-null-fixture.so", sdir / "tmp/alsa-null-fixture.so")
    (sdir / "tmp/home").mkdir(parents=True, exist_ok=True)
    (sdir / "tmp/ui-eval.js").write_text("noop\nvoid 0;\n")
    (sdir / "tmp/engine_runguard.lock").unlink(missing_ok=True)
    subprocess.run(
        twin.ns_chroot_cmd(side, "touch /run/enginex-rbx-analysis-trace && /etc/enginex/engine-ui-guard pre"),
        check=True,
    )
    sync_log = open(sdir / "logs/stream-sync.log", "w")
    proc_sync = subprocess.Popen(
        twin.ns_chroot_cmd(side, "exec /tmp/stream-sync --deemixd='' --disable-guard --listen :8080"),
        stdout=sync_log,
        stderr=subprocess.STDOUT,
    )
    (sdir / "sync.pid").write_text(str(proc_sync.pid))
    cfg = twin.SIDE_CFG[side]
    for _ in range(50):
        r = subprocess.run(["ip", "netns", "exec", cfg["netns"], "curl", "-sf", "http://127.0.0.1:8080/v1/enginex/ui"], capture_output=True)
        if r.returncode == 0:
            break
        time.sleep(0.1)
    else:
        raise RuntimeError(f"stream-sync on {side} failed to listen")

    eng_log = open(sdir / "logs/engine.log", "w")
    eng_cmd = (
        "exec /usr/bin/env "
        "HOME=/tmp/home "
        "LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib "
        "LD_PRELOAD=/tmp/alsa-null-fixture.so:/etc/enginex/libengine-ui-hook.so "
        "SBX_CARD=JP07 SBX_OUT=4 SBX_IN=2 SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 "
        "QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms "
        "LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2 "
        "QML_DISABLE_DISK_CACHE=1 QML_XHR_ALLOW_FILE_READ=1 MALLOC_ARENA_MAX=1 "
        f"/usr/Engine/Engine -d0 {extra_args}"
    )
    proc_eng = subprocess.Popen(twin.ns_chroot_cmd(side, eng_cmd), stdout=eng_log, stderr=subprocess.STDOUT)
    (sdir / "engine.pid").write_text(str(proc_eng.pid))


def restart_stream_sync(side: str):
    sdir = twin.WORK / side
    subprocess.run(twin.ns_chroot_cmd(side, "pkill -TERM -f '/tmp/stream-sync|shadow-twin-sync' 2>/dev/null || true"), check=False)
    time.sleep(0.4)
    subprocess.run(twin.ns_chroot_cmd(side, "pkill -KILL -f '/tmp/stream-sync|shadow-twin-sync' 2>/dev/null || true"), check=False)
    (sdir / "run/enginex/rb-ondemand.sock").unlink(missing_ok=True)
    sync_log = open(sdir / "logs/stream-sync.log", "a")
    proc = subprocess.Popen(
        twin.ns_chroot_cmd(side, "exec /tmp/stream-sync --deemixd='' --disable-guard --listen :8080"),
        stdout=sync_log,
        stderr=subprocess.STDOUT,
    )
    (sdir / "sync.pid").write_text(str(proc.pid))
    cfg = twin.SIDE_CFG[side]
    for _ in range(50):
        r = subprocess.run(["ip", "netns", "exec", cfg["netns"], "curl", "-sf", "http://127.0.0.1:8080/v1/enginex/ui"], capture_output=True)
        if r.returncode == 0:
            break
        time.sleep(0.1)
    else:
        raise RuntimeError(f"restarted stream-sync on {side} failed to listen")


def list_sources(side: str):
    return twin.evj(
        side,
        '(() => {'
        ' const bsc = Planck.librarianController.browserSourceControllerPtr();'
        ' const m = bsc.connectedSourceModel();'
        ' const out = [];'
        ' for (let i = 0; i < m.rowCount(); i++) {'
        '   const idx = m.index(i, 0);'
        '   const vals = [];'
        '   for (const r of [0, 256, 257, 258, 259, 260, 261, 262, 263, 264, 265, 266, 267, 268, 269, 270]) {'
        '     try { const v = m.data(idx, r); if (v !== undefined && v !== null && String(v) !== "" && String(v) !== "false") vals.push(r + ":" + String(v).slice(0, 40)); } catch(e) {}'
        '   }'
        '   out.push({i: i, vals: vals});'
        ' }'
        ' return JSON.stringify(out);'
        '})()',
    )


def browse_rows(side: str):
    return twin.evj(
        side,
        '(() => {'
        ' const cc = Planck.librarianController.currentCollectionController;'
        ' const cm = cc ? cc.collectionModel : null;'
        ' const n = cm ? cm.rowCount() : 0;'
        ' const rows = [];'
        ' for (let i = 0; i < n && i < 20; i++) {'
        '   const idx = cm.index(i, 0);'
        '   const vals = [];'
        '   for (const r of [0, 256, 257, 258, 259, 260, 261, 262, 263, 264, 265, 266, 267, 268, 269, 270]) {'
        '     try { const v = cm.data(idx, r); if (v !== undefined && v !== null && String(v) !== "" && String(v) !== "false" && String(v) !== "0") vals.push(r + ":" + String(v).slice(0, 40)); } catch(e) {}'
        '   }'
        '   rows.push({i: i, vals: vals});'
        ' }'
        ' return JSON.stringify({who: cc ? cc.who() : "", n: n, rows: rows});'
        '})()',
    )


def select_source(side: str, volume_name: str, min_rows: int):
    srcs = wait_for(
        lambda: (s := list_sources(side)) and any(volume_name in json.dumps(x) for x in s) and s,
        timeout=25,
        interval=0.4,
        label=f"{side} sees {volume_name}",
    )
    idx = next(x["i"] for x in srcs if volume_name in json.dumps(x))
    twin.ev(
        side,
        f'const bsc = Planck.librarianController.browserSourceControllerPtr(); '
        f'const m = bsc.connectedSourceModel(); '
        f'bsc.select(m.index({idx}, 0)); '
        f'root.rtTr("/GUI/ShowBrowseView").state = true; "sel"',
    )
    return wait_for(
        lambda: (b := browse_rows(side)) and b["n"] >= min_rows and b,
        timeout=25,
        interval=0.25,
        label=f"{side} browse rows for {volume_name}",
    )


def load_b_row(title_sub: str, expected_fn: str, timeout: float = 45.0):
    b = browse_rows("B")
    hit = next((r for r in b["rows"] if title_sub in json.dumps(r["vals"])), None)
    assert hit is not None, f"row {title_sub!r} not found in {b}"
    twin.ev("B", 'root.rtTr("/GUI/ShowBrowseView").state = true; "open"')
    time.sleep(0.25)
    twin.ev("B", f'root.rtTr("/Client/Librarian/{b["who"]}/CurrentIndex").unnormalized = {hit["i"]}; "idx"')
    time.sleep(0.15)
    t0 = now_ms()
    twin.ev("B", 'root.rtPress("/Client/Librarian/LoadToDeck2"); "load"')
    t_loaded = None
    t_analyzed = None
    final_state = None
    dl = time.monotonic() + timeout
    while time.monotonic() < dl:
        s = deck_info("B", 2)
        tnow = now_ms()
        if s["loaded"] and s["fn"] == expected_fn and t_loaded is None:
            t_loaded = tnow - t0
        if s["loaded"] and s["analyzed"] and s["fn"] == expected_fn:
            t_analyzed = tnow - t0
            final_state = s
            break
        time.sleep(0.05)
    assert final_state is not None, f"timed out loading {expected_fn} on B Deck 2 (last={deck_info('B', 2)})"
    return t_loaded, t_analyzed, final_state


def start_stub_http_in_a(mode: str, log_path: Path):
    """Run an HTTP stub on A's :8080 inside A's netns (mode: 'hang' or 'older404')."""
    subprocess.run(twin.ns_chroot_cmd("A", "pkill -TERM -f '/tmp/stream-sync|shadow-twin-sync' 2>/dev/null || true"), check=False)
    time.sleep(0.4)
    subprocess.run(twin.ns_chroot_cmd("A", "pkill -KILL -f '/tmp/stream-sync|shadow-twin-sync' 2>/dev/null || true"), check=False)
    stub_code = f"""
import http.server, socketserver, time
log = open({str(log_path)!r}, "w", buffering=1)
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        if n: self.rfile.read(n)
        log.write(f"{{int(time.time()*1000)}} POST {{self.path}}\\n")
        if {mode!r} == "hang":
            time.sleep(3.0)
            try:
                self.send_response(503); self.end_headers()
            except Exception: pass
        else:
            self.send_response(405)
            self.send_header("Content-Length", "18")
            self.end_headers()
            self.wfile.write(b"Method Not Allowed")
    def do_GET(self):
        self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer(("0.0.0.0", 8080), H) as srv:
    srv.serve_forever()
"""
    proc = subprocess.Popen(["ip", "netns", "exec", twin.SIDE_CFG["A"]["netns"], "python3", "-c", stub_code])
    time.sleep(0.35)
    return proc


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-only", action="store_true", help="Run only Case 1 to prove base 83627d2c fails (B runs OA)")
    args = ap.parse_args()
    assert os.environ.get("SHADOW_TWIN_WORK"), "set a fresh SHADOW_TWIN_WORK"

    src_dir, ver_info = twin.prepare_build_source(ref=None, fetch=False, local=True)
    twin.ensure_base_artifacts(src_dir, ver_info, build_suite=True, build_packs=True, build_hook=True)
    twin.setup_network(delay_ms=2.0, jitter_ms=1.1)
    for s in twin.SIDES:
        twin.write_instance_configs(s, "auto", src_dir, ver_info)
        twin.start_namespace_holder(s, src_dir)

    # 1. Mount /media/RB_ONDEMAND on Unit A (7 tracks: cases 1,2,3,4,6,7a,7b)
    rb_vol = twin.WORK / "A/tmp/vols/RB_ONDEMAND"
    rb_vol.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        twin.ns_chroot_cmd("A", "mkdir -p /tmp/vols/RB_ONDEMAND /media/RB_ONDEMAND && mount --bind /tmp/vols/RB_ONDEMAND /media/RB_ONDEMAND"),
        check=True,
    )
    rb_tracks = [
        (1, "RB Case1 Cold4D",   "Contents/RBOD/case1-cold4d.wav",   "/PIONEER/USBANLZ/P001/ANLZ0000.DAT", 128.0, 20.0, 250.0, "Case1Cue"),
        (2, "RB Case2 Prefill",  "Contents/RBOD/case2-prefill.wav",  "/PIONEER/USBANLZ/P002/ANLZ0000.DAT", 128.0, 14.0, 260.0, "Case2Cue"),
        (3, "RB Case3 Latefill", "Contents/RBOD/case3-latefill.wav", "/PIONEER/USBANLZ/P003/ANLZ0000.DAT", 128.0, 14.0, 270.0, "Case3Cue"),
        (4, "RB Case4 Cold2D",   "Contents/RBOD/case4-cold2d.wav",   "/PIONEER/USBANLZ/P004/ANLZ0000.DAT", 128.0, 14.0, 280.0, "Case4Cue"),
        (5, "RB Case6 HangA",    "Contents/RBOD/case6-hang.wav",     "/PIONEER/USBANLZ/P005/ANLZ0000.DAT", 128.0, 10.0, 290.0, "Case6Cue"),
        (6, "RB Case7a Older1",  "Contents/RBOD/case7a-older1.wav",  "/PIONEER/USBANLZ/P006/ANLZ0000.DAT", 128.0, 10.0, 300.0, "Case7aCue"),
        (7, "RB Case7b Older2",  "Contents/RBOD/case7b-older2.wav",  "/PIONEER/USBANLZ/P007/ANLZ0000.DAT", 128.0, 10.0, 310.0, "Case7bCue"),
    ]
    pdb_specs = []
    for tid, title, rel, anlz, bpm, dur, b0_ms, cue in rb_tracks:
        sz = make_click_wav(rb_vol / rel, seconds=dur, bpm=bpm)
        dat, ext, ex2 = build_anlz(bpm, dur, b0_ms, cue)
        adir = rb_vol / Path(anlz.lstrip("/")).parent
        adir.mkdir(parents=True, exist_ok=True)
        (adir / "ANLZ0000.DAT").write_bytes(dat)
        (adir / "ANLZ0000.EXT").write_bytes(ext)
        (adir / "ANLZ0000.2EX").write_bytes(ex2)
        pdb_specs.append((tid, title, rel, anlz, bpm, dur, sz))
    pdb_dir = rb_vol / "PIONEER/rekordbox"
    pdb_dir.mkdir(parents=True, exist_ok=True)
    (pdb_dir / "export.pdb").write_bytes(build_pdb(pdb_specs))

    shutil.copy2(twin.WORK / "bin/enginex-suite", twin.WORK / "A/tmp/rb-bridge-bin")
    subprocess.run(
        twin.ns_chroot_cmd("A", "ln -sf rb-bridge-bin /tmp/rb-bridge && ENGINEX_RB_ONDEMAND=1 /tmp/rb-bridge --mount-path /media/RB_ONDEMAND >/tmp/rb-mount.log 2>&1"),
        check=True,
    )
    rb_mdb = rb_vol / "Engine Library/Database2/m.db"
    with closing(sqlite3.connect(rb_mdb.as_uri() + "?mode=ro", uri=True)) as conn:
        rb_uuid = conn.execute("SELECT uuid FROM Information LIMIT 1").fetchone()[0]
    a_filled_path = twin.WORK / "A/data/enginex/rb-stage" / rb_uuid / "filled.jsonl"

    # 2. Mount /media/STOCK_USB on Unit A (stock Engine stick: pdbImportKey IS NULL, no bridge snapshot)
    stock_vol = twin.WORK / "A/tmp/vols/STOCK_USB"
    stock_vol.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        twin.ns_chroot_cmd("A", "mkdir -p /tmp/vols/STOCK_USB /media/STOCK_USB && mount --bind /tmp/vols/STOCK_USB /media/STOCK_USB"),
        check=True,
    )
    stock_mdb = stock_vol / "Engine Library/Database2/m.db"
    stock_mdb.parent.mkdir(parents=True, exist_ok=True)
    stock_mdb.unlink(missing_ok=True)
    make_click_wav(stock_vol / "Music/stock-track.wav", seconds=10.0, bpm=128.0)
    schema_sql = (src_dir / "pkg/enginedb/schema.sql").read_text()
    with closing(sqlite3.connect(rb_mdb.as_uri() + "?mode=ro", uri=True)) as src_conn:
        info_row = src_conn.execute(
            "SELECT schemaVersionMajor, schemaVersionMinor, schemaVersionPatch FROM Information LIMIT 1"
        ).fetchone()
    with closing(sqlite3.connect(stock_mdb)) as conn:
        conn.executescript(schema_sql)
        conn.execute(
            "INSERT INTO Information(uuid, schemaVersionMajor, schemaVersionMinor, schemaVersionPatch) VALUES(?, ?, ?, ?)",
            ("stock-engine-uuid-1111", info_row[0], info_row[1], info_row[2]),
        )
        conn.execute(
            "INSERT INTO Track(path, filename, title, bpm, length, pdbImportKey, isAnalyzed) "
            "VALUES('../Music/stock-track.wav', 'stock-track.wav', 'Stock Track', 128, 10, NULL, 0)"
        )
        conn.commit()

    lan_log = twin.WORK / "lan-peer-reqs.log"
    sniffer = start_lan_counter(twin.SIDE_CFG["B"]["netns"], lan_log)
    stub_proc = None
    results = []
    try:
        start_side_custom("A", src_dir, extra_args="-extraDriveDir=/media/RB_ONDEMAND -extraDriveDir=/media/STOCK_USB")
        start_side_custom("B", src_dir, extra_args="")
        twin.both(twin.wait_engine_ready)
        time.sleep(2.5)

        # Pre-fill Case 2 track on Unit A BEFORE Unit B opens the remote source.
        if not args.base_only:
            twin.ev("A", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url="file:///media/RB_ONDEMAND/Contents/RBOD/case2-prefill.wav"; "ok"')
            wait_for(lambda: (s := deck_info("A", 1))["loaded"] and s["analyzed"] and s["fn"] == "case2-prefill.wav" and s, timeout=30, label="A prefill case2")
            time.sleep(0.8)
            twin.ev("A", 'var t=root.rtTr("/Engine/Deck1/Track/EjectTrack"); if(t) t.state=true; "eject"')
            time.sleep(1.0)

        # Browse RB_ONDEMAND on B and verify 0 LAN on-demand calls during browse!
        lan_before_browse = count_peer_reqs(lan_log)
        select_source("B", "RB_ONDEMAND", min_rows=7)
        time.sleep(0.5)
        lan_browse_delta = count_peer_reqs(lan_log) - lan_before_browse
        assert lan_browse_delta == 0, f"browse triggered {lan_browse_delta} LAN /peer/rekordbox/track-data calls"
        print(f"BROWSE CHECK: 0 LAN on-demand calls (PASS)", flush=True)

        # CASE 1: Cold Link load, 4-deck shadow ON
        oa_b0, oa_a0 = oa_cpu_ticks("B"), oa_cpu_ticks("A")
        lan0 = count_peer_reqs(lan_log)
        t_ld, t_an, st = load_b_row("RB Case1", "case1-cold4d.wav")
        time.sleep(1.5)
        st = deck_info("B", 2)
        oa_b_d = oa_cpu_ticks("B") - oa_b0
        oa_a_d = oa_cpu_ticks("A") - oa_a0
        lan_d = count_peer_reqs(lan_log) - lan0
        row1 = read_mdb_row(rb_mdb, "case1-cold4d.wav")
        filled_ids = read_filled_ids(a_filled_path)
        err1 = abs((row1["beat"]["b0_ms"] or 0.0) - 250.0) if row1 and row1["beat"] else 999.0
        rec1 = {
            "case": "1. Cold Link (shadow ON)",
            "b_oa_ticks": oa_b_d,
            "a_oa_ticks": oa_a_d,
            "lan_reqs": lan_d,
            "load_ms": t_ld,
            "analyzed_ms": t_an,
            "b0_ms": row1["beat"]["b0_ms"] if row1 and row1["beat"] else None,
            "pqtz_err_ms": round(err1, 3),
            "n_def": row1["beat"]["n_def"] if row1 and row1["beat"] else 0,
            "cue1": st["cue1"],
            "cue1pos": st["cue1pos"],
            "qc": row1["qc"] if row1 else 0,
            "cue_kept": st["cue1"] == "Case1Cue" and abs(st["cue1pos"] - 44100) <= 5 and row1["qc"] > 26,
            "filled_jsonl": 1 in filled_ids,
        }
        results.append(rec1)
        print("CASE 1:", json.dumps(rec1), flush=True)
        assert oa_b_d < 15, f"Case 1 ran B OfflineAnalyzer (ticks={oa_b_d})"
        assert err1 <= 0.15 and rec1["n_def"] >= 20, f"Case 1 grid mismatch vs PQTZ: {rec1}"
        assert rec1["cue_kept"], f"Case 1 lost rekordbox cue: state={st} row={row1}"
        assert rec1["filled_jsonl"], f"Case 1 missing Track 1 in A filled.jsonl ({a_filled_path})"
        assert lan_d == 1, f"Case 1 LAN reqs={lan_d}, want 1"
        if args.base_only:
            return

        # CASE 2: Pre-filled on A before B opened source -> 0 LAN reqs, 0 B OA
        oa_b0 = oa_cpu_ticks("B")
        lan0 = count_peer_reqs(lan_log)
        t_ld, t_an, st = load_b_row("RB Case2", "case2-prefill.wav")
        time.sleep(0.8)
        st = deck_info("B", 2)
        oa_b_d = oa_cpu_ticks("B") - oa_b0
        lan_d = count_peer_reqs(lan_log) - lan0
        row2 = read_mdb_row(rb_mdb, "case2-prefill.wav")
        err2 = abs((row2["beat"]["b0_ms"] or 0.0) - 260.0) if row2 and row2["beat"] else 999.0
        rec2 = {
            "case": "2. Pre-filled on A",
            "b_oa_ticks": oa_b_d,
            "lan_reqs": lan_d,
            "load_ms": t_ld,
            "analyzed_ms": t_an,
            "b0_ms": row2["beat"]["b0_ms"] if row2 and row2["beat"] else None,
            "pqtz_err_ms": round(err2, 3),
            "n_def": row2["beat"]["n_def"] if row2 and row2["beat"] else 0,
            "cue1": st["cue1"],
            "cue1pos": st["cue1pos"],
            "qc": row2["qc"] if row2 else 0,
            "cue_kept": st["cue1"] == "Case2Cue" and abs(st["cue1pos"] - 44100) <= 5 and row2["qc"] > 26,
            "filled_jsonl": 2 in read_filled_ids(a_filled_path),
        }
        results.append(rec2)
        print("CASE 2:", json.dumps(rec2), flush=True)
        assert lan_d == 0 and oa_b_d < 15 and err2 <= 0.15 and rec2["cue_kept"], f"Case 2 failed: {rec2}"

        # CASE 3: Filled on A AFTER B browsed -> 0 LAN reqs, 0 B OA
        twin.ev("A", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url="file:///media/RB_ONDEMAND/Contents/RBOD/case3-latefill.wav"; "ok"')
        wait_for(lambda: (s := deck_info("A", 1))["loaded"] and s["analyzed"] and s["fn"] == "case3-latefill.wav" and s, timeout=30, label="A latefill case3")
        time.sleep(0.8)
        twin.ev("A", 'var t=root.rtTr("/Engine/Deck1/Track/EjectTrack"); if(t) t.state=true; "eject"')
        time.sleep(0.8)
        oa_b0 = oa_cpu_ticks("B")
        lan0 = count_peer_reqs(lan_log)
        t_ld, t_an, st = load_b_row("RB Case3", "case3-latefill.wav")
        time.sleep(0.8)
        st = deck_info("B", 2)
        oa_b_d = oa_cpu_ticks("B") - oa_b0
        lan_d = count_peer_reqs(lan_log) - lan0
        row3 = read_mdb_row(rb_mdb, "case3-latefill.wav")
        err3 = abs((row3["beat"]["b0_ms"] or 0.0) - 270.0) if row3 and row3["beat"] else 999.0
        rec3 = {
            "case": "3. Filled after browse",
            "b_oa_ticks": oa_b_d,
            "lan_reqs": lan_d,
            "load_ms": t_ld,
            "analyzed_ms": t_an,
            "b0_ms": row3["beat"]["b0_ms"] if row3 and row3["beat"] else None,
            "pqtz_err_ms": round(err3, 3),
            "n_def": row3["beat"]["n_def"] if row3 and row3["beat"] else 0,
            "cue1": st["cue1"],
            "cue1pos": st["cue1pos"],
            "qc": row3["qc"] if row3 else 0,
            "cue_kept": st["cue1"] == "Case3Cue" and abs(st["cue1pos"] - 44100) <= 5 and row3["qc"] > 26,
            "filled_jsonl": 3 in read_filled_ids(a_filled_path),
        }
        results.append(rec3)
        print("CASE 3:", json.dumps(rec3), flush=True)
        assert lan_d == 0 and oa_b_d < 15 and err3 <= 0.15 and rec3["cue_kept"], f"Case 3 failed: {rec3}"

        # CASE 4: Cold Link load with shadow OFF / 2-deck mode (block UDP 50199 + restart B stream-sync so shadow.peers is empty)
        twin.ev("B", 'scShadowTickTimer.stop(); var t=root.rtTr("/Engine/Deck2/Track/EjectTrack"); if(t) t.state=true; "stopped"')
        subprocess.run(["ip", "netns", "exec", twin.SIDE_CFG["B"]["netns"], "iptables", "-I", "INPUT", "-p", "udp", "--dport", "50199", "-j", "DROP"], check=True)
        subprocess.run(["ip", "netns", "exec", twin.SIDE_CFG["B"]["netns"], "iptables", "-I", "OUTPUT", "-p", "udp", "--dport", "50199", "-j", "DROP"], check=True)
        try:
            restart_stream_sync("B")
            oa_b0 = oa_cpu_ticks("B")
            lan0 = count_peer_reqs(lan_log)
            t_ld, t_an, st = load_b_row("RB Case4", "case4-cold2d.wav")
            time.sleep(1.2)
            st = deck_info("B", 2)
            oa_b_d = oa_cpu_ticks("B") - oa_b0
            lan_d = count_peer_reqs(lan_log) - lan0
            row4 = read_mdb_row(rb_mdb, "case4-cold2d.wav")
            filled_ids = read_filled_ids(a_filled_path)
            err4 = abs((row4["beat"]["b0_ms"] or 0.0) - 280.0) if row4 and row4["beat"] else 999.0
            rec4 = {
                "case": "4. Cold Link (shadow OFF / 2-deck)",
                "b_oa_ticks": oa_b_d,
                "lan_reqs": lan_d,
                "load_ms": t_ld,
                "analyzed_ms": t_an,
                "b0_ms": row4["beat"]["b0_ms"] if row4 and row4["beat"] else None,
                "pqtz_err_ms": round(err4, 3),
                "n_def": row4["beat"]["n_def"] if row4 and row4["beat"] else 0,
                "cue1": st["cue1"],
                "cue1pos": st["cue1pos"],
                "qc": row4["qc"] if row4 else 0,
                "cue_kept": st["cue1"] == "Case4Cue" and abs(st["cue1pos"] - 44100) <= 5 and row4["qc"] > 26,
                "filled_jsonl": 4 in filled_ids,
            }
            results.append(rec4)
            print("CASE 4:", json.dumps(rec4), flush=True)
            assert oa_b_d < 15 and lan_d == 1 and err4 <= 0.15 and rec4["n_def"] >= 20 and rec4["cue_kept"] and rec4["filled_jsonl"], f"Case 4 failed: {rec4}"
        finally:
            subprocess.run(["ip", "netns", "exec", twin.SIDE_CFG["B"]["netns"], "iptables", "-D", "INPUT", "-p", "udp", "--dport", "50199", "-j", "DROP"], check=False)
            subprocess.run(["ip", "netns", "exec", twin.SIDE_CFG["B"]["netns"], "iptables", "-D", "OUTPUT", "-p", "udp", "--dport", "50199", "-j", "DROP"], check=False)

        # CASE 5: Stock Engine stick (pdbImportKey IS NULL) -> 0 LAN on-demand calls, stock OA runs normally
        select_source("B", "STOCK_USB", min_rows=1)
        oa_b0 = oa_cpu_ticks("B")
        lan0 = count_peer_reqs(lan_log)
        t_ld, t_an, st = load_b_row("Stock Track", "stock-track.wav")
        time.sleep(1.5)
        oa_b_d = oa_cpu_ticks("B") - oa_b0
        lan_d = count_peer_reqs(lan_log) - lan0
        row5 = read_mdb_row(stock_mdb, "stock-track.wav")
        rec5 = {
            "case": "5. Stock Engine stick",
            "b_oa_ticks": oa_b_d,
            "lan_reqs": lan_d,
            "load_ms": t_ld,
            "analyzed_ms": t_an,
            "b0_ms": row5["beat"]["b0_ms"] if row5 and row5["beat"] else None,
            "n_def": row5["beat"]["n_def"] if row5 and row5["beat"] else 0,
            "analyzed_db": row5["analyzed"] if row5 else 0,
        }
        results.append(rec5)
        print("CASE 5:", json.dumps(rec5), flush=True)
        assert lan_d == 0 and row5["analyzed"] == 1 and oa_b_d >= 10, f"Case 5 failed: {rec5}"

        # Switch back to RB_ONDEMAND for Cases 6 & 7
        select_source("B", "RB_ONDEMAND", min_rows=7)

        # CASE 6: Unit A not answering (hung :8080 -> 750/800 ms budget -> clean stock Engine fallback)
        hang_log = twin.WORK / "stub-hang.log"
        stub_proc = start_stub_http_in_a("hang", hang_log)
        restart_stream_sync("B")
        oa_b0 = oa_cpu_ticks("B")
        lan0 = count_peer_reqs(lan_log)
        t_ld, t_an, st = load_b_row("RB Case6", "case6-hang.wav")
        time.sleep(1.5)
        oa_b_d = oa_cpu_ticks("B") - oa_b0
        lan_d = count_peer_reqs(lan_log) - lan0
        row6 = read_mdb_row(rb_mdb, "case6-hang.wav")
        rec6 = {
            "case": "6. A not answering (budget timeout)",
            "b_oa_ticks": oa_b_d,
            "lan_reqs": lan_d,
            "load_ms": t_ld,
            "analyzed_ms": t_an,
            "b0_ms": row6["beat"]["b0_ms"] if row6 and row6["beat"] else None,
            "n_def": row6["beat"]["n_def"] if row6 and row6["beat"] else 0,
            "analyzed_db": row6["analyzed"] if row6 else 0,
            "filled_jsonl": 5 in read_filled_ids(a_filled_path),
        }
        results.append(rec6)
        print("CASE 6:", json.dumps(rec6), flush=True)
        assert lan_d >= 1 and row6["analyzed"] == 1 and oa_b_d >= 10 and not rec6["filled_jsonl"], f"Case 6 failed: {rec6}"
        stub_proc.terminate()
        stub_proc.wait(timeout=3)
        stub_proc = None

        # CASE 7: Older/non-EngineX peer (plain 404 without X-RB-Status -> peer_unsupported cached -> 2nd load 0 LAN)
        old_log = twin.WORK / "stub-old.log"
        stub_proc = start_stub_http_in_a("older404", old_log)
        restart_stream_sync("B")
        oa_b0 = oa_cpu_ticks("B")
        lan0 = count_peer_reqs(lan_log)
        t_ld7a, t_an7a, _ = load_b_row("RB Case7a", "case7a-older1.wav")
        time.sleep(1.2)
        lan_d7a = count_peer_reqs(lan_log) - lan0
        oa_b_d7a = oa_cpu_ticks("B") - oa_b0
        row7a = read_mdb_row(rb_mdb, "case7a-older1.wav")

        # Second load on the SAME older peer must make 0 LAN requests (hook + Go cached peer_unsupported!)
        lan1 = count_peer_reqs(lan_log)
        oa_b1 = oa_cpu_ticks("B")
        t_ld7b, t_an7b, _ = load_b_row("RB Case7b", "case7b-older2.wav")
        time.sleep(1.2)
        lan_d7b = count_peer_reqs(lan_log) - lan1
        oa_b_d7b = oa_cpu_ticks("B") - oa_b1
        row7b = read_mdb_row(rb_mdb, "case7b-older2.wav")
        rec7 = {
            "case": "7. Older/non-EngineX peer (1st + cached 2nd)",
            "b_oa_ticks_1": oa_b_d7a,
            "b_oa_ticks_2": oa_b_d7b,
            "lan_reqs_1": lan_d7a,
            "lan_reqs_2": lan_d7b,
            "load_ms_1": t_ld7a,
            "analyzed_ms_1": t_an7a,
            "load_ms_2": t_ld7b,
            "analyzed_ms_2": t_an7b,
            "analyzed_db": (row7a["analyzed"], row7b["analyzed"]),
        }
        results.append(rec7)
        print("CASE 7:", json.dumps(rec7), flush=True)
        assert lan_d7a == 1 and lan_d7b == 0 and row7a["analyzed"] == 1 and row7b["analyzed"] == 1, f"Case 7 failed: {rec7}"

        eng_b = twin.WORK / "B/logs/engine.log"
        if eng_b.is_file():
            rblines = [ln for ln in eng_b.read_text(errors="replace").splitlines() if "rekordbox:" in ln]
            print("HOOK B LOGS (rekordbox):", flush=True)
            for ln in rblines[-30:]:
                print(" ", ln, flush=True)
        print("\n=== LINK ON-DEMAND 7-CASE MATRIX: PASS ===", flush=True)
        print(json.dumps(results, indent=2), flush=True)
    finally:
        if stub_proc is not None:
            stub_proc.terminate()
            try:
                stub_proc.wait(timeout=3)
            except Exception:
                stub_proc.kill()
        sniffer.terminate()
        try:
            sniffer.wait(timeout=2)
        except Exception:
            sniffer.kill()
        twin.cmd_down(argparse.Namespace(clean=False, purge=True), quiet=False)


if __name__ == "__main__":
    main()
