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
- `util-linux` (`losetup`, `findmnt`, `mount`, `umount`)

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

The adapter extracts a separate rootfs for each instance and modifies only that copy. Release images and tracked overlays remain unchanged. Its loop mount is under a private per-run temporary directory; it does not bind the host's `/dev`, `/proc`, `/sys` or `/run` into the image.
1. **Drop-in preservation**: Upstream QEngine writes `engine.service.d/override.conf`. The test adapter preserves EngineX's `override.conf` and moves QEngine's hardware shims into `99-qengine.conf`.
2. **Engine preload**: Appends QEngine's hardware shims (`dtshim`, `drmatomic`, `alsashim`, `quitshim`) to `LD_PRELOAD` in the instance's `engine-ui-guard`. It does not configure global `/etc/ld.so.preload`, so OpenSSH's preauth sandbox does not load those shims.
3. **Automated probe access**: Configures `systemd-networkd` DHCP, seeds a test SSH public key, removes unsupported `UsePAM` directives in the instance, and enables serial autologin. Hardware-only `az0x-info`, `az0x-loadpin-trustpoints` and `az0x-setup-hostname` units are masked in the instance.

Upstream host scripts execute directly and use Docker (including a privileged rootfs builder). A per-run Docker wrapper records real container IDs for cleanup. The adapter redirects upstream host temporary files into its own directory and arms loop detach immediately after allocation, before mounting. Cleanup removes recorded containers, detaches the instance's loops, and checks for remaining mounts before deleting its directories; a directory with remaining mounts is retained and reported as an error.

`python3 -m unittest discover -s sim/qengine -p test_smoke.py` checks the architecture guard and cleanup after a failed builder without running QEMU. The previous `run_qengine.sh` QMP timeout/reconnect controller and its test were retired when Tier 2 replaced that launcher; no QMP reconnect coverage is claimed.
