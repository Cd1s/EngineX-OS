#!/bin/sh
# EngineX OS: read-only heat and CPU measurement on a deck. Run it from a computer that
# reaches the deck; nothing is copied to the deck:
#   timeout 420 ssh root@<deck> 'chrt --idle 0 sh -s -- 300 idle' < heat-probe.sh > idle.txt
#   timeout 420 ssh root@<deck> 'chrt --idle 0 sh -s -- 300 playing' < heat-probe.sh > playing.txt
# Arguments: seconds to measure (default 300) and a label for the report. Start the second
# run with a track already playing. It reads /proc and /sys only: nothing is written to the
# deck, no setting is changed, no profiler or signal is used. Between its two snapshots it
# sleeps, waking once a second to read the CPU clock and the sound card's buffer state
# (this kernel keeps no time_in_state), so it ends by itself after the given seconds (the
# stock image has no timeout or nice command). BusyBox sh and awk are enough.
# Reading a PCM status file returns avail_max, the fullest the buffer was read back since the
# previous read, and restarts that count: a diagnostic value only. An underrun is
# avail_max >= buffer_size.
set -u
SECS=${1:-300}
LABEL=${2:-unlabelled}
case $SECS in ''|*[!0-9]*) echo 'seconds: digits only' >&2; exit 2 ;; esac
export LC_ALL=C
# The deck's sshd and consoles share CPU 3: never compete with them, however this was started.
chrt --idle -p 0 $$ >/dev/null 2>&1 || :
SYS=${HEAT_PROBE_SYS:-/sys}
ASOUND=${HEAT_PROBE_ASOUND:-/proc/asound}
CPU=$SYS/devices/system/cpu
val() { v=; read -r v 2>/dev/null < "$1"; printf '%s' "${v:-?}"; }

temps() {
    for z in "$SYS"/class/thermal/thermal_zone*; do
        [ -r "$z/temp" ] && printf ' %s=%s' "$(val "$z/type")" "$(val "$z/temp")"
    done
    for c in "$CPU"/cpu[0-9]*/cpufreq/scaling_cur_freq; do
        [ -r "$c" ] && printf ' %s' "$(val "$c")"
    done
    echo
}
# One sample: the shared CPU clock, and every open playback stream's state and avail_max.
sample() {
    echo "@S clock $(val "$CPU/cpu0/cpufreq/scaling_cur_freq")"
    for f in "$ASOUND"/card*/pcm*p/sub*/status; do
        st=; mx=
        while read -r k a b; do
            case $k in state:) st=$a ;; avail_max) mx=$b ;; esac
        done 2>/dev/null < "$f"
        [ -z "$st" ] || echo "@S pcm ${f#"$ASOUND"/} $st ${mx:-0}"
    done
}
# Everything that is compared between the start and the end, one tagged line each.
snap() {
    read -r up _ < /proc/uptime
    echo "@$1 uptime $up"
    sed "s/^/@$1 stat /" /proc/stat
    sed "s/^/@$1 irq /" /proc/interrupts
    for t in /proc/[0-9]*/task/[0-9]*; do
        s=; read -r s 2>/dev/null < "$t/stat" || continue
        echo "@$1 task ${t#/proc/} $s"
    done
    for f in "$CPU"/cpu[0-9]*/cpufreq/stats/time_in_state "$SYS"/class/devfreq/*/trans_stat; do
        [ -r "$f" ] && sed "s|^|@$1 file $f |" "$f"
    done
    for f in "$CPU"/cpu[0-9]*/cpuidle/state*/time; do
        [ -r "$f" ] && echo "@$1 idle ${f%/time} $(val "${f%/time}/name") $(val "$f") $(val "${f%/time}/usage")"
    done
}

echo "EngineX heat probe v1 (read-only): label=$LABEL seconds=$SECS"
echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)  uptime: $(val /proc/uptime)  loadavg: $(val /proc/loadavg)"
echo "model: $(tr -d '\0' 2>/dev/null < "$SYS/firmware/devicetree/base/inmusic,product-code")  kernel: $(uname -r)"
echo "version: image $(val /etc/sc5000/version.txt)  data $(val /data/enginex/version.txt)  kit $(val /data/enginex/.test-kit-ref)"
echo "four-deck role: $(val /run/sc5000-assignments/role)  safe-mode: $([ -e /data/enginex/ui/safe-mode ] && echo yes || echo no)"

echo
echo '## CPU clock settings (per core: governor min cur max)'
for c in "$CPU"/cpu[0-9]*; do
    [ -d "$c/cpufreq" ] || continue
    echo "${c##*/}: $(val "$c/cpufreq/scaling_governor") $(val "$c/cpufreq/scaling_min_freq") $(val "$c/cpufreq/scaling_cur_freq") $(val "$c/cpufreq/scaling_max_freq")  online=$(val "$c/online")"
done
echo "available: $(val "$CPU/cpu0/cpufreq/scaling_available_frequencies")"
for f in "$CPU"/cpufreq/ondemand/* "$CPU"/cpu0/cpufreq/ondemand/*; do
    [ -f "$f" ] && echo "ondemand ${f##*/}=$(val "$f")"
done
echo
echo '## GPU and other devfreq clocks (governor min cur max)'
for d in "$SYS"/class/devfreq/*; do
    [ -d "$d" ] && echo "${d##*/}: $(val "$d/governor") $(val "$d/min_freq") $(val "$d/cur_freq") $(val "$d/max_freq")"
done
echo
echo '## Thermal zones, trip points and cooling devices'
for z in "$SYS"/class/thermal/thermal_zone*; do
    [ -d "$z" ] || continue
    printf '%s %s temp=%s policy=%s trips:' "${z##*/}" "$(val "$z/type")" "$(val "$z/temp")" "$(val "$z/policy")"
    for t in "$z"/trip_point_*_temp; do [ -r "$t" ] && printf ' %s(%s)' "$(val "$t")" "$(val "${t%_temp}_type")"; done
    echo
done
for c in "$SYS"/class/thermal/cooling_device*; do
    [ -d "$c" ] && echo "${c##*/} $(val "$c/type") state=$(val "$c/cur_state")/$(val "$c/max_state")"
done

echo
echo '## Sound card streams (an underrun is avail_max >= buffer_size)'
for f in "$ASOUND"/card*/pcm*p/sub*/hw_params; do
    [ -r "$f" ] && echo "${f#"$ASOUND"/}: $(tr '\n' ' ' < "$f")"
done
echo
echo "## Temperatures (millidegrees) and core clocks (kHz) every 30 s"
printf 't=0'; temps
exec 3>&1 # the 30 s lines go straight out, not through awk
{
    snap A
    t=0
    sample > /dev/null # restart avail_max: count the window only
    while [ "$t" -lt "$SECS" ]; do
        ${HEAT_PROBE_SLEEP:-sleep} 1; t=$((t + 1))
        sample
        [ $((t % 30)) != 0 ] && [ "$t" != "$SECS" ] || { printf 't=%s' "$t"; temps; } >&3
    done
    snap B
    # Names for the processes that are alive at the end (the suite daemons share one comm).
    for p in /proc/[0-9]*; do
        c=$(tr '\0' ' ' 2>/dev/null < "$p/cmdline") || continue
        [ -n "$c" ] && echo "@B cmd ${p#/proc/} $c"
    done
} | awk '
function task(line,    a, b, rest, f) {       # "<pid>/task/<tid> <stat line>" -> key, comm, ticks
    key = line; sub(/ .*/, "", key); sub(/\/task\//, " ", key)
    a = index(line, "("); b = 0
    while ((f = index(substr(line, b + 1), ")")) > 0) b += f
    comm = substr(line, a + 1, b - a - 1)
    split(substr(line, b + 2), rest, " ")
    ticks = rest[12] + rest[13]; policy = rest[39]; last_cpu = rest[37]
}
{
    snapshot = substr($1, 2); kind = $2
    line = $0; sub(/^[^ ]+ [^ ]+ /, "", line)
}
kind == "uptime" { up[snapshot] = $3 }
kind == "stat" && $3 ~ /^cpu/ { for (i = 4; i <= 11; i++) cpu[snapshot, $3, i - 3] = $i; if (snapshot == "A") cpus[++ncpu] = $3 }
kind == "stat" && ($3 == "ctxt" || $3 == "processes") { misc[snapshot, $3] = $4 }
kind == "irq" && $3 ~ /^[0-9A-Za-z]+:$/ {
    n = 0; name = ""
    for (i = 4; i <= NF; i++) { if ($i ~ /^[0-9]+$/ && name == "") n += $i; else name = name " " $i }
    irq[snapshot, $3] = n; irqname[$3] = name
    if (snapshot == "A") irqs[++nirq] = $3
}
kind == "task" { task(line); tk[snapshot, key] = ticks; if (snapshot == "B") { tcomm[key] = comm; tpol[key] = policy; tcpu[key] = last_cpu; keys[++nkey] = key } }
kind == "cmd" { pid = $3; c = line; sub(/^[^ ]+ /, "", c); cmd[pid] = substr(c, 1, 60) }
kind == "file" { f = $3; k = $4; v = $5; if ($3 ~ /time_in_state$/) { tis[snapshot, f, k] = v; if (snapshot == "A") { tf[++ntis] = f; tk2[ntis] = k } } else if (snapshot == "B") trans[++ntrans] = line }
kind == "clock" { clock[$3]++; nclock++ }
kind == "pcm" { pcm[$3]++; if ($4 != "RUNNING") stopped[$3]++; if ($5 > fullest[$3]) fullest[$3] = $5 }
kind == "idle" { idt[snapshot, $3] = $5; idu[snapshot, $3] = $6; idn[$3] = $4; if (snapshot == "A") ids[++nid] = $3 }
END {
    secs = up["B"] - up["A"]; hz = 100
    if (secs <= 0) { print "no window measured"; exit 1 }
    printf "\n## Over %.0f s\n", secs
    printf "context switches/s %.0f   forks/s %.1f\n", (misc["B", "ctxt"] - misc["A", "ctxt"]) / secs, (misc["B", "processes"] - misc["A", "processes"]) / secs
    print "\n## CPU busy % (user nice system irq softirq | idle iowait)"
    for (n = 1; n <= ncpu; n++) {
        c = cpus[n]; total = 0
        for (i = 1; i <= 8; i++) { d[i] = cpu["B", c, i] - cpu["A", c, i]; total += d[i] }
        if (total > 0) printf "%-5s %5.1f %5.1f %5.1f %5.1f %5.1f | %5.1f %5.1f\n", c, 100 * d[1] / total, 100 * d[2] / total, 100 * d[3] / total, 100 * d[6] / total, 100 * d[7] / total, 100 * d[4] / total, 100 * d[5] / total
    }
    print "\n## CPU clock, % of the 1 s samples (kHz)"
    for (k in clock) printf "%8d kHz %5.1f\n", k, 100 * clock[k] / nclock | "sort -n"
    fflush(); close("sort -n")
    print "\n## Sound card streams: samples, samples not RUNNING, highest avail_max (frames)"
    for (k in pcm) printf "%s %d %d %d\n", k, pcm[k], stopped[k], fullest[k]
    print "\n## Time at each CPU clock, % of the window (kHz), where the kernel counts it"
    for (n = 1; n <= ntis; n++) {
        f = tf[n]; k = tk2[n]; share = (tis["B", f, k] - tis["A", f, k]) / secs      # 10 ms units
        c = f; sub(/\/cpufreq.*/, "", c); sub(/.*\//, "", c)
        if (share >= 0.05) printf "%s %8d kHz %5.1f\n", c, k, share
    }
    if (nid) print "\n## Idle states: % of the window, entries/s"
    for (n = 1; n <= nid; n++) {
        s = ids[n]; c = s; sub(/.*cpu\/cpu/, "cpu", c)
        printf "%s %s %5.1f %8.1f\n", c, idn[s], (idt["B", s] - idt["A", s]) / secs / 10000, (idu["B", s] - idu["A", s]) / secs
    }
    print "\n## Interrupts per second (above 1/s)"
    for (n = 1; n <= nirq; n++) {
        i = irqs[n]; r = (irq["B", i] - irq["A", i]) / secs
        if (r >= 1) printf "%8.1f  %s%s\n", r, i, irqname[i]
    }
    print "\n## Threads by CPU time: % of one core, cpu last run on, policy (1/2 = real time), process | thread"
    for (n = 1; n <= nkey; n++) {
        k = keys[n]; pct[k] = 100 * (tk["B", k] - tk["A", k]) / hz / secs
        split(k, parts, " "); proc[parts[1]] += pct[k]; all += pct[k]
    }
    top = "sort -rn | sed -n 1,45p"
    for (n = 1; n <= nkey; n++) {
        k = keys[n]; split(k, parts, " ")
        if (pct[k] >= 0.05) printf "%6.2f  cpu%s  pol%s  %s | %s\n", pct[k], tcpu[k], tpol[k], (parts[1] in cmd) ? cmd[parts[1]] : "[kernel]", tcomm[k] | top
    }
    fflush(); close(top)
    printf "%6.2f  all threads alive at the end\n", all
    print "\n## Processes by CPU time (% of one core, above 0.05)"
    for (p in proc) if (proc[p] >= 0.05) printf "%6.2f  %s\n", proc[p], (p in cmd) ? cmd[p] : "[kernel " p "]" | "sort -rn"
    fflush(); close("sort -rn")
    if (ntrans) print "\n## devfreq transition tables at the end"
    for (n = 1; n <= ntrans; n++) print trans[n]
}'
