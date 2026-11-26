"""Check instant shadow analysis across every source, request time, overview height, and paused follow."""

import json
import os
import re
import shutil
import socket
import sqlite3
import struct
import subprocess
import sys
import threading
import time
import zlib
from contextlib import closing
from pathlib import Path
from urllib.parse import urlencode

sys.path.insert(0, str(Path(__file__).resolve().parent))
import twin

REQUEST_TIMES_MS = (0, 100, 1000, 3000)
SOURCES = ("stock-usb", "rb-ondemand", "rb-mdb", "prolink", "streaming")


def db_path(side, name="m.db"):
    return twin.WORK / side / "data/AppDataUser/AIR Music Technology/Engine/Engine Library/Database2" / name


def row(side, filename, analysed_only=False):
    db = db_path(side)
    if not db.is_file():
        return None
    with closing(sqlite3.connect(db.as_uri() + "?mode=ro", uri=True)) as conn:
        return conn.execute(
            "SELECT t.id,t.path,p.trackData,p.overviewWaveFormData,p.beatData,p.quickCues,p.loops, "
            "t.isAnalyzed,t.isBeatGridLocked,t.bpm,t.bpmAnalyzed "
            "FROM Track t JOIN PerformanceData p ON t.id=p.trackId WHERE t.filename=?"
            + (" AND t.isAnalyzed=1" if analysed_only else "")
            + " ORDER BY t.id DESC LIMIT 1",
            (filename,),
        ).fetchone()


def wait_for(fn, timeout=85, label="sandbox state"):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if result := fn():
            return result
        time.sleep(0.2)
    raise AssertionError(f"timed out waiting for {label}")


def analyzer_log(side):
    return subprocess.check_output(twin.ns_chroot_cmd(side, "cat /var/log/enginex-uimod-oa.log"), text=True)


def engine_logs(side):
    return "\n".join(p.read_text(errors="replace") for p in (twin.WORK / side / "logs").glob("engine*") if p.is_file())


def sync_logs(side):
    p = twin.WORK / side / "logs/stream-sync.log"
    return p.read_text(errors="replace") if p.is_file() else ""


def set_link(side, state):
    subprocess.run(["ip", "-n", twin.SIDE_CFG[side]["netns"], "link", "set", "eth0", state], check=True)


def ns_curl(side, path, method="GET", data=None, timeout=15):
    cmd = ["ip", "netns", "exec", twin.SIDE_CFG[side]["netns"], "curl", "-sS", "-m", str(timeout)]
    if method != "GET":
        cmd += ["-X", method]
    if data is not None:
        cmd += ["-H", "Content-Type: application/json", "--data-binary", data]
    cmd.append("http://127.0.0.1:8080" + path)
    return subprocess.check_output(cmd)


def quncompress(blob):
    assert blob and len(blob) > 4, f"empty compressed blob ({len(blob or b'')})"
    want_len = struct.unpack(">I", blob[:4])[0]
    raw = zlib.decompress(blob[4:])
    assert len(raw) == want_len, f"qUncompress len {len(raw)} != {want_len}"
    return raw


def qcompress(raw):
    return struct.pack(">I", len(raw)) + zlib.compress(raw)


def assert_full_height_overview(label, track_blob, overview_blob):
    raw_ov = quncompress(overview_blob)
    assert len(raw_ov) >= 27 and len(raw_ov) % 3 == 0, f"{label}: bad overview len={len(raw_ov)}"
    bins = raw_ov[24:-3]
    want = bytes((max(bins[0::3]), max(bins[1::3]), max(bins[2::3])))
    assert any(want), f"{label}: overview bins are all silent"
    got = raw_ov[-3:]
    assert got == want, f"{label}: overview band-maxima trailer {list(got)} != {list(want)} (half-height bug)"

    raw_tr = quncompress(track_blob)
    assert len(raw_tr) == 68, f"{label}: trackData len={len(raw_tr)}, want 68 (5.3.0 peaks)"
    peaks = struct.unpack(">3d", raw_tr[20:44])
    assert any(v > 0 for v in peaks), f"{label}: trackData peaks are all zero: {peaks}"


def slice_wav_seconds(src_wav: Path, dst_wav: Path, seconds: float = 3.0):
    raw = src_wav.read_bytes()
    assert raw[:4] == b"RIFF" and raw[8:12] == b"WAVE", "source WAV header invalid"
    pos = 12
    fmt_chunk = None
    data_bytes = None
    sr, channels, bps = 44100, 2, 16
    while pos + 8 <= len(raw):
        cid = raw[pos : pos + 4]
        clen = struct.unpack("<I", raw[pos + 4 : pos + 8])[0]
        body = raw[pos + 8 : pos + 8 + clen]
        if cid == b"fmt ":
            fmt_chunk = body
            _, channels, sr, _, _, bps = struct.unpack("<HHIIHH", body[:16])
        elif cid == b"data":
            data_bytes = body
        pos += 8 + clen + (clen & 1)
    assert fmt_chunk is not None and data_bytes is not None
    frame_bytes = channels * (bps // 8)
    n_frames = min(int(sr * seconds), len(data_bytes) // frame_bytes)
    pcm = data_bytes[: n_frames * frame_bytes]
    out = bytearray()
    out += b"RIFF" + struct.pack("<I", 4 + 8 + len(fmt_chunk) + 8 + len(pcm)) + b"WAVE"
    out += b"fmt " + struct.pack("<I", len(fmt_chunk)) + fmt_chunk
    out += b"data" + struct.pack("<I", len(pcm)) + pcm
    dst_wav.parent.mkdir(parents=True, exist_ok=True)
    dst_wav.write_bytes(bytes(out))
    return sr, n_frames


def make_engine_blobs(sr, samples, bpm=128.0, legacy_zero_maxima=False):
    # 68-byte trackData (or 44-byte zero-peak legacy when legacy_zero_maxima=True).
    if legacy_zero_maxima:
        raw_tr = struct.pack(">dQI", float(sr), samples, 0) + b"\x00" * 24
    else:
        raw_tr = struct.pack(">dQI3d3d", float(sr), samples, 0, 0.68, 0.40, 0.18, 0.0, 0.0, 0.0)
    # 1024-bin 3-band overview with deterministic non-zero peaks (180, 120, 60).
    bins = bytearray()
    for i in range(1024):
        bins += bytes(((180 if i % 2 == 0 else 90), (120 if i % 3 == 0 else 60), (60 if i % 5 == 0 else 30)))
    trailer = bytes((0, 0, 0)) if legacy_zero_maxima else bytes((180, 120, 60))
    raw_ov = struct.pack(">QQd", 1024, 1024, samples / 1024.0) + bytes(bins) + trailer
    # Engine beatData: >ddB + 2 grids of markers (<dqII).
    step = sr * 60.0 / bpm
    n_beats = max(2, int(samples / step))
    grid = bytearray()
    grid += struct.pack(">Q", n_beats)
    for i in range(n_beats):
        grid += struct.pack("<dqII", i * step, i, 1 if i + 1 < n_beats else 0, 0)
    raw_bt = struct.pack(">ddB", float(sr), float(samples), 1) + bytes(grid) + bytes(grid) + b"\x00"
    # Empty quickCues (8 unset slots + unset main cue) & empty loops.
    raw_qc = bytearray(struct.pack(">Q", 8))
    for _ in range(8):
        raw_qc += b"\x00" + struct.pack(">d", -1.0) + b"\x00\x00\x00\x00"
    raw_qc += struct.pack(">d", -1.0) + b"\x00" * 9
    raw_lp = b"\x00" * 8
    return qcompress(raw_tr), qcompress(raw_ov), qcompress(raw_bt), qcompress(bytes(raw_qc)), raw_lp


def build_pioneer_section(tag: bytes, hdr_len: int, payload: bytes) -> bytes:
    return tag + struct.pack(">II", hdr_len, 12 + len(payload)) + payload


def build_pmai(*sections: bytes) -> bytes:
    body = b"".join(sections)
    return b"PMAI" + struct.pack(">II", 28, 28 + len(body)) + body


def build_anlz_files(bpm=128.0, duration_s=3.0):
    step_ms = 60000.0 / bpm
    n_beats = max(4, int(duration_s * 1000.0 / step_ms))
    pqtz = bytearray(12)
    struct.pack_into(">II", pqtz, 4, 0x80000, n_beats)
    for i in range(n_beats):
        pqtz += struct.pack(">HHI", (i % 4) + 1, int(round(bpm * 100)), int(round(i * step_ms)))
    pqtz_sec = build_pioneer_section(b"PQTZ", 24, bytes(pqtz))

    def pcpt(hot, ms):
        b = bytearray(56)
        b[0:4] = b"PCPT"
        struct.pack_into(">III", b, 4, 28, 56, hot)
        b[28] = 1
        struct.pack_into(">I", b, 32, ms)
        return bytes(b)

    hot = bytearray(12)
    struct.pack_into(">I", hot, 0, 1)
    struct.pack_into(">H", hot, 6, 1)
    hot += pcpt(1, 500)
    pcob_sec = build_pioneer_section(b"PCOB", 24, bytes(hot))

    # Extended cue (PCO2 / PCP2)
    name_u16 = ("OD Cue\x00").encode("utf-16-be")
    pcp2 = bytearray(48 + len(name_u16))
    pcp2[0:4] = b"PCP2"
    struct.pack_into(">III", pcp2, 4, 16, len(pcp2), 1)
    pcp2[16] = 1
    struct.pack_into(">II", pcp2, 20, 500, 0)
    struct.pack_into(">I", pcp2, 40, len(name_u16))
    pcp2[44 : 44 + len(name_u16)] = name_u16
    q = 44 + len(name_u16)
    pcp2[q : q + 4] = bytes((7, 12, 34, 56))
    pco2 = bytearray(8)
    struct.pack_into(">IH", pco2, 0, 1, 1)
    pco2 += pcp2
    pco2_sec = build_pioneer_section(b"PCO2", 20, bytes(pco2))

    # PWV7 3-band detail (150 Hz) -> 450 entries for 3.0 s
    entries = max(150, int(round(duration_s * 150)))
    wave = bytearray(12 + 3 * entries)
    struct.pack_into(">III", wave, 0, 3, entries, 0)
    for i in range(entries):
        wave[12 + 3 * i : 12 + 3 * i + 3] = bytes((180 if i % 2 == 0 else 80, 120, 60))
    pwv7_sec = build_pioneer_section(b"PWV7", 24, bytes(wave))

    dat = build_pmai(pqtz_sec, pcob_sec)
    ext = build_pmai(pco2_sec)
    ex2 = build_pmai(pwv7_sec)
    return dat, ext, ex2, pqtz_sec, pco2_sec, pwv7_sec


def build_pdb(track_specs, wav_size):
    """Build a minimal DeviceSQL export.pdb matching internal/testfixture/large.go."""
    page_size = 4096

    def dstr(s: str) -> bytes:
        u = s.encode("utf-16-le")
        b = bytearray(4 + len(u))
        b[0] = 0x90
        struct.pack_into("<H", b, 1, len(b))
        b[4:] = u
        return bytes(b)

    track_rows = []
    entry_rows = []
    for idx, (tid, title, rel_path, bpm, duration_s) in enumerate(track_specs, 1):
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
                s = "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"
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
    pl += dstr("OnDemand Set")
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
            assert count > 0, "PDB row exceeds page"
            struct.pack_into("<H", p, 24, count)
            struct.pack_into("<H", p, 30, off - 40)
            left = left[count:]
            if left:
                struct.pack_into("<I", p, 12, num + 1)
            out += p
        h = 28 + 16 * ti
        struct.pack_into("<IIII", out, h, typ, 0, first, len(out) // page_size - 1)
    struct.pack_into("<I", out, 12, len(out) // page_size)
    return bytes(out)


def mount_tmpfs_in_a(vol_name: str):
    subprocess.run(
        twin.ns_chroot_cmd(
            "A",
            f"mkdir -p /media/{vol_name} && mountpoint -q /media/{vol_name} || mount -t tmpfs tmpfs /media/{vol_name}",
        ),
        check=True,
    )
    return twin.WORK / "A/root/media" / vol_name


def clone_empty_engine_db(src_mdb: Path, dst_mdb: Path):
    dst_mdb.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src_mdb, dst_mdb)
    for suf in ("-wal", "-shm"):
        p = Path(str(dst_mdb) + suf)
        p.unlink(missing_ok=True)
    with closing(sqlite3.connect(dst_mdb)) as conn:
        for tbl in ("PerformanceData", "Track", "PlaylistEntity", "AlbumArt"):
            try:
                conn.execute(f"DELETE FROM {tbl}")
            except sqlite3.OperationalError:
                pass
        conn.commit()


def insert_engine_track(mdb: Path, rel_path: str, fname: str, blobs, bpm=128.0, analysed=1, uri=None):
    with closing(sqlite3.connect(mdb)) as conn:
        cols = {r[1] for r in conn.execute("PRAGMA table_info(Track)")}
        if uri is not None and "uri" in cols:
            cur = conn.execute(
                "INSERT INTO Track(path,filename,title,artist,uri,bpm,bpmAnalyzed,isAnalyzed) VALUES(?,?,?,?,?,?,?,?)",
                (rel_path, fname, fname, "Test Artist", uri, int(round(bpm)), bpm if analysed else None, analysed),
            )
        else:
            cur = conn.execute(
                "INSERT INTO Track(path,filename,title,artist,bpm,bpmAnalyzed,isAnalyzed) VALUES(?,?,?,?,?,?,?)",
                (rel_path, fname, fname, "Test Artist", int(round(bpm)), bpm if analysed else None, analysed),
            )
        tid = cur.lastrowid
        conn.execute("INSERT OR IGNORE INTO PerformanceData(trackId) VALUES(?)", (tid,))
        if blobs is not None:
            conn.execute(
                "UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=? WHERE trackId=?",
                (*blobs, tid),
            )
        conn.commit()
        return tid


def update_engine_track(mdb: Path, tid: int, blobs, bpm=128.0):
    with closing(sqlite3.connect(mdb)) as conn:
        conn.execute(
            "UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=? WHERE trackId=?",
            (*blobs, tid),
        )
        conn.execute("UPDATE Track SET isAnalyzed=1, bpmAnalyzed=? WHERE id=?", (bpm, tid))
        conn.commit()


def parse_rbxa1(b: bytes):
    assert b.startswith(b"RBXA1"), f"not RBXA1: {b[:16]!r}"
    off = 5
    parts = []
    for _ in range(5):
        n = struct.unpack_from("<I", b, off)[0]
        off += 4
        parts.append(b[off : off + n])
        off += n
    return tuple(parts)


MOCK_PROLINK_SERVER = r"""
import json, socket, struct, sys, threading, time

cfg = json.loads(sys.argv[1])
tracks = {int(k): v for k, v in cfg["tracks"].items()}
blobs = {k: bytes.fromhex(v) for k, v in cfg["blobs"].items()}
delay_ids = set(cfg["delay_ids"])
delayed_once = set()
delay_lock = threading.Lock()

MAGIC = 0x872349AE

def f_num(n): return (0x11, n)
def f_str(s):
    u = (s + "\x00").encode("utf-16-be")
    return (0x26, u)
def f_blob(b): return (0x14, b)

def arg_tag(t):
    return {0x11: 6, 0x26: 2, 0x14: 3}[t]

def enc_field(f):
    t, v = f
    if t == 0x0F: return struct.pack(">BB", t, v)
    if t == 0x10: return struct.pack(">BH", t, v)
    if t == 0x11: return struct.pack(">BI", t, v)
    if t == 0x26: return struct.pack(">BI", t, len(v) // 2) + v
    return struct.pack(">BI", t, len(v)) + v

def compact(tx, kind, *args):
    out = bytearray()
    out += enc_field((0x11, MAGIC))
    out += enc_field((0x11, tx))
    out += enc_field((0x10, kind))
    out += enc_field((0x0F, len(args)))
    tags = bytes(arg_tag(a[0]) for a in args)
    out += enc_field((0x14, tags))
    for i, a in enumerate(args):
        if a[0] == 0x14 and len(a[1]) == 0 and i > 0 and args[i - 1][0] == 0x11 and args[i - 1][1] == 0:
            continue
        out += enc_field(a)
    return bytes(out)

def recv_exact(sock, n):
    buf = bytearray()
    while len(buf) < n:
        c = sock.recv(n - len(buf))
        if not c:
            raise EOFError()
        buf += c
    return bytes(buf)

def read_field(sock):
    t = recv_exact(sock, 1)[0]
    if t == 0x0F: return (t, recv_exact(sock, 1)[0])
    if t == 0x10: return (t, struct.unpack(">H", recv_exact(sock, 2))[0])
    if t == 0x11: return (t, struct.unpack(">I", recv_exact(sock, 4))[0])
    n = struct.unpack(">I", recv_exact(sock, 4))[0]
    if t == 0x26: n *= 2
    return (t, recv_exact(sock, n))

def read_msg(sock):
    h = [read_field(sock) for _ in range(5)]
    tx, kind, argc, tags = h[1][1], h[2][1], h[3][1], h[4][1]
    args = []
    for i in range(argc):
        if tags[i] == 3 and i > 0 and args[i - 1][0] == 0x11 and args[i - 1][1] == 0:
            args.append((0x14, b""))
        else:
            args.append(read_field(sock))
    return tx, kind, args

def item_fields(parent, tid, kind, label, second="", pos=0, art=0):
    a, b = f_str(label), f_str(second)
    return [f_num(parent), f_num(tid), f_num(len(a[1])), a, f_num(len(b[1])), b,
            f_num(kind), f_num(0), f_num(art), f_num(pos), f_num(0), f_num(0)]

def handle_db(conn):
    try:
        conn.sendall(recv_exact(conn, 5))
        pending = []
        while True:
            tx, kind, args = read_msg(conn)
            if kind == 0:
                conn.sendall(compact(tx, 0x4000, f_num(0), f_num(17)))
            elif kind == 0x0100:
                return
            elif kind == 0x1004:
                sort = args[1][1] if len(args) > 1 else 0
                pending = []
                for tid, info in sorted(tracks.items()):
                    if sort == 2:
                        pending.append(item_fields(0, tid, 0x0704, info["title"], "Link Artist"))
                    elif sort == 4:
                        pending.append(item_fields(12800, tid, 0x0D04, info["title"], "128.0 bpm - 8B"))
                    elif sort == 8:
                        pending.append(item_fields(3, tid, 0x0B04, info["title"], ""))
                    else:
                        pending.append(item_fields(0, tid, 4, info["title"], ""))
                conn.sendall(compact(tx, 0x4000, f_num(kind), f_num(len(pending))))
            elif kind == 0x1105:
                pending = []
                conn.sendall(compact(tx, 0x4000, f_num(kind), f_num(0)))
            elif kind == 0x2102:
                tid = args[1][1]
                pending = [item_fields(0, tid, 0, tracks[tid]["path"])]
                conn.sendall(compact(tx, 0x4000, f_num(kind), f_num(len(pending))))
            elif kind == 0x2002:
                tid = args[1][1]
                pending = [
                    item_fields(0, tid, 4, tracks[tid]["title"]),
                    item_fields(0, tid, 7, "Link Artist"),
                    item_fields(3, tid, 0x0B, ""),
                    item_fields(12800, tid, 0x0D, "128.0"),
                    item_fields(0, tid, 0x0F, "8B"),
                ]
                conn.sendall(compact(tx, 0x4000, f_num(kind), f_num(len(pending))))
            elif kind == 0x3000:
                start, n = args[1][1], args[2][1]
                conn.sendall(compact(tx, 0x4001, f_num(0x3000), f_num(n)))
                for row in pending[start : start + n]:
                    conn.sendall(compact(tx, 0x4101, *row))
                conn.sendall(compact(tx, 0x4201, f_num(0x3000), f_num(0)))
            elif kind == 0x2C04:
                tid = args[1][1]
                tag = struct.pack("<I", args[2][1]).rstrip(b"\x00").decode("ascii", "ignore")
                if tid in delay_ids:
                    do_sleep = False
                    with delay_lock:
                        if tid not in delayed_once:
                            delayed_once.add(tid)
                            do_sleep = True
                    if do_sleep:
                        time.sleep(3.5)
                sec = blobs.get(tag, b"")
                if sec:
                    payload = struct.pack("<I", len(sec)) + sec
                    conn.sendall(compact(tx, 0x4F02, f_num(kind), f_num(0), f_num(len(payload)), f_blob(payload), f_num(1)))
                else:
                    conn.sendall(compact(tx, 0x4F02, f_num(kind), f_num(50), f_num(0), f_blob(b""), f_num(1)))
            elif kind in (0x2204, 0x2004, 0x2904, 0x2104, 0x2B04, 0x2003):
                reply = {0x2204: 0x4602, 0x2004: 0x4402, 0x2904: 0x4A02, 0x2104: 0x4702, 0x2B04: 0x4E02, 0x2003: 0x4002}[kind]
                conn.sendall(compact(tx, reply, f_num(kind), f_num(50), f_num(0), f_blob(b""), f_num(1)))
            else:
                conn.sendall(compact(tx, 0x4003, f_num(kind)))
    except Exception:
        pass
    finally:
        conn.close()

db_sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
db_sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
db_sock.bind(("127.0.0.1", 0))
db_port = db_sock.getsockname()[1]
db_sock.listen(16)

q_sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
q_sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
q_sock.bind(("127.0.0.1", 12523))
q_sock.listen(16)

def serve_q():
    while True:
        c, _ = q_sock.accept()
        try:
            recv_exact(c, 19)
            c.sendall(struct.pack(">H", db_port))
        except Exception:
            pass
        finally:
            c.close()

threading.Thread(target=serve_q, daemon=True).start()
print(f"READY {db_port}", flush=True)
while True:
    c, _ = db_sock.accept()
    threading.Thread(target=handle_db, args=(c,), daemon=True).start()
"""


class DeckFeeder:
    """Keeps A's /v1/enginex/decks fresh with the desired deck payload while A's QML timer is paused."""

    def __init__(self):
        self.lock = threading.Lock()
        self.payload = None
        self.stop_ev = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def set(self, deck_obj):
        data = json.dumps([deck_obj]) if deck_obj is not None else json.dumps([{"deck": 1, "net": ""}])
        with self.lock:
            self.payload = data
        ns_curl("A", "/v1/enginex/decks", method="POST", data=data)

    def _run(self):
        while not self.stop_ev.wait(0.35):
            with self.lock:
                data = self.payload
            if data is not None:
                try:
                    ns_curl("A", "/v1/enginex/decks", method="POST", data=data, timeout=4)
                except Exception:
                    pass

    def close(self):
        self.stop_ev.set()
        self.thread.join(timeout=2)


def deck_state_b():
    # Keep JSON short (<75 chars) so Engine's console log never wraps across a string field.
    s = twin.evj(
        "B",
        'JSON.stringify({c:(root.rtTr("/Engine/Deck1/Track/QuickCue1Name")||{}).string||"",'
        'l:!!(root.rtTr("/Engine/Deck1/Track/SongLoaded")||{}).state,'
        'a:!!(root.rtTr("/Engine/Deck1/Track/SongAnalyzed")||{}).state,'
        'p:!!(root.rtTr("/Engine/Deck1/PlayState")||{}).state,'
        'b:(root.rtTr("/Engine/Deck1/OriginalBPM")||{}).unnormalized||0,'
        'x:Math.round((root.rtTr("/Engine/Deck1/Track/TrackPosition")||{}).unnormalized||0)})',
    )
    return {
        "cue": s["c"],
        "loaded": s["l"],
        "analysed": s["a"],
        "play": s["p"],
        "bpm": s["b"],
        "pos": s["x"],
    }


def assert_no_oa_on_b(fname: str):
    logs = engine_logs("B")
    if re.search(rf'OfflineAnalyzer .*Analyzed "{re.escape(fname)}"', logs):
        raise AssertionError(f"B ran OfflineAnalyzer for {fname}!")
    oa = analyzer_log("B")
    if fname in oa:
        raise AssertionError(f"B entered OfflineAnalyzer hook for {fname}:\n{oa}")


def count_shadow_fills(track_id: int) -> int:
    return len(re.findall(rf"shadow analysis filled track {track_id}\b", engine_logs("B")))


def count_b_404s() -> int:
    return sync_logs("B").count("the source gave no analysis (http 404)")


def main():
    assert os.environ.get("SHADOW_TWIN_WORK"), "use a fresh SHADOW_TWIN_WORK"
    assert row("A", "track-a.wav") is None and row("B", "track-a.wav") is None
    (twin.WORK / "B/data/music/track-a.wav").unlink()

    # --- Phase 1: Original baseline (real Engine A load + hot cue + single fill + paused follow) ---
    set_link("B", "down")
    try:
        twin.ev("A", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url="file:///data/music/track-a.wav"; "requested"')
        source = wait_for(lambda: (r := row("A", "track-a.wav", analysed_only=True)) and all(r[2:5]) and r, label="A track-a analysis")
        twin.ev(
            "A",
            'root.rtTr("/Engine/Deck1/Track/QuickCue1SetPosition").value=441000;'
            'root.rtTr("/Engine/Deck1/Track/QuickCue1Name").string="Shadow test cue"; "set cue"',
        )
        source = wait_for(lambda: (r := row("A", "track-a.wav", analysed_only=True)) and r[5] != source[5] and r, label="A cue save")
        decks = wait_for(lambda: [d for d in json.loads(ns_curl("A", "/peer/decks"))["decks"] or [] if d.get("key")], label="A /peer/decks")
        assert len(decks) == 1, decks
        response = ns_curl("A", "/peer/decks/1/perf?" + urlencode({"key": decks[0]["key"], "format": "native"}))
        assert response.startswith(b"RBXA1"), response[:60]
        print("Source analysis fields:", [len(b or b"") for b in source[2:7]], "flags:", source[7:], flush=True)
    finally:
        set_link("B", "up")

    def received_base():
        assert_no_oa_on_b("track-a.wav")
        r = row("B", "track-a.wav")
        if r and count_shadow_fills(r[0]) >= 1:
            return r
        return None

    target = wait_for(received_base, label="B baseline shadow fill")
    assert "/shadow-v3/peer-" in target[1], target[:2]
    assert target[2:7] == source[2:7], "receiving Engine did not store the source blobs"
    assert_full_height_overview("baseline track-a.wav", target[2], target[3])
    try:
        wait_for(
            lambda: (s := deck_state_b()) and s["loaded"] and s["analysed"] and s["cue"] == "Shadow test cue" and abs(s["bpm"] - 128) < 0.1 and s,
            label="B baseline deck ready",
        )
    except AssertionError:
        print("DIAG B baseline deck_state:", deck_state_b(), "row flags:", row("B", "track-a.wav")[7:], flush=True)
        print("DIAG B engine shadow lines:", [l for l in engine_logs("B").splitlines() if "shadow" in l.lower() or "offline" in l.lower()][-15:], flush=True)
        print("DIAG B sync log tail:", sync_logs("B").splitlines()[-15:], flush=True)
        raise
    print("Receiver analysis flags:", target[7:], flush=True)
    assert target[7:9] == source[7:9], (source[7:], target[7:])
    sidecars = twin.WORK / "B/data/enginex/shadow-v3"
    assert any(p.read_bytes().startswith(b"EXPF") for p in sidecars.rglob("track-a.wav.perf")), "missing EXPF fallback"

    # Wait 2 s to ensure Engine post-load updates do NOT trigger a second shadow fill.
    time.sleep(2.0)
    base_fills = count_shadow_fills(target[0])
    assert base_fills == 1, f"baseline track-a.wav filled {base_fills} times, want 1 (double-fill bug)"

    # Verify paused follow (on=shadow-paused-follow): copy follows playing A while PlayState stays False!
    hf_b = subprocess.check_output(twin.ns_chroot_cmd("B", "cat /run/enginex/hook-features"), text=True)
    assert "shadow-paused-follow" in hf_b and "on=" in hf_b, f"unexpected B hook-features: {hf_b!r}"
    pos_a0 = twin.evj("A", '(root.rtTr("/Engine/Deck1/Track/TrackPosition")||{}).unnormalized||0')
    pos_b0 = deck_state_b()["pos"]
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Play").state=true; "play"')
    try:
        b_max_pos = pos_b0
        deadline = time.monotonic() + 5.0
        while time.monotonic() < deadline:
            sb = deck_state_b()
            assert not sb["play"], f"paused-follow regression: B PlayState became True while following A ({sb})"
            if sb["pos"] > b_max_pos:
                b_max_pos = sb["pos"]
            pos_a = twin.evj("A", '(root.rtTr("/Engine/Deck1/Track/TrackPosition")||{}).unnormalized||0')
            if pos_a - pos_a0 >= 44100 and b_max_pos - pos_b0 >= 22050:
                break
            time.sleep(0.25)
        pos_a1 = twin.evj("A", '(root.rtTr("/Engine/Deck1/Track/TrackPosition")||{}).unnormalized||0')
        assert pos_a1 - pos_a0 >= 44100, f"A did not advance while playing: {pos_a0} -> {pos_a1}"
        assert b_max_pos - pos_b0 >= 22050, f"B did not follow playing A while paused: {pos_b0} -> {b_max_pos}"
        print(f"PAUSED-FOLLOW: PASS (A +{pos_a1 - pos_a0:.0f} smp, B +{b_max_pos - pos_b0:.0f} smp, B PlayState=False)", flush=True)
    finally:
        twin.ev("A", 'root.rtTr("/Engine/Deck1/Play").state=false; "pause"')

    # --- Phase 2: Prepare all 5 sources on A and run 5 x 4 = 20 timing matrix ---
    # Pause A's QML shadow tick timer so DeckFeeder drives A's real stream-sync with exact timings.
    twin.ev("A", 'scShadowTickTimer.stop(); "stopped"')
    feeder = DeckFeeder()
    prolink_proc = None
    try:
        # Unload B Deck 1 before starting the matrix.
        feeder.set(None)
        wait_for(lambda: not deck_state_b()["loaded"], timeout=25, label="B unload before matrix")

        sample_wav = twin.WORK / "A/data/music/sample3s.wav"
        sr, n_frames = slice_wav_seconds(twin.WORK / "A/data/music/track-a.wav", sample_wav, 3.0)
        wav_bytes = sample_wav.read_bytes()
        valid_blobs = make_engine_blobs(sr, n_frames, bpm=128.0, legacy_zero_maxima=False)
        legacy_blobs = make_engine_blobs(sr, n_frames, bpm=128.0, legacy_zero_maxima=True)
        src_mdb = db_path("A")

        # 1. stock-usb mount (/media/STOCK_USB)
        vol_stock = mount_tmpfs_in_a("STOCK_USB")
        stock_mdb = vol_stock / "Engine Library/Database2/m.db"
        clone_empty_engine_db(src_mdb, stock_mdb)
        for t_ms in REQUEST_TIMES_MS:
            fn = f"stock-usb-{t_ms}.wav"
            dst = vol_stock / "Music" / fn
            dst.parent.mkdir(parents=True, exist_ok=True)
            dst.write_bytes(wav_bytes)
            insert_engine_track(stock_mdb, f"../Music/{fn}", fn, valid_blobs, bpm=128.0, analysed=1)

        # 2. rb-ondemand mount (/media/RB_ONDEMAND) converted by real rb-bridge (OnDemand=true)
        vol_rbod = mount_tmpfs_in_a("RB_ONDEMAND")
        dat, ext, ex2, pqtz_sec, pco2_sec, pwv7_sec = build_anlz_files(bpm=128.0, duration_s=3.0)
        anlz_dir = vol_rbod / "PIONEER/USBANLZ/P001"
        anlz_dir.mkdir(parents=True, exist_ok=True)
        (anlz_dir / "ANLZ0000.DAT").write_bytes(dat)
        (anlz_dir / "ANLZ0000.EXT").write_bytes(ext)
        (anlz_dir / "ANLZ0000.2EX").write_bytes(ex2)
        rbod_specs = []
        for idx, t_ms in enumerate(REQUEST_TIMES_MS, 1):
            fn = f"rb-od-{t_ms}.wav"
            rel = f"Contents/RBOD/{fn}"
            dst = vol_rbod / rel
            dst.parent.mkdir(parents=True, exist_ok=True)
            dst.write_bytes(wav_bytes)
            rbod_specs.append((idx, f"RBOD {t_ms}", rel, 128.0, 3.0))
        pdb_dir = vol_rbod / "PIONEER/rekordbox"
        pdb_dir.mkdir(parents=True, exist_ok=True)
        (pdb_dir / "export.pdb").write_bytes(build_pdb(rbod_specs, len(wav_bytes)))
        subprocess.run(
            twin.ns_chroot_cmd(
                "A",
                "ln -sf shadow-twin-sync /tmp/rb-bridge && "
                "ENGINEX_RB_ONDEMAND=1 /tmp/rb-bridge --mount-path /media/RB_ONDEMAND >/tmp/rbod-mount.log 2>&1",
            ),
            check=True,
        )
        rbod_mdb = vol_rbod / "Engine Library/Database2/m.db"
        assert rbod_mdb.is_file() and (vol_rbod / "Engine Library/Database2/.rb-state.bin").is_file(), "rb-bridge on-demand mount did not produce m.db + .rb-state.bin"
        with closing(sqlite3.connect(rbod_mdb)) as conn:
            rbod_uuid = conn.execute("SELECT uuid FROM Information LIMIT 1").fetchone()[0]
            rbod_ids = {r[1]: r[0] for r in conn.execute("SELECT id, filename, isAnalyzed FROM Track")}
            not_analysed = conn.execute("SELECT count(*) FROM Track WHERE isAnalyzed=0").fetchone()[0]
            assert not_analysed == len(REQUEST_TIMES_MS), f"expected {len(REQUEST_TIMES_MS)} unanalysed on-demand tracks, got {not_analysed}"

        # 3. rb-mdb mount (/media/RB_MDB) with legacy [0,0,0] band-maxima trailer + 44-byte trackData
        vol_rbmdb = mount_tmpfs_in_a("RB_MDB")
        rbmdb_mdb = vol_rbmdb / "Engine Library/Database2/m.db"
        clone_empty_engine_db(src_mdb, rbmdb_mdb)
        for t_ms in REQUEST_TIMES_MS:
            fn = f"rb-mdb-{t_ms}.wav"
            dst = vol_rbmdb / "Contents/RBMDB" / fn
            dst.parent.mkdir(parents=True, exist_ok=True)
            dst.write_bytes(wav_bytes)
            insert_engine_track(rbmdb_mdb, f"../Contents/RBMDB/{fn}", fn, legacy_blobs, bpm=128.0, analysed=1)

        # 4. prolink setup: /media/RBX_SHARE + mounts.json + prolink.json + mock dbserver on 127.0.0.1:12523
        vol_pl = mount_tmpfs_in_a("RBX_SHARE")
        pl_tracks_cfg = {}
        for idx, t_ms in enumerate(REQUEST_TIMES_MS, 1):
            fn = f"prolink-{t_ms}.wav"
            dst = vol_pl / "Music" / fn
            dst.parent.mkdir(parents=True, exist_ok=True)
            dst.write_bytes(wav_bytes)
            pl_tracks_cfg[str(idx)] = {"title": f"ProLink {t_ms}", "path": f"/Music/{fn}"}
        mounts_json = json.dumps([{"id": "pl-share", "name": "Music", "mountPoint": "/media/RBX_SHARE", "enabled": True}])
        for mpath in (twin.WORK / "A/data/enginex/mounts.json", twin.WORK / "A/data/enginex/ui/mounts.json", twin.WORK / "A/tmp/home/.config/enginex/mounts.json"):
            mpath.parent.mkdir(parents=True, exist_ok=True)
            mpath.write_text(mounts_json)
        def refresh_prolink_snap():
            now_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            snap = json.dumps({
                "updatedAt": now_iso,
                "devices": [{
                    "name": "rekordbox",
                    "number": 17,
                    "address": "127.0.0.1",
                    "kind": "rekordbox",
                    "lastSeen": now_iso,
                }],
            })
            for ppath in (
                twin.WORK / "A/tmp/enginex-prolink.json",
                twin.WORK / "A/data/enginex/prolink.json",
                twin.WORK / "A/data/enginex/ui/prolink.json",
            ):
                ppath.parent.mkdir(parents=True, exist_ok=True)
                ppath.write_text(snap)

        refresh_prolink_snap()
        prolink_cfg = json.dumps({
            "tracks": pl_tracks_cfg,
            "blobs": {"PQTZ": pqtz_sec.hex(), "PCO2": pco2_sec.hex(), "PWV7": pwv7_sec.hex()},
            "delay_ids": [1, 2],  # 0 ms and 100 ms exercise 503 while rbxWriteAnalysis runs
        })
        prolink_proc = subprocess.Popen(
            ["ip", "netns", "exec", twin.SIDE_CFG["A"]["netns"], "python3", "-u", "-c", MOCK_PROLINK_SERVER, prolink_cfg],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        ready_line = prolink_proc.stdout.readline().strip()
        assert ready_line.startswith("READY "), f"mock prolink server failed: {ready_line!r} {prolink_proc.stderr.read() if prolink_proc.poll() is not None else ''}"
        ns_curl("A", "/v1/enginex/ui?prolinkShare=1&rekordboxShare=/&scope=rekordbox", method="POST")

        # Browse Pro DJ Link Tracks playlist through real tidal_gateway so stream-sync assigns gateway IDs!
        def get_prolink_gids():
            lists = json.loads(ns_curl("A", "/v1/users/1/playlists"))
            for item in lists.get("items") or []:
                if item.get("title") == "Tracks" and item.get("uuid"):
                    tr_resp = json.loads(ns_curl("A", f"/v1/playlists/{item['uuid']}/tracks"))
                    out = {}
                    for tr in tr_resp.get("items") or []:
                        out[tr["title"]] = int(tr["id"])
                    if len(out) == len(REQUEST_TIMES_MS):
                        return out
            return None

        prolink_gids = wait_for(get_prolink_gids, timeout=25, label="A Pro DJ Link track list")

        # 5. streaming setup: /data/downloads/TIDAL/stream-*.wav + A's stm.db
        stm_mdb = db_path("A", "stm.db")
        clone_empty_engine_db(src_mdb, stm_mdb)
        stm_dir = twin.WORK / "A/data/downloads/TIDAL"
        stm_dir.mkdir(parents=True, exist_ok=True)
        stm_rows = {}
        for idx, t_ms in enumerate(REQUEST_TIMES_MS, 1):
            fn = f"stream-{t_ms}.wav"
            (stm_dir / fn).write_bytes(wav_bytes)
            uri = f"streaming://TIDAL/Track/{9000 + idx}"
            pre_ready = t_ms >= 1000
            tid = insert_engine_track(
                stm_mdb,
                f"../../../../downloads/TIDAL/{fn}",
                fn,
                legacy_blobs if pre_ready else None,
                bpm=128.0,
                analysed=1 if pre_ready else 0,
                uri=uri,
            )
            stm_rows[t_ms] = (tid, uri, fn)

        def run_cell(source_name: str, t_ms: int):
            set_link("B", "down")
            bg_threads = []
            # Ensure B unloaded previous track cleanly while link was up before cutting eth0.
            # Choose source-specific deck payload and background source-side completion.
            if source_name == "stock-usb":
                fname = f"stock-usb-{t_ms}.wav"
                deck_obj = {"deck": 1, "net": f"net://src-uuid/STOCK_USB/Music/{fname}", "bpm": 0 if t_ms == 0 else 128}
            elif source_name == "rb-ondemand":
                fname = f"rb-od-{t_ms}.wav"
                deck_obj = {"deck": 1, "net": f"net://src-uuid/RB_ONDEMAND/Contents/RBOD/{fname}", "bpm": 0 if t_ms == 0 else 128}
                tid = rbod_ids[fname]
                od_req = json.dumps({"mount": "/media/RB_ONDEMAND", "uuid": rbod_uuid, "path": f"../Contents/RBOD/{fname}", "id": tid, "bpm": 128})

                def fill_ondemand_at_230ms():
                    # Simulate source Engine calling /run/enginex/rb-ondemand.sock at t=0 and committing to m.db at ~230 ms.
                    import http.client

                    sock_path = str(twin.WORK / "A/run/enginex/rb-ondemand.sock")

                    class UnixHTTP(http.client.HTTPConnection):
                        def connect(self):
                            self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                            self.sock.settimeout(self.timeout)
                            self.sock.connect(sock_path)

                    conn = UnixHTTP("localhost", timeout=10)
                    try:
                        conn.request("POST", "/v1/enginex/rekordbox/track-data", od_req.encode(), {"Content-Type": "application/json"})
                        resp = conn.getresponse()
                        body = resp.read()
                        assert resp.status == 200, f"rb-ondemand.sock status={resp.status} body={body[:120]!r}"
                    finally:
                        conn.close()
                    time.sleep(0.23)
                    update_engine_track(rbod_mdb, tid, parse_rbxa1(body), bpm=128.0)

                bg_threads.append(threading.Thread(target=fill_ondemand_at_230ms, daemon=True))
            elif source_name == "rb-mdb":
                fname = f"rb-mdb-{t_ms}.wav"
                deck_obj = {"deck": 1, "net": f"net://src-uuid/RB_MDB/Contents/RBMDB/{fname}", "bpm": 128}
            elif source_name == "prolink":
                refresh_prolink_snap()
                fname = f"prolink-{t_ms}.wav"
                gid = prolink_gids[f"ProLink {t_ms}"]
                # Trigger real rbxPrepare (+ rbxWriteAnalysis; 0ms & 100ms keep rbxAnalysing running past 3 s wait).
                ns_curl("A", f"/v1/tracks/{gid}/playbackinfopostpaywall", timeout=15)
                deck_obj = {"deck": 1, "net": f"streaming://TIDAL/Track/{gid}", "title": f"ProLink {t_ms}", "artist": "Link Artist", "bpm": 0 if t_ms == 0 else 128}
                # Peer copy filename is displayFileName("Link Artist", "ProLink <t>", ".wav")!
                fname = f"Link Artist - ProLink {t_ms}.wav"
            elif source_name == "streaming":
                tid, uri, raw_fn = stm_rows[t_ms]
                deck_obj = {"deck": 1, "net": uri, "bpm": 0 if t_ms == 0 else 128}
                fname = f"Test Artist - {raw_fn}"
                if t_ms < 1000:
                    def finish_stream_analysis():
                        time.sleep(0.4)
                        update_engine_track(stm_mdb, tid, legacy_blobs, bpm=128.0)

                    bg_threads.append(threading.Thread(target=finish_stream_analysis, daemon=True))
            else:
                raise ValueError(source_name)

            # t = 0: start source load + background source completion; wait t_ms; bring B's eth0 up!
            before_404 = count_b_404s()
            for th in bg_threads:
                th.start()
            feeder.set(deck_obj)
            if t_ms > 0:
                time.sleep(t_ms / 1000.0)
            set_link("B", "up")

            def cell_done():
                assert_no_oa_on_b(fname)
                r = row("B", fname)
                if r and count_shadow_fills(r[0]) >= 1:
                    return r
                return None

            try:
                got = wait_for(cell_done, timeout=35, label=f"{source_name}@{t_ms}ms ({fname})")
            except AssertionError:
                print(f"DIAG {source_name}@{t_ms}ms A /peer/decks:", ns_curl("A", "/peer/decks").decode("utf-8", "replace")[:400], flush=True)
                print(f"DIAG {source_name}@{t_ms}ms B sync log tail:", "\n".join(sync_logs("B").splitlines()[-10:]), flush=True)
                raise
            for th in bg_threads:
                th.join(timeout=5)
            wait_for(lambda: (s := deck_state_b()) and s["loaded"] and s["analysed"] and s, timeout=20, label=f"B ready {source_name}@{t_ms}ms")
            time.sleep(0.5)
            assert_no_oa_on_b(fname)
            fills = count_shadow_fills(got[0])
            assert fills == 1, f"{source_name}@{t_ms}ms: filled {fills} times, want 1"
            after_404 = count_b_404s()
            assert after_404 == before_404, f"{source_name}@{t_ms}ms: B logged http 404 ({after_404 - before_404} new)!"
            assert "/shadow-v3/peer-" in got[1], got[:2]
            assert_full_height_overview(f"{source_name}@{t_ms}ms", got[2], got[3])
            print(f"MATRIX PASS: {source_name:11s} @ {t_ms:4d}ms | trackId={got[0]} fills={fills} oa=0 404=0 overview=FULL", flush=True)

            # Unload Deck 1 on A so on-demand cache evicts and B unloads cleanly for next cell.
            feeder.set(None)
            wait_for(lambda: not deck_state_b()["loaded"], timeout=20, label=f"B unload after {source_name}@{t_ms}ms")

        for src_name in SOURCES:
            for t_ms in REQUEST_TIMES_MS:
                run_cell(src_name, t_ms)
    finally:
        set_link("B", "up")
        feeder.close()
        if prolink_proc is not None:
            prolink_proc.terminate()
            try:
                prolink_proc.wait(timeout=3)
            except Exception:
                prolink_proc.kill()

    # Final sanity: finish one real stock analysis on B Deck 2 so oa.log proves the hook is live
    # and none of the 21 shadowed tracks ever entered OfflineAnalyzer.
    twin.ev("B", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck2").url="file:///data/music/track-b.wav"; "requested"')
    wait_for(lambda: (r := row("B", "track-b.wav")) and r[7] == 1, label="B stock Deck 2 analysis")
    log = analyzer_log("B")
    assert "installed" in log, log
    for needle in ("track-a.wav", "stock-usb-", "rb-od-", "rb-mdb-", "ProLink ", "stream-"):
        assert needle not in log, f"shadow track {needle!r} entered OfflineAnalyzer:\n{log}"
    print("PASS: all 5 sources x 4 request times (20/20) + baseline + paused-follow replayed with 0 OfflineAnalyzer, 0 404s, 1 fill each, and full-height overviews")


if __name__ == "__main__":
    main()
