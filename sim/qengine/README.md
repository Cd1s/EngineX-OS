# QEngine Test Tier (Tier 2)

Automated full-system emulation test tier for Engine OS and EngineX firmware images using QEMU and upstream QEngine.

## Overview

Unlike chroot and unshare container sandboxes, the QEngine tier boots the complete Linux kernel and systemd (PID 1) in QEMU to verify:
- live systemd unit startup ordering, dependencies, and critical chains (`sc5000-custom-prepare`, `engine-guardian`, `stream-sync`, `engine`);
- UI hook injection and QML pack replacement under live `engine.service`;
- HTTP API health (`stream-sync` UI endpoint at `127.0.0.1:8080/v1/sc5000/ui`);
- absence of failed systemd units and unit journal errors.

Upstream is [Peyton-C/QEngine](https://github.com/Peyton-C/QEngine) (pinned at `9c2dfd6f206c2e2c3d77cfd1ac483350c68a77c8`), cloned on demand at runtime and not vendored.

## Host Architecture & Requirements

Tier 2 full-system emulation runs on an x86_64 host (such as the AMD build host). It explicitly refuses execution on native ARM/aarch64 hosts to prevent registering `qemu-arm` binfmt, which would silently interfere with native 32-bit armhf execution required by the release gate.

Host requirements on the AMD build host:
- `docker.io`
- `qemu-system-arm` (providing `qemu-system-aarch64`)
- `qemu-utils` (`qemu-img`)
- `qemu-user-static`, `binfmt-support`
- `file`, `e2fsprogs` (`dumpe2fs`, `debugfs`)

## Measured Boot Performance

Measured boot-to-Engine times on the AMD build host (16 vCPU AMD EPYC Zen 4, 62 GB RAM, Debian 13):

### QEMU Configuration Tuning (JP07 SC5000)
- Single-thread TCG baseline (`-accel tcg -smp 4 -cpu cortex-a15`): **154.67s**
- Multi-Threaded TCG (`-accel tcg,thread=multi -smp 4 -cpu cortex-a15`): **155.29s**
- MTTCG with 8 vCPUs (`-accel tcg,thread=multi -smp 8 -cpu cortex-a15`): **184.70s** (slower due to ARMv7 atomic lock contention)
- MTTCG with larger translation cache (`-accel tcg,thread=multi,tb-size=1024 -smp 4 -cpu cortex-a15`): **151.17s** (fastest stable configuration)
- tmpfs disk storage with `cache=unsafe`: **156.68s** (overhead of initial copy exceeds RAM cache savings, as host filesystem page cache already backs images in RAM)
- `-cpu max`: Unstable under 32-bit ARM virt guest kernel due to udev/TLB flush broadcast race conditions; `cortex-a15` is fully stable and matches physical hardware.

### Firmware Benchmarks (Fastest Stable Config)
- **JP07** (SC5000): **151.17s**
- **JP13** (SC6000): **151.67s**
- **JC11** (PRIME 4): **174.68s**

## Usage

Run the tier against any stock or EngineX `*-Update.img`:

```bash
sim/qengine/tier2.sh /path/to/PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img
```

Options:
- `--timeout <seconds>`: Maximum boot wait time (default: 450s).
- `--keep`: Retain built QEngine instance directory on disk after testing.

## Adapter Architecture

The adapter applies test-side rootfs modifications inside the emulated image copy (isolated via `unshare -m`) without modifying release artifacts:
1. **Drop-in preservation**: Upstream QEngine writes `engine.service.d/override.conf`. The test adapter preserves EngineX's `override.conf` and moves QEngine's hardware shims into `99-qengine.conf`.
2. **LD_PRELOAD preservation**: Configures `/etc/ld.so.preload` and patches the test copy of `/etc/sc5000/engine-ui-guard` to ensure QEngine's hardware emulation shims (`dtshim`, `drmatomic`, `alsashim`, `quitshim`) remain loaded when `engine-ui-guard` unsets and sets `LD_PRELOAD`.
3. **Automated probe access**: Configures `systemd-networkd` DHCP, seeds automated public keys for OpenSSH, and enables serial autologin for automated probe execution.
