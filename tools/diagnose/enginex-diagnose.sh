#!/bin/sh
# EngineX OS one-click diagnostics for Linux / macOS (read-only).
#   sh enginex-diagnose.sh [deck-ip]
# Collects what we usually have to ask for one question at a time: OS, USB devices of the
# Denon / Numark / Rane / Rockchip ids, fastboot answers, the host network and, with a deck
# IP, whether the deck answers on ports 22 / 80 / 8080. It never writes to the deck, never
# flashes, erases or reboots anything, and masks USB serial numbers.
# The report is written to ./enginex-diagnose-<time>.txt; please read it, then attach it.
set -u
DECK=${1:-}
case $DECK in *[!A-Za-z0-9.:-]*) echo "deck address: letters, digits, . : - only" >&2; exit 2 ;; esac
OUT=enginex-diagnose-$(date +%Y%m%d-%H%M%S).txt
have() { command -v "$1" >/dev/null 2>&1; }
sec() { printf '\n## %s\n' "$*"; }

usb() {
    # Only the ids that matter here (inMusic 15e4, AZ0x update 1cc5, Rockchip 2207, Google fastboot 18d1)
    # or a name that says Denon / Numark / Rane / inMusic. Everything else stays out of the report.
    if [ "$(uname)" = Darwin ]; then
        system_profiler SPUSBDataType 2>/dev/null | awk -v RS= \
            '{t = tolower($0)} t ~ /vendor id: 0x(15e4|1cc5|2207|18d1)/ || t ~ /denon|numark|rane|inmusic|mixstream|prime/ {print $0 "\n"}' |
            grep -v -i 'serial'
    else
        for d in /sys/bus/usb/devices/*; do
            [ -r "$d/idVendor" ] || continue
            v=$(cat "$d/idVendor"); p=$(cat "$d/idProduct")
            m=$(cat "$d/manufacturer" 2>/dev/null); n=$(cat "$d/product" 2>/dev/null)
            case "$v $m $n" in
            15e4*|1cc5*|2207*|18d1*|*[Dd]enon*|*[Nn]umark*|*[Rr]ane*|*[Ii]n[Mm]usic*) echo "$v:$p $m $n" ;;
            esac
        done
    fi
}

http() { # <path> -> "HTTP <code>" and the page title if any
    code=$(curl -s -m 4 -o /tmp/enginex-diag.$$ -w '%{http_code}' "http://$DECK:8080$1" 2>/dev/null)
    printf '%s -> HTTP %s' "$1" "${code:-none}"
    [ "$1" != / ] || printf '  title: %s' "$(grep -o '<title>[^<]*</title>' /tmp/enginex-diag.$$ 2>/dev/null | head -1)"
    echo; rm -f /tmp/enginex-diag.$$
}

{
    echo "EngineX OS diagnostics v1 (read-only)"
    echo "time (UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    sec "Host"
    uname -srm
    [ "$(uname)" != Darwin ] || sw_vers 2>/dev/null
    [ ! -r /etc/os-release ] || grep -E '^(PRETTY_NAME|VERSION)=' /etc/os-release
    sec "USB devices (Denon / Numark / Rane / Rockchip / fastboot ids only)"
    u=$(usb); if [ -n "$u" ]; then echo "$u"; else echo "none found: the deck is not enumerated (cable, port, or not in Firmware Update Mode)"; fi
    sec "fastboot"
    if have fastboot; then
        fastboot --version 2>&1 | head -1
        list=$(fastboot devices -l 2>&1 | sed 's/^[^[:space:]]*/<serial>/')
        echo "devices: ${list:-none}"
        if [ -n "$list" ]; then
            for v in product variant secure version-bootloader max-download-size partition-type:data partition-size:data; do
                printf '%s: ' "$v"; fastboot getvar "$v" 2>&1 | head -1
            done
        fi
    else
        echo "fastboot is not installed (install Android platform-tools; Linux needs root or a udev rule to see the deck)"
    fi
    sec "Network (IPv4)"
    if have ip; then ip -4 -o addr show 2>/dev/null | awk '$2 != "lo" {print $2, $4}'
    else ifconfig 2>/dev/null | awk '/^[a-z0-9]+:/ {i=$1} /inet / && $2 != "127.0.0.1" {print i, $2, $4}'; fi
    if [ -n "$DECK" ]; then
        sec "Deck $DECK"
        if ping -c 1 "$DECK" >/dev/null 2>&1; then echo "ping: reply"; else echo "ping: no reply (a firewall can also cause this)"; fi
        for port in 22 80 8080; do
            if have nc; then
                if nc -z -w 2 "$DECK" "$port" >/dev/null 2>&1; then echo "tcp $port: open"; else echo "tcp $port: closed or filtered"; fi
            fi
        done
        if have curl; then http /; http /v1/sc5000/ui; else echo "curl is not installed: HTTP probes skipped"; fi
        echo "(a 403 on /v1/sc5000/ui with a 'Stream Sync' title means EngineX is running and asks for its access code)"
    else
        sec "Deck"
        echo "no deck IP given; run again as: sh enginex-diagnose.sh 192.168.x.y  (Settings → About on the deck shows it)"
    fi
} > "$OUT" 2>&1

echo "Report written to: $OUT"
echo "It holds your local IP addresses and USB ids of the deck, no serial numbers or passwords. Please attach it to the GitHub issue."
