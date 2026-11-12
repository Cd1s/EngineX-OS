# SC5000 storage and RK3288 runtime overlay

This tree is merged into the unpacked Engine OS 5.0.4 rootfs by the firmware
packaging workflow. It does not change the build host. Kernel artifacts and
`fsck.hfsplus` come from [kernel-modules](../kernel-modules/README.md).

## Storage lifecycle

Engine OS mounts removable media through `edisksd`, and Engine follows edisksd
over D-Bus (`/io/github/inmusicdev/edisksd/dev/<dev>`). A second mounter races
that state machine, so this tree never mounts, checks or writes block devices.
`udev` starts `sc5000-storage@<dev>.service` after blkid probing; the worker
waits for edisksd's mountpoint and then:

logs the result. It does not write databases.

`edisksd` runs with `/etc/sc5000/libedisksd-hook.so` preloaded (source and
`build.sh` in `tools/edisksd-hook`). edisksd answers Engine's Mount call only
after `mnt_context_mount()` returns, so the hook finishes within that call:

1. read-only HFS+ with `PIONEER/` or `.PIONEER/` gets a writable view on
   `<mount>/Engine Library`, kept in `/data/enginex/hfs-libraries/<volume id>`
   (only the `/data/sc5000-custom -> enginex` compat link is followed on the way);
2. `rb-bridge --mount-path <mount>` converts the export;
3. `engine-guardian --heal-mount <mount> --data-only` repairs table data.

The steps share a 20 s budget. An unfinished import is killed and rolled back,
so a library too large to convert in that time stays unconverted.

Nothing may write these databases once Engine has attached. Concurrent writers
produced `SQLITE_CORRUPT` and index damage on real media, and Engine then deleted
`m.db`. So `rb-bridge --watch` and `engine-guardian --watch` exit at once (the
player's base `/etc` still enables both from older images),
`engine-guardian.service` is not enabled, `rb-bridge.service` runs only
`--prolink` (receives Pro DJ Link, writes no database), and hook repairs do not alter
Engine's views or triggers, which Engine compares on attach.

HFS+ must stay read-only. On the 6.6.119 RT kernel `hfsplus.ko` hits
`kernel BUG at mm/highmem.c:553` (`hfsplus_mkdir`) on the first write; with
`panic_on_oops=1` the player reboots, and the interrupted write corrupted a
catalog node on real media. Therefore `/etc/edisksd/main.conf` has no
`[hfsplus] force` option, and dirty HFS+ volumes are never "cleaned" by
editing the volume header. OverlayFS also refuses HFS+ as a lower layer
(case-insensitive dentry ops), so the overlay's lower layer is empty: it hides
any Engine Library already on the stick, and library changes live on the
player, not on the stick.

The scripts are POSIX `sh`: the stock rootfs has BusyBox ash, no bash and no
`xz` (BusyBox `xzcat` unpacks `sc5000-suite.xz`).

## CPU and I/O policy

| Work | CPUs | Mechanism |
| --- | --- | --- |
| ALSA/I2S/SPDIF IRQs and their RT IRQ threads | 0 | IRQ mask `1`, task affinity |
| Existing FIFO/RR Engine audio threads | 0 | comm-name match plus policy check |
| Other `/usr/Engine/` threads, including GUI/waveform | 1-2 | per-thread affinity |
| Bridge, stream-sync, ssh/sshd and mount workers | 3 | service CPUAffinity, background slice |

The background slice has `AllowedCPUs=3`, `CPUWeight=10`, `IOWeight=10`.
The optional Engine slice has `AllowedCPUs=0-2` and higher foreground weights.
Use cgroup v2 with cpuset/cpu/io controllers for these limits; weights depend
on controller and device support. Affinity also works without weight support.
These settings do not reserve CPU0 against every stock daemon or unmovable
kernel interrupt, and do not replace a verified RT kernel/boot policy.

`sc5000-perf-tune` sets CPU0-3 to `ondemand` with a 1.2 GHz minimum (lower
clocks caused ALSA xruns), `up_threshold` 50 and `sampling_down_factor` 10, and the `ffa30000.gpu`
devfreq to `simple_ondemand` from 300 MHz (its lowest clock on battery or above 85 C). IRQs:
audio on CPU0, GPU/VOP/HDMI on CPU1 (Engine's GUI core), everything else (touch I2C, MMC, USB,
eth0) on CPU2. Engine's SceneGraph thread moves to CPU2 and its normal-priority helpers on CPU1
to CPUs 0 and 3 (again 20 s and 60 s after each Engine start); EMain and real-time threads stay. It writes sysctl
values (swappiness 10, dirty background 5, dirty ratio 15, file-max 1048576),
and selects `mq-deadline`, otherwise `none`, when advertised, except on devices
where Engine already chose `bfq`. USB and SD devices read ahead 1 MiB (LC003). Movable IRQs and their
PREEMPT_RT threads are placed by name; `/etc/default/sc5000-perf` holds the
patterns.

The service runs at boot and after each Engine start, because Engine's setup
script sets the GPU back to `performance`. Missing,
read-only or disappearing knobs and exited threads are nonfatal. The service
conflicts with irqbalance to prevent it from undoing the IRQ policy. Enable
symlinks for discovery and tuning are included in this tree.

## Dependencies and validation

Runtime: BusyBox sh, systemd/udev, util-linux findmnt/flock/taskset and
edisksd from the stock image. Stock kernel exFAT/NTFS3 and
SD filesystem support must also be present. The internal cache must have
enough persistent free space and OverlayFS-compatible xattrs/d_type.

From the repository root:

```bash
set -o pipefail
bash -n kernel-modules/build_modules.sh rootfs-overlay/usr/local/bin/sc5000-storage-mount rootfs-overlay/usr/local/bin/sc5000-perf-tune | cut -c1-200 | head -40
bash tests/test_rootfs_overlay.sh | cut -c1-200 | head -40
```

Tests use private command/sysfs/procfs fixtures inside the worktree. They do
not mount filesystems, write real sysctls, alter host services or require root.
They cover branch selection, mount options/order, persistent caches, rollback,
bridge errors/timeouts, device safety, clock/IRQ/thread tuning, repeat runs,
and missing facilities. They do not establish real hardware latency, driver
ABI compatibility, journal repair correctness, or stock Engine notification.
Complete those checks in QEngine and then on disposable device/media before
packaging a production image. No UI, bootloader or MCU firmware is modified.
