#!/usr/bin/env python3
"""Helpers of test_console_link_sandbox.sh (pair mode). Two jobs, started inside two network namespaces:
  player <secs>        the linked player: its QML posts two decks (player number 1, deck 3 on its active layer) twice a second
                       and asks its own stream-sync what it shows; every answer is logged (PLAYERVIEW).
  sample <engine.log>  the console side: at the end of each phase of console-link-probe.qml it reads the console's
                       stream-sync (is its deck loop running, who are its peers), whether UDP 50199 is bound in this
                       namespace, and, in the phases with a Line channel, the lanes it would answer (SAMPLE).
Only loopback requests; nothing is written but to stdout."""
import json
import re
import sys
import time
import urllib.request

BASE = "http://127.0.0.1:8080"
DATA = "enginex"  # the directory under /data that holds Stream/a.mp3 in the player's namespace
LINES = {"off": "", "on-noline": "", "on-line3": "3", "on-line34": "3,4", "release4": "3", "on-line234": "2,3,4", "cdj-prolink-off": "3", "release-all": "", "off-again": ""}
LEN_MS, SAMPLE_AT_MS = 18000, 15500


def get(path, timeout=4):
    with urllib.request.urlopen(BASE + path, timeout=timeout) as r:
        return json.loads(r.read() or b"{}")


def post(path, obj):
    req = urllib.request.Request(BASE + path, data=json.dumps(obj).encode(), method="POST")
    with urllib.request.urlopen(req, timeout=4) as r:
        return r.read()


def udp_bound(port):
    want = ":%04X" % port
    for f in ("/proc/net/udp", "/proc/net/udp6"):
        try:
            for line in open(f).read().splitlines()[1:]:
                if line.split()[1].upper().endswith(want):
                    return True
        except OSError:
            pass
    return False


def player(secs):
    end = time.time() + secs
    last = None
    while time.time() < end:
        now = int(time.time() * 1000)
        decks = [{"deck": d, "title": "Player one deck %d" % d, "net": "net://u/(Unknown)/%s/Stream/a.mp3" % DATA, "playing": True, "positionMs": 2000, "at": now, "player": 1, "active": d == 3} for d in (3, 4)]
        try:
            post("/v1/enginex/decks", decks)
            r = get("/v1/enginex/shadow")
            seen = sorted((d["deck"], d.get("title", "")) for d in (r.get("decks") or []))
            print("PLAYERVIEW|%d|%s" % (int(time.time() * 1000), json.dumps(seen)), flush=True)
            last = seen
        except Exception as e:
            print("PLAYERVIEW|error|%s" % e, flush=True)
        time.sleep(0.5)
    try:
        d = get("/v1/enginex/debug/shadow")
        print("PLAYERDEBUG|peers=%s|running=%s" % (sorted((d.get("peers") or {}).keys()), d.get("running")), flush=True)
    except Exception as e:
        print("PLAYERDEBUG|error|%s" % e, flush=True)


def sample(log_path):
    seen = set()
    pos = 0
    pending = []  # (due epoch ms, name)
    deadline = time.time() + 400
    while time.time() < deadline:
        try:
            data = open(log_path, "rb").read().decode("utf-8", "replace")
        except OSError:
            data = ""
        for m in re.finditer(r"CLPHASE\|BEGIN\|([^|]+)\|(\d+)", data[pos:]):
            if m.group(1) not in seen:
                seen.add(m.group(1))
                pending.append((int(m.group(2)) + SAMPLE_AT_MS, m.group(1)))
        pos = len(data)
        for due, name in list(pending):
            if time.time() * 1000 >= due:
                pending.remove((due, name))
                row = {"phase": name}
                try:
                    d = get("/v1/enginex/debug/shadow")
                    peers = d.get("peers") or {}  # address -> age in ms; the table keeps a silent peer, only the fresh ones (6 s) count
                    row.update(running=d.get("running"), peers=len(peers), fresh=sum(1 for age in peers.values() if age <= 6000), own=len(d.get("own") or []))
                except Exception as e:
                    row["debugError"] = str(e)
                row["udp50199"] = udp_bound(50199)
                if LINES.get(name):  # asking with a Line channel is what a polling console does; idle phases never ask
                    try:
                        r = get("/v1/enginex/shadow?line=" + LINES[name])
                        row["lanes"] = [(x["deck"], x.get("title", ""), x.get("player"), x.get("model"), bool(x.get("lineIn")), bool(x.get("displayOnly")), x.get("path", ""), x.get("net", "")) for x in (r.get("decks") or [])]
                    except Exception as e:
                        row["lanesError"] = str(e)
                print("SAMPLE|" + json.dumps(row), flush=True)
        if "off-again" in seen and not pending:
            break
        time.sleep(0.4)


if sys.argv[1] == "player":
    player(float(sys.argv[2]))
else:
    sample(sys.argv[2])
