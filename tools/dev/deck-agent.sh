#!/bin/sh
# Deck side of tools/dev/deck-push. deck-push passes this whole script as the
# argument of `sh -c` over SSH; it is never installed on the deck.
#
# Every replaced file is a bind mount from tmpfs (/run/enginex-dev), so the files
# underneath stay untouched and a reboot puts them back. `rollback` unmounts
# without a reboot. Managed deploys only replace the names in target() and keep
# their working files in /run/enginex-dev and /run/enginex-dev-lock. This script never
# starts a Factory Reset or runs az01-update. Generic exec needs an explicit
# --allow-reboot for reboot-family commands.
set -eu
STATE=/run/enginex-dev
IN=$STATE/incoming
export LC_ALL=C

die() { echo "deck-agent: $*" >&2; exit 1; }

# This directory is shared by every host. Keep it after release: removing a
# directory that another process is using as a mutex creates an ABA race.
LOCK=/run/enginex-dev-lock
refuse() { echo "deck-agent: $*" >&2; exit 3; }
field() { sed -n "s/^$1 //p" "$LOCK/owner" 2>/dev/null || :; }
process_start() { sed 's/.*) //' "/proc/$1/stat" 2>/dev/null | awk '{ print $20 }'; }
lock_cleanup() {
    # Retire this directory before deleting metadata. A concurrent recovery may
    # have pinned its inode, but a new operation can now take a fresh mutex.
    retired=$(mktemp -d "$LOCK/.done.XXXXXX")
    mv "$LOCK/.mutex" "$retired/mutex"
    rm -f "$retired/mutex/process"
    rmdir "$retired/mutex" "$retired" 2>/dev/null || :
}
lock_guard() {
    mkdir -p "$LOCK"
    # A child command can arrive during its wrapper's brief lease renewal.
    attempt=0
    until mkdir "$LOCK/.mutex" 2>/dev/null; do
        [ "$attempt" -lt 10 ] || refuse 'deck operation busy; retry or inspect deck-lock status'
        attempt=$((attempt + 1))
        sleep 0.05
    done
    trap lock_cleanup EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 141' PIPE
    trap 'exit 143' TERM
    printf '%s %s\n' "$$" "$(process_start "$$")" > "$LOCK/.mutex/process"
}
lock_recover() {
    [ -d "$LOCK/.mutex" ] || { echo 'no mutex to recover'; return; }
    # Pin the inode before taking the recovery guard. Cleanup can retire it
    # while a new operation creates a different directory at the same path.
    cd "$LOCK/.mutex" || refuse 'mutex changed; retry recovery'
    mkdir .recovery 2>/dev/null || refuse 'mutex recovery already running'
    trap 'rmdir .recovery 2>/dev/null || :' EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 141' PIPE
    trap 'exit 143' TERM
    read -r pid ticks < process || refuse 'mutex process metadata missing; inspect with the maintainer'
    case $pid in ''|*[!0-9]*) refuse 'invalid mutex pid' ;; esac
    case $ticks in ''|*[!0-9]*) refuse 'invalid mutex start time' ;; esac
    [ "$(process_start "$pid")" != "$ticks" ] || refuse "mutex held by live process $pid"
    quarantine=$(mktemp -d "$LOCK/.stale.XXXXXX")
    stale_path=$(pwd -P)
    mv "$stale_path" "$quarantine/mutex"
    rm -f process
    rmdir .recovery
    trap - EXIT
    cd "$LOCK"
    rmdir "$quarantine/mutex" "$quarantine"
    case $stale_path in "$LOCK"/.done.*/mutex) rmdir "${stale_path%/mutex}" 2>/dev/null || : ;; esac
    echo "recovered dead mutex $pid $ticks"
}
lock_live() {
    expires=$(field expires)
    [ -n "$expires" ] && [ "$expires" -gt "$(date +%s)" ]
}
lock_require() {
    lock_live || refuse 'deck is not locked (or lease expired); use deck-lock acquire'
    [ "$(field owner)" = "$1" ] || refuse "deck is locked by $(field owner)"
}
lock_write() {
    # owner host purpose ttl. All values arrive as quoted literal argv.
    owner=$1 host=$2 purpose=$3 ttl=$4
    now=$(date +%s)
    acquired=$(field acquired) started_at=$(field started)
    [ -n "$acquired" ] || acquired=$now
    [ -n "$started_at" ] || started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    {
        printf 'owner %s\nhost %s\npurpose %s\n' "$owner" "$host" "$purpose"
        printf 'acquired %s\nstarted %s\n' "$acquired" "$started_at"
        printf 'expires %s\nttl %s\n' "$((now + ttl))" "$ttl"
    } > "$LOCK/.owner.$$"
    mv "$LOCK/.owner.$$" "$LOCK/owner"
}
lock_finish() {
    # A command pins the lease while it runs, then gives its caller a full TTL.
    lock_write "$(field owner)" "$(field host)" "$(field purpose)" "$(field ttl)"
    lock_cleanup
}

case ${1:-} in
lock-status)
    # Truly read-only, including on a freshly booted deck.
    if [ -f "$LOCK/owner" ]; then
        cat "$LOCK/owner"
        if lock_live; then echo 'live yes'; else echo 'live no'; fi
    fi
    if [ -d "$LOCK/.mutex" ]; then
        echo 'busy yes'
        if [ -r "$LOCK/.mutex/process" ]; then
            read -r pid ticks < "$LOCK/.mutex/process" || :
            echo "mutex_pid ${pid:-unknown}"
            echo "mutex_started ${ticks:-unknown}"
        fi
    fi
    exit 0 ;;
lock-recover)
    lock_recover
    exit 0 ;;
lock-acquire)
    shift
    lock_guard
    if lock_live; then
        [ "$(field owner)" = "$1" ] || refuse "deck is locked by $(field owner): $(field purpose)"
    else
        rm -f "$LOCK/owner"
    fi
    lock_write "$1" "$2" "$3" "$4"
    cat "$LOCK/owner"
    echo 'live yes'
    exit 0 ;;
lock-renew)
    shift
    lock_guard
    lock_require "$1"
    lock_write "$1" "$(field host)" "$(field purpose)" "$(field ttl)"
    exit 0 ;;
lock-release)
    shift
    lock_guard
    [ -f "$LOCK/owner" ] || exit 0
    if [ "$(field owner)" != "$1" ]; then
        refuse 'lease belongs to another agent'
    fi
    rm -f "$LOCK/owner"
    exit 0 ;;
lock-require)
    shift
    lock_guard
    lock_require "$1"
    cat "$LOCK/owner"
    echo 'live yes'
    exit 0 ;;
--locked)
    shift
    lock_guard
    lock_require "$1"
    trap lock_finish EXIT
    shift ;;
digest) ;; # Local digest parity test; does not operate a deck.
*) refuse 'every deck command requires a lease' ;;
esac

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

mount_id() { awk -v t="$1" '$5 == t { id = $1 } END { print id }' /proc/self/mountinfo; }
owned_mount() {
    [ -f "$STATE/mount-ids/$1" ] && [ "$(cat "$STATE/mount-ids/$1")" = "$(mount_id "$2")" ] &&
        [ "$(stat -c %d:%i "$STATE/files/$1")" = "$(stat -c %d:%i "$2")" ]
}
require_owned_mount() { owned_mount "$1" "$2" || refuse "$2 has a mount not created by deck-push; leaving it intact"; }

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

check_command() {
    # Catch common destructive commands, including ordinary sh -c usage.
    # This is a convenience guard; arbitrary root programs can bypass it.
    text=$(printf '%s\n' "$@" | tr '[:upper:]' '[:lower:]' | tr '\n\t' '  ' | tr -d "\"'")
    case $text in
    *az01-update*--erase-data*|*factoryreset*|*factory-reset*|*factory_reset*|*factory\ reset*)
        refuse 'factory reset and erase-data commands are forbidden' ;;
    esac
    if printf '%s\n' "$text" | grep -Eq '(^|[^a-z0-9_.-])mkfs([.a-z0-9_-]*)([^a-z0-9_.-]|$)|(^|[^a-z0-9_.-])dd[[:space:]].*of=/dev/|(^|[^a-z0-9_.-])rm[[:space:]].*(-[^[:space:]]*r|--recursive).*[[:space:]]/data([/[:space:];&|]|$)'; then
        refuse 'destructive filesystem command is forbidden'
    fi
    if [ "$allow_reboot" != 1 ] && printf '%s\n' "$text" | grep -Eq '(^|[^a-z0-9_.-])(reboot|poweroff|halt|shutdown|kexec)([^a-z0-9_.-]|$)|(reboot|poweroff|halt|kexec|soft-reboot)\.target|(^|[^a-z0-9_.-])(telinit|init)[[:space:]]+[06]([^0-9]|$)'; then
        refuse 'reboot-family command needs --allow-reboot'
    fi
}

cmd=${1:-}
[ $# -eq 0 ] || shift
case $cmd in
exec)
    # Keep the mutex until the child ends, even if its lease expires meanwhile.
    allow_reboot=0
    case ${1:-} in --allow-reboot) allow_reboot=1; shift ;; --) shift ;; esac
    check_command "$@"
    "$@"
    ;;
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
        if mounted "$p"; then require_owned_mount "$n" "$p"; fi
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
    mkdir -p "$IN" "$STATE/files" "$STATE/orig" "$STATE/mount-ids"
    tar -C "$IN" -xf -
    for f in plan services manifest.json; do [ -f "$IN/$f" ] || die "incomplete upload: no $f"; done
    while read -r n src; do p=$(target "$n"); if mounted "$p"; then require_owned_mount "$n" "$p"; fi; case $src in '' | /* | *..*) die "bad source path for $n: $src" ;; esac; done < "$IN/plan"
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
        mount_id "$p" > "$STATE/mount-ids/$n"
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
    while read -r n; do
        p=$(target "$n")
        if mounted "$p"; then require_owned_mount "$n" "$p"; fi
    done < "$STATE/mounted"
    # Last mounted first; one name per line, names have no spaces.
    # shellcheck disable=SC2013
    for n in $(awk '{ l[NR] = $0 } END { for (i = NR; i > 0; i--) print l[i] }' "$STATE/mounted"); do
        p=$(target "$n")
        # Lazy, as in apply: the services still map these files until their restart.
        if mounted "$p"; then umount -l "$p"; fi
        ! mounted "$p" || refuse "$p still has another mount; leaving it intact"
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
        q='Type [A-Za-z_][A-Za-z0-9_]* unavailable|PropertyChanges does not support creating|state-specific objects|is not a type|is not installed|Cannot assign to non-existent|Expected token|Syntax error|QQmlComponent: Component is not ready|failed to load component|possible QQuickItem::polish\(\)|called polish\(\) inside updatePolish'
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
