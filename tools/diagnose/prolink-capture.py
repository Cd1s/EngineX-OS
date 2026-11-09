#!/usr/bin/env python3
"""EngineX OS: read-only Pro DJ Link capture for a PRIME 4 / SC5000 + CDJ-3000 setup.

Run it on a computer plugged into the SAME network switch/router as the PRIME 4 and the CDJ.
It only listens to the broadcasts the players already send, plus a few read-only port checks.
It never sends commands to the decks, never changes settings and never flashes anything.

    python3 prolink-capture.py                       # 3 minutes, auto-detects devices
    python3 prolink-capture.py --seconds 300 --prime4 192.168.1.50 --cdj 192.168.1.60

While it runs: load a rekordbox-analysed track on the CDJ, press play, then pause.
At the end it writes two files next to itself; attach both to the GitHub issue:
    prolink-capture-<time>.txt    the summary
    prolink-capture-<time>.jsonl  every packet it heard (raw hex)
Needs only Python 3.8+ (Windows: install from python.org; allow it in the firewall prompt).
"""
import argparse, json, os, platform, re, select, socket, struct, subprocess, sys, time, urllib.request

MAGIC = b"Qspt1WmJOL"
PORTS = {50000: "prolink-announce", 50001: "prolink-beat", 50002: "prolink-status",
         51337: "stagelinq-discovery", 11224: "engine-eaas"}


def name_at(b, off):
    return b[off:off + 20].split(b"\0", 1)[0].decode("ascii", "replace").strip()


def decode(port, b):
    """Best-effort decode of one Pro DJ Link packet (layouts from the public dysentery docs)."""
    if not b.startswith(MAGIC) or len(b) < 0x24:
        return None
    t = b[0x0A]
    if port == 50000:  # announce / keep-alive: name at 0x0c
        d = {"kind": "announce", "type": t, "name": name_at(b, 0x0C)}
        if t == 0x06 and len(b) >= 0x30:
            d.update(player=b[0x24], ip=socket.inet_ntoa(b[0x2C:0x30]))
        return d
    d = {"type": t, "name": name_at(b, 0x0B), "player": b[0x21]}  # 50001/50002: name at 0x0b
    if port == 50001 and t == 0x28 and len(b) >= 0x5C:
        d.update(kind="beat", bpm=struct.unpack(">H", b[0x5A:0x5C])[0] / 100)
    elif port == 50002 and t == 0x0A and len(b) >= 0x94:
        pitch = struct.unpack(">I", b[0x8C:0x90])[0]
        d.update(kind="cdj-status", source_player=b[0x28], slot=b[0x29], track_type=b[0x2A],
                 rekordbox_id=struct.unpack(">I", b[0x2C:0x30])[0],
                 playing=bool(b[0x89] & 0x40), master=bool(b[0x89] & 0x20), on_air=bool(b[0x89] & 0x08),
                 pitch_pct=round((pitch - 0x100000) / 0x100000 * 100, 2),
                 bpm=struct.unpack(">H", b[0x92:0x94])[0] / 100)
    else:
        d["kind"] = "other"
    return d


def listen(port):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    if hasattr(socket, "SO_REUSEPORT"):
        try:
            s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
        except OSError:
            pass
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    s.bind(("", port))
    s.setblocking(False)
    return s


def tcp_open(ip, port, timeout=2.0):
    try:
        with socket.create_connection((ip, port), timeout=timeout):
            return "open"
    except OSError as e:
        return "closed/filtered (%s)" % type(e).__name__


def dbserver_port(ip):
    """Ask the CDJ which TCP port its rekordbox database server uses (read-only query)."""
    try:
        with socket.create_connection((ip, 12523), timeout=2) as c:
            c.sendall(b"\x00\x00\x00\x0fRemoteDBServer\x00")
            c.settimeout(2)
            r = c.recv(2)
            return struct.unpack(">H", r)[0] if len(r) == 2 else None
    except OSError:
        return None


def http_title(url):
    try:
        with urllib.request.urlopen(url, timeout=4) as r:
            body = r.read(4096).decode("utf-8", "replace")
            m = re.search(r"<title>([^<]*)</title>", body)
            return "HTTP %d %s" % (r.status, m.group(1) if m else "")
    except urllib.error.HTTPError as e:
        return "HTTP %d" % e.code
    except OSError as e:
        return "no answer (%s)" % type(e).__name__


def local_network():
    cmd = ["ipconfig"] if os.name == "nt" else (["ip", "-4", "-o", "addr"] if sys.platform.startswith("linux") else ["ifconfig"])
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=10).stdout
    except (OSError, subprocess.SubprocessError):
        return "(could not run %s)" % cmd[0]
    out = re.sub(r"(?i)\b([0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b", "<mac>", out)  # no hardware ids in the report
    keep = [l for l in out.splitlines() if re.search(r"inet |IPv4|169\.254|^\S", l)]
    return "\n".join(keep[:60])


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--seconds", type=int, default=180)
    ap.add_argument("--prime4", help="IP of the PRIME 4 / SC5000 (optional)")
    ap.add_argument("--cdj", action="append", default=[], help="IP of a CDJ (optional, repeatable)")
    a = ap.parse_args()

    stamp = time.strftime("%Y%m%d-%H%M%S")
    here = os.path.dirname(os.path.abspath(__file__))
    raw_path = os.path.join(here, "prolink-capture-%s.jsonl" % stamp)
    txt_path = os.path.join(here, "prolink-capture-%s.txt" % stamp)
    socks, report = {}, []
    say = lambda s="": (print(s), report.append(s))

    say("EngineX OS Pro DJ Link capture  %s  (%s %s, Python %s)" % (stamp, platform.system(), platform.release(), platform.python_version()))
    for port, label in PORTS.items():
        try:
            socks[listen(port)] = port
            say("listening  UDP %-5d %s" % (port, label))
        except OSError as e:
            say("CANNOT listen on UDP %d (%s): close rekordbox / Engine DJ / other DJ apps and run again" % (port, e))
    say("\nCapturing for %d s. Now: load an analysed track on the CDJ, press PLAY, then PAUSE.\n" % a.seconds)

    devices, status, counts, other_src = {}, {}, {}, {}
    end, last_print = time.time() + a.seconds, 0
    with open(raw_path, "w") as raw:
        while time.time() < end:
            ready, _, _ = select.select(list(socks), [], [], 1.0)
            for s in ready:
                try:
                    b, (src, _) = s.recvfrom(4096)
                except OSError:
                    continue
                port = socks[s]
                d = decode(port, b) if port < 51000 else None
                counts[port] = counts.get(port, 0) + 1
                raw.write(json.dumps({"t": round(time.time(), 3), "port": port, "src": src, "hex": b.hex(), "decoded": d}) + "\n")
                if d and d.get("kind") == "announce" and "player" in d:
                    key = (src, d["player"])
                    if key not in devices:
                        say("NEW device   %-15s player %d  name '%s'" % (src, d["player"], d["name"]))
                    devices[key] = d["name"]
                elif d and d.get("kind") == "cdj-status":
                    status[d["player"]] = (src, d)
                elif port >= 51000 and src not in other_src.get(port, set()):
                    other_src.setdefault(port, set()).add(src)
                    text = re.sub(r"[^ -~]", "", b.replace(b"\0", b"").decode("ascii", "replace"))[:60]
                    say("NEW %s from %s  %s" % (PORTS[port], src, text))
            if time.time() - last_print >= 5 and status:
                last_print = time.time()
                for p, (src, d) in sorted(status.items()):
                    print("  player %d  %-15s rekordbox_id=%-8d from player %d slot %d  bpm=%.2f pitch=%+.2f%%  %s%s" % (
                        p, src, d["rekordbox_id"], d["source_player"], d["slot"], d["bpm"], d["pitch_pct"],
                        "PLAYING" if d["playing"] else "paused", " MASTER" if d["master"] else ""))

    say("\n=== Summary ===")
    say("packets per port: " + ", ".join("%d=%d" % (p, counts.get(p, 0)) for p in PORTS))
    say("Pro DJ Link devices heard:")
    for (src, num), name in sorted(devices.items(), key=lambda kv: kv[0][1]):
        say("  player %d  %-15s '%s'%s" % (num, src, name, "   <- EngineX bridge on the Denon" if name.startswith("EngineX") else ""))
    if not devices:
        say("  NONE. This computer hears no Pro DJ Link at all: it must be on the same switch/router as the CDJ "
            "(a direct cable between PRIME 4 and CDJ leaves the computer outside that network).")
    elif not any(n.startswith("EngineX") for n in devices.values()):
        say("  No 'EngineX' device: the Denon is not taking part in Pro DJ Link (feature off, older EngineX build, or not on this network).")
    for p, (src, d) in sorted(status.items()):
        say("  last status player %d: %s" % (p, json.dumps(d)))
    for port, srcs in other_src.items():
        say("  %s seen from: %s" % (PORTS[port], ", ".join(sorted(srcs))))

    cdjs = set(a.cdj) | {src for (src, _), name in devices.items() if not name.startswith("EngineX")}
    say("\n=== Read-only checks ===")
    for ip in sorted(cdjs):
        port = dbserver_port(ip)
        say("CDJ %-15s dbserver port query: %s; NFS 2049: %s; portmap 111: %s" % (
            ip, port if port else "no answer", tcp_open(ip, 2049), tcp_open(ip, 111)))
        if port:
            say("    dbserver tcp %d: %s" % (port, tcp_open(ip, port)))
    prime = a.prime4 or next((src for (src, _), name in devices.items() if name.startswith("EngineX")), None)
    if not prime:
        prime = next(iter(sorted(other_src.get(51337, set()) | other_src.get(11224, set()))), None)
    if prime:
        say("Denon %-15s web console: %s; /v1/sc5000/ui: %s" % (
            prime, http_title("http://%s:8080/" % prime), http_title("http://%s:8080/v1/sc5000/ui" % prime)))
    else:
        say("Denon: not found (pass --prime4 <IP> to check its web console)")

    say("\n=== This computer's network ===")
    say(local_network())
    with open(txt_path, "w") as f:
        f.write("\n".join(report) + "\n")
    print("\nDone. Please attach these two files to the GitHub issue:\n  %s\n  %s" % (txt_path, raw_path))


def selftest():
    ka = bytearray(0x36); ka[:10] = MAGIC; ka[0x0A] = 0x06; ka[0x0C:0x14] = b"CDJ-3000"
    ka[0x24] = 4; ka[0x2C:0x30] = socket.inet_aton("169.254.1.4")
    assert decode(50000, bytes(ka)) == {"kind": "announce", "type": 6, "name": "CDJ-3000", "player": 4, "ip": "169.254.1.4"}
    st = bytearray(0x100); st[:10] = MAGIC; st[0x0A] = 0x0A; st[0x0B:0x13] = b"CDJ-3000"; st[0x21] = 4
    st[0x28], st[0x29], st[0x2A] = 4, 3, 1; st[0x2C:0x30] = struct.pack(">I", 1234); st[0x89] = 0x60
    st[0x8C:0x90] = struct.pack(">I", 0x100000 - 0x100000 * 57 // 1000); st[0x92:0x94] = struct.pack(">H", 17500)
    d = decode(50002, bytes(st))
    assert (d["player"], d["rekordbox_id"], d["bpm"], d["playing"], d["master"], d["slot"]) == (4, 1234, 175.0, True, True, 3), d
    assert abs(d["pitch_pct"] + 5.7) < 0.01, d
    assert decode(50000, b"junk") is None
    print("selftest ok")


if __name__ == "__main__":
    selftest() if "--selftest" in sys.argv else main()
