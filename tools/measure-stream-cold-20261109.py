#!/usr/bin/env python3
"""PM-only deck measurements: metadata GET timing or passive file-size sampling.

No playback, stream, playback-info, cache deletion, credentials or log bodies.
Run only after PM has released the performance-test reservation.
"""

import argparse
import re
import shlex
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--deck", choices=("L", "R"), required=True)
    parser.add_argument("--playlist", help="UUID already visible in Engine")
    parser.add_argument("--repeat", type=int, default=3)
    parser.add_argument("--observe-seconds", type=int, default=0,
                        help="passively sample sizes while PM manually loads a track")
    args = parser.parse_args()
    if not 1 <= args.repeat <= 5 or not 0 <= args.observe_seconds <= 35:
        parser.error("repeat must be 1..5; observe-seconds must be 0..35")
    if args.observe_seconds:
        # No names or file contents are printed. Aggregates can contain unrelated
        # playback; isolate the deck and record the LOAD/ready times separately.
        script = r'''
sizes() {
  find "$1" -maxdepth 1 -type f -exec stat -c '%s' {} \; 2>/dev/null |
    awk '{n++; bytes+=$1} END {printf "%d:%.0f",n,bytes}'
}
i=0
while [ "$i" -lt SAMPLES ]; do
  stamp=$(cut -d ' ' -f 1 /proc/uptime)
  printf 'uptime_s=%s stream_files:bytes=' "$stamp"
  sizes /data/sc5000-custom/Stream
  printf ' engine_temp_files:bytes='
  sizes /data/inMusicTempFiles
  printf '\n'
  i=$((i+1))
  sleep 1
done
'''.replace("SAMPLES", str(args.observe_seconds))
    else:
        if not args.playlist or not re.fullmatch(
                r"[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}", args.playlist):
            parser.error("browse timing requires --playlist <UUID>")
        paths = [
            ("ping", "/v1/ping"),
            ("lists", "/v1/users/1/playlists?limit=10&offset=0"),
            ("favorite-lists", "/v1/users/1/favorites/playlists?limit=10&offset=0"),
            ("likes", "/v1/users/1/favorites/tracks?limit=10&offset=0"),
        ]
        for kind, offset in (("tracks", 0), ("items", 0), ("items", 10)):
            paths.append((f"{kind}-{offset}",
                          f"/v1/playlists/{args.playlist}/{kind}?limit=10&offset={offset}"))
        lines = []
        for label, path in paths:
            fmt = f"{label} code=%{{http_code}} ttfb=%{{time_starttransfer}} total=%{{time_total}}\n"
            command = ("curl -s -o /dev/null --max-time 90 --write-out " + shlex.quote(fmt)
                       + " " + shlex.quote("http://127.0.0.1:8080" + path) + " || :")
            lines.extend([command] * args.repeat)
        script = "\n".join(lines)
    # deck's argument is a shell command. Quote both shell boundaries; sshctl
    # itself receives literal argv. Keep output capped, with no raw response body.
    remote = "deck " + args.deck + " " + shlex.quote("sh -c " + shlex.quote(script))
    result = subprocess.run(
        ["sshctl", "run", "Oracle-Batam-arm", "--no-stdin", "--timeout", "60m",
         "--argv", "sh", "-c", remote],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, check=False,
    )
    # Print only our numeric measurement records, even if SSH/deck emits banners.
    allowed = re.compile(r"^(?:[a-z0-9-]+ code=\d{3} ttfb=[0-9.]+ total=[0-9.]+|"
                         r"uptime_s=[0-9.]+ stream_files:bytes=\d+:\d+ engine_temp_files:bytes=\d+:\d+)$")
    records = [line for line in result.stdout.splitlines() if allowed.fullmatch(line)]
    for line in records[:40]:
        print(line[:200])
    if result.returncode or not records:
        parser.exit(1, "measurement transport failed or returned no timing records; raw output suppressed\n")


if __name__ == "__main__":
    main()
