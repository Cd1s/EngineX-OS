#!/usr/bin/env python3
"""End-to-end two-instance SC5000 ARM sandbox proof for deck-share acceptance 1, 2, 3.

Runs inside SHADOW_TWIN_WORK=/var/tmp/deck-share-twin on Oracle-Batam-arm with:
- Real armhf Engine 5.3.0 + libengine-ui-hook.so + alsa-null-fixture.so + enginex-suite + UI packs
- Isolated 3rd netns deck-share-srv (192.168.200.10) hosting real rclone serve webdav,
  real Samba smbd, and mock streaming endpoints for Deezer, NetEase, Spotify, Apple Music, SoundCloud
- Real Acceptance 1 (Over Link with B isolated from 192.168.200.10), Acceptance 3 (wrong code
  401/429 rate-limit lockout, summary, undo), and Acceptance 2 (one-shot device code copy +
  browse/load/play on real Engine for every copied service and server with A's stream-sync stopped).
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.parse
from pathlib import Path

WORK_ROOT = Path(os.environ.get("SHADOW_TWIN_WORK", "/var/tmp/deck-share-twin"))
os.environ["SHADOW_TWIN_WORK"] = str(WORK_ROOT)

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import twin  # noqa: E402

SRV_NETNS = "deck-share-srv"
SRV_BRIDGE = "br-deckshare"
SRV_IP = "192.168.200.10"
A_SRV_IP = "192.168.200.1"
B_SRV_IP = "192.168.200.2"
DAV_PORT = 8090
SMB_PORT = 445
MOCK_PORT = 9090

DAV_USER = "davshare"
DAV_PASS = "dav-sandbox-secret-9988"
SMB_USER = "root"
SMB_PASS = "smb-sandbox-secret-7766"

DZ_ARL = "testdzarlsandbox0123456789abcdef"
NE_COOKIE = "MUSIC_U=ne-sandbox-cookie-secret-1122;"
SP_ACCESS = "sp-sandbox-access-secret-3344"
SP_REFRESH = "sp-sandbox-refresh-secret-5566"
AM_USER_TOK = "am-sandbox-user-secret-7788"
AM_HELPER_TOK = "am-sandbox-helper-secret-9900"
SC_CID = "sc-sandbox-cid-1212"
SC_SEC = "sc-sandbox-sec-3434"
SC_STATIC_TOK = "sc-sandbox-static-secret-5656"
SC_ROT_REF = "sc-sandbox-rot-refresh-do-not-copy"
BP_ROT_REF = "bp-sandbox-rot-refresh-do-not-copy"
TD_ROT_REF = "td-sandbox-rot-refresh-do-not-copy"

ALL_SECRETS = [
    DAV_PASS,
    SMB_PASS,
    DZ_ARL,
    NE_COOKIE,
    SP_ACCESS,
    SP_REFRESH,
    AM_USER_TOK,
    AM_HELPER_TOK,
    SC_SEC,
    SC_STATIC_TOK,
    SC_ROT_REF,
    BP_ROT_REF,
    TD_ROT_REF,
]


def sh(cmd: str, check: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(["bash", "-c", cmd], check=check, capture_output=True, text=True)


def wait_for(fn, timeout: float = 45.0, interval: float = 0.2, label: str = "condition"):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            res = fn()
            if res:
                return res
        except Exception as exc:
            last = exc
        time.sleep(interval)
    raise AssertionError(f"timeout waiting for {label} (last={last})")


def ns_http(side: str, method: str, path: str, body: dict | None = None, base: str = "http://127.0.0.1:8080") -> tuple[int, str]:
    netns = twin.SIDE_CFG[side]["netns"]
    cmd = ["ip", "netns", "exec", netns, "curl", "-sS", "-m", "15", "-X", method, "-w", "\n%{http_code}"]
    if body is not None:
        cmd.extend(["-H", "Content-Type: application/json", "-d", json.dumps(body)])
    cmd.append(base + path)
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        raise RuntimeError(f"curl {side} {method} {path} failed ({r.returncode}): {r.stderr}")
    out, status_str = r.stdout.rsplit("\n", 1)
    return int(status_str), out


def ns_http_bytes(side: str, url: str) -> tuple[int, bytes]:
    netns = twin.SIDE_CFG[side]["netns"]
    with tempfile.NamedTemporaryFile(delete=False) as tf:
        tmp_path = tf.name
    try:
        r = subprocess.run(
            ["ip", "netns", "exec", netns, "curl", "-sS", "-m", "30", "-o", tmp_path, "-w", "%{http_code}", url],
            capture_output=True,
            text=True,
        )
        if r.returncode != 0:
            raise RuntimeError(f"curl bytes {side} {url} failed ({r.returncode}): {r.stderr}")
        return int(r.stdout.strip()), Path(tmp_path).read_bytes()
    finally:
        Path(tmp_path).unlink(missing_ok=True)


def can_connect_from_ns(netns: str, host: str, port: int) -> bool:
    r = subprocess.run(
        [
            "ip",
            "netns",
            "exec",
            netns,
            "python3",
            "-c",
            f"import socket; s=socket.create_connection(({host!r},{port}), 1.0); s.close()",
        ],
        capture_output=True,
    )
    return r.returncode == 0


def generate_audio_fixtures(fix_dir: Path) -> dict[str, Path]:
    fix_dir.mkdir(parents=True, exist_ok=True)
    specs = {
        "dav": ("dav-track.mp3", 330, "libmp3lame"),
        "smb": ("smb-track.mp3", 392, "libmp3lame"),
        "dz": ("dz-track.mp3", 440, "libmp3lame"),
        "ne": ("ne-track.mp3", 494, "libmp3lame"),
        "am": ("am-track.flac", 523, "flac"),
        "sc": ("sc-track.mp3", 587, "libmp3lame"),
    }
    out = {}
    for key, (fname, freq, codec) in specs.items():
        dst = fix_dir / fname
        cmd = [
            "ffmpeg",
            "-y",
            "-loglevel",
            "error",
            "-f",
            "lavfi",
            "-i",
            f"sine=frequency={freq}:duration=12:sample_rate=44100",
            "-ac",
            "2",
            "-ar",
            "44100",
            "-c:a",
            codec,
            str(dst),
        ]
        subprocess.run(cmd, check=True)
        out[key] = dst
    return out


def setup_srv_network(ns_a: str, ns_b: str) -> None:
    teardown_srv_network(ns_a, ns_b)
    netns_dir = Path("/etc/netns") / SRV_NETNS
    netns_dir.mkdir(parents=True, exist_ok=True)
    # Empty resolv.conf avoids multi-second Samba DNS timeouts inside netns.
    (netns_dir / "resolv.conf").write_text("")
    (netns_dir / "hosts").write_text("127.0.0.1 localhost\n")

    sh(f"ip netns add {SRV_NETNS}")
    sh(f"ip -n {SRV_NETNS} link set lo up")
    sh(f"ip link add {SRV_BRIDGE} type bridge")
    sh(f"ip link set {SRV_BRIDGE} up")

    # Server veth
    sh(f"ip link add ds-srv-br type veth peer name eth0 netns {SRV_NETNS}")
    sh(f"ip link set ds-srv-br master {SRV_BRIDGE} up")
    sh(f"ip -n {SRV_NETNS} addr add {SRV_IP}/24 dev eth0")
    sh(f"ip -n {SRV_NETNS} link set eth0 up")

    # Deck A veth (UP)
    sh(f"ip link add ds-a-br type veth peer name eth1 netns {ns_a}")
    sh(f"ip link set ds-a-br master {SRV_BRIDGE} up")
    sh(f"ip -n {ns_a} addr add {A_SRV_IP}/24 dev eth1")
    sh(f"ip -n {ns_a} link set eth1 up")
    sh(f"ip netns exec {ns_a} sysctl -q -w net.ipv4.ip_forward=0")

    # Deck B veth (DOWN during Acceptance 1 + blackhole route)
    sh(f"ip link add ds-b-br type veth peer name eth1 netns {ns_b}")
    sh(f"ip link set ds-b-br master {SRV_BRIDGE} up")
    sh(f"ip -n {ns_b} addr add {B_SRV_IP}/24 dev eth1")
    sh(f"ip -n {ns_b} link set eth1 down")
    sh(f"ip -n {ns_b} route add blackhole 192.168.200.0/24")


def enable_b_srv_link(ns_b: str) -> None:
    sh(f"ip -n {ns_b} route del blackhole 192.168.200.0/24 2>/dev/null || true", check=False)
    sh(f"ip -n {ns_b} link set eth1 up")


def teardown_srv_network(ns_a: str = "", ns_b: str = "") -> None:
    for cmd in (
        f"ip netns del {SRV_NETNS} 2>/dev/null || true",
        f"ip link del {SRV_BRIDGE} 2>/dev/null || true",
        "ip link del ds-srv-br 2>/dev/null || true",
        "ip link del ds-a-br 2>/dev/null || true",
        "ip link del ds-b-br 2>/dev/null || true",
    ):
        sh(cmd, check=False)
    netns_dir = Path("/etc/netns") / SRV_NETNS
    shutil.rmtree(netns_dir, ignore_errors=True)


def write_mock_servers_script(path: Path, fixtures: dict[str, Path]) -> None:
    code = r'''
import json, sys, urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HOST, PORT = sys.argv[1], int(sys.argv[2])
NE_FILE = Path(sys.argv[3])
AM_FILE = Path(sys.argv[4])
SC_FILE = Path(sys.argv[5])

NE_COOKIE = sys.argv[6]
SP_ACCESS = sys.argv[7]
AM_HELPER_TOK = sys.argv[8]
SC_STATIC = sys.argv[9]
BASE = f"http://{HOST}:{PORT}"
AM_DEV = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJURUFNIiwiZXhwIjo0MTAyNDQ0ODAwfQ.c2lnbmF0dXJlLWJ5dGVzLWxvbmctZW5vdWdo"

class H(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def send_json(self, obj, code=200):
        raw = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
    def send_bytes(self, raw, mime):
        self.send_response(200)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
    def handle_netease(self, p, qs):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length).decode("utf-8", "replace") if length else ""
        form = urllib.parse.parse_qs(body)
        cookie = form.get("cookie", [""])[0] or self.headers.get("Cookie", "")
        if "MUSIC_U=ne-sandbox-cookie-secret-1122" not in cookie:
            return self.send_json({"code": 301}, 401)
        if p == "/user/playlist":
            return self.send_json({"code": 200, "playlist": [{"id": 601, "name": "NE Sandbox Set", "trackCount": 1}]})
        if p == "/album/sublist":
            return self.send_json({"code": 200, "data": [], "hasMore": False})
        if p == "/dj/sublist":
            return self.send_json({"code": 200, "djRadios": [], "hasMore": False})
        if p == "/playlist/detail":
            return self.send_json({"code": 200, "playlist": {"id": 601, "name": "NE Sandbox Set", "trackIds": [{"id": 6001}], "tracks": [{"id": 6001, "name": "NetEase Sandbox Track", "dt": 12000, "ar": [{"name": "NE Artist"}], "al": {"name": "NE Album"}}]}})
        if p == "/song/detail":
            return self.send_json({"code": 200, "songs": [{"id": 6001, "name": "NetEase Sandbox Track", "dt": 12000, "ar": [{"name": "NE Artist"}], "al": {"name": "NE Album"}}]})
        if p == "/song/url/v1":
            return self.send_json({"code": 200, "data": [{"id": 6001, "url": f"{BASE}/audio/ne.mp3", "type": "mp3"}]})
        return self.send_json({"code": 404}, 404)
    def do_POST(self):
        p = urllib.parse.urlparse(self.path).path
        qs = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        if p in ("/user/playlist", "/album/sublist", "/dj/sublist", "/playlist/detail", "/song/detail", "/song/url/v1"):
            return self.handle_netease(p, qs)
        if p == "/v1/download":
            if self.headers.get("Authorization") != "Bearer " + AM_HELPER_TOK:
                return self.send_json({"error": "unauthorized"}, 401)
            length = int(self.headers.get("Content-Length") or 0)
            _ = self.rfile.read(length)
            return self.send_json({"file": "/am.flac", "quality": {"lossless": True, "sampleRate": 44100, "channels": 2}})
        self.send_response(404); self.end_headers()
    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        p = parsed.path
        if p == "/audio/ne.mp3":
            return self.send_bytes(NE_FILE.read_bytes(), "audio/mpeg")
        if p == "/am.flac":
            return self.send_bytes(AM_FILE.read_bytes(), "audio/flac")
        if p == "/sc.mp3":
            return self.send_bytes(SC_FILE.read_bytes(), "audio/mpeg")
        # Deezer catalog
        if p == "/user/101/playlists":
            return self.send_json({"data": [{"id": 701, "title": "DZ Sandbox Set", "nb_tracks": 1}]})
        if p == "/chart/0/playlists":
            return self.send_json({"data": []})
        if p == "/playlist/701/tracks":
            return self.send_json({"data": [{"id": 9001, "title": "Deezer Sandbox Track", "duration": 12, "isrc": "USRC22222222", "artist": {"name": "DZ Artist"}, "album": {"title": "DZ Album"}}]})
        if p == "/track/isrc:USRC22222222":
            return self.send_json({"id": 9001, "title": "Spotify Matched Track", "duration": 12, "isrc": "USRC22222222", "artist": {"name": "SP Artist"}, "album": {"title": "SP Album"}})
        if p == "/search":
            return self.send_json({"data": [{"id": 9001, "title": "Deezer Sandbox Track", "duration": 12, "artist": {"name": "DZ Artist"}, "album": {"title": "DZ Album"}}]})
        # Spotify API (Bearer auth) vs SoundCloud /me (OAuth auth)
        auth_hdr = self.headers.get("Authorization", "")
        if p in ("/me", "/me/playlists", "/me/tracks", "/playlists/sp1/items") and not auth_hdr.startswith("OAuth "):
            if auth_hdr != "Bearer " + SP_ACCESS:
                return self.send_json({"error": "unauthorized"}, 401)
            if p == "/me":
                return self.send_json({"id": "spuser"})
            if p == "/me/playlists":
                return self.send_json({"items": [{"id": "sp1", "name": "SP Sandbox Set", "tracks": {"total": 1}}]})
            if p == "/me/tracks":
                return self.send_json({"total": 1, "items": [{"track": {"id": "spt1", "name": "Spotify Sandbox Track", "duration_ms": 12000, "external_ids": {"isrc": "USRC22222222"}, "artists": [{"name": "SP Artist"}], "album": {"name": "SP Album"}}}]})
            if p == "/playlists/sp1/items":
                return self.send_json({"items": [{"track": {"id": "spt1", "name": "Spotify Sandbox Track", "duration_ms": 12000, "external_ids": {"isrc": "USRC22222222"}, "artists": [{"name": "SP Artist"}], "album": {"name": "SP Album"}}}]})
        # Apple Music web + catalog
        if p == "/us/browse":
            return self.send_bytes(b'<script type="module" src="/assets/index-Ab12.js"></script>', "text/html")
        if p == "/assets/index-Ab12.js":
            return self.send_bytes(f'const k="{AM_DEV}";'.encode(), "application/javascript")
        if p.startswith("/v1/"):
            if self.headers.get("Authorization") != "Bearer " + AM_DEV:
                return self.send_json({"error": "unauthorized"}, 401)
            if p in ("/v1/me/library/playlists", "/v1/me/library/albums", "/v1/me/library/songs"):
                return self.send_json({"data": []})
            song = {"id": "1440", "type": "songs", "attributes": {"name": "Apple Sandbox Track", "artistName": "AM Artist", "albumName": "AM Album", "url": "https://music.apple.com/us/song/1440", "durationInMillis": 12000}}
            if p == "/v1/catalog/us/playlists/pl.decka":
                return self.send_json({"data": [{"id": "pl.decka", "type": "playlists", "attributes": {"name": "AM Sandbox Set"}, "relationships": {"tracks": {"data": [song]}}}]})
            if p == "/v1/catalog/us/playlists/pl.decka/tracks":
                return self.send_json({"data": [song]})
        # SoundCloud API
        if p in ("/sc/me", "/me", "/me/playlists", "/users/42/playlists", "/me/likes/playlists", "/me/playlist_likes", "/users/42/likes/playlists", "/users/42/playlist_likes", "/me/likes/tracks", "/me/track_likes", "/users/42/likes/tracks", "/users/42/track_likes", "/playlists/801/tracks", "/tracks/901", "/tracks/901/streams"):
            if self.headers.get("Authorization") != "OAuth " + SC_STATIC:
                return self.send_json({"error": "unauthorized"}, 401)
            if p in ("/me", "/sc/me"):
                return self.send_json({"id": 42, "username": "dj-a", "consumer_subscription": {"product": {"id": "consumer-high-tier"}}})
            if p in ("/me/playlists", "/users/42/playlists"):
                return self.send_json({"collection": [{"id": 801, "title": "SC Sandbox Set", "track_count": 1}]})
            if p in ("/me/likes/playlists", "/me/playlist_likes", "/users/42/likes/playlists", "/users/42/playlist_likes"):
                return self.send_json({"collection": []})
            if p in ("/me/likes/tracks", "/me/track_likes", "/users/42/likes/tracks", "/users/42/track_likes", "/playlists/801/tracks"):
                return self.send_json({"collection": [{"id": 901, "title": "SC Sandbox Track", "duration": 12000, "access": "playable", "user": {"username": "SC Artist"}}]})
            if p == "/tracks/901":
                return self.send_json({"id": 901, "title": "SC Sandbox Track", "duration": 12000, "access": "playable", "user": {"username": "SC Artist"}})
            if p == "/tracks/901/streams":
                return self.send_json({"http_mp3_128_url": f"{BASE}/sc.mp3"})
        self.send_json({"error": "not found", "path": p}, 404)

ThreadingHTTPServer((HOST, PORT), H).serve_forever()
'''
    path.write_text(code.lstrip())


def write_deemixd_script(path: Path) -> None:
    code = r'''
import json, shutil, sys, urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

PORT = int(sys.argv[1])
STATE_DIR = Path(sys.argv[2])
HOST_DL_DIR = Path(sys.argv[3])
CHROOT_DL_DIR = Path(sys.argv[4])
DZ_FILE = Path(sys.argv[5])
WANT_ARL = sys.argv[6]

class H(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def do_POST(self):
        p = urllib.parse.urlparse(self.path).path
        length = int(self.headers.get("Content-Length") or 0)
        _ = self.rfile.read(length)
        owner = STATE_DIR / "deezer-owner.json"
        if not owner.is_file() or WANT_ARL not in owner.read_text():
            self.send_response(401); self.end_headers(); self.wfile.write(b"missing deezer ARL")
            return
        HOST_DL_DIR.mkdir(parents=True, exist_ok=True)
        shutil.copy2(DZ_FILE, HOST_DL_DIR / "9001.mp3")
        raw = json.dumps({"id": "q1", "state": "completed", "progress": 1.0, "path": str(CHROOT_DL_DIR / "9001.mp3")}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
    def do_GET(self):
        p = urllib.parse.urlparse(self.path).path
        if p.startswith("/api/queue"):
            raw = json.dumps({"id": "q1", "state": "completed", "progress": 1.0, "path": str(CHROOT_DL_DIR / "9001.mp3")}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
            return
        self.send_response(404); self.end_headers()

ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
'''
    path.write_text(code.lstrip())


def restart_streamsync(side: str, mock_base: str) -> subprocess.Popen:
    sdir = WORK_ROOT / side
    subprocess.run(twin.ns_chroot_cmd(side, "pkill -TERM -f '/tmp/stream-sync' 2>/dev/null || true"), check=False)
    time.sleep(0.4)
    subprocess.run(twin.ns_chroot_cmd(side, "pkill -KILL -f '/tmp/stream-sync' 2>/dev/null || true"), check=False)
    dl_dir = sdir / "data/enginex/Stream"
    dl_dir.mkdir(parents=True, exist_ok=True)

    flags = " ".join(
        [
            "exec /tmp/stream-sync",
            "--listen :8080",
            "--state-dir /data/enginex/state",
            "--downloads /data/enginex/Stream",
            "--deemixd=''",
            "--deemix-url http://127.0.0.1:7272",
            "--disable-guard",
            "--relay-url ''",
            f"--deezer-url {mock_base}",
            f"--deezer-gateway-url {mock_base}",
            f"--spotify-api-url {mock_base}",
            f"--spotify-accounts-url {mock_base}",
            f"--netease-url {mock_base}",
            f"--soundcloud-url {mock_base}",
            f"--soundcloud-token-url {mock_base}",
            f"--applemusic-url {mock_base}",
            f"--applemusic-web-url {mock_base}",
            f"--beatport-url {mock_base}",
            f"--tidal-api-url {mock_base}",
            f"--tidal-auth-url {mock_base}",
        ]
    )
    log_f = open(sdir / "logs/stream-sync-share.log", "w")
    proc = subprocess.Popen(twin.ns_chroot_cmd(side, flags), stdout=log_f, stderr=subprocess.STDOUT)
    (sdir / "sync.pid").write_text(str(proc.pid))
    wait_for(lambda: ns_http(side, "GET", "/v1/enginex/ui")[0] == 200, timeout=20.0, label=f"{side} stream-sync")
    return proc


def stop_streamsync(side: str) -> None:
    subprocess.run(twin.ns_chroot_cmd(side, "pkill -TERM -f '/tmp/stream-sync' 2>/dev/null || true"), check=False)
    time.sleep(0.3)
    subprocess.run(twin.ns_chroot_cmd(side, "pkill -KILL -f '/tmp/stream-sync' 2>/dev/null || true"), check=False)


def restart_engine_with_dri(side: str) -> None:
    sdir = WORK_ROOT / side
    subprocess.run(twin.ns_chroot_cmd(side, "pkill -TERM -f '^/usr/Engine/Engine' 2>/dev/null || true"), check=False)
    time.sleep(0.3)
    subprocess.run(twin.ns_chroot_cmd(side, "pkill -KILL -f '^/usr/Engine/Engine' 2>/dev/null || true"), check=False)
    (sdir / "tmp/engine_runguard.lock").unlink(missing_ok=True)
    (sdir / "data/enginex/ui/boot-ready").unlink(missing_ok=True)
    subprocess.run(
        twin.ns_chroot_cmd(
            side,
            "if [ ! -e /dev/dri/card0 ] && [ -r /sys/class/drm/card0/dev ]; then "
            "umount /dev/dri 2>/dev/null || true; "
            "mount -t tmpfs tmpfs /dev/dri; "
            "IFS=: read -r maj min < /sys/class/drm/card0/dev; "
            "mknod -m 666 /dev/dri/card0 c \"$maj\" \"$min\"; "
            "if [ -r /sys/class/drm/renderD128/dev ]; then "
            "IFS=: read -r rmaj rmin < /sys/class/drm/renderD128/dev; "
            "mknod -m 666 /dev/dri/renderD128 c \"$rmaj\" \"$rmin\"; "
            "fi; "
            "fi",
        ),
        check=True,
    )
    eng_log = open(sdir / "logs/engine.log", "w")
    eng_cmd = (
        "exec /usr/bin/env "
        "HOME=/tmp/home "
        "LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib "
        "LD_PRELOAD=/tmp/alsa-null-fixture.so:/etc/enginex/libengine-ui-hook.so "
        "SBX_CARD=JP07 SBX_OUT=4 SBX_IN=2 SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 "
        "QT_QPA_PLATFORM=eglfs "
        "QT_QPA_EGLFS_INTEGRATION=eglfs_kms "
        "LIBGL_ALWAYS_SOFTWARE=1 "
        "GALLIUM_DRIVER=llvmpipe "
        "LP_NUM_THREADS=2 "
        "QML_DISABLE_DISK_CACHE=1 "
        "QML_XHR_ALLOW_FILE_READ=1 "
        "MALLOC_ARENA_MAX=1 "
        "/usr/Engine/Engine -d0"
    )
    proc_eng = subprocess.Popen(twin.ns_chroot_cmd(side, eng_cmd), stdout=eng_log, stderr=subprocess.STDOUT)
    (sdir / "engine.pid").write_text(str(proc_eng.pid))


def seed_deck_a(mock_base: str) -> None:
    state_a = WORK_ROOT / "A/data/enginex/state"
    ui_a = WORK_ROOT / "A/data/enginex/ui"
    state_a.mkdir(parents=True, exist_ok=True)
    ui_a.mkdir(parents=True, exist_ok=True)

    (state_a / "deezer-owner.json").write_text(json.dumps({"UserID": "101", "ARL": DZ_ARL}))
    (state_a / "netease-owner.json").write_text(json.dumps({"Cookie": NE_COOKIE, "UserID": 202}))
    (state_a / "spotify-token.json").write_text(
        json.dumps(
            {
                "access_token": SP_ACCESS,
                "refresh_token": SP_REFRESH,
                "token_type": "Bearer",
                "client_id": "65b708073fc0480ea92a077233ca87bd",
                "expiry": "2035-01-01T00:00:00Z",
            }
        )
    )
    (state_a / "spotify-links.json").write_text(
        json.dumps(
            [
                {
                    "id": "37i9dQZF1DXcBWIGoYBM5M",
                    "name": "SP Linked Set",
                    "tracks": [
                        {
                            "ID": "sptl1",
                            "Title": "SP Linked Track",
                            "Artist": "SP Artist",
                            "Album": "SP Album",
                            "ISRC": "USRC22222222",
                            "DurationMS": 12000,
                            "Provider": "spotify",
                        }
                    ],
                }
            ]
        )
    )
    (state_a / "spotify-device-id").write_text("deck-a-device-id-only\n")
    (state_a / "spotify-librespot.json").write_text(json.dumps({"username": "librespot-device-only"}))
    (state_a / "applemusic-owner.json").write_text(
        json.dumps(
            {
                "Storefront": "us",
                "UserToken": AM_USER_TOK,
                "Links": ["https://music.apple.com/us/playlist/am/pl.decka"],
            }
        )
    )
    (state_a / "applemusic-helper.json").write_text(json.dumps({"url": mock_base, "token": AM_HELPER_TOK}))
    (state_a / "soundcloud-owner.json").write_text(
        json.dumps({"UserID": 42, "Username": "dj-a", "ClientID": SC_CID, "Secret": SC_SEC, "Token": SC_STATIC_TOK})
    )
    (state_a / "bpm-config.json").write_text(
        json.dumps({"soundcloudClientId": SC_CID, "soundcloudSecret": SC_SEC, "soundcloudToken": SC_STATIC_TOK})
    )
    (state_a / "soundcloud-token.json").write_text(
        json.dumps({"refresh_token": SC_ROT_REF, "access_token": "sc-rot-acc", "expires_at": "2035-01-01T00:00:00Z"})
    )
    (state_a / "beatport-owner.json").write_text(
        json.dumps({"access_token": "bp-acc", "refresh_token": BP_ROT_REF, "expires_at": 2051222400})
    )
    (state_a / "tidal-auth.json").write_text(
        json.dumps({"accessToken": "td-acc", "refreshToken": TD_ROT_REF, "expiresAt": "2035-01-01T00:00:00Z", "userId": "777"})
    )

    nas_cfg = {
        "protocol": "smb",
        "url": f"smb://{SRV_IP}:{SMB_PORT}/music",
        "username": SMB_USER,
        "password": SMB_PASS,
        "enabled": True,
        "name": "Deck A NAS",
        "webdav": [
            {
                "name": "Deck A WebDAV",
                "url": f"http://{SRV_IP}:{DAV_PORT}/",
                "username": DAV_USER,
                "password": DAV_PASS,
                "enabled": True,
            }
        ],
    }
    (ui_a / "nas.json").write_text(json.dumps(nas_cfg, indent=2))
    conf_path = ui_a / "settings.conf"
    cur = conf_path.read_text()
    cur += "\nwebdav=1\nnas=1\nsources=deezer,netease,spotify,applemusic,soundcloud\nscope=deezer\n"
    conf_path.write_text(cur)


def fetch_first_playable(side: str, scope: str) -> tuple[bytes, str]:
    st, body = ns_http(side, "POST", f"/v1/enginex/ui?scope={urllib.parse.quote(scope, safe='')}")
    assert st == 200, f"set scope={scope} on {side}: HTTP {st} {body}"

    st, raw_pl = ns_http(side, "GET", "/v1/users/1/playlists?limit=100&offset=0")
    assert st == 200, f"playlists {scope}: HTTP {st} {raw_pl}"
    playlists = json.loads(raw_pl).get("items") or []
    assert playlists, f"no playlists for scope={scope}"

    chosen_tracks = []
    for pl in playlists:
        title = pl.get("title", "")
        uuid = pl.get("uuid", "")
        if title in ("SP Linked Set", "Liked Tracks & Sets（我点赞的音乐）") or uuid == "enginex-hint":
            continue
        st, raw_tr = ns_http(side, "GET", f"/v1/playlists/{urllib.parse.quote(uuid, safe='')}/tracks?limit=50&offset=0")
        assert st == 200, f"tracks {scope} {uuid}: HTTP {st} {raw_tr}"
        items = json.loads(raw_tr).get("items") or []
        if items:
            chosen_tracks = items
            break
    if not chosen_tracks:
        for pl in playlists:
            uuid = pl.get("uuid", "")
            if uuid == "enginex-hint":
                continue
            st, raw_tr = ns_http(side, "GET", f"/v1/playlists/{urllib.parse.quote(uuid, safe='')}/tracks?limit=50&offset=0")
            items = json.loads(raw_tr).get("items") or []
            if items:
                chosen_tracks = items
                break
    assert chosen_tracks, f"no tracks found in any playlist for scope={scope}: {playlists}"

    tid = int(chosen_tracks[0]["id"])
    st, manifest_raw = ns_http(
        side,
        "GET",
        f"/v1/tracks/{tid}/playbackinfopostpaywall?audioquality=LOSSLESS&playbackmode=STREAM&assetpresentation=FULL",
    )
    assert st == 200, f"manifest {scope} track={tid}: HTTP {st} {manifest_raw}"
    manifest_env = json.loads(manifest_raw)
    if isinstance(manifest_env.get("manifest"), str):
        bts = json.loads(base64.b64decode(manifest_env["manifest"]).decode("utf-8"))
        url = bts["urls"][0]
    else:
        url = manifest_env["urls"][0]
    ext = Path(urllib.parse.urlparse(url).path).suffix.lstrip(".") or "mp3"
    code, audio = ns_http_bytes(side, url)
    assert code == 200 and len(audio) > 8192, f"stream {scope} {url}: HTTP {code} bytes={len(audio)}"
    return audio, ext


def load_and_play_on_b(tag: str, ext: str, audio: bytes) -> int:
    fname = f"ds-{tag}.{ext}"
    dst = WORK_ROOT / "B/data/music" / fname
    dst.parent.mkdir(parents=True, exist_ok=True)
    dst.write_bytes(audio)
    os.chmod(dst, 0o644)
    ch_url = f"file:///data/music/{fname}"

    twin.ev("B", "for(var n=1;n<=4;n++){var p=root.rtTr('/Engine/Deck'+n+'/Play');if(p)p.state=false;} 'paused'")
    twin.ev("B", f"root.rtTr('/Client/Librarian/AddToDatabaseAndLoadToDeck2').url={json.dumps(ch_url)}; 'load'")

    def loaded():
        st = twin.evj(
            "B",
            f"JSON.stringify({{loaded:!!(root.rtTr('/Engine/Deck2/Track/SongLoaded')||{{}}).state, len:(root.rtTr('/Engine/Deck2/Track/TrackLength')||{{}}).unnormalized||0, file:(root.rtTr('/Engine/Deck2/Track/FileName')||{{}}).string||''}})",
            name="poll-ds-load",
        )
        return st if st.get("loaded") and st.get("len", 0) > 0 and st.get("file") == fname else None

    wait_for(loaded, timeout=25.0, interval=0.35, label=f"Engine Deck2 load {fname}")
    time.sleep(0.5)
    twin.ev("B", "root.rtTr('/Engine/Deck2/Track/SetSongPosition').value=0.10; 'seeked'")
    twin.ev("B", "root.rtTr('/Engine/Deck2/Play').state=true; 'play'")
    time.sleep(1.2)
    pos1 = twin.evj(
        "B",
        "JSON.stringify({play:!!(root.rtTr('/Engine/Deck2/PlayState')||{}).state, pos:Math.round((root.rtTr('/Engine/Deck2/Track/TrackPosition')||{}).unnormalized||0)})",
        name="ds-pos1",
    )
    time.sleep(0.5)
    pos2 = twin.evj(
        "B",
        "JSON.stringify({play:!!(root.rtTr('/Engine/Deck2/PlayState')||{}).state, pos:Math.round((root.rtTr('/Engine/Deck2/Track/TrackPosition')||{}).unnormalized||0)})",
        name="ds-pos2",
    )
    twin.ev("B", "var p=root.rtTr('/Engine/Deck2/Play');if(p)p.state=false; 'stop'")
    assert pos1["play"] and pos2["play"], f"{tag}: Engine Deck2 PlayState false ({pos1}, {pos2})"
    assert pos2["pos"] > pos1["pos"] and pos2["pos"] > 20000, f"{tag}: TrackPosition did not advance ({pos1} -> {pos2})"
    return pos2["pos"]


def assert_no_secrets_in_b() -> None:
    for rel in ("data/enginex", "logs/stream-sync-share.log"):
        target = WORK_ROOT / "B" / rel
        if not target.exists():
            continue
        files = [target] if target.is_file() else [p for p in target.rglob("*") if p.is_file()]
        for p in files:
            raw = p.read_bytes()
            for s in ALL_SECRETS:
                assert s.encode() not in raw, f"secret {s} found inside Deck B file {p}"


def main() -> None:
    procs: list[subprocess.Popen] = []
    files_to_close = []
    srv_work = WORK_ROOT / "srv-run"
    ns_a = twin.SIDE_CFG["A"]["netns"]
    ns_b = twin.SIDE_CFG["B"]["netns"]
    try:
        print("==> Booting isolated two-instance SC5000 sandbox (display mode, minimal before DRI+restart)...", flush=True)
        twin.cmd_up(
            argparse.Namespace(
                ref=None,
                fetch=False,
                local=True,
                mode="display",
                delay_ms=2.0,
                jitter_ms=1.1,
                build_hook=False,
                minimal=True,
            )
        )

        fixtures = generate_audio_fixtures(srv_work / "fixtures")
        dav_dir = srv_work / "dav"
        (dav_dir / "DeckA").mkdir(parents=True, exist_ok=True)
        shutil.copy2(fixtures["dav"], dav_dir / "dav-root.mp3")
        shutil.copy2(fixtures["dav"], dav_dir / "DeckA/dav-track.mp3")

        smb_dir = srv_work / "smb"
        smb_dir.mkdir(parents=True, exist_ok=True)
        shutil.copy2(fixtures["smb"], smb_dir / "smb-track.mp3")

        setup_srv_network(ns_a, ns_b)

        # Start real rclone serve webdav
        rclone_log = open(srv_work / "rclone.log", "w")
        files_to_close.append(rclone_log)
        procs.append(
            subprocess.Popen(
                [
                    "ip",
                    "netns",
                    "exec",
                    SRV_NETNS,
                    "rclone",
                    "serve",
                    "webdav",
                    str(dav_dir),
                    "--addr",
                    f"{SRV_IP}:{DAV_PORT}",
                    "--user",
                    DAV_USER,
                    "--pass",
                    DAV_PASS,
                ],
                stdout=rclone_log,
                stderr=subprocess.STDOUT,
            )
        )

        # Start real smbd inside deck-share-srv
        smb_run = srv_work / "smbd"
        smb_run.mkdir(parents=True, exist_ok=True)
        smb_conf = smb_run / "smb.conf"
        smb_conf.write_text(
            f"""[global]
netbios name = LOCALHOST
server role = standalone server
security = user
map to guest = Never
server min protocol = SMB3_00
interfaces = {SRV_IP}
bind interfaces only = yes
disable netbios = yes
smb ports = {SMB_PORT}
private dir = {smb_run}
state directory = {smb_run}
cache directory = {smb_run}
lock directory = {smb_run}
pid directory = {smb_run}
ncalrpc dir = {smb_run}
log file = {smb_run}/log
load printers = no
printing = bsd
printcap name = /dev/null
[music]
path = {smb_dir}
read only = yes
valid users = {SMB_USER}
"""
        )
        subprocess.run(
            ["smbpasswd", "-s", "-a", SMB_USER, "-c", str(smb_conf)],
            input=f"{SMB_PASS}\n{SMB_PASS}\n".encode(),
            check=True,
        )
        r_pipe, w_pipe = os.pipe()
        smbd_log = open(smb_run / "stdout.log", "w")
        files_to_close.append(smbd_log)
        procs.append(
            subprocess.Popen(
                [
                    "ip",
                    "netns",
                    "exec",
                    SRV_NETNS,
                    "smbd",
                    "--foreground",
                    "--no-process-group",
                    f"--configfile={smb_conf}",
                    "--debug-stdout",
                    "--debuglevel=1",
                ],
                stdin=r_pipe,
                stdout=smbd_log,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
        )

        # Start Python mock streaming server in deck-share-srv
        mock_script = srv_work / "mock_streaming.py"
        write_mock_servers_script(mock_script, fixtures)
        mock_log = open(srv_work / "mock.log", "w")
        files_to_close.append(mock_log)
        procs.append(
            subprocess.Popen(
                [
                    "ip",
                    "netns",
                    "exec",
                    SRV_NETNS,
                    "python3",
                    str(mock_script),
                    SRV_IP,
                    str(MOCK_PORT),
                    str(fixtures["ne"]),
                    str(fixtures["am"]),
                    str(fixtures["sc"]),
                    NE_COOKIE,
                    SP_ACCESS,
                    AM_HELPER_TOK,
                    SC_STATIC_TOK,
                ],
                stdout=mock_log,
                stderr=subprocess.STDOUT,
            )
        )

        # Start local test deemixd inside A and B
        deemix_script = srv_work / "deemixd_stub.py"
        write_deemixd_script(deemix_script)
        for side in ("A", "B"):
            dlog = open(srv_work / f"deemix-{side}.log", "w")
            files_to_close.append(dlog)
            procs.append(
                subprocess.Popen(
                    [
                        "ip",
                        "netns",
                        "exec",
                        twin.SIDE_CFG[side]["netns"],
                        "python3",
                        str(deemix_script),
                        "7272",
                        str(WORK_ROOT / side / "data/enginex/state"),
                        str(WORK_ROOT / side / "data/enginex/Stream"),
                        "/data/enginex/Stream",
                        str(fixtures["dz"]),
                        DZ_ARL,
                    ],
                    stdout=dlog,
                    stderr=subprocess.STDOUT,
                )
            )

        # Wait until rclone, smbd, mock server are listening inside deck-share-srv
        for port, label in ((DAV_PORT, "rclone webdav"), (SMB_PORT, "smbd"), (MOCK_PORT, "mock streaming")):
            wait_for(lambda p=port: can_connect_from_ns(SRV_NETNS, SRV_IP, p), timeout=15.0, label=label)

        mock_base = f"http://{SRV_IP}:{MOCK_PORT}"
        seed_deck_a(mock_base)

        # Verify B is clean before starting
        assert not (WORK_ROOT / "B/data/enginex/ui/nas.json").exists(), "Deck B must start without nas.json"
        for f in ("deezer-owner.json", "netease-owner.json", "spotify-token.json", "applemusic-owner.json", "soundcloud-owner.json"):
            assert not (WORK_ROOT / "B/data/enginex/state" / f).exists(), f"Deck B unexpectedly had {f}"

        print("==> Restarting stream-sync and Engine (with /dev/dri/card0) on A and B...", flush=True)
        restart_streamsync("A", mock_base)
        restart_streamsync("B", mock_base)
        twin.both(restart_engine_with_dri)
        info = twin.both(twin.wait_engine_ready)
        print(f"==> Engine ready on A & B: A={info[0]} B={info[1]}", flush=True)
        code_a = (WORK_ROOT / "A/data/enginex/state/access-token").read_text().strip()
        assert code_a and "-" in code_a, f"unexpected Deck A access-token: {code_a!r}"

        # ================= ACCEPTANCE 1: OVER LINK =================
        assert not can_connect_from_ns(ns_b, SRV_IP, DAV_PORT), "B must NOT be able to reach WebDAV IP directly in Acceptance 1"
        assert not can_connect_from_ns(ns_b, SRV_IP, SMB_PORT), "B must NOT be able to reach SMB IP directly in Acceptance 1"

        def b_has_both_linked():
            st, raw = ns_http("B", "GET", "/v1/enginex/ui")
            if st != 200:
                return None
            v = json.loads(raw)
            linked = v.get("linkedServers") or []
            kinds = {it.get("kind"): it for it in linked}
            if "webdav" in kinds and "nas" in kinds:
                return v
            return None

        ui_b1 = wait_for(b_has_both_linked, timeout=30.0, interval=0.5, label="B discovering A's WebDAV & NAS over Link")
        linked_by_kind = {it["kind"]: it for it in ui_b1["linkedServers"]}
        dav_scope = linked_by_kind["webdav"]["scope"]
        nas_scope = linked_by_kind["nas"]["scope"]
        assert dav_scope in ui_b1["sources"] and dav_scope in ui_b1["loggedIn"]
        assert nas_scope in ui_b1["sources"] and nas_scope in ui_b1["loggedIn"]

        twin.ev("B", "root.scCall(''); 'sync'")
        qml_entries = wait_for(
            lambda: (lambda es: es if any(e.get("id") == dav_scope for e in es) and any(e.get("id") == nas_scope for e in es) else None)(
                twin.evj("B", "JSON.stringify(root.scSourceEntries)")
            ),
            timeout=15.0,
            interval=0.5,
            label="QML scSourceEntries showing linked WebDAV + NAS",
        )

        # Ensure /peer/servers and uiStatus never leak passwords or URLs
        _, peer_json = ns_http("B", "GET", "/peer/servers", base=f"http://{twin.SIDE_CFG['A']['ip']}:8080")
        assert DAV_PASS not in peer_json and SMB_PASS not in peer_json and SRV_IP not in peer_json
        assert DAV_PASS not in json.dumps(ui_b1) and SMB_PASS not in json.dumps(ui_b1) and SRV_IP not in json.dumps(ui_b1)

        dav_audio, dav_ext = fetch_first_playable("B", dav_scope)
        pos_dav = load_and_play_on_b("link-dav", dav_ext, dav_audio)
        nas_audio, nas_ext = fetch_first_playable("B", nas_scope)
        pos_nas = load_and_play_on_b("link-nas", nas_ext, nas_audio)
        assert_no_secrets_in_b()
        print(
            f"PASS [Acceptance 1 - Over Link]: B QML sources={[e['id'] for e in qml_entries]}; "
            f"WebDAV ({dav_scope}) played TrackPosition={pos_dav}; "
            f"NAS ({nas_scope}) played TrackPosition={pos_nas}; zero passwords on B",
            flush=True,
        )

        # ================= ACCEPTANCE 3a: WRONG CODE & RATE LIMIT =================
        wait_for(
            lambda: twin.SIDE_CFG["A"]["ip"] in (json.loads(ns_http("B", "GET", "/v1/enginex/deck-share")[1]).get("peers") or []),
            timeout=15.0,
            interval=0.4,
            label="deck-share peer discovery",
        )
        statuses = []
        for i in range(5):
            st, _ = ns_http("B", "POST", "/v1/enginex/deck-share", {"action": "pull", "peer": twin.SIDE_CFG["A"]["ip"], "code": f"BAD{i}-000{i}"})
            statuses.append(st)
        assert statuses == [401, 401, 401, 401, 429], f"unexpected wrong-code statuses: {statuses}"

        st_locked, _ = ns_http("B", "POST", "/v1/enginex/deck-share", {"action": "pull", "peer": twin.SIDE_CFG["A"]["ip"], "code": code_a})
        assert st_locked == 429, f"right code must be 429 during lockout, got {st_locked}"
        assert not (WORK_ROOT / "B/data/enginex/ui/nas.json").exists(), "nas.json created during lockout"
        assert not (WORK_ROOT / "B/data/enginex/state/deezer-owner.json").exists(), "deezer-owner.json created during lockout"
        print(
            f"PASS [Acceptance 3a - Wrong Code Rate Limit]: wrong codes={statuses}; "
            f"right code during lockout={st_locked}; nothing moved to B. Waiting 61s for lockout expiry...",
            flush=True,
        )
        time.sleep(61.5)

        # ================= ACCEPTANCE 2: DEVICE CODE TRANSFER =================
        enable_b_srv_link(ns_b)
        assert can_connect_from_ns(ns_b, SRV_IP, DAV_PORT) and can_connect_from_ns(ns_b, SRV_IP, SMB_PORT)

        st_pull, raw_pull = ns_http("B", "POST", "/v1/enginex/deck-share", {"action": "pull", "peer": twin.SIDE_CFG["A"]["ip"], "code": code_a})
        assert st_pull == 200, f"deck-share pull failed: HTTP {st_pull} {raw_pull}"
        for sec in ALL_SECRETS:
            assert sec not in raw_pull, f"deck-share pull response leaked {sec}"
        pull_obj = json.loads(raw_pull)
        summary = pull_obj["summary"]
        assert pull_obj["undo"] is True
        for want_svc in ("deezer", "netease", "spotify", "applemusic", "soundcloud"):
            assert want_svc in summary["copied"], f"missing {want_svc} in copied: {summary}"
        assert "Deck A WebDAV" in (summary.get("webdav") or []) and "Deck A NAS" in (summary.get("nas") or []), f"missing servers in {summary}"
        skipped_text = "\n".join(summary.get("skipped") or [])
        assert "beatport" in skipped_text and "tidal" in skipped_text, f"expected beatport/tidal in skipped: {summary}"

        # Excluded credentials did not move to B
        b_state = WORK_ROOT / "B/data/enginex/state"
        for excl in ("spotify-librespot.json", "soundcloud-token.json", "beatport-owner.json", "tidal-auth.json"):
            assert not (b_state / excl).exists(), f"excluded {excl} appeared on B"
        if (b_state / "spotify-device-id").is_file():
            assert "deck-a-device-id-only" not in (b_state / "spotify-device-id").read_text()

        # Stop A's stream-sync completely to prove B runs independently
        stop_streamsync("A")
        _, raw_ui_b2 = ns_http("B", "GET", "/v1/enginex/ui")
        ui_b2 = json.loads(raw_ui_b2)
        for want_svc in ("deezer", "netease", "spotify", "applemusic", "soundcloud", "nas"):
            assert want_svc in (ui_b2.get("loggedIn") or []), f"{want_svc} missing in B loggedIn: {ui_b2}"
        assert ui_b2.get("webdavOn") and ui_b2.get("nasOn") and ui_b2.get("webdav"), f"servers missing on B: {ui_b2}"
        wd_scope_b = ui_b2["webdav"][0]["scope"]

        played = {}
        for tag, scope in (
            ("deezer", "deezer"),
            ("netease", "netease"),
            ("spotify", "spotify"),
            ("applemusic", "applemusic"),
            ("soundcloud", "soundcloud"),
            ("webdav", wd_scope_b),
            ("nas", "nas"),
        ):
            audio, ext = fetch_first_playable("B", scope)
            pos = load_and_play_on_b(f"copy-{tag}", ext, audio)
            played[tag] = pos

        print(
            f"PASS [Acceptance 2 - Device Code Transfer]: copied={summary['copied']} "
            f"webdav={summary['webdav']} nas={summary['nas']} skipped={summary['skipped']}; "
            f"played on real Engine with A stopped: {played}",
            flush=True,
        )

        # ================= ACCEPTANCE 3b: SUMMARY & UNDO =================
        twin.ev("B", "root.scSharePoll(); 'poll'")
        qml_share = wait_for(
            lambda: (lambda s: s if s.get("undo") else None)(twin.evj("B", "JSON.stringify(root.scShareState)")),
            timeout=10.0,
            interval=0.3,
            label="QML scShareState.undo=true",
        )
        assert qml_share.get("lastCopy") and "deezer" in qml_share["lastCopy"].get("copied", [])

        st_undo, raw_undo = ns_http("B", "POST", "/v1/enginex/deck-share", {"action": "undo"})
        assert st_undo == 200 and json.loads(raw_undo).get("undo") is False, f"undo failed: HTTP {st_undo} {raw_undo}"

        assert not (WORK_ROOT / "B/data/enginex/ui/nas.json").exists(), "nas.json still exists after undo"
        for removed in ("deezer-owner.json", "netease-owner.json", "spotify-token.json", "applemusic-owner.json", "soundcloud-owner.json"):
            assert not (b_state / removed).exists(), f"{removed} still exists after undo"

        _, raw_ui_b3 = ns_http("B", "GET", "/v1/enginex/ui")
        ui_b3 = json.loads(raw_ui_b3)
        for svc in ("deezer", "netease", "spotify", "applemusic", "soundcloud"):
            assert svc not in (ui_b3.get("loggedIn") or []), f"{svc} still in loggedIn after undo: {ui_b3}"
        assert not ui_b3.get("webdavOn") and not ui_b3.get("nasOn")

        twin.ev("B", "root.scSharePoll(); 'poll-undo'")
        qml_after_undo = wait_for(
            lambda: (lambda s: s if not s.get("undo") else None)(twin.evj("B", "JSON.stringify(root.scShareState)")),
            timeout=10.0,
            interval=0.3,
            label="QML scShareState.undo=false",
        )
        print(
            f"PASS [Acceptance 3b - Summary & Undo]: QML undo toggled {qml_share['undo']} -> {qml_after_undo['undo']}; "
            f"all copied logins & nas.json removed cleanly",
            flush=True,
        )
        print("ALL DECK-SHARE SANDBOX ACCEPTANCE CHECKS (1, 2, 3) PASSED", flush=True)
    finally:
        for p in procs:
            try:
                p.terminate()
            except Exception:
                pass
        for p in procs:
            try:
                p.wait(timeout=2)
            except Exception:
                try:
                    p.kill()
                except Exception:
                    pass
        for f in files_to_close:
            try:
                f.close()
            except Exception:
                pass
        try:
            twin.cmd_down(argparse.Namespace(clean=True, purge=True), quiet=True)
        except Exception:
            pass
        teardown_srv_network(ns_a, ns_b)
        shutil.rmtree(WORK_ROOT, ignore_errors=True)


if __name__ == "__main__":
    main()
