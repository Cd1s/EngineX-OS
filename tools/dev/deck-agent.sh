#!/bin/sh
# Deck side of tools/dev/deck-push. deck-push passes this whole script as the
# argument of `sh -c` over SSH; it is never installed on the deck.
#
# Every replaced file is a bind mount from tmpfs (/run/enginex-dev), so the files
# underneath stay untouched and a reboot puts them back. `rollback` unmounts
# without a reboot. Only the names in target() can be replaced; nothing else is
# written outside /run/enginex-dev. This script never starts a Factory Reset,
# never runs az01-update and never reboots.
set -eu
STATE=/run/enginex-dev
IN=$STATE/incoming
export LC_ALL=C

die() { echo "deck-agent: $*" >&2; exit 1; }

# <name> -> its path on the deck, resolved through the compatibility links
# (/data/sc5000-custom -> /data/enginex, /etc/enginex -> /etc/sc5000).
target() {
    case $1 in
    ui-packs) want=/data/enginex/ui/packs alt=/data/sc5000-custom/ui/packs kind=d ;;
    ui-icons) want=/data/enginex/ui/icons alt=/data/sc5000-custom/ui/icons kind=d ;;
    hook) want=/etc/sc5000/libengine-ui-hook.so alt=/etc/enginex/libengine-ui-hook.so kind=f ;;
    edisksd) want=/etc/sc5000/libedisksd-hook.so alt=/etc/enginex/libedisksd-hook.so kind=f ;;
    suite) want=/data/enginex/bin/sc5000-suite alt=/data/sc5000-custom/bin/sc5000-suite kind=f ;;
    *) die "not a managed name: $1" ;;
    esac
    p=$(readlink -f "$want" 2>/dev/null || :)
    [ -n "$p" ] && [ -e "$p" ] || p=$(readlink -f "$alt" 2>/dev/null || :)
    case $p in
    "$want" | "$alt") ;;
    *) die "$1: $want is missing or resolves outside the managed paths (${p:-none})" ;;
    esac
    if [ "$kind" = d ]; then [ -d "$p" ] || die "$1: $p is not a directory"
    else [ -f "$p" ] || die "$1: $p is not a regular file"; fi
    echo "$p"
}

mounted() { awk -v t="$1" '$5 == t { f = 1 } END { exit !f }' /proc/self/mountinfo; }

# A file's sha256; for a directory the sha256 of the "<sha256>  ./<path>" lines of
# all its files in byte order (deck-push computes the same on the build host).
digest() {
    if [ -d "$1" ]; then
        (cd "$1" && find . -type f | sort | while IFS= read -r f; do sha256sum "$f"; done) | sha256sum | cut -d' ' -f1
    elif [ -f "$1" ]; then
        sha256sum < "$1" | cut -d' ' -f1
    else
        echo -
    fi
}

# Engine's process names itself EMain.
engine_pid() {
    for c in /proc/[0-9]*/comm; do
        [ "$(cat "$c" 2>/dev/null)" = EMain ] || continue
        c=${c%/comm}
        # Not a zombie that nobody has reaped yet.
        [ "$(sed 's/.*) //' "$c/stat" 2>/dev/null | cut -d' ' -f1)" != Z ] || continue
        echo "${c#/proc/}"
        return
    done
}

# Seconds since the epoch at which <pid> started (/proc/<pid>/stat field 22).
started() {
    btime=$(awk '/^btime/ { print $2 }' /proc/stat)
    ticks=$(sed 's/.*) //' "/proc/$1/stat" | awk '{ print $20 }')
    hz=$(getconf CLK_TCK 2>/dev/null || echo 100)
    echo $((btime + ticks / hz))
}

fetch() {
    if command -v curl >/dev/null 2>&1; then curl -sf -m 5 "$1"
    else wget -q -T 5 -O - "$1"; fi
}

restart() {
    for s in "$@"; do
        case $s in
        engine.service | edisksd.service | stream-sync.service | sc5000-deemixd.service | rb-bridge.service) ;;
        *) die "will not restart $s" ;;
        esac
        echo "restart $s"
        systemctl restart "$s" < /dev/null
    done
}

cmd=${1:-}
[ $# -eq 0 ] || shift
case $cmd in
digest)
    digest "$1"
    ;;
preflight)
    # preflight <name>...: what the build host needs before a push; read-only.
    echo "now $(date +%s)"
    echo "version $(cat /etc/sc5000/version.txt 2>/dev/null || echo -)"
    echo "run_free_kb $(df -Pk /run | awk 'NR == 2 { print $4 }')"
    pid=$(engine_pid)
    echo "emain ${pid:--}"
    for n in "$@"; do
        p=$(target "$n")
        echo "target $n $p $(digest "$p")"
    done
    [ ! -f "$STATE/mounted" ] || while read -r n; do echo "devmount $n $(target "$n")"; done < "$STATE/mounted"
    [ ! -f "$STATE/manifest.json" ] || echo "manifest $(tr -d '\n' < "$STATE/manifest.json")"
    ;;
backup)
    # backup <name>...: a tar of what the deck shows at those paths now, on stdout.
    for n in "$@"; do target "$n" > /dev/null; done
    # shellcheck disable=SC2046
    set -- $(for n in "$@"; do target "$n" | cut -c2-; done)
    tar -C / -cf - "$@"
    ;;
apply)
    # stdin: a tar with plan ("<name> <path in the tar>" lines), services, manifest.json, files.
    rm -rf "$IN"
    mkdir -p "$IN" "$STATE/files" "$STATE/orig"
    tar -C "$IN" -xf -
    for f in plan services manifest.json; do [ -f "$IN/$f" ] || die "incomplete upload: no $f"; done
    while read -r n src; do target "$n" > /dev/null; case $src in '' | /* | *..*) die "bad source path for $n: $src" ;; esac; done < "$IN/plan"
    while read -r n src; do
        p=$(target "$n")
        [ -e "$IN/$src" ] || die "$n: $src missing from the upload"
        if mounted "$p"; then
            # Lazy: the running service may still map the old file (EBUSY otherwise);
            # it keeps that copy until the restart below.
            umount -l "$p"
        else
            # What a rollback must bring back: the digest before the first push.
            digest "$p" > "$STATE/orig/$n"
        fi
        ! mounted "$p" || die "$p is still a mount point that deck-push did not make; reboot the deck"
        rm -rf "$STATE/files/$n"
        mv "$IN/$src" "$STATE/files/$n"
        mount --bind "$STATE/files/$n" "$p"
        grep -qx "$n" "$STATE/mounted" 2>/dev/null || echo "$n" >> "$STATE/mounted"
        echo "mounted $n $p $(digest "$p")"
    done < "$IN/plan"
    cp "$IN/manifest.json" "$STATE/manifest.json"
    touch "$STATE/services"
    # shellcheck disable=SC2046
    set -- $(cat "$IN/services")
    for s in "$@"; do grep -qx "$s" "$STATE/services" || echo "$s" >> "$STATE/services"; done
    rm -rf "$IN"
    restart "$@"
    ;;
rollback)
    [ -f "$STATE/mounted" ] || { echo "nothing to roll back"; exit 0; }
    # Last mounted first; one name per line, names have no spaces.
    # shellcheck disable=SC2013
    for n in $(awk '{ l[NR] = $0 } END { for (i = NR; i > 0; i--) print l[i] }' "$STATE/mounted"); do
        p=$(target "$n")
        # Lazy, as in apply: the services still map these files until their restart.
        while mounted "$p"; do umount -l "$p"; done
        echo "restored $n $p $(digest "$p") $(cat "$STATE/orig/$n" 2>/dev/null || echo -)"
    done
    # shellcheck disable=SC2046
    set -- $(cat "$STATE/services" 2>/dev/null)
    rm -rf "$STATE"
    restart "$@"
    ;;
smoke)
    # smoke <api url> <since> <daemon>...: facts only; deck-push decides PASS or FAIL.
    # Engine's journal counts from <since> (epoch; the build host passes the deck's time
    # just before it restarted Engine), or with "-" from 2 s before EMain started. Read-only.
    api=$1 since=$2
    shift 2
    echo "now $(date +%s)"
    pid=$(engine_pid)
    echo "emain ${pid:--}"
    if [ -n "$pid" ]; then
        start=$(started "$pid")
        echo "emain_started $start"
        [ "$since" != - ] || since=$((start - 2))
        if grep -q libengine-ui-hook "/proc/$pid/maps" 2>/dev/null; then echo "hook_mapped yes"; else echo "hook_mapped no"; fi
        log=$(journalctl -u engine.service --since "@$since" -o cat --no-pager 2>/dev/null || :)
        echo "applied $(printf '%s\n' "$log" | grep -c 'engine-ui-hook: applied 0x' || :)"
        printf '%s\n' "$log" | grep -m1 'engine-ui-hook: Engine ' | sed 's/^/engine_line /' || :
        printf '%s\n' "$log" | grep 'packs/[A-Za-z-]*: [0-9]* files' | tail -1 | sed 's/^/packs_line /' || :
        # The release gate's QML load error patterns (tools/az01-packer/release-gate.sh).
        q='Type [A-Za-z_][A-Za-z0-9_]* unavailable|PropertyChanges does not support creating|state-specific objects|is not a type|is not installed|Cannot assign to non-existent|Expected token|Syntax error|QQmlComponent: Component is not ready|failed to load component'
        echo "qml_errors $(printf '%s\n' "$log" | grep -cE "$q" || :)"
        printf '%s\n' "$log" | grep -E "$q" | head -3 | sed 's/^/qml_error /' || :
        echo "crashes $(printf '%s\n' "$log" | grep -cE 'Crash with|Unknown exit code' || :)"
    fi
    for d in "$@"; do echo "daemon $d $(systemctl is-active "$d.service" 2>/dev/null || :)"; done
    body=$(fetch "$api" 2>/dev/null || :)
    if [ -n "$body" ]; then echo "api_ok 1"; else echo "api_ok 0"; fi
    echo "api_version $(printf '%s' "$body" | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
    echo "version_txt $(cat /etc/sc5000/version.txt 2>/dev/null || echo -)"
    [ ! -f "$STATE/manifest.json" ] || echo "manifest $(tr -d '\n' < "$STATE/manifest.json")"
    [ ! -f "$STATE/mounted" ] || while read -r n; do
        p=$(target "$n")
        if mounted "$p"; then m=mounted; else m=unmounted; fi
        echo "digest $n $(digest "$p") $m"
    done < "$STATE/mounted"
    ;;
*)
    die "usage: deck-agent.sh digest <path> | preflight <name>... | backup <name>... | apply < upload.tar | rollback | smoke <api url> <since|-> <daemon>..."
    ;;
esac
