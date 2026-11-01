# SC5000 storage and RK3288 runtime overlay

This tree is merged into the unpacked Engine OS 5.0.4 rootfs by the firmware
packaging workflow. It does not change the build host. Kernel artifacts and
`fsck.hfsplus` come from [kernel-modules](../kernel-modules/README.md).

## Storage lifecycle

`udev` probes filesystems using its blkid builtin, then activates
`sc5000-storage@<kernel-device>.service`. Mounts run in the host namespace,
outside udev's private namespace. Device removal stops the worker through
`BindsTo=dev-<device>.device`; `ExecStop` unmounts the public view before its
APFS lower. Busy mounts are preserved; no lazy or forced unmount is used.
Coldplug discovery processes MMC devices before USB devices. A global flock
serializes discovery, hotplug, conversion and ejection.

The front cache candidate must be `mmcblk*` with sysfs `device/type=SD`.
Soldered eMMC (`MMC`) is excluded, including its partitions. Set
`SC5000_FRONT_SD_DEVICE=mmcblkN` in `/etc/default/sc5000-storage` if more than
one removable MMC slot exists. The first writable, supported SD filesystem
is mounted at `/media/sc5000-sd-cache`, with:

```text
Engine Library/
Downloads/
apfs-overlays/<filesystem-UUID>/{upper,work,view-upper,view-work}/
```

No disk is formatted. Use ext4 or f2fs on the permanent SD for an OverlayFS
upper. FAT/exFAT/NTFS/HFS+ SD cards still supply a library/download area, but
APFS uppers use internal storage because those filesystems lack the required
upperdir semantics. APFS-formatted SD is handled as an external APFS volume,
using internal cache, rather than becoming the writable cache repository.

HFS+ mounting loads `hfsplus`, runs `fsck.hfsplus -f <device>` with closed
stdin, and permits `force,rw,uid=1000,gid=1000,umask=0000` only after status
**0**. This pinned hfsprogs returns nonzero for usage/dirty/check failures;
all are rejected. The device must be unmounted first. Force-RW does not add
macOS journal replay to Linux: cleanly eject Mac media and validate HFS+
repair behavior with disposable media before production use.

APFS mounting loads `apfs` and `overlay`, then:

1. Mounts the external filesystem **read-only** at
   `/run/sc5000-storage/lower/<device>`, with `nodev,nosuid`.
2. Creates library `upper`/`work` and root-view `view-upper`/`view-work` pairs
   together under the SD UUID directory, or under
   `/var/cache/sc5000/apfs-overlays/<UUID>` if SD is absent/incompatible.
   ext4/f2fs/xfs or a compatible writable internal overlay filesystem is needed.
3. Mounts a whole-volume OverlayFS view at `/media/sc5000-<device>` using
   `view-upper`/`view-work`, then mounts a dedicated library OverlayFS at
   `<public-path>/Engine Library` using `upper`/`work`. The library lower is
   the APFS `Engine Library` directory, or an empty runtime directory when
   the original disk lacks one. Directory creation happens in the root
   view's upper, never on the original disk. Music and `PIONEER` remain
   readable through the public view.
4. Calls `/usr/local/bin/rb-bridge --mount-path <public-path>` when
   `PIONEER/rekordbox/export.pdb` exists, then invokes the notification hook.

The root view keeps track paths stable for bridge/database clients; writes
outside `Engine Library` stay in `view-upper`, while database/cue/history
writes use the dedicated library `upper`. Ejection unmounts library, root
view, then APFS lower. If a track keeps the root view busy after library
unmount succeeds, the worker restores the library overlay before reporting
busy, preserving access to saved cues.
An existing internal UUID cache remains in use when a new SD is inserted,
so adding an empty SD cannot hide previously saved cues. There is no live
migration. Eject APFS volumes before SD. Clone UUIDs cannot use the same
upper/work pair concurrently. No upper/work cache is deleted on eject.
Malformed UUIDs and symlinks in cache/library paths are rejected.

`rb-bridge` inherits CPU3 and the worker's background resource weights.
`SC5000_BRIDGE_TIMEOUT` defaults to `10s`, with a two-second kill grace period.
Bridge failure/timeout preserves access to original music and still invokes
notification. This suite controls call ordering; actual conversion speed
depends on the separately supplied bridge implementation and library size.
FAT/exFAT use kernel mounts; NTFS explicitly uses the kernel `ntfs3` driver.

## Required Engine integration before firmware release

The current repository contains no stock Engine automounter protocol or
confirmed Engine service name. Therefore:

- Install a firmware-specific executable at
  `/usr/local/libexec/sc5000-storage-notify`, accepting `--mount-path <path>`.
  It must publish the completed mount to Engine. It is called after the
  bridge. The generic worker deliberately does not invent a DBus signal or
  process restart. Without this adapter, the volume mounts but Engine
  notification is not implemented.
- Disable/route the stock automounter for devices claimed by this worker.
  `UDISKS_IGNORE=1` handles UDisks only; it cannot suppress a proprietary
  Engine watcher. Engine must consume completed worker notifications rather
  than discover the mount before conversion finishes. The worker refuses an
  already mounted source, preventing fsck against a live filesystem.
- In the **verified stock Engine service**, set
  `Slice=sc5000-engine.slice`. This provides CPUs 0-2 for the process's audio
  and GUI threads. Do not set process-wide `CPUAffinity=1 2`, which would
  interfere with audio on CPU0. If the stock service has an existing cpuset,
  ensure it permits CPUs 0-2.

These adapters must be validated against the unpacked firmware/QEngine;
their completion cannot be inferred from shell mocks.

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

`sc5000-perf-tune` sets CPU0-3 governors to `performance`, raises their minimum
to `cpuinfo_max_freq`, and selects GPU performance/maximum minimum frequency
at `ffa30000.gpu` devfreq. Kernel thermal protections still apply. It writes
sysctl values (swappiness 10, dirty background 5, dirty ratio 15, file-max
1048576), and selects `mq-deadline`, otherwise `none`, when advertised.
Movable non-audio IRQs use mask `e` (CPUs 1-3); managed IRQ rejection is
nonfatal. `/etc/default/sc5000-perf` controls IRQ/audio-thread name patterns.
Confirm actual firmware thread names; only existing FIFO/RR audio threads
are pinned to CPU0. Scheduling policy/priority is never escalated.

The service runs at boot and a timer reapplies it after two seconds, then
15 seconds after each run, covering newly created Engine threads. Missing,
read-only or disappearing knobs and exited threads are nonfatal. The service
conflicts with irqbalance to prevent it from undoing the IRQ policy. Enable
symlinks for discovery, tuning and its timer are included in this tree.

## Dependencies and validation

Runtime: Bash, systemd/udev with blkid APFS probing, util-linux mount/umount,
blkid/findmnt/flock/mountpoint/taskset, kmod, coreutils timeout/chown/chmod,
`fsck.hfsplus`, and matching filesystem modules. Stock kernel exFAT/NTFS3 and
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
