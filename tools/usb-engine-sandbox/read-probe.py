#!/usr/bin/env python3
"""read-probe.py <out/engine.log>: the probe's records (P) joined back from Engine's split log
lines, then Engine's own lines about drives and libraries (E)."""
import re
import sys

raw = open(sys.argv[1], encoding="utf-8", errors="replace").read().splitlines()


def strip(line):  # Engine appends "[time] [thread] [category]" after padding
    return re.sub(r" {2,}\[20\d\d-[^\]]*\] \[[^\]]*\] \[[^\]]*\]\s*$", "", line)


joined = "".join(strip(l)[len("[W] FSP "):] for l in raw if l.startswith("[W] FSP "))
for rec in re.split(r"(?=(?:t\d+|(?<![A-Z])[A-Z][A-Z0-9_-]*\.\d+)\|)", joined):
    if rec.strip():
        print("P", rec.strip())
for line in map(strip, raw):
    if "FSP " not in line and re.search(r"EDisks|databases available|schema|OneLibrary|ekordbox|[Ii]mport|/media/|Database", line):
        print("E", line)
