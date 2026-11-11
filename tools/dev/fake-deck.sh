#!/bin/bash
# A fake first-generation deck for testing deck-push on the build host (root only):
#   bash tools/dev/fake-deck.sh up <new dir>      # prints the path of its decks.ini
#   bash tools/dev/fake-deck.sh reboot <dir>      # /run and every bind mount go, /data and /etc/sc5000 stay
#   bash tools/dev/fake-deck.sh down <dir>        # stops it and removes <dir>
# The deck is a chroot onto an overlay of the host's / in its own mount, pid and
# network namespaces, so nothing it mounts or writes reaches the host. /data and
# /etc/sc5000 live in <dir>/persist like the deck's flash; /run is tmpfs. ssh
# reaches its sshd -i through nsenter (ProxyCommand), so no port is opened.
# systemctl, journalctl and Engine (EMain) are stand-ins from tools/dev/fake-deck/;
# the service wrappers are the real ones from rootfs-overlay.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
FIX=$HERE/fake-deck

inner() { # runs as pid 1 of the new namespaces
    local D=$1 R=$1/root
    mount --make-rprivate /
    mount -t tmpfs tmpfs "$D/t"
    mkdir -p "$D/t/upper" "$D/t/work"
    mount -t overlay overlay -o "lowerdir=/,upperdir=$D/t/upper,workdir=$D/t/work" "$R"
    mount -t proc proc "$R/proc"
    mount --rbind /dev "$R/dev"
    mount -t tmpfs tmpfs "$R/run"
    mount -t tmpfs tmpfs "$R/tmp"
    mkdir -p "$R/data" "$R/etc/sc5000"
    mount --bind "$D/persist/data" "$R/data"
    mount --bind "$D/persist/etc-sc5000" "$R/etc/sc5000"
    mkdir -p "$R/usr/local/lib/fake-deck" "$R/usr/Engine" "$R/usr/share/sc5000-custom" "$R/etc/fake-deck"
    install -m 0755 "$FIX/systemctl" "$FIX/journalctl" "$R/usr/local/sbin/"
    install -m 0755 "$FIX/EMain" "$R/usr/Engine/EMain"
    install -m 0755 "$FIX/daemon" "$R/usr/local/lib/fake-deck/daemon"
    install -m 0644 "$FIX/api.py" "$R/usr/local/lib/fake-deck/api.py"
    for f in sc5000-suite-exec stream-sync sc5000-deemixd rb-bridge; do
        install -m 0755 "$REPO/rootfs-overlay/usr/local/bin/$f" "$R/usr/local/bin/$f"
    done
    cp "$D/persist/sc5000-suite.xz" "$R/usr/share/sc5000-custom/sc5000-suite.xz"
    cp "$D/sshd_config" "$D/host_key" "$D/client_key.pub" "$R/etc/fake-deck/"
    # sshd refuses a locked root account ("!" in shadow) without PAM; only the overlay changes.
    sed -i 's/^root:![^:]*:/root:*:/' "$R/etc/shadow"
    mkdir -p "$R/run/sshd" "$R/run/fake-journal"
    ip link set lo up
    # shellcheck disable=SC2016  # expanded by the chroot's shell
    exec chroot "$R" /bin/sh -c '
        for u in edisksd sc5000-deemixd stream-sync rb-bridge engine; do systemctl start "$u.service"; done
        exec python3 -I -c "
import os, signal, time
signal.signal(signal.SIGTERM, lambda *a: os._exit(0))
while True:
    try:
        os.waitpid(-1, 0)
    except ChildProcessError:
        time.sleep(1)
"'
}

start() {
    local D=$1 pid
    mkdir -p "$D/t" "$D/root"
    unshare -m -n -p -f --kill-child "$0" --inner "$D" > "$D/inner.log" 2>&1 &
    echo $! > "$D/unshare.pid"
    for _ in $(seq 50); do
        pid=$(pgrep -P "$(cat "$D/unshare.pid")" || :)
        [ -n "$pid" ] && [ "$(readlink "/proc/$pid/root")" = "$D/root" ] && break
        sleep 0.2
    done
    [ -n "$pid" ] || { cat "$D/inner.log" >&2; echo "fake deck did not start" >&2; exit 1; }
    cat > "$D/decks.ini" <<INI
[defaults]
state_dir = $D/state
work_dir = $D/work

[fake]
ssh = root@fake-deck
ssh_options = -F /dev/null -i $D/client_key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o "ProxyCommand=nsenter -t $pid -m -n -p -r -w /usr/sbin/sshd -i -f /etc/fake-deck/sshd_config"
smoke_timeout = 30
settle = 1
INI
    mkdir -p "$D/state" "$D/work"
    # Wait for its stream-sync and Engine like a booting deck.
    for _ in $(seq 50); do
        nsenter -t "$pid" -m -n -p -r -w sh -c 'wget -q -O /dev/null http://127.0.0.1:8080/v1/sc5000/ui 2>/dev/null || curl -sf -o /dev/null http://127.0.0.1:8080/v1/sc5000/ui' && break
        sleep 0.2
    done
    echo "$D/decks.ini"
}

stop() {
    local D=$1
    [ -f "$D/unshare.pid" ] || return 0
    kill "$(cat "$D/unshare.pid")" 2>/dev/null || :
    for _ in $(seq 50); do kill -0 "$(cat "$D/unshare.pid")" 2>/dev/null || break; sleep 0.1; done
    rm -f "$D/unshare.pid"
}

case ${1:-} in
--inner) inner "$2" ;;
up)
    D=$(realpath -m "${2:?dir}")
    [ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 2; }
    [ ! -e "$D" ] || { echo "$D exists" >&2; exit 2; }
    mkdir -p "$D/persist/data/enginex/ui/packs/en/Skins/Embedded" "$D/persist/data/enginex/ui/icons" \
        "$D/persist/data/enginex/bin" "$D/persist/etc-sc5000"
    ln -s enginex "$D/persist/data/sc5000-custom"
    echo 'import QtQuick 2.15 // original pack' > "$D/persist/data/enginex/ui/packs/en/Skins/Embedded/EmbeddedMain.qml"
    echo '<svg/>' > "$D/persist/data/enginex/ui/icons/original.svg"
    echo 5.3.0.4 > "$D/persist/etc-sc5000/version.txt"
    cp "$REPO/rootfs-overlay/etc/sc5000/libengine-ui-hook.so" "$REPO/rootfs-overlay/etc/sc5000/libedisksd-hook.so" "$D/persist/etc-sc5000/"
    install -m 0755 "$FIX/sc5000-suite" "$D/persist/data/enginex/bin/sc5000-suite"
    # The suite wrapper keeps a binary whose .id matches the image's archive.
    xz -c "$FIX/sc5000-suite" > "$D/persist/sc5000-suite.xz"
    sha256sum < "$D/persist/sc5000-suite.xz" | cut -d' ' -f1 > "$D/persist/data/enginex/bin/sc5000-suite.id"
    ssh-keygen -q -t ed25519 -N '' -f "$D/host_key"
    ssh-keygen -q -t ed25519 -N '' -f "$D/client_key"
    printf '%s\n' 'HostKey /etc/fake-deck/host_key' 'AuthorizedKeysFile /etc/fake-deck/client_key.pub' \
        'PermitRootLogin prohibit-password' 'PasswordAuthentication no' 'KbdInteractiveAuthentication no' \
        'UsePAM no' 'StrictModes no' 'PidFile none' > "$D/sshd_config"
    start "$D"
    ;;
reboot)
    D=$(realpath "${2:?dir}")
    stop "$D"
    start "$D"
    ;;
down)
    D=$(realpath -m "${2:?dir}")
    stop "$D"
    case $D in /var/tmp/* | /tmp/*) rm -rf "$D" ;; *) echo "left $D in place (not under /var/tmp or /tmp)" ;; esac
    ;;
*)
    sed -n '2,5p' "$0" >&2
    exit 2
    ;;
esac
