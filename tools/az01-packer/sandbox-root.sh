# Shared by boot-safety-sandbox.sh and test_data_full_sandbox.sh: mounts the unpacked rootfs of a candidate
# image in the private mount namespace, with its own tmpfs /tmp /run /dev /sys /proc, the fake devicetree and
# the shims for the commands that would touch the host or a deck. Sourced, not run; the caller sets W, C, R,
# PRODUCT, has mounted nothing under R yet and has the data file system ready at "$C/data".
mount --make-rprivate /
ip link set lo up
mount -o loop "$W/image/rootfs.ext2" "$R"
for d in tmp run dev sys proc; do mount -t tmpfs tmpfs "$R/$d"; done
mount --bind "$C/data" "$R/data"
mkdir -p "$R/dev/shm" "$R/dev/pts" "$R/run/gate" "$R/run/dbus" "$R/tmp/home" \
    "$R/sys/firmware/devicetree/base" "$R/sys/devices/system/cpu"
for n in null zero full random urandom; do touch "$R/dev/$n"; mount --bind "/dev/$n" "$R/dev/$n"; done
printf '%s' "$PRODUCT" > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,%s\0rockchip,rk3288\0' "${PRODUCT,,}" > "$R/sys/firmware/devicetree/base/compatible"
for n in online possible present; do echo 0-3 > "$R/sys/devices/system/cpu/$n"; done
cat > "$R/run/gate/systemctl" <<'SH'
#!/bin/sh
echo "$*" >> /tmp/systemctl.log
case $1 in
    is-system-running) echo running ;;
    restart) touch /tmp/fallback-restart ;;
esac
SH
cat > "$R/run/gate/blocked" <<'SH'
#!/bin/sh
echo "BLOCKED $0 $*" >> /tmp/blocked.log
exit 0
SH
cat > "$R/run/gate/journalctl" <<'SH'
#!/bin/sh
echo '{"MESSAGE":"fixture accessToken=test-secret","_SYSTEMD_UNIT":"engine.service","__REALTIME_TIMESTAMP":"1794390000000000"}'
echo '{"MESSAGE":"Engine boot test: waiting for UI ready","_SYSTEMD_UNIT":"engine.service","__REALTIME_TIMESTAMP":"1794390000000001"}'
SH
cat > "$R/run/gate/dmesg" <<'SH'
#!/bin/sh
echo '[ 1.000] sandbox kernel: no physical devices exposed; peer 198.51.100.2'
SH
chmod +x "$R/run/gate/"*
# Block the vendor's absolute systemctl path as well as PATH lookups.
if [ -f "$R/usr/bin/systemctl" ]; then mount --bind "$R/run/gate/systemctl" "$R/usr/bin/systemctl"; fi
for tool in systemctl journalctl dmesg; do
    mkdir -p "$R/usr/local/sbin"
    cp "$R/run/gate/$tool" "$R/usr/local/sbin/$tool"
    target=$(chroot "$R" /bin/sh -c 'command -v "$1"' sh "$tool")
    target=$(chroot "$R" /bin/readlink -f "$target")
    # BusyBox applets share an inode: replace the applet path in our private
    # image instead of bind-mounting over the shared BusyBox executable.
    if [ -L "$R$target" ] || [ "${target##*/}" = busybox ]; then
        mkdir -p "$R/usr/local/sbin"
        cp "$R/run/gate/$tool" "$R/usr/local/sbin/$tool"
    else
        mount --bind "$R/run/gate/$tool" "$R$target"
    fi
done
for f in /sbin/reboot /usr/sbin/kexec /usr/sbin/az01-update /usr/Engine/Scripts/encrypt-fs.sh /usr/Engine/Reporter; do
    if [ -L "$R$f" ]; then
        rm "$R$f"
        cp "$R/run/gate/blocked" "$R$f"
    elif [ -f "$R$f" ]; then
        mount --bind "$R/run/gate/blocked" "$R$f"
    fi
done
mkdir -p "$R/proc/sys/net/core" "$R/proc/sys/net/ipv4" "$R/proc/sys/vm" "$R/proc/sys/fs"
for k in net/core/rmem_max net/core/wmem_max net/ipv4/tcp_rmem net/ipv4/tcp_wmem vm/swappiness fs/file-max; do echo 0 > "$R/proc/sys/$k"; done
in_root() { chroot "$R" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin "$@"; }
# The real /proc of the sandbox's pid namespace, with the serial IRQ Engine insists on (the gate does the same).
sandbox_proc() {
    umount "$R/proc"
    mount -t proc proc "$R/proc"
    printf '           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n' > "$R/run/gate/interrupts"
    mount --bind "$R/run/gate/interrupts" "$R/proc/interrupts"
    mount -t tmpfs tmpfs "$R/proc/irq"
    mkdir -p "$R/proc/irq/41"
    echo f > "$R/proc/irq/41/smp_affinity"; echo 0-3 > "$R/proc/irq/41/smp_affinity_list"
}
