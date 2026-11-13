# QEngine Test Tier (Tier 2)

Automated full-system emulation test tier for Engine OS and EngineX firmware images using QEMU and upstream QEngine.

## Overview

Unlike chroot and unshare sandboxes, the QEngine tier boots the full Linux kernel and systemd (PID 1) in QEMU to verify:
- systemd unit startup ordering, dependencies, and critical chains (`sc5000-custom-prepare`, `engine-guardian`, `stream-sync`, `engine`);
- UI hook injection and QML pack replacement under live `engine.service`;
- HTTP API health (`stream-sync` UI endpoint);
- absence of failed systemd units and unit journal errors.

Upstream is [Peyton-C/QEngine](https://github.com/Peyton-C/QEngine) (pinned at `9c2dfd6f206c2e2c3d77cfd1ac483350c68a77c8`), cloned on demand at runtime and not vendored.

## Requirements

The test host (e.g. `Oracle-Bogota-arm`) requires:
- `docker.io`
- `qemu-system-arm` (providing `qemu-system-aarch64`)
- `qemu-utils` (`qemu-img`)
- `file`, `e2fsprogs` (`dumpe2fs`, `debugfs`)
- `qemu-user-static` / `qemu-user-binfmt` (for `linux/arm/v7` containers on arm64 hosts)

## Usage

Run the tier against any stock or EngineX `*-Update.img`:

```bash
sim/qengine/tier2.sh /path/to/PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img
```

Options:
- `--timeout <seconds>`: Maximum boot wait time (default: 450s).
- `--keep`: Retain built QEngine instance directory on disk after testing.

## Adapter Architecture

The adapter applies test-side rootfs modifications inside the emulated image copy without modifying release artifacts:
1. **Drop-in preservation**: Upstream QEngine writes `engine.service.d/override.conf`. The test adapter preserves EngineX's `override.conf` and moves QEngine's hardware shims into `99-qengine.conf`.
2. **LD_PRELOAD preservation**: Configures `/etc/ld.so.preload` and patches the test copy of `/etc/sc5000/engine-ui-guard` to ensure QEngine's hardware emulation shims (`dtshim`, `drmatomic`, `alsashim`, `quitshim`) remain loaded when `engine-ui-guard` unsets and sets `LD_PRELOAD`.
3. **Automated probe access**: Configures `systemd-networkd` DHCP, OpenSSH server, and serial autologin for automated probe execution.
