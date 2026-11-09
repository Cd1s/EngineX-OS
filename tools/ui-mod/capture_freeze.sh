#!/bin/sh
# BusyBox-compatible, bounded Engine freeze capture. Run only from /tmp.
# Usage: sh capture_freeze.sh /tmp/r2a-capture [seconds=900] [snapshot]
set -u
out=$1
duration=${2:-900}
mkdir -p "$out"
start=$(date +%s)
machine=$(uname -m)
end=$((start + duration))
last_pid= last_cpu= unchanged=0 tick=0 quiet_dumps=0 journal_dumps=0 last_change=$start
dump() {
    reason=$1
    stamp=$(date +%s)
    dest=$out/$stamp-$reason
    mkdir -p "$dest"
    cat /proc/locks > "$dest/locks"
    cat /proc/loadavg /proc/stat /proc/pressure/cpu > "$dest/system" 2>/dev/null
    for pid in $(pidof Engine) $(pidof OfflineAnalyzer); do
        cat /proc/$pid/maps > "$dest/maps-$pid" 2>/dev/null
        for task in /proc/$pid/task/[0-9]*; do
            tid=${task##*/}
            {
                for item in stat wchan syscall schedstat sched; do
                    printf '\n%s: ' "$item"
                    cat "$task/$item" 2>/dev/null
                done
                grep -E '^(Name|State|Tgid|Pid|Cpus_allowed_list|voluntary_ctxt_switches|nonvoluntary_ctxt_switches):' "$task/status"
                chrt -p "$tid"
            } > "$dest/thread-$tid" 2>&1
            # A futex word is not necessarily an owner TID (Qt commonly uses
            # non-PI wait words). Preserve nearby bytes for later lock analysis.
            syscall=$(cat "$task/syscall" 2>/dev/null)
            printf '%s\n' "$syscall" > "$dest/memory-$tid.syscall"
            set -- $syscall
            case $machine:${1:-} in arm*:240|arm*:422|aarch64:98)
                address=${2:-0}
                case $address in 0x*)
                    dd if=/proc/$pid/mem bs=1 skip=$((address - 32)) count=96 2>"$dest/memory-$tid.error" |
                        od -x > "$dest/memory-$tid"
                    ;;
                esac
                ;;
            esac
        done
    done
    journalctl -u engine.service -n 100 --no-pager > "$dest/journal" 2>&1
    printf '%s %s\n' "$stamp" "$reason" >> "$out/triggers"
}
if [ "${3:-}" = snapshot ]; then dump manual; exit; fi
# Follow only new log entries; never retrigger on the historical freeze.
journalctl -u engine.service -n 0 -f --no-pager > "$out/journal-follow" 2>&1 &
journal_pid=$!
trap 'kill "$journal_pid" 2>/dev/null; exit' INT TERM EXIT
while [ "$(date +%s)" -lt "$end" ]; do
    now=$(date +%s)
    engine=$(pidof Engine)
    pids="$engine $(pidof OfflineAnalyzer)"
    IFS=' ' read -r uptime unused < /proc/uptime
    for pid in $pids; do
        for task in /proc/$pid/task/[0-9]*; do
            IFS= read -r line 2>/dev/null < "$task/stat" || continue
            printf '%s %s %s\n' "$uptime" "$pid" "$line" >> "$out/all.tsv"
            fields=${line##*) }
            set -- $fields
            state=$1
            shift 11
            cpu=$1
            shift 5
            nice=$1
            shift 22
            policy=$1
            tid=${task##*/}
            if [ "$policy" = 5 ] || [ "$nice" = 19 ]; then
                # schedstat: runtime_ns runqueue_wait_ns timeslices. Deltas give
                # actual runnable waiting, unlike treating sleeping as starvation.
                sched=unavailable
                if [ -r "$task/schedstat" ]; then IFS= read -r sched < "$task/schedstat" || :; fi
                printf '%s %s %s %s %s %s %s\n' "$uptime" "$pid" "$tid" "$state" "$nice" "$policy" "$sched" >> "$out/degraded.tsv"
            fi
            if [ "$pid" = "$engine" ] && [ "$tid" = "$pid" ]; then
                if [ "$last_pid" != "$pid" ] || [ "$last_cpu" != "$cpu" ]; then last_change=$now; fi
                unchanged=$((now - last_change))
                last_pid=$pid last_cpu=$cpu
                if [ "$unchanged" -ge 10 ] && [ "$quiet_dumps" -lt 3 ]; then
                    dump cpu-stall-suspect
                    quiet_dumps=$((quiet_dumps + 1))
                    last_change=$now
                fi
                printf '%s %s\n' "$now" "$line" >> "$out/main.tsv"
            fi
        done
    done
    if [ "$((tick % 4))" = 0 ]; then
        printf '%s ' "$now" >> "$out/loadavg"
        cat /proc/loadavg >> "$out/loadavg"
        matches=$(grep -c 'probes have frozen' "$out/journal-follow" || :)
        if [ "$matches" -gt "$journal_dumps" ]; then dump journal-freeze; journal_dumps=$matches; fi
    fi
    tick=$((tick + 1))
    IFS=' ' read -r finished unused < /proc/uptime
    began_cs=${uptime%.*}${uptime#*.}
    finished_cs=${finished%.*}${finished#*.}
    delay=$((50 - (finished_cs - began_cs)))
    # Keep half-second cadence where possible; an overrun is visible in uptime.
    if [ "$delay" -gt 0 ]; then usleep $((delay * 10000)); fi
done
