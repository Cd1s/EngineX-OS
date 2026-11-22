#!/usr/bin/env python3
"""Stand-in for stream-sync in test_console_link_sandbox.sh (stub mode): serves what the console's QML asks for
and counts every request by kind. The Line In view's answer is built the way the real one is: the linked units present,
in ascending player number, on the channels the QML names in ?line=, each with its player number and model; the
answer is held 1 s while nothing changes (stream-sync's quiet poll). usage: console_link_stub.py <stats.jsonl> [stage1|send|player]
send (console linking, stage 2, the console side): also answers the console QML's held request
(GET /v1/enginex/consolesend?asked=N, held up to 25 s until "somebody asks" differs from N; POST /ctl?asked=0|1 sets it)
and counts it. player: this process stands in for a player's stream-sync (the JC11 Engine's QML is told it is a player in
the 4-deck layout): with the row on (POST /ctl?row=1) its lane answer shows two console decks on the free lanes 3 and 4."""
import collections
import http.server
import json
import socketserver
import struct
import sys
import threading
import time
import urllib.parse
import zlib

stats_file = sys.argv[1]
MODE = sys.argv[2] if len(sys.argv) > 2 else "stage1"
UNITS = [  # the linked units: player one is an SC6000, player two a CDJ-3000
    {"player": 1, "model": "SC6000", "title": "Linked player one", "artist": "SC6000 deck 1", "bpm": 126.0, "color": (74, 120, 255, 255)},
    {"player": 2, "model": "CDJ-3000", "title": "Linked player two", "artist": "CDJ-3000", "bpm": 124.0, "color": (255, 150, 40, 255)},
    {"player": 3, "model": "SC5000", "title": "Linked player three", "artist": "SC5000 deck 3", "bpm": 128.0, "color": (90, 220, 120, 255)},
]
state = {"consoleLineView": False, "units": 2, "cdjoff": False, "asked": False, "row": False}  # units: how many of the linked units are on the network (the probe sets it per phase: POST /ctl?units=N&cdjoff=0|1: with 1 the only unit is the CDJ, Pro DJ Link off)
counts = collections.Counter()
lock = threading.Lock()
version = {"n": 1, "sig": ""}
start = time.time()


def chunk(kind, data):
    body = kind + data
    return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)


def tile(color):
    w, h = 2048, 128
    rows = []
    for y in range(h):
        row = bytearray([0])
        for x in range(w):
            height = 8 + (x // 13) % 50
            row.extend(color if abs(y - 64) < height else (0, 0, 0, 0))
        rows.append(bytes(row))
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(b"".join(rows))) + chunk(b"IEND", b"")


TILES = {u["player"]: tile(u["color"]) for u in UNITS}


def lanes(lines):
    now = int(time.time() * 1000)
    out = []
    if state["cdjoff"]:  # the only unit present is the CDJ and this unit's Pro DJ Link switch is off: metadata only, with the reason
        u = UNITS[1]
        for ch in sorted(lines)[:1]:
            out.append({"deck": ch, "key": "cdj:127.0.0.1:%d:3:7:" % u["player"], "title": u["title"], "artist": u["artist"], "bpm": u["bpm"],
                        "lineIn": True, "displayOnly": True, "displayDetail": False, "metadataOnly": True, "playing": True, "fallback": "prolink-off",
                        "positionMs": 20000, "durationMs": 240000, "at": now, "player": u["player"], "model": u["model"]})
        return out
    for i, ch in enumerate(sorted(lines)):
        if i >= state["units"]:
            break
        u = UNITS[i]
        out.append({"deck": ch, "key": "peer-%d-%d" % (u["player"], ch), "title": u["title"], "artist": u["artist"], "bpm": u["bpm"],
                    "lineIn": True, "displayOnly": True, "displayDetail": True, "metadataOnly": False, "playing": True,
                    "positionMs": 20000, "durationMs": 240000, "at": now, "player": u["player"], "model": u["model"]})
    return out


def consoleLanes():
    """Two decks of a console on a player's free lanes 3 and 4, as the player's stream-sync answers with the row on."""
    now = int(time.time() * 1000)
    out = []
    for n, lane in ((1, 3), (2, 4)):
        out.append({"deck": lane, "key": "console-%d" % n, "title": "Console deck %d" % n, "artist": "PRIME 4", "bpm": 126.0 + n, "console": n, "model": "PRIME4",
                    "displayOnly": True, "displayDetail": True, "playing": True, "positionMs": 20000 * n, "durationMs": 240000, "at": now, "link": "{console}"})
    return out


def ui():
    if MODE == "player":
        return {"version": "5.3.0.6", "player": True, "fourDeckWaveform": True, "fourDeckRole": "12", "fourDeckRoleActive": "12", "pairedView": False,
                "shadowMode": "auto", "sources": ["tidal"], "consoleDecks": state["row"], "consoleLineView": False}
    return {"version": "5.3.0.6", "player": False, "fourDeckWaveform": False, "fourDeckRole": "auto", "pairedView": False,
            "shadowMode": "auto", "sources": ["tidal"], "consoleLineView": state["consoleLineView"]}


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def reply(self, obj, headers=None):
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        u = urllib.parse.urlsplit(self.path)
        q = urllib.parse.parse_qs(u.query)
        with lock:
            counts["GET " + u.path] += 1
        if u.path == "/v1/enginex/ui":
            return self.reply(ui())
        if u.path == "/v1/enginex/views":
            return self.reply({"active": 1, "views": {}})
        if u.path == "/v1/enginex/consolesend":
            believed = q.get("asked", ["0"])[0] == "1"
            end = time.time() + 25
            while time.time() < end and state["asked"] == believed:
                time.sleep(0.1)
            return self.reply({"asked": state["asked"]})
        if u.path == "/v1/enginex/shadow" and MODE == "player":
            decks = consoleLanes() if state["row"] else []
            time.sleep(0.25)
            return self.reply({"role": "12", "decks": decks, "ver": 1 + len(decks), "debug": False, "displayOnly": False})
        if u.path == "/v1/enginex/shadow":
            lines = [int(c) for c in q.get("line", [""])[0].split(",") if c.strip().isdigit()]
            with lock:
                if lines:
                    counts["lines " + ",".join(str(c) for c in sorted(lines))] += 1
            if not state["consoleLineView"] or not lines:
                return self.reply({"role": "", "decks": [], "ver": version["n"], "debug": False, "displayOnly": True})
            decks = lanes(lines)
            sig = json.dumps([(d["deck"], d["key"]) for d in decks])
            if sig != version["sig"]:
                version["sig"] = sig
                version["n"] += 1
            if q.get("ver", [""])[0] == str(version["n"]):
                time.sleep(1.0)  # stream-sync holds an unchanged answer for its quiet poll
            # stream-sync leaves the header out when a lane is metadata only (a CDJ): such an answer always reaches the QML
            headers = {} if any(d["metadataOnly"] for d in decks) else {"X-Shadow-State": "%d:%d" % (version["n"], hash(sig) & 0xFFFFFF)}
            return self.reply({"role": "line", "decks": decks, "ver": version["n"], "debug": False, "displayOnly": True}, headers)
        if u.path == "/v1/enginex/shadow/display":
            key = q.get("key", [""])[0]
            with lock:
                counts["display of " + key] += 1
            player = int(key.split("-")[1]) if key.startswith(("peer-", "console-")) else 1
            base = "http://127.0.0.1:8080/tile.png?player=%d" % player
            unit = next((x for x in UNITS if x["player"] == player), UNITS[0])
            return self.reply({"key": key, "title": unit["title"], "artist": unit["artist"], "bpm": unit["bpm"], "keyName": "8A", "sampleRate": 44100,
                               "durationMs": 240000, "beats": [{"ms": 476 * i, "number": i % 4} for i in range(40)], "cues": [{"i": 1, "ms": 30000, "n": "DROP", "c": "#ffb400"}],
                               "loops": [], "detail": True, "detailPending": False, "waveformUrl": base, "waveformTiles": [base], "waveformWidth": 2048,
                               "waveformDurationMs": 240000, "tileWidth": 2048, "overviewUrl": base + "&overview=1", "artworkUrl": ""})
        if u.path in ("/tile.png",):
            body = TILES.get(int(q.get("player", ["1"])[0]), TILES[1])
            self.send_response(200)
            self.send_header("Content-Type", "image/png")
            self.end_headers()
            self.wfile.write(body)
            return
        return self.reply({})

    def do_POST(self):
        u = urllib.parse.urlsplit(self.path)
        n = int(self.headers.get("Content-Length", "0") or "0")
        if n:
            self.rfile.read(n)
        with lock:
            counts["POST " + u.path] += 1
        if u.path == "/ctl":
            q = urllib.parse.parse_qs(u.query)
            state["units"] = max(0, min(len(UNITS), int(q.get("units", ["2"])[0])))
            state["cdjoff"] = q.get("cdjoff", ["0"])[0] == "1"
            state["asked"] = q.get("asked", ["0"])[0] == "1"
            state["row"] = q.get("row", ["0"])[0] == "1"
            return self.reply({"units": state["units"], "cdjoff": state["cdjoff"], "asked": state["asked"], "row": state["row"]})
        if u.path == "/v1/enginex/ui":
            for k, v in urllib.parse.parse_qs(u.query).items():
                if k == "consoleLineView":
                    state["consoleLineView"] = v[0] in ("1", "true")
            return self.reply(ui())
        return self.reply({"ok": True})


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


def dump():
    while True:
        time.sleep(1)
        with lock:
            line = json.dumps({"t": round(time.time(), 3), "counts": dict(counts)})
        with open(stats_file, "a") as f:
            f.write(line + "\n")


threading.Thread(target=dump, daemon=True).start()
Server(("127.0.0.1", 8080), Handler).serve_forever()
