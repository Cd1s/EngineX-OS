#!/usr/bin/env bash
# Called only by test_sclive4_bootstrap.sh, inside unshare -m -n -p -f.
# The only block device exposed to the stock scripts is our temporary image.
set -euo pipefail
ROOTFS=$1 IMG=$2 WORK=$3
UUID=931ad49d-ad59-0849-833a-9bf00af5b60e
TARGET="$WORK/jc11s-firstboot"
mkdir -p "$TARGET"
mount --make-rprivate /
mount --bind "$ROOTFS" "$TARGET"
mount -o remount,ro,bind "$TARGET"
mount -t tmpfs tmpfs "$TARGET/dev"
mount -t tmpfs tmpfs "$TARGET/tmp"
mount -t tmpfs tmpfs "$TARGET/run"
mkdir -p "$TARGET/dev/disk/by-partuuid" "$TARGET/tmp/safe-bin"
mknod "$TARGET/dev/null" c 1 3
mknod "$TARGET/dev/urandom" c 1 9
touch "$TARGET/dev/disk/by-partuuid/$UUID"
cat > "$TARGET/tmp/safe-bin/blocked" <<'SH'
#!/bin/sh
echo "BLOCKED $0 $*" >> /tmp/blocked.log
exit 99
SH
chmod +x "$TARGET/tmp/safe-bin/blocked"
for name in az01-update reboot systemctl; do
    ln -s blocked "$TARGET/tmp/safe-bin/$name"
done
mount --bind "$TARGET/tmp/safe-bin/blocked" "$TARGET/usr/Engine/Reporter"

LOOP=""
cleanup() {
    umount -R "$TARGET" 2>/dev/null || true
    [ -z "$LOOP" ] || losetup -d "$LOOP"
}
trap cleanup EXIT

# A real blank first boot: run the shipped formatter against an isolated loop.
truncate -s 64M "$WORK/jc11s-blank.img"
LOOP=$(losetup --find --show "$WORK/jc11s-blank.img")
mount --bind "$LOOP" "$TARGET/dev/disk/by-partuuid/$UUID"
chroot "$TARGET" /sbin/az0x-data-mkfs > "$WORK/jc11s-blank.log" 2>&1
test "$(e2label "$LOOP")" = az01-data
dumpe2fs -h "$LOOP" 2>/dev/null | grep -q 'Filesystem features:.*encrypt'
umount "$TARGET/dev/disk/by-partuuid/$UUID"
losetup -d "$LOOP"
LOOP=""

# Installed ext4: the same stock unit must preserve the bundle and boot hook.
LOOP=$(losetup --find --show "$IMG")
mount --bind "$LOOP" "$TARGET/dev/disk/by-partuuid/$UUID"
chroot "$TARGET" /sbin/az0x-data-mkfs > "$WORK/jc11s-existing.log" 2>&1
mount "$LOOP" "$TARGET/data"
test -f "$TARGET/data/Music/user_track.flac"
test -f "$TARGET/data/enginex/bundle/boot-activate.sh"
test -f "$TARGET/data/system/var/overlay/lib/alsa/conf.d/99-sc5000-custom.conf"

# Run the actual stock fscrypt implementation and encrypt-fs.sh. Reset/reboot
# paths are blocked in this namespace, including Reporter's absolute path.
chroot "$TARGET" /usr/bin/env PATH=/tmp/safe-bin:/usr/sbin:/usr/bin:/sbin:/bin \
    /bin/sh /usr/Engine/Scripts/encrypt-fs.sh > "$WORK/jc11s-encrypt.log" 2>&1
test ! -f "$TARGET/tmp/blocked.log"
for name in downloads stems; do
    chroot "$TARGET" /bin/sh -c 'fscryptctl get_policy "$1"' sh "/data/$name" >> "$WORK/jc11s-encrypt.log"
done
test -f "$TARGET/data/Music/user_track.flac"
test -f "$TARGET/data/sc5000-custom/bundle/boot-activate.sh"
test -f "$TARGET/data/system/var/overlay/lib/alsa/conf.d/99-sc5000-custom.conf"
echo '  JC11S stock mkfs (blank/existing) and fscrypt first boot: PASS'
